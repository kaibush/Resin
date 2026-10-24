package routing

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/netutil"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
	"github.com/puzpuzpuz/xsync/v4"
)

var (
	ErrPlatformNotFound = errors.New("platform not found")
)

type PoolAccessor interface {
	GetEntry(hash node.Hash) (*node.NodeEntry, bool)
	GetPlatform(id string) (*platform.Platform, bool)
	GetPlatformByName(name string) (*platform.Platform, bool)
	RangePlatforms(fn func(*platform.Platform) bool)
}

// Router handles route selection and lease management.
type Router struct {
	prepareRoute    func(*platform.Platform, node.Hash) error
	verifyNode      func(*platform.Platform, node.Hash) error
	pool            PoolAccessor
	states          *xsync.Map[string, *PlatformRoutingState]
	globalIPStats   *IPLoadStats
	allocationMu    sync.Mutex // serializes short cross-platform IP reservations
	authorities     func() []string
	p2cWindow       func() time.Duration
	onLeaseEvent    LeaseEventFunc
	nodeTagResolver func(node.Hash) string
}

type RouterConfig struct {
	PrepareRoute func(*platform.Platform, node.Hash) error
	VerifyNode   func(*platform.Platform, node.Hash) error
	Pool         PoolAccessor
	Authorities  func() []string
	P2CWindow    func() time.Duration
	// OnLeaseEvent is called synchronously; handlers must stay lightweight.
	OnLeaseEvent LeaseEventFunc
	// NodeTagResolver resolves a node hash to its display tag ("<Sub>/<Tag>").
	// If nil, NodeTag will be empty.
	NodeTagResolver func(node.Hash) string
}

func NewRouter(cfg RouterConfig) *Router {
	return &Router{
		prepareRoute:    cfg.PrepareRoute,
		verifyNode:      cfg.VerifyNode,
		pool:            cfg.Pool,
		states:          xsync.NewMap[string, *PlatformRoutingState](),
		globalIPStats:   NewIPLoadStats(),
		authorities:     cfg.Authorities,
		p2cWindow:       cfg.P2CWindow,
		onLeaseEvent:    cfg.OnLeaseEvent,
		nodeTagResolver: cfg.NodeTagResolver,
	}
}

type RouteResult struct {
	Governed     bool
	PlatformID   string
	PlatformName string
	NodeHash     node.Hash
	EgressIP     netip.Addr
	NodeTag      string // display tag: "<Subscription>/<Tag>" (DESIGN.md §601)
	LeaseCreated bool
}

const livePickAttempts = 2 // first pick + one retry

type leaseInvalidationReason int

const (
	leaseInvalidationNone leaseInvalidationReason = iota
	leaseInvalidationExpire
	leaseInvalidationRemove
)

func (r *Router) RouteRequest(platName, account, target string) (RouteResult, error) {
	plat, err := r.resolvePlatform(platName)
	if err != nil {
		return RouteResult{}, err
	}

	targetDomain := netutil.ExtractDomain(target)
	state := r.ensurePlatformState(plat.ID)
	var result RouteResult
	if account == "" {
		result, err = r.routeRandom(plat, state, targetDomain)
	} else {
		result, err = r.routeSticky(plat, state, account, targetDomain, time.Now())
	}
	if err != nil {
		return RouteResult{}, err
	}
	result = withPlatformContext(plat, result)
	if r.nodeTagResolver != nil {
		result.NodeTag = r.nodeTagResolver(result.NodeHash)
	}
	return result, nil
}

func withPlatformContext(plat *platform.Platform, res RouteResult) RouteResult {
	res.PlatformID = plat.ID
	res.PlatformName = plat.Name
	return res
}

func (r *Router) resolvePlatform(platName string) (*platform.Platform, error) {
	if platName == "" {
		if p, ok := r.pool.GetPlatform(platform.DefaultPlatformID); ok {
			return p, nil
		}
		return nil, ErrPlatformNotFound
	}
	p, ok := r.pool.GetPlatformByName(platName)
	if !ok {
		return nil, ErrPlatformNotFound
	}
	return p, nil
}

func (r *Router) ensurePlatformState(platformID string) *PlatformRoutingState {
	state, _ := r.states.LoadOrCompute(platformID, func() (*PlatformRoutingState, bool) {
		return r.newPlatformRoutingState(), false
	})
	return state
}

func (r *Router) newPlatformRoutingState() *PlatformRoutingState {
	state := NewPlatformRoutingState()
	state.IPLoadStats.global = r.globalIPStats
	state.IPLoadStats.loadAcrossPlatforms = r.globalIPStats.GetForAllocation
	return state
}

func (r *Router) routeRandom(
	plat *platform.Platform,
	state *PlatformRoutingState,
	targetDomain string,
) (RouteResult, error) {
	if r.prepareRoute != nil {
		if err := r.prepareRoute(plat, node.Zero); err != nil {
			return RouteResult{}, err
		}
	}
	h, entry, err := r.selectLiveRandomRoute(plat, state.IPLoadStats, targetDomain, false, netip.Addr{})
	if err != nil {
		return RouteResult{}, err
	}
	return RouteResult{
		NodeHash:     h,
		EgressIP:     entry.GetEgressIP(),
		LeaseCreated: false,
	}, nil
}

func (r *Router) routeSticky(
	plat *platform.Platform,
	state *PlatformRoutingState,
	account string,
	targetDomain string,
	now time.Time,
) (RouteResult, error) {
	nowNs := now.UnixNano()
	var result RouteResult
	var routeErr error

	state.Leases.Compute(account, func(current Lease, loaded bool) (Lease, xsync.ComputeOp) {
		if r.prepareRoute != nil {
			h := node.Zero
			if loaded && !current.IsExpired(time.Now()) {
				h = current.NodeHash
			}
			if err := r.prepareRoute(plat, h); err != nil {
				routeErr = err
				return current, xsync.CancelOp
			}
		}
		// Verification can block; start the new lease TTL after it completes.
		now = time.Now()
		nowNs = now.UnixNano()
		newLease, op, routeResult, err := r.decideStickyLease(
			plat,
			state,
			account,
			targetDomain,
			now,
			nowNs,
			current,
			loaded,
		)
		if err != nil {
			routeErr = err
			return newLease, op
		}
		result = routeResult
		return newLease, op
	})

	return result, routeErr
}

func (r *Router) decideStickyLease(
	plat *platform.Platform,
	state *PlatformRoutingState,
	account string,
	targetDomain string,
	now time.Time,
	nowNs int64,
	current Lease,
	loaded bool,
) (Lease, xsync.ComputeOp, RouteResult, error) {
	hadPreviousLease := loaded
	invalidation := leaseInvalidationNone

	if loaded && current.IsExpired(now) {
		invalidation = leaseInvalidationExpire
		loaded = false
	}

	if loaded {
		if newLease, hitResult, ok := r.tryLeaseHit(plat, account, current, nowNs); ok {
			return newLease, xsync.UpdateOp, hitResult, nil
		}
		if newLease, rotatedResult, ok := r.tryLeaseSameIPRotation(plat, account, current, targetDomain, nowNs); ok {
			return newLease, xsync.UpdateOp, rotatedResult, nil
		}
		invalidation = leaseInvalidationRemove
	}

	// New bindings reserve their IP atomically across platform views in
	// selectLiveRandomRoute. Node verification runs after that short lock is
	// released, so one slow proxy probe cannot block other account allocations.
	return r.createOrAbortStickyLease(
		plat,
		state,
		account,
		targetDomain,
		now,
		nowNs,
		current,
		hadPreviousLease,
		invalidation,
	)
}

func (r *Router) createOrAbortStickyLease(
	plat *platform.Platform,
	state *PlatformRoutingState,
	account string,
	targetDomain string,
	now time.Time,
	nowNs int64,
	previous Lease,
	hadPreviousLease bool,
	invalidation leaseInvalidationReason,
) (Lease, xsync.ComputeOp, RouteResult, error) {
	retireIP := netip.Addr{}
	if hadPreviousLease {
		retireIP = previous.EgressIP
	}
	newLease, createdResult, err := r.createLease(plat, state, targetDomain, now, nowNs, retireIP)
	if err != nil {
		r.cleanupPreviousLease(previous, hadPreviousLease, invalidation, plat.ID, account)
		lease, op := abortLeaseCreate(previous, hadPreviousLease)
		return lease, op, RouteResult{}, err
	}

	r.cleanupPreviousLease(previous, hadPreviousLease, invalidation, plat.ID, account)
	state.IPLoadStats.CommitReservation(newLease.EgressIP)
	r.emitLeaseEvent(LeaseEvent{
		Type:       LeaseCreate,
		PlatformID: plat.ID,
		Account:    account,
		NodeHash:   newLease.NodeHash,
		EgressIP:   newLease.EgressIP,
	})
	return newLease, xsync.UpdateOp, createdResult, nil
}

func (r *Router) tryLeaseHit(
	plat *platform.Platform,
	account string,
	current Lease,
	nowNs int64,
) (Lease, RouteResult, bool) {
	entry, ok := r.pool.GetEntry(current.NodeHash)
	if !ok || !plat.View().Contains(current.NodeHash) || entry.GetEgressIP() != current.EgressIP {
		return Lease{}, RouteResult{}, false
	}

	newLease := current
	newLease.LastAccessedNs = nowNs
	r.emitLeaseEvent(LeaseEvent{
		Type:       LeaseTouch,
		PlatformID: plat.ID,
		Account:    account,
		NodeHash:   current.NodeHash,
		EgressIP:   current.EgressIP,
	})
	return newLease, RouteResult{
		NodeHash:     current.NodeHash,
		EgressIP:     current.EgressIP,
		LeaseCreated: false,
	}, true
}

func (r *Router) tryLeaseSameIPRotation(
	plat *platform.Platform,
	account string,
	current Lease,
	targetDomain string,
	nowNs int64,
) (Lease, RouteResult, bool) {
	bestHash, ok := chooseSameIPRotationCandidate(
		plat,
		r.pool,
		current.EgressIP,
		targetDomain,
		r.authorities(),
		r.p2cWindow(),
	)
	if !ok {
		return Lease{}, RouteResult{}, false
	}

	if r.verifyNode != nil {
		if err := r.verifyNode(plat, bestHash); err != nil {
			return Lease{}, RouteResult{}, false
		}
		entry, exists := r.pool.GetEntry(bestHash)
		if !exists || !plat.View().Contains(bestHash) || entry.GetEgressIP() != current.EgressIP {
			return Lease{}, RouteResult{}, false
		}
	}
	newLease := current
	newLease.NodeHash = bestHash
	newLease.LastAccessedNs = nowNs
	r.emitLeaseEvent(LeaseEvent{
		Type:       LeaseReplace,
		PlatformID: plat.ID,
		Account:    account,
		NodeHash:   bestHash,
		EgressIP:   current.EgressIP,
	})
	return newLease, RouteResult{
		NodeHash:     bestHash,
		EgressIP:     current.EgressIP,
		LeaseCreated: false,
	}, true
}

func (r *Router) createLease(
	plat *platform.Platform,
	state *PlatformRoutingState,
	targetDomain string,
	now time.Time,
	nowNs int64,
	retireIP netip.Addr,
) (Lease, RouteResult, error) {
	h, entry, err := r.selectLiveRandomRoute(plat, state.IPLoadStats, targetDomain, true, retireIP)
	if err != nil {
		return Lease{}, RouteResult{}, err
	}
	ttl := plat.StickyTTLNs
	if ttl <= 0 {
		ttl = int64(24 * time.Hour) // Default safeguard
	}

	lease := Lease{
		NodeHash:       h,
		EgressIP:       entry.GetEgressIP(),
		CreatedAtNs:    nowNs,
		ExpiryNs:       now.Add(time.Duration(ttl)).UnixNano(),
		LastAccessedNs: nowNs,
	}
	return lease, RouteResult{
		NodeHash:     lease.NodeHash,
		EgressIP:     lease.EgressIP,
		LeaseCreated: true,
	}, nil
}

func (r *Router) cleanupPreviousLease(
	lease Lease,
	hadPreviousLease bool,
	invalidation leaseInvalidationReason,
	platformID string,
	account string,
) {
	if !hadPreviousLease {
		return
	}
	// The old lease's load was removed under allocationMu before choosing its
	// replacement, so it cannot bias the new least-loaded-IP decision.
	switch invalidation {
	case leaseInvalidationExpire:
		r.emitLeaseEvent(LeaseEvent{
			Type:        LeaseExpire,
			PlatformID:  platformID,
			Account:     account,
			NodeHash:    lease.NodeHash,
			EgressIP:    lease.EgressIP,
			CreatedAtNs: lease.CreatedAtNs,
		})
	case leaseInvalidationRemove:
		r.emitLeaseEvent(LeaseEvent{
			Type:        LeaseRemove,
			PlatformID:  platformID,
			Account:     account,
			NodeHash:    lease.NodeHash,
			EgressIP:    lease.EgressIP,
			CreatedAtNs: lease.CreatedAtNs,
		})
	}
}

func abortLeaseCreate(current Lease, hadPreviousLease bool) (Lease, xsync.ComputeOp) {
	if hadPreviousLease {
		return current, xsync.DeleteOp
	}
	return current, xsync.CancelOp
}

func (r *Router) emitLeaseEvent(event LeaseEvent) {
	if r.onLeaseEvent != nil {
		r.onLeaseEvent(event)
	}
}

func (r *Router) selectLiveRandomRoute(
	plat *platform.Platform,
	stats *IPLoadStats,
	targetDomain string,
	reserveLease bool,
	retireIP netip.Addr,
) (node.Hash, *node.NodeEntry, error) {
	var lastMissing node.Hash
	if reserveLease || plat.AllocationPolicy == platform.AllocationPolicyPreferIdleIP {
		// A cold pool snapshot can range over all routable nodes. Build it before
		// taking the allocator mutex so one initial request does not hold up all
		// concurrent sticky allocations while it groups unique IPs.
		stats.candidateNodes(plat.View(), r.pool)
	}
	for i := 0; i < livePickAttempts; i++ {
		// Sticky identities are assigned by current unique-IP load, regardless
		// of the platform's node scoring policy. Random requests keep the
		// configured platform policy.
		balanceIPs := reserveLease || plat.AllocationPolicy == platform.AllocationPolicyPreferIdleIP
		reserve := reserveLease
		if reserve {
			r.allocationMu.Lock()
			stats.allocationMu.Lock()
			if retireIP.IsValid() {
				stats.Dec(retireIP)
				retireIP = netip.Addr{}
			}
		}
		h, err := randomRoute(plat, stats, r.pool, targetDomain, r.authorities(), r.p2cWindow(), reserveLease)
		if err != nil {
			if reserve {
				stats.allocationMu.Unlock()
				r.allocationMu.Unlock()
			}
			return node.Zero, nil, err
		}
		entry, ok := r.pool.GetEntry(h)
		if ok {
			selectedIP := entry.GetEgressIP().Unmap()
			if reserve {
				stats.Reserve(selectedIP)
				r.globalIPStats.Reserve(selectedIP)
				stats.allocationMu.Unlock()
				r.allocationMu.Unlock()
			}
			if balanceIPs && (!selectedIP.IsValid() || !plat.View().Contains(h)) {
				stats.InvalidateCandidateSnapshot()
				if reserve {
					r.releaseSelectionReservation(stats, selectedIP)
				}
				continue
			}
			if r.verifyNode != nil {
				if err := r.verifyNode(plat, h); err != nil {
					if reserve {
						r.releaseSelectionReservation(stats, selectedIP)
					}
					return node.Zero, nil, err
				}
			}
			currentIP := entry.GetEgressIP().Unmap()
			if !currentIP.IsValid() || (balanceIPs && (!plat.View().Contains(h) || currentIP != selectedIP)) || (r.verifyNode != nil && !entry.IsHealthy()) {
				if balanceIPs {
					stats.InvalidateCandidateSnapshot()
				}
				if reserve {
					r.releaseSelectionReservation(stats, selectedIP)
				}
				continue
			}
			return h, entry, nil
		}
		if reserve {
			stats.allocationMu.Unlock()
			r.allocationMu.Unlock()
		}
		lastMissing = h
	}
	if lastMissing != node.Zero {
		return node.Zero, nil, fmt.Errorf("%w: selected node %s no longer in pool", ErrNoAvailableNodes, lastMissing.Hex())
	}
	return node.Zero, nil, ErrNoAvailableNodes
}

func (r *Router) releaseSelectionReservation(stats *IPLoadStats, ip netip.Addr) {
	stats.ReleaseReservation(ip)
	r.globalIPStats.ReleaseReservation(ip)
}

func chooseSameIPRotationCandidate(
	plat *platform.Platform,
	pool PoolAccessor,
	targetIP netip.Addr,
	targetDomain string,
	authorities []string,
	window time.Duration,
) (node.Hash, bool) {
	bestKnownHash := node.Zero
	bestKnownLatency := time.Duration(math.MaxInt64)
	fallbackHash := node.Zero

	plat.View().Range(func(h node.Hash) bool {
		entry, ok := pool.GetEntry(h)
		if !ok || entry.GetEgressIP() != targetIP {
			return true
		}
		if fallbackHash == node.Zero {
			fallbackHash = h
		}

		latency, hasLatency := sameIPCandidateLatency(entry, targetDomain, authorities, window)
		if hasLatency && latency < bestKnownLatency {
			bestKnownLatency = latency
			bestKnownHash = h
		}
		return true
	})

	if bestKnownHash != node.Zero {
		return bestKnownHash, true
	}
	if fallbackHash != node.Zero {
		return fallbackHash, true
	}
	return node.Zero, false
}

func sameIPCandidateLatency(
	entry *node.NodeEntry,
	targetDomain string,
	authorities []string,
	window time.Duration,
) (time.Duration, bool) {
	now := time.Now()
	if latency, ok := lookupRecentDomainLatency(entry, targetDomain, now, window); ok {
		return latency, true
	}

	if latency, ok := averageRecentAuthorityLatency(entry, authorities, now, window); ok {
		return latency, true
	}
	return 0, false
}

// ReadLease implements weak persistence read.
func (r *Router) ReadLease(key model.LeaseKey) *model.Lease {
	state, ok := r.states.Load(key.PlatformID)
	if !ok {
		return nil
	}
	lease, ok := state.Leases.GetLease(key.Account)
	if !ok {
		return nil
	}
	return &model.Lease{
		PlatformID:     key.PlatformID,
		Account:        key.Account,
		NodeHash:       lease.NodeHash.Hex(),
		EgressIP:       lease.EgressIP.String(),
		CreatedAtNs:    lease.CreatedAtNs,
		ExpiryNs:       lease.ExpiryNs,
		LastAccessedNs: lease.LastAccessedNs,
	}
}

// UpsertLease writes or replaces a lease for (platform_id, account).
// It updates per-IP lease counters and emits LeaseCreate/LeaseReplace events.
func (r *Router) UpsertLease(ml model.Lease) error {
	platformID := strings.TrimSpace(ml.PlatformID)
	if platformID == "" {
		return errors.New("platform_id is required")
	}
	account := strings.TrimSpace(ml.Account)
	if account == "" {
		return errors.New("account is required")
	}
	if r.governsPlatform(platformID) {
		p, ok := r.pool.GetPlatform(platformID)
		if !ok {
			return ErrPlatformNotFound
		}
		// The external Grok policy callback was removed. Resin owns allocation
		// and chooses from this platform's current routable IP pool locally.
		_, err := r.RouteRequest(p.Name, account, "")
		return err
	}

	h, err := node.ParseHex(ml.NodeHash)
	if err != nil {
		return fmt.Errorf("parse node_hash: %w", err)
	}
	ip, err := netip.ParseAddr(ml.EgressIP)
	if err != nil {
		return fmt.Errorf("parse egress_ip: %w", err)
	}

	state := r.ensurePlatformState(platformID)
	lease := Lease{
		NodeHash:       h,
		EgressIP:       ip,
		CreatedAtNs:    ml.CreatedAtNs,
		ExpiryNs:       ml.ExpiryNs,
		LastAccessedNs: ml.LastAccessedNs,
	}

	eventType := LeaseCreate
	state.Leases.Compute(account, func(current Lease, loaded bool) (Lease, xsync.ComputeOp) {
		if loaded {
			state.Leases.stats.Dec(current.EgressIP)
			eventType = LeaseReplace
		}
		state.Leases.stats.Inc(lease.EgressIP)
		return lease, xsync.UpdateOp
	})

	r.emitLeaseEvent(LeaseEvent{
		Type:       eventType,
		PlatformID: platformID,
		Account:    account,
		NodeHash:   lease.NodeHash,
		EgressIP:   lease.EgressIP,
	})
	return nil
}

// SnapshotIPLoad returns a best-effort point-in-time IP load snapshot for a platform.
// If the platform has no routing state yet, it returns an empty snapshot.
func (r *Router) SnapshotIPLoad(platformID string) map[netip.Addr]int64 {
	state, ok := r.states.Load(platformID)
	if !ok {
		return map[netip.Addr]int64{}
	}
	return state.IPLoadStats.Snapshot()
}

// LeaseIPCount returns the best-effort active lease count for an IP across all
// platforms. It reads per-IP atomic counters and never walks account leases.
// Expired entries are removed by the bounded lease sweeper.
func (r *Router) LeaseIPCount(ip netip.Addr) int64 {
	if !ip.IsValid() {
		return 0
	}
	return r.globalIPStats.Get(ip.Unmap())
}

// RestoreLeases restores leases from persistence during bootstrap.
func (r *Router) RestoreLeases(leases []model.Lease) {
	for _, ml := range leases {
		h, err := node.ParseHex(ml.NodeHash)
		if err != nil {
			continue
		}
		ip, err := netip.ParseAddr(ml.EgressIP)
		if err != nil {
			continue
		}

		state, _ := r.states.LoadOrCompute(ml.PlatformID, func() (*PlatformRoutingState, bool) {
			return r.newPlatformRoutingState(), false
		})

		l := Lease{
			NodeHash:       h,
			EgressIP:       ip,
			CreatedAtNs:    ml.CreatedAtNs,
			ExpiryNs:       ml.ExpiryNs,
			LastAccessedNs: ml.LastAccessedNs,
		}
		// Directly insert into table and stats
		state.Leases.CreateLease(ml.Account, l)
	}
}

// LeaseCount returns the current number of sticky leases without walking the
// account map. Periodic metric collection uses this constant-time count.
func (r *Router) LeaseCount(platformID string) int {
	state, ok := r.states.Load(platformID)
	if !ok {
		return 0
	}
	return state.Leases.Size()
}

// RangeLeases iterates over all leases for a platform.
// Returns false if the platform has no routing state.
func (r *Router) RangeLeases(platformID string, fn func(account string, lease Lease) bool) bool {
	state, ok := r.states.Load(platformID)
	if !ok {
		return false
	}
	state.Leases.Range(fn)
	return true
}

// DeleteLease removes a single lease by platform and account.
// Returns true if a lease was deleted. Emits a LeaseRemove event.
func (r *Router) DeleteLease(platformID, account string) bool {
	state, ok := r.states.Load(platformID)
	if !ok {
		return false
	}
	lease, deleted := state.Leases.DeleteLease(account)
	if !deleted {
		return false
	}
	r.emitLeaseEvent(LeaseEvent{
		Type:        LeaseRemove,
		PlatformID:  platformID,
		Account:     account,
		NodeHash:    lease.NodeHash,
		EgressIP:    lease.EgressIP,
		CreatedAtNs: lease.CreatedAtNs,
	})
	return true
}

// DeleteAllLeases removes all leases for a platform.
// Returns the number of leases deleted. Emits a LeaseRemove event for each.
func (r *Router) DeleteAllLeases(platformID string) int {
	state, ok := r.states.Load(platformID)
	if !ok {
		return 0
	}
	count := 0
	state.Leases.Range(func(account string, _ Lease) bool {
		removed, deleted := state.Leases.DeleteLease(account)
		if deleted {
			r.emitLeaseEvent(LeaseEvent{
				Type:        LeaseRemove,
				PlatformID:  platformID,
				Account:     account,
				NodeHash:    removed.NodeHash,
				EgressIP:    removed.EgressIP,
				CreatedAtNs: removed.CreatedAtNs,
			})
			count++
		}
		return true
	})
	return count
}
