package routing

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/puzpuzpuz/xsync/v4"
)

type ipHistoryKey struct {
	IP          netip.Addr
	AccountHash string
}

type ipHistory struct {
	allocationMu *sync.Mutex
	maintenance  sync.RWMutex
	entries      *xsync.Map[netip.Addr, *xsync.Map[[sha256.Size]byte, struct{}]]
	counts       *IPLoadStats
	save         func([]model.IPHistory) error
	remove       func(string) error
}

func newIPHistory(save func([]model.IPHistory) error, remove func(string) error, allocationMu *sync.Mutex) *ipHistory {
	return &ipHistory{entries: xsync.NewMap[netip.Addr, *xsync.Map[[sha256.Size]byte, struct{}]](), counts: NewIPLoadStats(), save: save, remove: remove, allocationMu: allocationMu}
}

func (h *ipHistory) accounts(ip netip.Addr) *xsync.Map[[sha256.Size]byte, struct{}] {
	accounts, _ := h.entries.LoadOrCompute(ip.Unmap(), func() (*xsync.Map[[sha256.Size]byte, struct{}], bool) {
		return xsync.NewMap[[sha256.Size]byte, struct{}](), false
	})
	return accounts
}

func (h *ipHistory) contains(key ipHistoryKey) bool {
	accounts, ok := h.entries.Load(key.IP)
	if !ok {
		return false
	}
	digest, valid := decodeHistoryDigest(key.AccountHash)
	if !valid {
		return false
	}
	_, ok = accounts.Load(digest)
	return ok
}

func decodeHistoryDigest(value string) ([sha256.Size]byte, bool) {
	var digest [sha256.Size]byte
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != len(digest) {
		return digest, false
	}
	copy(digest[:], decoded)
	return digest, true
}

func historyEntry(account string, ip netip.Addr, firstSeen int64) model.IPHistory {
	digest := sha256.Sum256([]byte(account))
	return model.IPHistory{EgressIP: ip.Unmap().String(), AccountHash: hex.EncodeToString(digest[:]), FirstSeenNs: firstSeen}
}

func (h *ipHistory) record(account string, ip netip.Addr, commit func()) error {
	if account == "" || !ip.IsValid() {
		return nil
	}
	h.maintenance.RLock()
	defer h.maintenance.RUnlock()
	digest := sha256.Sum256([]byte(account))
	accounts := h.accounts(ip)
	finish := func(newAccount bool) {
		h.allocationMu.Lock()
		defer h.allocationMu.Unlock()
		if newAccount {
			h.counts.Inc(ip)
		}
		if commit != nil {
			commit()
		}
	}
	if _, ok := accounts.Load(digest); ok {
		if commit != nil {
			finish(false)
		}
		return nil
	}
	var saveErr error
	accounts.Compute(digest, func(old struct{}, loaded bool) (struct{}, xsync.ComputeOp) {
		if loaded {
			if commit != nil {
				finish(false)
			}
			return old, xsync.CancelOp
		}
		if h.save != nil {
			entry := model.IPHistory{EgressIP: ip.Unmap().String(), AccountHash: hex.EncodeToString(digest[:]), FirstSeenNs: time.Now().UnixNano()}
			if saveErr = h.save([]model.IPHistory{entry}); saveErr != nil {
				return old, xsync.CancelOp
			}
		}
		finish(true)
		return struct{}{}, xsync.UpdateOp
	})
	if saveErr != nil {
		return fmt.Errorf("persist IP history: %w", saveErr)
	}
	return nil
}

// RestoreIPHistory is called once, before serving traffic.
func (r *Router) RestoreIPHistory(entries []model.IPHistory) {
	for _, entry := range entries {
		ip, err := netip.ParseAddr(entry.EgressIP)
		digest, valid := decodeHistoryDigest(entry.AccountHash)
		if err != nil || !valid {
			continue
		}
		if _, loaded := r.ipHistory.accounts(ip).LoadOrStore(digest, struct{}{}); !loaded {
			r.ipHistory.counts.Inc(ip)
		}
	}
}

// SeedIPHistory uses recoverable enabled-platform bindings as the upgrade
// baseline. Lost historical leases cannot be reconstructed. Startup only.
func (r *Router) SeedIPHistory(leases []model.Lease) error {
	entries := make([]model.IPHistory, 0)
	seen := make(map[ipHistoryKey]bool)
	for _, lease := range leases {
		if !r.governsPlatform(lease.PlatformID) || lease.Account == "" {
			continue
		}
		ip, err := netip.ParseAddr(lease.EgressIP)
		if err != nil {
			continue
		}
		entry := historyEntry(lease.Account, ip, lease.CreatedAtNs)
		key := ipHistoryKey{ip.Unmap(), entry.AccountHash}
		if r.ipHistory.contains(key) || seen[key] {
			continue
		}
		seen[key] = true
		entries = append(entries, entry)
	}
	if r.ipHistory.save != nil {
		if err := r.ipHistory.save(entries); err != nil {
			return err
		}
	}
	r.RestoreIPHistory(entries)
	return nil
}

func (r *Router) HistoricalIPCount(ip netip.Addr) int64 { return r.ipHistory.counts.Get(ip) }

// ResetIPHistory explicitly clears this exit across platforms. Lease cleanup,
// disabling governance and removing a node never call this method.
func (r *Router) ResetIPHistory(ip netip.Addr) error {
	ip = ip.Unmap()
	r.ipHistory.maintenance.Lock()
	defer r.ipHistory.maintenance.Unlock()
	if r.ipHistory.remove != nil {
		if err := r.ipHistory.remove(ip.String()); err != nil {
			return err
		}
	}
	r.ipHistory.entries.Delete(ip)
	if count, ok := r.ipHistory.counts.counts.Load(ip); ok {
		count.Store(0)
	}
	return nil
}

type IPHistoryLoad struct {
	EgressIP           string `json:"egress_ip"`
	HistoricalAccounts int64  `json:"historical_accounts"`
	ActiveAccounts     int64  `json:"active_accounts"`
	NodeCount          int    `json:"node_count"`
}

// SnapshotIPHistory includes every available unique exit in this platform,
// including never-used exits, using the same candidate view as allocation.
func (r *Router) SnapshotIPHistory(platformID string) ([]IPHistoryLoad, error) {
	plat, ok := r.pool.GetPlatform(platformID)
	if !ok {
		return nil, ErrPlatformNotFound
	}
	state := r.ensurePlatformState(platformID)
	byIP := state.IPLoadStats.candidateNodes(plat.View(), r.pool)
	entries := make([]IPHistoryLoad, 0, len(byIP))
	for ip, nodes := range byIP {
		entries = append(entries, IPHistoryLoad{ip.String(), r.HistoricalIPCount(ip), r.LeaseIPCount(ip), len(nodes)})
	}
	return entries, nil
}
