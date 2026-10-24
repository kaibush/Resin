package routing

import (
	"bytes"
	"container/heap"
	"hash/maphash"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
	"github.com/puzpuzpuz/xsync/v4"
)

// Lease represents a sticky routing lease.
// It is a value type to avoid pointer aliasing races.
type Lease struct {
	NodeHash                    node.Hash
	EgressIP                    netip.Addr
	CreatedAtNs                 int64
	ExpiryNs                    int64
	LastAccessedNs              int64
	GovernanceMode              string
	GovernanceValidUntilNs      int64
	GovernanceSupplyUntilNs     int64
	GovernancePolicyRevision    string
	GovernanceFollowPlatformTTL bool
}

// LeaseTable manages per-account sticky leases for a single platform.
type LeaseTable struct {
	// key: account
	leases       *xsync.Map[string, Lease]
	stats        *IPLoadStats
	expiryShards [leaseExpiryShardCount]leaseExpiryQueue
}

const leaseExpiryShardCount = 64

var leaseExpirySeed = maphash.MakeSeed()

type leaseExpiryItem struct {
	account  string
	expiryNs int64
	index    int
}

type leaseExpiryHeap []*leaseExpiryItem

type leaseExpiryQueue struct {
	mu        sync.Mutex
	heap      leaseExpiryHeap
	byAccount map[string]*leaseExpiryItem
}

func (h leaseExpiryHeap) Len() int { return len(h) }
func (h leaseExpiryHeap) Less(i, j int) bool {
	if h[i].expiryNs != h[j].expiryNs {
		return h[i].expiryNs < h[j].expiryNs
	}
	return h[i].account < h[j].account
}
func (h leaseExpiryHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}
func (h *leaseExpiryHeap) Push(value any) {
	item := value.(*leaseExpiryItem)
	item.index = len(*h)
	*h = append(*h, item)
}
func (h *leaseExpiryHeap) Pop() any {
	old := *h
	last := len(old) - 1
	item := old[last]
	old[last] = nil
	*h = old[:last]
	item.index = -1
	return item
}

func (t *LeaseTable) expiryQueue(account string) *leaseExpiryQueue {
	return &t.expiryShards[maphash.String(leaseExpirySeed, account)%leaseExpiryShardCount]
}

func (q *leaseExpiryQueue) setExpiry(account string, expiryNs int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.byAccount == nil {
		q.byAccount = make(map[string]*leaseExpiryItem)
	}
	if item, ok := q.byAccount[account]; ok {
		if item.expiryNs == expiryNs {
			return
		}
		item.expiryNs = expiryNs
		heap.Fix(&q.heap, item.index)
		return
	}
	item := &leaseExpiryItem{account: account, expiryNs: expiryNs}
	heap.Push(&q.heap, item)
	q.byAccount[account] = item
}

func (q *leaseExpiryQueue) removeExpiry(account string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if item, ok := q.byAccount[account]; ok {
		heap.Remove(&q.heap, item.index)
		delete(q.byAccount, account)
	}
}

// takeExpiredAccount removes just the earliest expired deadline. The caller
// must confirm and delete the corresponding lease under its per-account lock.
func (t *LeaseTable) takeExpiredAccount(nowNs int64) (string, bool) {
	for i := range t.expiryShards {
		q := &t.expiryShards[i]
		q.mu.Lock()
		if len(q.heap) == 0 || q.heap[0].expiryNs >= nowNs {
			q.mu.Unlock()
			continue
		}
		item := heap.Pop(&q.heap).(*leaseExpiryItem)
		delete(q.byAccount, item.account)
		q.mu.Unlock()
		return item.account, true
	}
	return "", false
}

// IPLoadStats tracks the number of active leases per egress IP.
type IPLoadStats struct {
	counts              *xsync.Map[netip.Addr, *atomic.Int64]
	reservations        *xsync.Map[netip.Addr, *atomic.Int64]
	global              *IPLoadStats
	loadAcrossPlatforms func(netip.Addr) int64
	allocationMu        sync.Mutex
	candidateMu         sync.Mutex
	candidates          ipRouteSnapshot
}

type ipRouteSnapshot struct {
	revision uint64
	byIP     map[netip.Addr][]node.Hash
}

// NewLeaseTable creates a new lease table linked to the given stats.
func NewLeaseTable(stats *IPLoadStats) *LeaseTable {
	return &LeaseTable{
		leases: xsync.NewMap[string, Lease](),
		stats:  stats,
	}
}

// Compute updates one lease and its expiry index together inside the map's
// per-account compute callback. This keeps expiry maintenance O(log n) without
// periodically walking every sticky identity.
func (t *LeaseTable) Compute(account string, fn func(old Lease, loaded bool) (Lease, xsync.ComputeOp)) {
	t.leases.Compute(account, func(old Lease, loaded bool) (Lease, xsync.ComputeOp) {
		next, op := fn(old, loaded)
		switch op {
		case xsync.UpdateOp:
			if !loaded || next.ExpiryNs != old.ExpiryNs {
				t.expiryQueue(account).setExpiry(account, next.ExpiryNs)
			}
		case xsync.DeleteOp:
			t.expiryQueue(account).removeExpiry(account)
		}
		return next, op
	})
}

// NewIPLoadStats creates a new IP load stats tracker.
func NewIPLoadStats() *IPLoadStats {
	return &IPLoadStats{
		counts:       xsync.NewMap[netip.Addr, *atomic.Int64](),
		reservations: xsync.NewMap[netip.Addr, *atomic.Int64](),
	}
}

// GetLease returns the lease for the given account.
func (t *LeaseTable) GetLease(account string) (Lease, bool) {
	return t.leases.Load(account)
}

// Range iterates over all leases.
func (t *LeaseTable) Range(fn func(account string, lease Lease) bool) {
	t.leases.Range(fn)
}

// CreateLease adds a new lease and increments the IP load count.
// If a lease explicitly exists (e.g. race), it is overwritten (and counts adjusted).
func (t *LeaseTable) CreateLease(account string, lease Lease) {
	t.Compute(account, func(oldVal Lease, loaded bool) (Lease, xsync.ComputeOp) {
		if loaded {
			t.stats.Dec(oldVal.EgressIP)
		}
		t.stats.Inc(lease.EgressIP)
		return lease, xsync.UpdateOp
	})
}

// DeleteLease atomically removes a lease and decrements IP load stats.
// Returns the deleted lease and true when a lease was actually deleted.
func (t *LeaseTable) DeleteLease(account string) (Lease, bool) {
	var deleted Lease
	ok := false
	t.Compute(account, func(oldVal Lease, loaded bool) (Lease, xsync.ComputeOp) {
		if loaded {
			t.stats.Dec(oldVal.EgressIP)
			deleted = oldVal
			ok = true
			return oldVal, xsync.DeleteOp
		}
		return oldVal, xsync.CancelOp
	})
	return deleted, ok
}

// Size returns the number of leases in the table.
func (t *LeaseTable) Size() int {
	return t.leases.Size()
}

// Inc increments the lease count for an IP.
func (s *IPLoadStats) Inc(ip netip.Addr) {
	if !ip.IsValid() {
		return
	}
	ip = ip.Unmap()
	ctr, _ := s.counts.LoadOrStore(ip, new(atomic.Int64))
	ctr.Add(1)
	if s.global != nil {
		s.global.Inc(ip)
	}
}

// Dec decrements the lease count for an IP.
func (s *IPLoadStats) Dec(ip netip.Addr) {
	if !ip.IsValid() {
		return
	}
	ip = ip.Unmap()
	if ctr, ok := s.counts.Load(ip); ok {
		ctr.Add(-1)
		if s.global != nil {
			s.global.Dec(ip)
		}
		// We don't remove zero counters to avoid race conditions with concurrent Inc.
		// Since the set of egress IPs is bounded (by number of nodes), this leakage
		// is acceptable and bounded.
	}
}

// Get returns the current lease count for an IP.
func (s *IPLoadStats) Get(ip netip.Addr) int64 {
	if !ip.IsValid() {
		return 0
	}
	ip = ip.Unmap()
	if ctr, ok := s.counts.Load(ip); ok {
		return ctr.Load()
	}
	return 0
}

// GetForAllocation includes selections that are being verified but have not
// yet become sticky leases. This prevents concurrent allocations from all
// observing the same empty-IP snapshot.
func (s *IPLoadStats) GetForAllocation(ip netip.Addr) int64 {
	load := s.Get(ip)
	if !ip.IsValid() {
		return load
	}
	if ctr, ok := s.reservations.Load(ip.Unmap()); ok {
		load += ctr.Load()
	}
	return load
}

func (s *IPLoadStats) allocationLoad(ip netip.Addr) int64 {
	if s.loadAcrossPlatforms != nil {
		return s.loadAcrossPlatforms(ip)
	}
	return s.GetForAllocation(ip)
}

func (s *IPLoadStats) Reserve(ip netip.Addr) {
	if !ip.IsValid() {
		return
	}
	ctr, _ := s.reservations.LoadOrStore(ip.Unmap(), new(atomic.Int64))
	ctr.Add(1)
}

func (s *IPLoadStats) Unreserve(ip netip.Addr) {
	if !ip.IsValid() {
		return
	}
	if ctr, ok := s.reservations.Load(ip.Unmap()); ok {
		ctr.Add(-1)
	}
}

func (s *IPLoadStats) ReleaseReservation(ip netip.Addr) {
	s.allocationMu.Lock()
	s.Unreserve(ip)
	s.allocationMu.Unlock()
}

func (s *IPLoadStats) CommitReservation(ip netip.Addr) {
	s.allocationMu.Lock()
	s.Inc(ip)
	s.Unreserve(ip)
	if s.global != nil { // Router-level reservations span platforms and are committed with the
		// per-platform lease count.
		s.global.Unreserve(ip)
	}
	s.allocationMu.Unlock()
}

// Snapshot returns a best-effort point-in-time copy of positive lease counts.
// Entries with non-positive counts are skipped.
func (s *IPLoadStats) Snapshot() map[netip.Addr]int64 {
	out := make(map[netip.Addr]int64)
	s.counts.Range(func(ip netip.Addr, ctr *atomic.Int64) bool {
		if !ip.IsValid() || ctr == nil {
			return true
		}
		n := ctr.Load()
		if n <= 0 {
			return true
		}
		out[ip] = n
		return true
	})
	return out
}

// candidateNodes groups routable nodes by their normalized egress IP. Reuse
// the pool scan until routable membership or an egress IP changes.
func (s *IPLoadStats) candidateNodes(view platform.ReadOnlyView, pool PoolAccessor) map[netip.Addr][]node.Hash {
	s.candidateMu.Lock()
	defer s.candidateMu.Unlock()
	var latest map[netip.Addr][]node.Hash
	for attempt := 0; attempt < 3; attempt++ {
		revision := view.Revision()
		if s.candidates.byIP != nil && s.candidates.revision == revision {
			return s.candidates.byIP
		}

		byIP := make(map[netip.Addr][]node.Hash)
		view.Range(func(hash node.Hash) bool {
			entry, ok := pool.GetEntry(hash)
			if !ok || entry.Outbound.Load() == nil {
				return true
			}
			ip := entry.GetEgressIP()
			if ip.IsValid() {
				byIP[ip.Unmap()] = append(byIP[ip.Unmap()], hash)
			}
			return true
		})
		latest = byIP
		for ip := range byIP {
			sort.Slice(byIP[ip], func(i, j int) bool {
				return bytes.Compare(byIP[ip][i][:], byIP[ip][j][:]) < 0
			})
		}
		if view.Revision() == revision {
			s.candidates = ipRouteSnapshot{revision: revision, byIP: byIP}
			return byIP
		}
	}
	// A continuously rebuilding platform is rare; return this fresh best-effort
	// view but leave it uncached so the next allocation retries against a stable
	// revision.
	s.candidates = ipRouteSnapshot{}
	return latest
}

// InvalidateCandidateSnapshot forces the next allocation to re-read the
// platform view after a cached node is found to be stale.
func (s *IPLoadStats) InvalidateCandidateSnapshot() {
	s.candidateMu.Lock()
	s.candidates = ipRouteSnapshot{}
	s.candidateMu.Unlock()
}

// IsExpired checks if a lease is expired relative to the given time.
func (l Lease) IsExpired(now time.Time) bool {
	return l.ExpiryNs < now.UnixNano()
}
