package routing

import (
	"crypto/sha256"
	"fmt"
	"math/rand/v2"
	"net/netip"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
	"github.com/puzpuzpuz/xsync/v4"
)

type stickyIPBinding struct {
	IP          netip.Addr
	UpdatedAtNs int64
}

// rememberStickyIP serializes writes per marker across platforms. Existing
// lease hits are O(1); same-IP renewals only advance the in-memory generation.
// Older leases and delayed cleaners cannot overwrite a newer global binding.
func (r *Router) rememberStickyIP(plat *platform.Platform, account string, ip netip.Addr, updatedAtNs int64) error {
	if !plat.StickyReuseIP {
		return nil
	}
	ip = ip.Unmap()
	digest := sha256.Sum256([]byte(account))
	if old, ok := r.stickyIPs.Load(digest); ok && old.UpdatedAtNs >= updatedAtNs {
		return nil
	}
	var saveErr error
	r.stickyIPs.Compute(digest, func(old stickyIPBinding, loaded bool) (stickyIPBinding, xsync.ComputeOp) {
		if loaded && old.UpdatedAtNs >= updatedAtNs {
			return old, xsync.CancelOp
		}
		if (!loaded || old.IP != ip) && r.saveStickyIP != nil {
			entry := model.StickyIP{AccountHash: digest, EgressIP: ip, UpdatedAtNs: updatedAtNs}
			if saveErr = r.saveStickyIP(entry); saveErr != nil {
				return old, xsync.CancelOp
			}
		}
		return stickyIPBinding{ip, updatedAtNs}, xsync.UpdateOp
	})
	if saveErr != nil {
		return fmt.Errorf("persist sticky IP: %w", saveErr)
	}
	return nil
}

// RestoreStickyIP is used only during bootstrap, before serving requests.
// It intentionally requires neither a surviving platform nor a live node.
func (r *Router) RestoreStickyIP(entry model.StickyIP) {
	if entry.EgressIP.IsValid() {
		r.stickyIPs.Store(entry.AccountHash, stickyIPBinding{entry.EgressIP.Unmap(), entry.UpdatedAtNs})
	}
}

func (r *Router) preferredStickyIP(account string, previous Lease) netip.Addr {
	if saved, ok := r.stickyIPs.Load(sha256.Sum256([]byte(account))); ok {
		if !previous.EgressIP.IsValid() || saved.UpdatedAtNs > previous.CreatedAtNs {
			return saved.IP
		}
	}
	return previous.EgressIP.Unmap()
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
