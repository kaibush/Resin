package routing

import (
	"net/netip"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
	"github.com/puzpuzpuz/xsync/v4"
)

func TestLeaseCleaner_StopWaitsForInFlightSweep(t *testing.T) {
	pool := newRouterTestPool()
	plat := platform.NewPlatform("plat-stop", "Plat-Stop", nil, nil)
	pool.addPlatform(plat)
	router := newTestRouter(pool, nil)

	cleaner := newLeaseCleanerWithIntervals(router, time.Millisecond, 0)

	started := make(chan struct{})
	release := make(chan struct{})
	cleaner.sweepHook = func() {
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
	}

	cleaner.Start()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("sweep did not start in time")
	}

	stopDone := make(chan struct{})
	go func() {
		cleaner.Stop()
		close(stopDone)
	}()

	select {
	case <-stopDone:
		t.Fatal("Stop returned before in-flight sweep completed")
	case <-time.After(30 * time.Millisecond):
	}

	close(release)

	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return after in-flight sweep completed")
	}
}

func TestLeaseCleaner_SweepPlatformsInParallel(t *testing.T) {
	oldMaxProcs := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(oldMaxProcs)

	pool := newRouterTestPool()
	platA := platform.NewPlatform("plat-a", "Plat-A", nil, nil)
	platB := platform.NewPlatform("plat-b", "Plat-B", nil, nil)
	pool.addPlatform(platA)
	pool.addPlatform(platB)

	releaseEvents := make(chan struct{})
	allStarted := make(chan struct{})
	var eventCount atomic.Int32
	router := newTestRouter(pool, func(e LeaseEvent) {
		if e.Type != LeaseExpire {
			return
		}
		if eventCount.Add(1) == 2 {
			close(allStarted)
		}
		<-releaseEvents
	})

	now := time.Now()
	stateA, _ := router.states.LoadOrCompute(platA.ID, func() (*PlatformRoutingState, bool) {
		return NewPlatformRoutingState(), false
	})
	stateB, _ := router.states.LoadOrCompute(platB.ID, func() (*PlatformRoutingState, bool) {
		return NewPlatformRoutingState(), false
	})

	stateA.Leases.CreateLease("acct-a", Lease{
		NodeHash:       node.HashFromRawOptions([]byte(`{"id":"lease-a"}`)),
		EgressIP:       netip.MustParseAddr("203.0.113.10"),
		CreatedAtNs:    now.Add(-2 * time.Minute).UnixNano(),
		ExpiryNs:       now.Add(-1 * time.Minute).UnixNano(),
		LastAccessedNs: now.Add(-2 * time.Minute).UnixNano(),
	})
	stateB.Leases.CreateLease("acct-b", Lease{
		NodeHash:       node.HashFromRawOptions([]byte(`{"id":"lease-b"}`)),
		EgressIP:       netip.MustParseAddr("203.0.113.11"),
		CreatedAtNs:    now.Add(-2 * time.Minute).UnixNano(),
		ExpiryNs:       now.Add(-1 * time.Minute).UnixNano(),
		LastAccessedNs: now.Add(-2 * time.Minute).UnixNano(),
	})

	cleaner := NewLeaseCleaner(router)
	done := make(chan struct{})
	go func() {
		cleaner.sweep()
		close(done)
	}()

	select {
	case <-allStarted:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("expected platform lease sweeps to run in parallel")
	}

	select {
	case <-done:
		t.Fatal("sweep should wait for in-flight platform sweeps")
	default:
	}

	close(releaseEvents)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sweep did not finish after releasing lease-expire handlers")
	}

	if got := eventCount.Load(); got != 2 {
		t.Fatalf("expected 2 LeaseExpire events, got %d", got)
	}
}

func TestLeaseExpiryIndexTracksRenewalAndDeletion(t *testing.T) {
	stats := NewIPLoadStats()
	leases := NewLeaseTable(stats)
	now := time.Now()
	ip := netip.MustParseAddr("203.0.113.90")

	leases.CreateLease("acct", Lease{EgressIP: ip, ExpiryNs: now.Add(-time.Second).UnixNano()})
	leases.CreateLease("acct", Lease{EgressIP: ip, ExpiryNs: now.Add(time.Hour).UnixNano()})
	if account, ok := leases.takeExpiredAccount(now.UnixNano()); ok {
		t.Fatalf("renewed lease remained on the expired queue: %q", account)
	}

	if _, deleted := leases.DeleteLease("acct"); !deleted {
		t.Fatal("expected renewed lease to be deleted")
	}
	if account, ok := leases.takeExpiredAccount(now.Add(2 * time.Hour).UnixNano()); ok {
		t.Fatalf("deleted lease remained on the expiry queue: %q", account)
	}
	if got := stats.Get(ip); got != 0 {
		t.Fatalf("deleted lease left IP load %d, want 0", got)
	}
}

func TestLeaseExpiryIndexKeepsRenewalAfterCleanerPopsOldDeadline(t *testing.T) {
	stats := NewIPLoadStats()
	leases := NewLeaseTable(stats)
	now := time.Now()
	ip := netip.MustParseAddr("203.0.113.91")
	leases.CreateLease("acct", Lease{EgressIP: ip, ExpiryNs: now.Add(-time.Second).UnixNano()})

	account, ok := leases.takeExpiredAccount(now.UnixNano())
	if !ok || account != "acct" {
		t.Fatalf("expired deadline pop = (%q, %v), want (acct, true)", account, ok)
	}
	leases.CreateLease("acct", Lease{EgressIP: ip, ExpiryNs: now.Add(time.Hour).UnixNano()})
	leases.Compute(account, func(current Lease, loaded bool) (Lease, xsync.ComputeOp) {
		if !loaded || current.ExpiryNs < now.UnixNano() {
			return current, xsync.DeleteOp
		}
		return current, xsync.CancelOp
	})
	if account, ok := leases.takeExpiredAccount(now.UnixNano()); ok {
		t.Fatalf("renewed lease was requeued as expired: %q", account)
	}
	account, ok = leases.takeExpiredAccount(now.Add(2 * time.Hour).UnixNano())
	if !ok || account != "acct" {
		t.Fatalf("renewed deadline pop = (%q, %v), want (acct, true)", account, ok)
	}
}
