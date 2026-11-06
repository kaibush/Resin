package routing

import (
	"crypto/sha256"
	"fmt"
	"math/rand/v2"
	"net/netip"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
)

type stickyIPBinding struct {
	IP          netip.Addr
	UpdatedAtNs int64
}

// rememberStickyIP is called under the existing per-account lease lock. Reads
// are O(1) and unchanged exits never write, even after expiry or restart.
func (r *Router) rememberStickyIP(plat *platform.Platform, state *PlatformRoutingState, account string, ip netip.Addr, updatedAtNs int64) error {
	if !plat.StickyReuseIP {
		return nil
	}
	ip = ip.Unmap()
	digest := sha256.Sum256([]byte(account))
	if old, ok := state.stickyIPs.Load(digest); ok && old.IP == ip {
		return nil
	}
	entry := model.StickyIP{PlatformID: plat.ID, AccountHash: digest, EgressIP: ip, UpdatedAtNs: updatedAtNs}
	if r.saveStickyIP != nil {
		if err := r.saveStickyIP(entry); err != nil {
			return fmt.Errorf("persist sticky IP: %w", err)
		}
	}
	state.stickyIPs.Store(digest, stickyIPBinding{ip, updatedAtNs})
	return nil
}

// RestoreStickyIP is used only during bootstrap, before serving requests.
func (r *Router) RestoreStickyIP(entry model.StickyIP) {
	if _, ok := r.pool.GetPlatform(entry.PlatformID); ok && entry.EgressIP.IsValid() {
		r.ensurePlatformState(entry.PlatformID).stickyIPs.Store(entry.AccountHash, stickyIPBinding{entry.EgressIP.Unmap(), entry.UpdatedAtNs})
	}
}

// ForgetPlatformStickyIPs releases the in-memory index after platform deletion.
// The database removes its rows via the platform foreign key.
func (r *Router) ForgetPlatformStickyIPs(platformID string) {
	if state, ok := r.states.Load(platformID); ok {
		state.stickyIPs.Clear()
	}
}

func preferredStickyIP(state *PlatformRoutingState, account string, previousIP netip.Addr) netip.Addr {
	// A still-retained lease is newer than an affinity saved before disabling.
	if previousIP.IsValid() {
		return previousIP.Unmap()
	}
	ip, _ := state.stickyIPs.Load(sha256.Sum256([]byte(account)))
	return ip.IP
}

// selectStickyIPRoute uses the versioned IP index, never a per-request pool or
// account scan. After the normal P2C pick, failover visits only same-IP nodes.
// Verification is outside allocator locks and can invalidate the cached view.
func (r *Router) selectStickyIPRoute(plat *platform.Platform, stats *IPLoadStats, ip netip.Addr, target string) (node.Hash, *node.NodeEntry, bool) {
	if !ip.IsValid() {
		return node.Zero, nil, false
	}
	nodes := stats.candidateNodes(plat.View(), r.pool)[ip]
	if len(nodes) == 0 {
		return node.Zero, nil, false
	}
	first := nodes[0]
	if len(nodes) > 1 {
		rng := randomRouteRNGPool.Get().(*rand.Rand)
		first = bestNodeWithinIP(nodes, plat, stats, r.pool, target, r.authorities(), r.p2cWindow(), rng)
		randomRouteRNGPool.Put(rng)
	}
	check := func(h node.Hash) (*node.NodeEntry, bool) {
		entry, ok := r.pool.GetEntry(h)
		if !ok || !plat.View().Contains(h) || !entry.IsHealthy() || entry.GetEgressIP().Unmap() != ip {
			return nil, false
		}
		if r.verifyNode != nil {
			if err := r.verifyNode(plat, h); err != nil {
				return nil, false
			}
		}
		entry, ok = r.pool.GetEntry(h)
		if !ok || !plat.View().Contains(h) || !entry.IsHealthy() || entry.GetEgressIP().Unmap() != ip {
			return nil, false
		}
		return entry, true
	}
	if entry, ok := check(first); ok {
		return first, entry, true
	}
	for _, h := range nodes {
		if h != first {
			if entry, ok := check(h); ok {
				return h, entry, true
			}
		}
	}
	return node.Zero, nil, false
}

// reserveStickyIP matches the normal allocator's accounting but does not
// compare historical loads: continuity takes precedence over the soft limit.
func (r *Router) reserveStickyIP(plat *platform.Platform, stats *IPLoadStats, ip netip.Addr) {
	r.allocationMu.Lock()
	defer r.allocationMu.Unlock()
	stats.allocationMu.Lock()
	defer stats.allocationMu.Unlock()
	stats.Reserve(ip)
	r.globalIPStats.Reserve(ip)
	if plat.IPGovernanceEnabled {
		r.ipHistory.counts.Reserve(ip)
	}
}
