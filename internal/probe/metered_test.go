package probe

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
	"github.com/Resinat/Resin/internal/probepolicy"
	"github.com/Resinat/Resin/internal/routing"
	"github.com/Resinat/Resin/internal/state"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/topology"
)

type meteredFixture struct {
	m     *ProbeManager
	c     *MeteredController
	pool  *topology.GlobalNodePool
	sub   *subscription.Subscription
	plat  *platform.Platform
	calls atomic.Int64
}

func newMeteredFixture(t *testing.T) *meteredFixture {
	t.Helper()
	engine, closer, err := state.PersistenceBootstrap(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closer.Close() })
	subs := topology.NewSubscriptionManager()
	sub := subscription.NewSubscription("paid", "paid", "", true, false)
	sub.SetProbePolicy(probepolicy.DefaultMeteredProbePolicy())
	subs.Register(sub)
	pool := topology.NewGlobalNodePool(topology.PoolConfig{SubLookup: subs.Lookup, MaxLatencyTableEntries: 16, MaxConsecutiveFailures: func() int { return 3 }})
	c, err := NewMeteredController(pool, subs, engine.StateRepo)
	if err != nil {
		t.Fatal(err)
	}
	f := &meteredFixture{c: c, pool: pool, sub: sub, plat: platform.NewPlatform(platform.DefaultPlatformID, "Default", nil, nil)}
	pool.RegisterPlatform(f.plat)
	f.m = NewProbeManager(ProbeConfig{Pool: pool, Metered: c, StatsFetcher: func(_ node.Hash, url string) ([]byte, time.Duration, int64, int64, error) {
		f.calls.Add(1)
		if url != egressTraceURL {
			t.Errorf("unexpected target %s", url)
		}
		return []byte("ip=203.0.113.7\nloc=US\n"), time.Millisecond, 4000, 1000, nil
	}})
	pool.SetOnNodeAdded(func(h node.Hash) { e, _ := pool.GetEntry(h); storeOutbound(e); f.m.TriggerImmediateEgressProbe(h) })
	t.Cleanup(func() {
		pool.Range(func(_ node.Hash, e *node.NodeEntry) bool {
			if e.LatencyTable != nil {
				e.LatencyTable.Close()
			}
			return true
		})
	})
	return f
}
func (f *meteredFixture) add(i int) node.Hash {
	raw := []byte(fmt.Sprintf(`{"type":"socks","username":"session-%d"}`, i))
	h := node.HashFromRawOptions(raw)
	f.pool.AddNodeFromSub(h, raw, f.sub.ID)
	f.sub.ManagedNodes().StoreNode(h, subscription.ManagedNode{Tags: []string{"paid"}})
	return h
}
func TestMeteredImport5000NodesDoesNotProbe(t *testing.T) {
	f := newMeteredFixture(t)
	for i := 0; i < 5000; i++ {
		f.add(i)
	}
	f.m.scanEgress()
	f.m.scanLatency()
	if f.calls.Load() != 0 || f.m.taskStates.Size() != 0 {
		t.Fatalf("import scheduled probes: calls=%d queued=%d", f.calls.Load(), f.m.taskStates.Size())
	}
}
func TestMeteredFirstRouteVerifiesAndReusesLease(t *testing.T) {
	f := newMeteredFixture(t)
	h := f.add(1)
	r := routing.NewRouter(routing.RouterConfig{Pool: f.pool, Authorities: func() []string { return []string{"cloudflare.com"} }, P2CWindow: func() time.Duration { return time.Hour }, PrepareRoute: f.m.PrepareRoute, VerifyNode: f.m.VerifyPlatformNode})
	t.Setenv("RESIN_GOVERNANCE_URL", "")
	for i := 0; i < 3; i++ {
		res, err := r.RouteRequest("", "account", "example.com")
		if err != nil {
			t.Fatal(err)
		}
		if res.NodeHash != h || !res.EgressIP.IsValid() {
			t.Fatalf("unverified route %+v", res)
		}
	}
	if f.calls.Load() != 1 {
		t.Fatalf("calls=%d", f.calls.Load())
	}
	f.m.scanEgress()
	f.m.scanLatency()
	if f.m.taskStates.Size() != 0 {
		t.Fatal("fresh route scheduled duplicate probes")
	}
}
func TestMeteredConcurrentValidationDeduplicated(t *testing.T) {
	f := newMeteredFixture(t)
	h := f.add(1)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.m.VerifyRouteNode(h); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.calls.Load() != 1 {
		t.Fatalf("calls=%d", f.calls.Load())
	}
}
func TestMeteredFailureBackoffAndManualCombinedProbe(t *testing.T) {
	f := newMeteredFixture(t)
	h := f.add(1)
	f.m.statsFetcher = func(node.Hash, string) ([]byte, time.Duration, int64, int64, error) {
		f.calls.Add(1)
		return nil, 0, 100, 100, errors.New("offline")
	}
	if err := f.m.VerifyRouteNode(h); err == nil {
		t.Fatal("failure accepted")
	}
	if err := f.m.VerifyRouteNode(h); err == nil {
		t.Fatal("backoff bypassed")
	}
	if f.calls.Load() != 1 {
		t.Fatal("immediate repeated failure probe")
	}
	f.m.statsFetcher = func(_ node.Hash, url string) ([]byte, time.Duration, int64, int64, error) {
		f.calls.Add(1)
		if url != egressTraceURL {
			t.Errorf("latency target %s", url)
		}
		return []byte("ip=203.0.113.8\nloc=US"), time.Millisecond, 100, 100, nil
	}
	if _, err := f.m.ProbeLatencySync(h); err != nil {
		t.Fatal(err)
	}
	if f.c.nodeState(h).NextRetryNs != 0 {
		t.Fatal("retry not cleared")
	}
}
func TestMeteredStrictBudgetAndIdleSuppression(t *testing.T) {
	f := newMeteredFixture(t)
	h := f.add(1)
	p := f.sub.ProbePolicy()
	p.MonthlyBudgetBytes = 1
	p.StrictBudget = true
	f.sub.SetProbePolicy(p)
	if err := f.m.VerifyRouteNode(h); !errors.Is(err, state.ErrProbeBudget) {
		t.Fatalf("strict budget: %v", err)
	}
	if f.calls.Load() != 0 {
		t.Fatal("budget allowed network")
	}
	p.StrictBudget = false
	f.sub.SetProbePolicy(p)
	if err := f.m.VerifyRouteNode(h); err != nil {
		t.Fatal(err)
	}
	if err := f.c.update(h, func(s *state.ProbeNodeState) {
		s.LastUsedNs = time.Now().Add(-48 * time.Hour).UnixNano()
		s.VerifiedNs = time.Now().Add(-48 * time.Hour).UnixNano()
	}); err != nil {
		t.Fatal(err)
	}
	if f.c.Due(h) {
		t.Fatal("idle node due")
	}
}
func TestMeteredMalformedTraceDoesNotBecomeHealthy(t *testing.T) {
	f := newMeteredFixture(t)
	h := f.add(1)
	f.m.statsFetcher = func(node.Hash, string) ([]byte, time.Duration, int64, int64, error) {
		return []byte("blocked"), time.Millisecond, 100, 100, nil
	}
	if err := f.m.VerifyRouteNode(h); err == nil {
		t.Fatal("invalid trace accepted")
	}
	e, _ := f.pool.GetEntry(h)
	if e.IsHealthy() || f.plat.View().Contains(h) {
		t.Fatal("invalid trace admitted")
	}
}

func TestMeteredPlatformMaximumAgeOverridesSubscription(t *testing.T) {
	f := newMeteredFixture(t)
	h := f.add(1)
	if err := f.m.VerifyRouteNode(h); err != nil {
		t.Fatal(err)
	}
	if err := f.c.update(h, func(s *state.ProbeNodeState) { s.VerifiedNs = time.Now().Add(-2 * time.Hour).UnixNano() }); err != nil {
		t.Fatal(err)
	}
	if err := f.m.VerifyRouteNode(h); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 1 {
		t.Fatal("subscription age not respected")
	}
	f.plat.EgressVerificationMaxAgeNs = int64(time.Hour)
	if err := f.m.VerifyPlatformNode(f.plat, h); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 2 {
		t.Fatal("platform age ignored")
	}
}
func TestMeteredRestartRetainsActivityVerificationAndBackoff(t *testing.T) {
	f := newMeteredFixture(t)
	h := f.add(1)
	if err := f.m.VerifyRouteNode(h); err != nil {
		t.Fatal(err)
	}
	restored, err := NewMeteredController(f.pool, f.c.subs, f.c.repo)
	if err != nil {
		t.Fatal(err)
	}
	f.m.metered = restored
	if err = f.m.VerifyRouteNode(h); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 1 || restored.Due(h) {
		t.Fatal("restart caused fresh probe")
	}
	f.m.ObservePassiveResult(h, false)
	restored, err = NewMeteredController(f.pool, f.c.subs, f.c.repo)
	if err != nil {
		t.Fatal(err)
	}
	if restored.nodeState(h).NextRetryNs <= time.Now().UnixNano() {
		t.Fatal("restart lost backoff")
	}
}
func TestMeteredSharedNodeAndDisabledSubscription(t *testing.T) {
	f := newMeteredFixture(t)
	h := f.add(1)
	other := subscription.NewSubscription("aaa", "other", "", true, false)
	other.SetProbePolicy(probepolicy.DefaultMeteredProbePolicy())
	f.c.subs.Register(other)
	e, _ := f.pool.GetEntry(h)
	f.pool.AddNodeFromSub(h, e.RawOptions, other.ID)
	if id, _, ok := f.c.Policy(h); !ok || id != "aaa" {
		t.Fatalf("unstable owner %q", id)
	}
	other.SetEnabled(false)
	if id, _, ok := f.c.Policy(h); !ok || id != f.sub.ID {
		t.Fatalf("disabled owner %q", id)
	}
	f.sub.SetEnabled(false)
	if _, _, ok := f.c.Policy(h); ok {
		t.Fatal("disabled node metered")
	}
	f.m.scanEgress()
	f.m.scanLatency()
	if f.m.taskStates.Size() != 0 {
		t.Fatal("disabled node queued")
	}
}

func TestMeteredStopDrainsAndRejectsManualProbes(t *testing.T) {
	f := newMeteredFixture(t)
	h := f.add(1)
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	f.m.statsFetcher = func(node.Hash, string) ([]byte, time.Duration, int64, int64, error) {
		close(started)
		<-release
		return []byte("ip=203.0.113.8\nloc=US"), time.Millisecond, 100, 100, nil
	}
	go func() { _, err := f.m.ProbeEgressSync(h); done <- err }()
	<-started
	stopped := make(chan struct{})
	go func() { f.m.Stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("stop returned during probe")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	<-stopped
	if _, err := f.m.ProbeEgressSync(h); err == nil {
		t.Fatal("post-stop probe accepted")
	}
}

func TestMeteredAdmissionWaitIsBounded(t *testing.T) {
	f := newMeteredFixture(t)
	h := f.add(1)
	f.c.slotWait = time.Millisecond
	for i := 0; i < cap(f.c.slots); i++ {
		f.c.slots <- struct{}{}
	}
	defer func() {
		for len(f.c.slots) > 0 {
			<-f.c.slots
		}
	}()
	if err := f.m.VerifyRouteNode(h); err == nil {
		t.Fatal("saturated admission accepted")
	}
	if f.calls.Load() != 0 {
		t.Fatal("saturated admission used traffic")
	}
}
