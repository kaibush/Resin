package routing

import (
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
)

func TestSoftIPCeilingUsesLeastLoadedCapacityAcrossPlatforms(t *testing.T) {
	pool := newRouterTestPool()
	platformA := platform.NewPlatform("capacity-a", "Capacity-A", nil, nil)
	platformA.StickyTTLNs = int64(time.Hour)
	platformA.IPGovernanceEnabled = true
	platformA.IPGovernanceMaxAccountsPerIP = 2
	platformB := platform.NewPlatform("capacity-b", "Capacity-B", nil, nil)
	pool.addPlatform(platformA)
	pool.addPlatform(platformB)

	hashA, entryA := newRoutableEntry(t, `{"id":"capacity-a"}`, "192.0.2.10")
	hashB, entryB := newRoutableEntry(t, `{"id":"capacity-b"}`, "192.0.2.11")
	pool.addEntry(hashA, entryA)
	pool.addEntry(hashB, entryB)
	pool.rebuildPlatformView(platformA)
	pool.rebuildPlatformView(platformB)

	router := newTestRouter(pool, nil)
	otherPlatform := router.ensurePlatformState(platformB.ID)
	otherPlatform.Leases.CreateLease("other-1", Lease{EgressIP: entryA.GetEgressIP(), ExpiryNs: time.Now().Add(time.Hour).UnixNano()})
	otherPlatform.Leases.CreateLease("other-2", Lease{EgressIP: entryA.GetEgressIP(), ExpiryNs: time.Now().Add(time.Hour).UnixNano()})
	localPlatform := router.ensurePlatformState(platformA.ID)
	localPlatform.Leases.CreateLease("local-1", Lease{EgressIP: entryB.GetEgressIP(), ExpiryNs: time.Now().Add(time.Hour).UnixNano()})
	for _, seed := range []struct {
		account string
		ip      netip.Addr
	}{
		{"other-1", entryA.GetEgressIP()}, {"other-2", entryA.GetEgressIP()}, {"local-1", entryB.GetEgressIP()},
	} {
		if err := router.ipHistory.record(seed.account, seed.ip, nil); err != nil {
			t.Fatal(err)
		}
	}

	result, err := router.RouteRequest(platformA.Name, "new-account", "https://example.com")
	if err != nil {
		t.Fatalf("RouteRequest: %v", err)
	}
	if result.EgressIP != entryB.GetEgressIP() {
		t.Fatalf("selected IP %s, want lower-loaded IP %s (other platform already has two active leases on %s)", result.EgressIP, entryB.GetEgressIP(), entryA.GetEgressIP())
	}
	if got := router.LeaseIPCount(netip.MustParseAddr("192.0.2.10")); got != 2 {
		t.Fatalf("cross-platform IP count = %d, want 2", got)
	}
	if got := router.LeaseIPCount(netip.MustParseAddr("192.0.2.11")); got != 2 {
		t.Fatalf("new lease was not counted on selected IP: got %d want 2", got)
	}
}

func TestSoftIPCeilingFallsBackToLeastLoadedWhenEveryIPIsFull(t *testing.T) {
	pool := newRouterTestPool()
	plat := platform.NewPlatform("capacity-fallback", "Capacity-Fallback", nil, nil)
	plat.IPGovernanceEnabled = true
	plat.IPGovernanceMaxAccountsPerIP = 2
	pool.addPlatform(plat)
	hashA, entryA := newRoutableEntry(t, `{"id":"capacity-fallback-a"}`, "198.51.100.20")
	hashB, entryB := newRoutableEntry(t, `{"id":"capacity-fallback-b"}`, "198.51.100.21")
	pool.addEntry(hashA, entryA)
	pool.addEntry(hashB, entryB)
	pool.rebuildPlatformView(plat)

	stats := NewIPLoadStats()
	// This unit fixture supplies historical counts through the same callback
	// used by Router; active leases are tested separately in ip_history_test.go.
	stats.historyLoad = stats.GetForAllocation
	stats.Inc(entryA.GetEgressIP())
	stats.Inc(entryA.GetEgressIP())
	stats.Inc(entryB.GetEgressIP())
	stats.Inc(entryB.GetEgressIP())
	stats.Inc(entryB.GetEgressIP())

	selected, err := randomRoute(plat, stats, pool, "example.com", nil, time.Minute, true)
	if err != nil {
		t.Fatalf("randomRoute: %v", err)
	}
	if selected != hashA {
		t.Fatalf("soft fallback selected %s, want least-loaded full IP node %s", selected.Hex(), hashA.Hex())
	}
}

func TestStickyIPReservationBalancesBeforeVerificationAndDoesNotHoldGlobalLock(t *testing.T) {
	pool := newRouterTestPool()
	plat := platform.NewPlatform("capacity-concurrent", "Capacity-Concurrent", nil, nil)
	plat.IPGovernanceEnabled = true
	plat.IPGovernanceMaxAccountsPerIP = 1
	pool.addPlatform(plat)
	hashA, entryA := newRoutableEntry(t, `{"id":"capacity-concurrent-a"}`, "203.0.113.10")
	hashB, entryB := newRoutableEntry(t, `{"id":"capacity-concurrent-b"}`, "203.0.113.11")
	pool.addEntry(hashA, entryA)
	pool.addEntry(hashB, entryB)
	pool.rebuildPlatformView(plat)

	verifiedIPs := make(chan netip.Addr, 2)
	releaseVerification := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseVerification) }) }
	defer release()
	router := NewRouter(RouterConfig{
		Pool:        pool,
		Authorities: func() []string { return nil },
		P2CWindow:   func() time.Duration { return time.Minute },
		VerifyNode: func(_ *platform.Platform, hash node.Hash) error {
			entry, ok := pool.GetEntry(hash)
			if !ok {
				return ErrNoAvailableNodes
			}
			verifiedIPs <- entry.GetEgressIP().Unmap()
			<-releaseVerification
			return nil
		},
	})
	results := make(chan error, 2)
	go func() {
		_, err := router.RouteRequest(plat.Name, "account-a", "")
		results <- err
	}()
	var firstIP netip.Addr
	select {
	case firstIP = <-verifiedIPs:
	case err := <-results:
		t.Fatalf("first allocation failed before verification: %v", err)
	case <-time.After(time.Second):
		t.Fatal("first sticky allocation did not reach node verification")
	}
	go func() {
		_, err := router.RouteRequest(plat.Name, "account-b", "")
		results <- err
	}()

	var secondIP netip.Addr
	select {
	case secondIP = <-verifiedIPs:
	case err := <-results:
		t.Fatalf("second allocation failed before verification: %v", err)
	case <-time.After(time.Second):
		t.Fatal("second sticky allocation waited for the first node verification")
	}
	if secondIP == firstIP {
		t.Fatalf("concurrent allocations selected the same reserved IP %s", firstIP)
	}
	release()
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("RouteRequest: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("sticky allocation did not finish after verification was released")
		}
	}
	if got := router.LeaseIPCount(entryA.GetEgressIP()); got != 1 {
		t.Fatalf("IP %s active lease count = %d, want 1", entryA.GetEgressIP(), got)
	}
	if got := router.LeaseIPCount(entryB.GetEgressIP()); got != 1 {
		t.Fatalf("IP %s active lease count = %d, want 1", entryB.GetEgressIP(), got)
	}
}
