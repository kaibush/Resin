package routing

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
)

func expireStickyTestLease(t *testing.T, r *Router, p *platform.Platform, account string, sweep bool) {
	t.Helper()
	s := r.ensurePlatformState(p.ID)
	lease, ok := s.Leases.GetLease(account)
	if !ok {
		t.Fatal("missing lease")
	}
	lease.ExpiryNs = time.Now().Add(-time.Hour).UnixNano()
	s.Leases.CreateLease(account, lease)
	if sweep {
		NewLeaseCleaner(r).sweep()
		if _, ok := s.Leases.GetLease(account); ok {
			t.Fatal("expired lease was not removed")
		}
	}
}

func TestStickyIPReuseAfterExpirySweepAndRestart(t *testing.T) {
	for _, sweep := range []bool{false, true} {
		t.Run(fmt.Sprintf("sweep=%v", sweep), func(t *testing.T) {
			r, p := historyTestRouter(t, "192.0.2.1", "192.0.2.2")
			p.StickyReuseIP = true
			p.IPGovernanceMaxAccountsPerIP = 1
			var saved []model.StickyIP
			r.saveStickyIP = func(e model.StickyIP) error { saved = append(saved, e); return nil }
			first, err := r.RouteRequest(p.Name, "alice", "")
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				expireStickyTestLease(t, r, p, "alice", sweep)
				got, err := r.RouteRequest(p.Name, "alice", "")
				if err != nil || got.EgressIP != first.EgressIP || !got.LeaseCreated {
					t.Fatalf("renewal=%+v err=%v, original=%s", got, err, first.EgressIP)
				}
				if r.LeaseIPCount(first.EgressIP) != 1 || r.HistoricalIPCount(first.EgressIP) != 1 {
					t.Fatal("renewal duplicated active or historical accounts")
				}
			}
			if len(saved) != 1 {
				t.Fatalf("same-IP renewals wrote %d rows", len(saved))
			}
			// Only the remembered exit survives; no lease or node hash is restored.
			restarted, other := historyTestRouter(t, "192.0.2.1", "192.0.2.2")
			other.StickyReuseIP = true
			restarted.RestoreStickyIP(saved[0])
			restarted.saveStickyIP = func(model.StickyIP) error { t.Error("restart rewrote unchanged binding"); return nil }
			got, err := restarted.RouteRequest(other.Name, "alice", "")
			if err != nil || got.EgressIP != first.EgressIP {
				t.Fatalf("restart=%+v err=%v", got, err)
			}
			fresh, err := r.RouteRequest(p.Name, "bob", "")
			if err != nil || fresh.EgressIP == first.EgressIP {
				t.Fatalf("new identity no longer balanced: %+v %v", fresh, err)
			}
		})
	}
}

func TestStickyIPFallbackAndLatestExit(t *testing.T) {
	r, p := historyTestRouter(t, "192.0.2.1", "192.0.2.2")
	p.StickyReuseIP = true
	var writes int
	r.saveStickyIP = func(model.StickyIP) error { writes++; return nil }
	first, err := r.RouteRequest(p.Name, "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	expireStickyTestLease(t, r, p, "alice", true)
	pool := r.pool.(*routerTestPool)
	entry, _ := pool.GetEntry(first.NodeHash)
	pool.removeEntry(first.NodeHash)
	pool.rebuildPlatformView(p)
	second, err := r.RouteRequest(p.Name, "alice", "")
	if err != nil || second.EgressIP == first.EgressIP {
		t.Fatalf("fallback=%+v err=%v", second, err)
	}
	pool.addEntry(first.NodeHash, entry)
	pool.rebuildPlatformView(p)
	expireStickyTestLease(t, r, p, "alice", true)
	third, err := r.RouteRequest(p.Name, "alice", "")
	if err != nil || third.EgressIP != second.EgressIP {
		t.Fatalf("latest exit not remembered: %+v %v", third, err)
	}
	if writes != 2 || r.stickyIPs.Size() != 1 {
		t.Fatalf("writes=%d, expected overwrite on fallback only", writes)
	}
}

func TestStickyIPDisabledAndCrossPlatformReuse(t *testing.T) {
	r, p := historyTestRouter(t, "192.0.2.1", "192.0.2.2")
	p.StickyReuseIP = true
	first, err := r.RouteRequest(p.Name, "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	expireStickyTestLease(t, r, p, "alice", true)
	other := platform.NewPlatform("other", "Other", nil, nil)
	other.IPGovernanceEnabled, other.StickyReuseIP = true, true
	pool := r.pool.(*routerTestPool)
	pool.addPlatform(other)
	pool.rebuildPlatformView(other)
	got, err := r.RouteRequest(other.Name, "alice", "")
	if err != nil || got.EgressIP != first.EgressIP {
		t.Fatalf("cross-platform affinity not reused: %+v %v", got, err)
	}
	p.StickyReuseIP = false
	r.saveStickyIP = func(model.StickyIP) error { t.Error("disabled feature wrote binding"); return nil }
	got, err = r.RouteRequest(p.Name, "alice", "")
	if err != nil || got.EgressIP == first.EgressIP {
		t.Fatalf("disabled still preferred old IP: %+v %v", got, err)
	}
	if r.preferredStickyIP("alice", Lease{}) != first.EgressIP {
		t.Fatal("disabling deleted saved affinity")
	}
	// Re-enabling must adopt the currently active lease, not the older memory.
	p.StickyReuseIP = true
	r.saveStickyIP = nil
	if _, err := r.RouteRequest(p.Name, "alice", ""); err != nil {
		t.Fatal(err)
	}
	expireStickyTestLease(t, r, p, "alice", true)
	renewed, err := r.RouteRequest(p.Name, "alice", "")
	if err != nil || renewed.EgressIP != got.EgressIP {
		t.Fatalf("reenable=%+v %v", renewed, err)
	}
}

func TestStickyIPVerificationTriesSameExitBeforeFallback(t *testing.T) {
	r, p := historyTestRouter(t, "192.0.2.1", "192.0.2.1", "192.0.2.2")
	p.StickyReuseIP = true
	ip := netip.MustParseAddr("192.0.2.1")
	r.RestoreStickyIP(model.StickyIP{AccountHash: sha256.Sum256([]byte("alice")), EgressIP: ip})
	var verified int
	r.prepareRoute = func(*platform.Platform, node.Hash) error {
		t.Error("warmed another exit before reusing available IP")
		return nil
	}
	r.verifyNode = func(_ *platform.Platform, h node.Hash) error {
		verified++
		entry, _ := r.pool.GetEntry(h)
		if entry.GetEgressIP() != ip {
			t.Error("verified unrelated IP")
		}
		if verified == 1 {
			return errors.New("node unavailable")
		}
		return nil
	}
	got, err := r.RouteRequest(p.Name, "alice", "")
	if err != nil || got.EgressIP != ip || verified != 2 {
		t.Fatalf("same-IP failover=%+v err=%v probes=%d", got, err, verified)
	}
}

func TestStickyIPVerificationChangedExitFallsBack(t *testing.T) {
	r, p := historyTestRouter(t, "192.0.2.1", "192.0.2.2")
	p.StickyReuseIP = true
	first, err := r.RouteRequest(p.Name, "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	expireStickyTestLease(t, r, p, "alice", true)
	pool := r.pool.(*routerTestPool)
	r.verifyNode = func(_ *platform.Platform, h node.Hash) error {
		if h == first.NodeHash {
			entry, _ := pool.GetEntry(h)
			entry.SetEgressIP(netip.MustParseAddr("192.0.2.99"))
			pool.removeEntry(h)
			pool.rebuildPlatformView(p)
		}
		return nil
	}
	got, err := r.RouteRequest(p.Name, "alice", "")
	if err != nil || got.EgressIP == first.EgressIP || got.EgressIP.String() == "192.0.2.99" {
		t.Fatalf("verification race=%+v %v", got, err)
	}
	if r.LeaseIPCount(first.EgressIP) != 0 || r.LeaseIPCount(got.EgressIP) != 1 {
		t.Fatal("verification race leaked counts")
	}
}

func TestStickyIPPersistenceFailureAndConcurrentRenewals(t *testing.T) {
	r, p := historyTestRouter(t, "192.0.2.1")
	p.StickyReuseIP = true
	ip := netip.MustParseAddr("192.0.2.1")
	r.saveStickyIP = func(model.StickyIP) error { return errors.New("disk unavailable") }
	if _, err := r.RouteRequest(p.Name, "alice", ""); err == nil {
		t.Fatal("persistence failure ignored")
	}
	s := r.ensurePlatformState(p.ID)
	if r.stickyIPs.Size() != 0 || s.Leases.Size() != 0 || s.IPLoadStats.GetForAllocation(ip) != 0 || r.globalIPStats.GetForAllocation(ip) != 0 || r.ipHistory.counts.GetForAllocation(ip) != 0 {
		t.Fatal("failed write leaked affinity or reservations")
	}
	var writes atomic.Int64
	r.saveStickyIP = func(model.StickyIP) error { writes.Add(1); return nil }
	if _, err := r.RouteRequest(p.Name, "alice", ""); err != nil {
		t.Fatal(err)
	}
	expireStickyTestLease(t, r, p, "alice", true)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.RouteRequest(p.Name, "alice", ""); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if writes.Load() != 1 || r.LeaseIPCount(ip) != 1 || r.HistoricalIPCount(ip) != 1 || s.IPLoadStats.Reserved(ip) != 0 {
		t.Fatal("concurrent renewal duplicated writes or counters")
	}
}

func TestStickyIPEnableBeforeCleanupPreservesExistingLease(t *testing.T) {
	r, p := historyTestRouter(t, "192.0.2.1", "192.0.2.2")
	first, err := r.RouteRequest(p.Name, "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	p.StickyReuseIP = true
	expireStickyTestLease(t, r, p, "alice", true)
	got, err := r.RouteRequest(p.Name, "alice", "")
	if err != nil || got.EgressIP != first.EgressIP {
		t.Fatalf("old lease lost on enable: %+v %v", got, err)
	}
}

func TestStickyIPRenewalDoesNotScanUnchangedPool(t *testing.T) {
	r, p := historyTestRouter(t, "192.0.2.1", "192.0.2.2", "192.0.2.3")
	p.StickyReuseIP = true
	if _, err := r.RouteRequest(p.Name, "alice", ""); err != nil {
		t.Fatal(err)
	}
	pool := &getEntryCountPool{inner: r.pool}
	r.pool = pool
	for i := 0; i < 4; i++ {
		expireStickyTestLease(t, r, p, "alice", true)
		pool.getCalls = 0
		if _, err := r.RouteRequest(p.Name, "alice", ""); err != nil {
			t.Fatal(err)
		}
		if pool.getCalls != 2 {
			t.Fatalf("renewal made %d pool reads, want two checks of selected node", pool.getCalls)
		}
	}
}

func BenchmarkStickyIPLookup(b *testing.B) {
	for _, size := range []int{1, 100000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			r := NewRouter(RouterConfig{})
			ip := netip.MustParseAddr("192.0.2.1")
			for i := 0; i < size; i++ {
				r.stickyIPs.Store(sha256.Sum256([]byte(fmt.Sprint(i))), stickyIPBinding{IP: ip})
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if r.preferredStickyIP("0", Lease{}) != ip {
					b.Fatal("lookup failed")
				}
			}
		})
	}
}

func TestStickyIPRestoreIgnoresOlderCachedExit(t *testing.T) {
	r, p := historyTestRouter(t, "192.0.2.1", "192.0.2.2")
	p.StickyReuseIP = true
	first, err := r.RouteRequest(p.Name, "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	stale := *r.ReadLease(model.LeaseKey{PlatformID: p.ID, Account: "alice"})
	newIP := netip.MustParseAddr("192.0.2.1")
	if newIP == first.EgressIP {
		newIP = netip.MustParseAddr("192.0.2.2")
	}
	restarted := newTestRouter(r.pool, nil)
	restarted.RestoreStickyIP(model.StickyIP{AccountHash: sha256.Sum256([]byte("alice")), EgressIP: newIP, UpdatedAtNs: stale.CreatedAtNs - 1})
	restarted.RestoreLeases([]model.Lease{stale})
	if restarted.LeaseCount(p.ID) != 0 {
		t.Fatal("stale cached lease overrode durable binding")
	}
	got, err := restarted.RouteRequest(p.Name, "alice", "")
	if err != nil || got.EgressIP != newIP {
		t.Fatalf("restored stale exit: %+v %v", got, err)
	}
}

func TestStickyIPReplacedPlatformViewAndIPv6(t *testing.T) {
	r, p := historyTestRouter(t, "2001:db8::1")
	p.StickyReuseIP = true
	first, err := r.RouteRequest(p.Name, "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	expireStickyTestLease(t, r, p, "alice", true)
	pool := r.pool.(*routerTestPool)
	pool.removeEntry(first.NodeHash)
	h, entry := newRoutableEntry(t, `{"id":"replacement"}`, "2001:db8::1")
	pool.addEntry(h, entry)
	replacement := platform.NewPlatform(p.ID, p.Name, nil, nil)
	replacement.StickyReuseIP, replacement.IPGovernanceEnabled = true, true
	pool.rebuildPlatformView(replacement)
	pool.addPlatform(replacement)
	if p.View().Revision() != replacement.View().Revision() {
		t.Fatal("setup should have equal revisions on distinct views")
	}
	// Warming must not be needed when the replacement view has the original IP.
	r.prepareRoute = func(*platform.Platform, node.Hash) error {
		t.Error("stale snapshot missed routable same-IP node")
		return nil
	}
	got, err := r.RouteRequest(p.Name, "alice", "")
	if err != nil || got.EgressIP != first.EgressIP || got.NodeHash != h {
		t.Fatalf("replaced view=%+v %v", got, err)
	}
}

func TestStickyIPCleanupRetriesFailedInitialSave(t *testing.T) {
	r, p := historyTestRouter(t, "192.0.2.1")
	if _, err := r.RouteRequest(p.Name, "alice", ""); err != nil {
		t.Fatal(err)
	}
	p.StickyReuseIP = true
	expireStickyTestLease(t, r, p, "alice", false)
	var writes int
	r.saveStickyIP = func(model.StickyIP) error { writes++; return errors.New("disk error") }
	c := NewLeaseCleaner(r)
	c.sweep()
	c.sweep()
	if r.LeaseCount(p.ID) != 1 || writes != 1 {
		t.Fatalf("failed save lost lease or spun: writes=%d", writes)
	}
	r.saveStickyIP = nil
	c.sweepPlatformState(p.ID, r.ensurePlatformState(p.ID), time.Now().Add(2*time.Minute).UnixNano())
	if r.LeaseCount(p.ID) != 0 || r.stickyIPs.Size() != 1 {
		t.Fatal("save was not retried")
	}
}

func TestStickyIPReuseWithoutHistoryAndFailedFallbackSave(t *testing.T) {
	r, p := historyTestRouter(t, "192.0.2.1", "192.0.2.2")
	p.IPGovernanceEnabled = false
	p.StickyReuseIP = true
	first, err := r.RouteRequest(p.Name, "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	expireStickyTestLease(t, r, p, "alice", true)
	got, err := r.RouteRequest(p.Name, "alice", "")
	if err != nil || got.EgressIP != first.EgressIP || r.HistoricalIPCount(got.EgressIP) != 0 {
		t.Fatalf("independent reuse=%+v %v", got, err)
	}
	expireStickyTestLease(t, r, p, "alice", false)
	pool := r.pool.(*routerTestPool)
	pool.removeEntry(first.NodeHash)
	pool.rebuildPlatformView(p)
	r.saveStickyIP = func(model.StickyIP) error { return errors.New("disk error") }
	if _, err := r.RouteRequest(p.Name, "alice", ""); err == nil {
		t.Fatal("failed replacement accepted")
	}
	s := r.ensurePlatformState(p.ID)
	for _, ip := range []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")} {
		if s.IPLoadStats.GetForAllocation(ip) != 0 || r.globalIPStats.GetForAllocation(ip) != 0 {
			t.Fatal("replacement leaked active load")
		}
	}
	if r.LeaseCount(p.ID) != 0 || r.preferredStickyIP("alice", Lease{}) != first.EgressIP {
		t.Fatal("failed update corrupted remembered IP")
	}
}

func TestStickyIPMappedIPv4RenewalRemainsALeaseHit(t *testing.T) {
	r, p := historyTestRouter(t, "::ffff:192.0.2.1")
	p.StickyReuseIP = true
	if _, err := r.RouteRequest(p.Name, "alice", ""); err != nil {
		t.Fatal(err)
	}
	expireStickyTestLease(t, r, p, "alice", true)
	renewed, err := r.RouteRequest(p.Name, "alice", "")
	if err != nil || !renewed.LeaseCreated {
		t.Fatalf("renewal=%+v %v", renewed, err)
	}
	hit, err := r.RouteRequest(p.Name, "alice", "")
	if err != nil || hit.LeaseCreated || hit.EgressIP.Unmap() != netip.MustParseAddr("192.0.2.1") {
		t.Fatalf("mapped exit lost stickiness: %+v %v", hit, err)
	}
	if r.HistoricalIPCount(hit.EgressIP) != 1 || r.LeaseIPCount(hit.EgressIP) != 1 {
		t.Fatal("mapped exit duplicated accounts")
	}
}

func TestStickyIPSurvivesPlatformDeletionAndReimport(t *testing.T) {
	r, p := historyTestRouter(t, "192.0.2.1", "192.0.2.2")
	p.StickyReuseIP = true
	var saved model.StickyIP
	r.saveStickyIP = func(e model.StickyIP) error { saved = e; return nil }
	first, err := r.RouteRequest(p.Name, "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	// Start with only a newly created platform and newly imported nodes. The
	// saved binding has no dependency on any original platform ID or node hash.
	pool := newRouterTestPool()
	next := platform.NewPlatform("new-platform-id", "Reimported", nil, nil)
	if next.StickyReuseIP {
		t.Fatal("new platforms must remain opt-in")
	}
	next.StickyReuseIP, next.IPGovernanceEnabled = true, true
	pool.addPlatform(next)
	for i, ip := range []string{"192.0.2.1", "192.0.2.2"} {
		h, e := newRoutableEntry(t, fmt.Sprintf(`{"reimport":%d}`, i), ip)
		pool.addEntry(h, e)
	}
	pool.rebuildPlatformView(next)
	restarted := newTestRouter(pool, nil)
	restarted.RestoreStickyIP(saved)
	if err := restarted.ipHistory.record("alice", first.EgressIP, nil); err != nil {
		t.Fatal(err)
	}
	got, err := restarted.RouteRequest(next.Name, "alice", "")
	if err != nil || got.EgressIP != first.EgressIP || got.NodeHash == first.NodeHash {
		t.Fatalf("reimport lost original exit: %+v %v", got, err)
	}
	if restarted.HistoricalIPCount(first.EgressIP) != 1 || restarted.stickyIPs.Size() != 1 {
		t.Fatal("reimport duplicated records")
	}
}

func TestStickyIPGlobalConcurrentWritesAndOlderLeaseTouch(t *testing.T) {
	r, p := historyTestRouter(t, "192.0.2.1")
	p.StickyReuseIP = true
	ip := netip.MustParseAddr("192.0.2.1")
	var writes atomic.Int64
	r.saveStickyIP = func(model.StickyIP) error { writes.Add(1); return nil }
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := r.rememberStickyIP(p, "alice", ip, int64(i+1)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if writes.Load() != 1 || r.stickyIPs.Size() != 1 {
		t.Fatalf("duplicate global saves: %d", writes.Load())
	}
	newIP := netip.MustParseAddr("192.0.2.2")
	if err := r.rememberStickyIP(p, "alice", newIP, 100); err != nil {
		t.Fatal(err)
	}
	if err := r.rememberStickyIP(p, "alice", ip, 50); err != nil {
		t.Fatal(err)
	}
	if r.preferredStickyIP("alice", Lease{EgressIP: ip, CreatedAtNs: 50}) != newIP || writes.Load() != 2 {
		t.Fatal("old platform overwrote newer global binding")
	}
}

func BenchmarkStickyIPSelection5000IPs(b *testing.B) {
	pool := newRouterTestPool()
	p := platform.NewPlatform("bench", "Bench", nil, nil)
	pool.addPlatform(p)
	for i := 0; i < 5000; i++ {
		h, e := newRoutableEntry(b, fmt.Sprintf(`{"id":%d}`, i), fmt.Sprintf("10.0.%d.%d", i/250, i%250+1))
		pool.addEntry(h, e)
	}
	pool.rebuildPlatformView(p)
	r := newTestRouter(pool, nil)
	s := r.ensurePlatformState(p.ID)
	s.IPLoadStats.candidateNodes(p.View(), pool)
	ip := netip.MustParseAddr("10.0.0.1")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, ok := r.selectStickyIPRoute(p, s.IPLoadStats, ip, ""); !ok {
			b.Fatal("missing remembered IP")
		}
	}
}
