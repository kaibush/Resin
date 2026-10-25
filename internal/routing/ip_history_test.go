package routing

import (
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
)

func historyTestRouter(t *testing.T, ips ...string) (*Router, *platform.Platform) {
	t.Helper()
	pool := newRouterTestPool()
	plat := platform.NewPlatform("history", "History", nil, nil)
	plat.IPGovernanceEnabled = true
	plat.IPGovernanceMaxAccountsPerIP = 3
	plat.StickyTTLNs = int64(time.Hour)
	pool.addPlatform(plat)
	for i, ip := range ips {
		h, entry := newRoutableEntry(t, fmt.Sprintf(`{"id":"history-%d"}`, i), ip)
		pool.addEntry(h, entry)
	}
	pool.rebuildPlatformView(plat)
	return newTestRouter(pool, nil), plat
}

func TestIPHistoryDistinctMarkersSurviveExpiryRemovalAndRestore(t *testing.T) {
	r, p := historyTestRouter(t, "192.0.2.1")
	var saved []model.IPHistory
	r.ipHistory.save = func(entries []model.IPHistory) error { saved = append(saved, entries...); return nil }
	ip := netip.MustParseAddr("192.0.2.1")
	for _, account := range []string{"alice", "alice", "bob"} {
		if _, err := r.RouteRequest(p.Name, account, ""); err != nil {
			t.Fatal(err)
		}
	}
	if n := r.HistoricalIPCount(ip); n != 2 {
		t.Fatalf("history=%d, want 2", n)
	}
	r.DeleteAllLeases(p.ID)
	if n := r.HistoricalIPCount(ip); n != 2 {
		t.Fatalf("lease deletion changed history to %d", n)
	}
	// Recreate the same identity after its old binding is gone.
	if _, err := r.RouteRequest(p.Name, "alice", ""); err != nil {
		t.Fatal(err)
	}
	s := r.ensurePlatformState(p.ID)
	lease, _ := s.Leases.GetLease("alice")
	lease.ExpiryNs = time.Now().Add(-time.Hour).UnixNano()
	s.Leases.CreateLease("alice", lease)
	NewLeaseCleaner(r).sweep()
	if n := r.HistoricalIPCount(ip); n != 2 || r.LeaseCount(p.ID) != 0 {
		t.Fatalf("expiry: history=%d leases=%d", n, r.LeaseCount(p.ID))
	}
	if len(saved) != 2 {
		t.Fatalf("duplicate marker wrote %d records, want 2", len(saved))
	}
	restarted, _ := historyTestRouter(t, "192.0.2.1")
	restarted.RestoreIPHistory(saved)
	restarted.RestoreIPHistory(saved)
	if n := restarted.HistoricalIPCount(ip); n != 2 {
		t.Fatalf("restore history=%d", n)
	}
}

func TestIPHistorySwitchUsesHistoryOnlyWhenEnabled(t *testing.T) {
	r, p := historyTestRouter(t, "192.0.2.1", "192.0.2.2")
	p.AllocationPolicy = platform.AllocationPolicyPreferIdleIP
	first, second := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")
	for i := 0; i < 10; i++ {
		if err := r.ipHistory.record(fmt.Sprintf("old-%d", i), first, nil); err != nil {
			t.Fatal(err)
		}
		r.ensurePlatformState(p.ID).Leases.CreateLease(fmt.Sprintf("active-%d", i), Lease{EgressIP: second, ExpiryNs: time.Now().Add(time.Hour).UnixNano()})
	}
	p.IPGovernanceEnabled = false
	result, err := r.RouteRequest(p.Name, "disabled-account", "")
	if err != nil || result.EgressIP != first {
		t.Fatalf("disabled did not use idle policy: %+v %v", result, err)
	}
	if n := r.HistoricalIPCount(first); n != 10 {
		t.Fatalf("disabled platform recorded history: %d", n)
	}
	p.IPGovernanceEnabled = true
	result, err = r.RouteRequest(p.Name, "enabled-account", "")
	if err != nil || result.EgressIP != second {
		t.Fatalf("enabled did not prefer history over active load: %+v %v", result, err)
	}
	if n := r.HistoricalIPCount(second); n != 1 {
		t.Fatalf("history=%d, want 1", n)
	}
}

func TestIPHistoryDeduplicatesAcrossPlatformsAndMappedIPs(t *testing.T) {
	r, p := historyTestRouter(t, "192.0.2.1")
	ip := netip.MustParseAddr("192.0.2.1")
	other := platform.NewPlatform("other", "Other", nil, nil)
	other.IPGovernanceEnabled = true
	pool := r.pool.(*routerTestPool)
	pool.addPlatform(other)
	pool.rebuildPlatformView(other)
	for _, name := range []string{p.Name, other.Name} {
		if _, err := r.RouteRequest(name, "same-marker", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.ipHistory.record("same-marker", netip.MustParseAddr("::ffff:192.0.2.1"), nil); err != nil {
		t.Fatal(err)
	}
	if n := r.HistoricalIPCount(ip); n != 1 {
		t.Fatalf("cross-platform distinct count=%d", n)
	}
}

func TestIPHistoryPersistenceFailureReleasesReservation(t *testing.T) {
	r, p := historyTestRouter(t, "192.0.2.1")
	r.ipHistory.save = func([]model.IPHistory) error { return errors.New("disk unavailable") }
	if _, err := r.RouteRequest(p.Name, "alice", ""); err == nil {
		t.Fatal("persistence error ignored")
	}
	ip := netip.MustParseAddr("192.0.2.1")
	if r.ipHistory.counts.GetForAllocation(ip) != 0 || r.globalIPStats.GetForAllocation(ip) != 0 || r.LeaseCount(p.ID) != 0 {
		t.Fatal("failed allocation leaked counts or reservation")
	}
	r.ipHistory.save = nil
	if _, err := r.RouteRequest(p.Name, "alice", ""); err != nil {
		t.Fatal(err)
	}
}

func TestIPHistorySnapshotIncludesUnusedUniqueIPsAndReset(t *testing.T) {
	r, p := historyTestRouter(t, "192.0.2.1", "192.0.2.1", "192.0.2.2")
	ip := netip.MustParseAddr("192.0.2.1")
	if err := r.ipHistory.record("alice", ip, nil); err != nil {
		t.Fatal(err)
	}
	rows, err := r.SnapshotIPHistory(p.ID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	for _, row := range rows {
		if row.EgressIP == ip.String() && (row.HistoricalAccounts != 1 || row.NodeCount != 2) {
			t.Fatalf("shared IP: %+v", row)
		}
		if row.EgressIP != ip.String() && row.HistoricalAccounts != 0 {
			t.Fatalf("unused IP: %+v", row)
		}
	}
	if err := r.ResetIPHistory(ip); err != nil {
		t.Fatal(err)
	}
	if r.HistoricalIPCount(ip) != 0 {
		t.Fatal("reset failed")
	}
	if err := r.ipHistory.record("alice", ip, nil); err != nil {
		t.Fatal(err)
	}
	if r.HistoricalIPCount(ip) != 1 {
		t.Fatal("reset marker did not count again")
	}
}

func TestIPHistoryConcurrentDistinctAndDuplicateMarkers(t *testing.T) {
	r, p := historyTestRouter(t, "192.0.2.1", "192.0.2.2")
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := r.RouteRequest(p.Name, fmt.Sprintf("account-%d", i/2), ""); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	for _, ip := range []string{"192.0.2.1", "192.0.2.2"} {
		if n := r.HistoricalIPCount(netip.MustParseAddr(ip)); n != 25 {
			t.Fatalf("IP %s history=%d, want 25", ip, n)
		}
	}
}

// Hits must remain independent of historical account volume. The benchmark
// intentionally preloads 100k records and reports allocations on the hit path.
func BenchmarkIPHistoryExistingMarker(b *testing.B) {
	r := NewRouter(RouterConfig{})
	ip := netip.MustParseAddr("192.0.2.1")
	for i := 0; i < 100000; i++ {
		if err := r.ipHistory.record(fmt.Sprintf("account-%d", i), ip, nil); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := r.ipHistory.record("account-0", ip, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkIPHistoryAllocation5000IPs(b *testing.B) {
	pool := newRouterTestPool()
	p := platform.NewPlatform("bench-history", "Bench-History", nil, nil)
	p.IPGovernanceEnabled = true
	pool.addPlatform(p)
	for i := 0; i < 5000; i++ {
		h, entry := newRoutableEntry(b, fmt.Sprintf(`{"id":"%d"}`, i), fmt.Sprintf("10.0.%d.%d", i/250, i%250+1))
		pool.addEntry(h, entry)
	}
	pool.rebuildPlatformView(p)
	r := NewRouter(RouterConfig{Pool: pool, Authorities: func() []string { return nil }, P2CWindow: func() time.Duration { return time.Minute }})
	stats := r.ensurePlatformState(p.ID).IPLoadStats
	stats.candidateNodes(p.View(), pool)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h, err := randomRoute(p, stats, pool, "", nil, time.Minute, true)
		if err != nil || h == node.Zero {
			b.Fatalf("pick: %v", err)
		}
	}
}
