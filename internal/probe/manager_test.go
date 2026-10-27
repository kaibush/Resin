package probe

import (
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/testutil"
	"github.com/Resinat/Resin/internal/topology"
)

// storeOutbound sets a non-nil outbound on the entry.
func storeOutbound(entry *node.NodeEntry) {
	ob := testutil.NewNoopOutbound()
	entry.Outbound.Store(&ob)
}

// TestProbeEgress_Success verifies that a successful egress probe calls
// RecordResult(true), RecordLatency("cloudflare.com"), and UpdateNodeEgressIP.
func TestProbeEgress_Success(t *testing.T) {
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
	})

	hash := node.HashFromRawOptions([]byte(`{"type":"egress-ok"}`))
	pool.AddNodeFromSub(hash, []byte(`{"type":"egress-ok"}`), "sub1")

	entry, ok := pool.GetEntry(hash)
	if !ok {
		t.Fatal("entry not found")
	}
	storeOutbound(entry)

	traceBody := []byte("fl=123\nip=203.0.113.1\nloc=US\nts=1234567890")
	mgr := NewProbeManager(ProbeConfig{
		Pool: pool,
		Fetcher: func(_ node.Hash, url string) ([]byte, time.Duration, error) {
			return traceBody, 42 * time.Millisecond, nil
		},
	})

	mgr.probeEgress(hash, entry)

	// Verify RecordResult(true) was applied.
	if entry.FailureCount.Load() != 0 {
		t.Fatalf("expected 0 failures, got %d", entry.FailureCount.Load())
	}
	if entry.CircuitOpenSince.Load() != 0 {
		t.Fatal("circuit should not be open")
	}

	// Verify UpdateNodeEgressIP.
	got := entry.GetEgressIP()
	want := netip.MustParseAddr("203.0.113.1")
	if got != want {
		t.Fatalf("egress IP: got %v, want %v", got, want)
	}
	if got := entry.GetEgressRegion(); got != "us" {
		t.Fatalf("egress region: got %q, want %q", got, "us")
	}

	// Verify RecordLatency for cloudflare.com.
	if !entry.HasLatency() {
		t.Fatal("expected latency data")
	}
	stats, ok := entry.LatencyTable.GetDomainStats("cloudflare.com")
	if !ok {
		t.Fatal("expected cloudflare.com latency entry")
	}
	if stats.Ewma != 42*time.Millisecond {
		t.Fatalf("ewma: got %v, want %v", stats.Ewma, 42*time.Millisecond)
	}
}

// TestProbeEgress_Failure verifies that a failed egress probe calls
// RecordResult(false) and accumulates failure count.
func TestProbeEgress_Failure(t *testing.T) {
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
	})

	hash := node.HashFromRawOptions([]byte(`{"type":"egress-fail"}`))
	pool.AddNodeFromSub(hash, []byte(`{"type":"egress-fail"}`), "sub1")

	entry, ok := pool.GetEntry(hash)
	if !ok {
		t.Fatal("entry not found")
	}
	storeOutbound(entry)

	mgr := NewProbeManager(ProbeConfig{
		Pool: pool,
		Fetcher: func(_ node.Hash, url string) ([]byte, time.Duration, error) {
			return nil, 0, errors.New("connection refused")
		},
	})

	mgr.probeEgress(hash, entry)

	if entry.FailureCount.Load() != 1 {
		t.Fatalf("expected 1 failure, got %d", entry.FailureCount.Load())
	}

	// No latency or egress IP should be recorded.
	if entry.HasLatency() {
		t.Fatal("should not have latency on failure")
	}
}

// TestProbeEgress_CircuitBreak verifies consecutive failures trigger circuit break.
func TestProbeEgress_CircuitBreak(t *testing.T) {
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 2 },
	})

	hash := node.HashFromRawOptions([]byte(`{"type":"egress-circuit"}`))
	pool.AddNodeFromSub(hash, []byte(`{"type":"egress-circuit"}`), "sub1")

	entry, ok := pool.GetEntry(hash)
	if !ok {
		t.Fatal("entry not found")
	}
	storeOutbound(entry)

	mgr := NewProbeManager(ProbeConfig{
		Pool: pool,
		Fetcher: func(_ node.Hash, url string) ([]byte, time.Duration, error) {
			return nil, 0, errors.New("timeout")
		},
	})

	mgr.probeEgress(hash, entry)
	mgr.probeEgress(hash, entry)

	if entry.CircuitOpenSince.Load() == 0 {
		t.Fatal("circuit should be open after 2 consecutive failures")
	}
}

// TestProbeLatency_Success verifies latency probe writes RecordResult+RecordLatency.
func TestProbeLatency_Success(t *testing.T) {
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
	})

	hash := node.HashFromRawOptions([]byte(`{"type":"latency-ok"}`))
	pool.AddNodeFromSub(hash, []byte(`{"type":"latency-ok"}`), "sub1")

	entry, ok := pool.GetEntry(hash)
	if !ok {
		t.Fatal("entry not found")
	}
	storeOutbound(entry)

	mgr := NewProbeManager(ProbeConfig{
		Pool: pool,
		Fetcher: func(_ node.Hash, url string) ([]byte, time.Duration, error) {
			return []byte("OK"), 50 * time.Millisecond, nil
		},
	})

	mgr.probeLatency(hash, entry, "https://www.gstatic.com/generate_204")

	if entry.FailureCount.Load() != 0 {
		t.Fatalf("expected 0 failures, got %d", entry.FailureCount.Load())
	}

	// Should have latency recorded.
	if !entry.HasLatency() {
		t.Fatal("expected latency data")
	}
}

// TestProbeLatency_Failure verifies latency probe failure calls RecordResult(false).
func TestProbeLatency_Failure(t *testing.T) {
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
	})

	hash := node.HashFromRawOptions([]byte(`{"type":"latency-fail"}`))
	pool.AddNodeFromSub(hash, []byte(`{"type":"latency-fail"}`), "sub1")

	entry, ok := pool.GetEntry(hash)
	if !ok {
		t.Fatal("entry not found")
	}
	storeOutbound(entry)

	mgr := NewProbeManager(ProbeConfig{
		Pool: pool,
		Fetcher: func(_ node.Hash, url string) ([]byte, time.Duration, error) {
			return nil, 0, errors.New("tls handshake failed")
		},
	})

	mgr.probeLatency(hash, entry, "https://www.gstatic.com/generate_204")

	if entry.FailureCount.Load() != 1 {
		t.Fatalf("expected 1 failure, got %d", entry.FailureCount.Load())
	}
}

// TestProbeEgress_ZeroLatencyIgnored verifies that a successful probe with a
// non-positive latency sample does not write latency stats.
func TestProbeEgress_ZeroLatencyIgnored(t *testing.T) {
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
	})

	hash := node.HashFromRawOptions([]byte(`{"type":"egress-zero-latency"}`))
	pool.AddNodeFromSub(hash, []byte(`{"type":"egress-zero-latency"}`), "sub1")

	entry, ok := pool.GetEntry(hash)
	if !ok {
		t.Fatal("entry not found")
	}
	storeOutbound(entry)

	traceBody := []byte("fl=123\nip=203.0.113.1\nts=1234567890")
	mgr := NewProbeManager(ProbeConfig{
		Pool: pool,
		Fetcher: func(_ node.Hash, url string) ([]byte, time.Duration, error) {
			return traceBody, 0, nil
		},
	})

	mgr.probeEgress(hash, entry)

	if entry.FailureCount.Load() != 0 {
		t.Fatalf("expected 0 failures, got %d", entry.FailureCount.Load())
	}
	if entry.HasLatency() {
		t.Fatal("zero latency sample should be ignored")
	}
	if got := entry.GetEgressIP(); got != netip.MustParseAddr("203.0.113.1") {
		t.Fatalf("egress IP: got %v, want 203.0.113.1", got)
	}
}

func TestProbeEgress_WithoutLoc_ClearsStoredRegion(t *testing.T) {
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
	})

	hash := node.HashFromRawOptions([]byte(`{"type":"egress-clear-loc"}`))
	pool.AddNodeFromSub(hash, []byte(`{"type":"egress-clear-loc"}`), "sub1")

	entry, ok := pool.GetEntry(hash)
	if !ok {
		t.Fatal("entry not found")
	}
	storeOutbound(entry)
	entry.SetEgressRegion("jp")

	mgr := NewProbeManager(ProbeConfig{
		Pool: pool,
		Fetcher: func(_ node.Hash, url string) ([]byte, time.Duration, error) {
			return []byte("ip=203.0.113.1"), 10 * time.Millisecond, nil
		},
	})

	mgr.probeEgress(hash, entry)

	if got := entry.GetEgressRegion(); got != "" {
		t.Fatalf("egress region: got %q, want empty", got)
	}
}

// TestProbeLatency_ZeroLatencyIgnored verifies that successful latency probes
// skip latency writeback when sample <= 0.
func TestProbeLatency_ZeroLatencyIgnored(t *testing.T) {
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
	})

	hash := node.HashFromRawOptions([]byte(`{"type":"latency-zero"}`))
	pool.AddNodeFromSub(hash, []byte(`{"type":"latency-zero"}`), "sub1")

	entry, ok := pool.GetEntry(hash)
	if !ok {
		t.Fatal("entry not found")
	}
	storeOutbound(entry)

	mgr := NewProbeManager(ProbeConfig{
		Pool: pool,
		Fetcher: func(_ node.Hash, url string) ([]byte, time.Duration, error) {
			return []byte("ok"), 0, nil
		},
	})

	mgr.probeLatency(hash, entry, "https://www.gstatic.com/generate_204")

	if entry.FailureCount.Load() != 0 {
		t.Fatalf("expected 0 failures, got %d", entry.FailureCount.Load())
	}
	if entry.HasLatency() {
		t.Fatal("zero latency sample should be ignored")
	}
}

// TestProbeEgress_NilFetcher verifies graceful handling of nil fetcher.
func TestProbeEgress_NilFetcher(t *testing.T) {
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
	})

	hash := node.HashFromRawOptions([]byte(`{"type":"nil-fetcher"}`))
	pool.AddNodeFromSub(hash, []byte(`{"type":"nil-fetcher"}`), "sub1")

	entry, ok := pool.GetEntry(hash)
	if !ok {
		t.Fatal("entry not found")
	}

	mgr := NewProbeManager(ProbeConfig{Pool: pool}) // no Fetcher
	mgr.probeEgress(hash, entry)                    // should not panic
}

// TestTriggerImmediateEgressProbe_WithFetcher is an integration test
// verifying async probe + health writeback.
func TestTriggerImmediateEgressProbe_WithFetcher(t *testing.T) {
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
	})

	hash := node.HashFromRawOptions([]byte(`{"type":"trigger-egress"}`))
	pool.AddNodeFromSub(hash, []byte(`{"type":"trigger-egress"}`), "sub1")

	entry, ok := pool.GetEntry(hash)
	if !ok {
		t.Fatal("entry not found")
	}
	storeOutbound(entry)

	var called sync.WaitGroup
	called.Add(1)
	mgr := NewProbeManager(ProbeConfig{
		Pool:        pool,
		Concurrency: 1,
		Fetcher: func(_ node.Hash, url string) ([]byte, time.Duration, error) {
			defer called.Done()
			return []byte("ip=198.51.100.1"), 10 * time.Millisecond, nil
		},
	})
	mgr.Start()
	defer mgr.Stop()

	mgr.TriggerImmediateEgressProbe(hash)
	called.Wait()

	// Allow goroutines to complete.
	time.Sleep(20 * time.Millisecond)

	got := entry.GetEgressIP()
	if got != netip.MustParseAddr("198.51.100.1") {
		t.Fatalf("egress IP: got %v, want 198.51.100.1", got)
	}
}

func TestTriggerImmediateLatencyProbe_WithFetcher(t *testing.T) {
	subMgr := topology.NewSubscriptionManager()
	sub := subscription.NewSubscription("sub1", "sub1", "url", true, false)
	subMgr.Register(sub)

	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		SubLookup:              subMgr.Lookup,
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
	})

	hash := node.HashFromRawOptions([]byte(`{"type":"trigger-latency"}`))
	pool.AddNodeFromSub(hash, []byte(`{"type":"trigger-latency"}`), "sub1")
	sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Tags: []string{"tag"}})

	entry, ok := pool.GetEntry(hash)
	if !ok {
		t.Fatal("entry not found")
	}
	storeOutbound(entry)

	var called sync.WaitGroup
	called.Add(1)
	mgr := NewProbeManager(ProbeConfig{
		Pool:        pool,
		Concurrency: 1,
		Fetcher: func(_ node.Hash, _ string) ([]byte, time.Duration, error) {
			defer called.Done()
			return []byte("ok"), 25 * time.Millisecond, nil
		},
	})
	mgr.Start()
	defer mgr.Stop()

	mgr.TriggerImmediateLatencyProbe(hash)
	called.Wait()
	time.Sleep(20 * time.Millisecond)

	if !entry.HasLatency() {
		t.Fatal("expected latency data after immediate latency probe")
	}
}

func TestScanEgress_SkipsDisabledNodes(t *testing.T) {
	subMgr := topology.NewSubscriptionManager()
	sub := subscription.NewSubscription("sub1", "sub1", "url", false, false)
	subMgr.Register(sub)

	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		SubLookup:              subMgr.Lookup,
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
	})

	hash := node.HashFromRawOptions([]byte(`{"type":"scan-egress-disabled"}`))
	pool.AddNodeFromSub(hash, []byte(`{"type":"scan-egress-disabled"}`), "sub1")
	sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Tags: []string{"tag"}})

	entry, ok := pool.GetEntry(hash)
	if !ok {
		t.Fatal("entry not found")
	}
	storeOutbound(entry)

	var calls atomic.Int32
	mgr := NewProbeManager(ProbeConfig{
		Pool: pool,
		Fetcher: func(_ node.Hash, _ string) ([]byte, time.Duration, error) {
			calls.Add(1)
			return []byte("ip=198.51.100.1"), 10 * time.Millisecond, nil
		},
	})

	mgr.scanEgress()
	time.Sleep(30 * time.Millisecond)

	if got := calls.Load(); got != 0 {
		t.Fatalf("disabled node should be skipped by scanEgress, calls=%d", got)
	}
}

func TestScanLatency_SkipsDisabledNodes(t *testing.T) {
	subMgr := topology.NewSubscriptionManager()
	sub := subscription.NewSubscription("sub1", "sub1", "url", false, false)
	subMgr.Register(sub)

	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		SubLookup:              subMgr.Lookup,
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
	})

	hash := node.HashFromRawOptions([]byte(`{"type":"scan-latency-disabled"}`))
	pool.AddNodeFromSub(hash, []byte(`{"type":"scan-latency-disabled"}`), "sub1")
	sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Tags: []string{"tag"}})

	entry, ok := pool.GetEntry(hash)
	if !ok {
		t.Fatal("entry not found")
	}
	storeOutbound(entry)

	var calls atomic.Int32
	mgr := NewProbeManager(ProbeConfig{
		Pool: pool,
		Fetcher: func(_ node.Hash, _ string) ([]byte, time.Duration, error) {
			calls.Add(1)
			return []byte("ok"), 15 * time.Millisecond, nil
		},
	})

	mgr.scanLatency()
	time.Sleep(30 * time.Millisecond)

	if got := calls.Load(); got != 0 {
		t.Fatalf("disabled node should be skipped by scanLatency, calls=%d", got)
	}
}

func TestQueuedAsyncProbe_SkipsNodeDisabledBeforeExecution(t *testing.T) {
	subMgr := topology.NewSubscriptionManager()
	sub := subscription.NewSubscription("sub1", "sub1", "url", true, false)
	subMgr.Register(sub)

	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		SubLookup:              subMgr.Lookup,
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
	})

	hash := node.HashFromRawOptions([]byte(`{"type":"queued-disabled-before-execution"}`))
	pool.AddNodeFromSub(hash, []byte(`{"type":"queued-disabled-before-execution"}`), "sub1")
	sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Tags: []string{"tag"}})

	entry, ok := pool.GetEntry(hash)
	if !ok {
		t.Fatal("entry not found")
	}
	storeOutbound(entry)

	var calls atomic.Int32
	mgr := NewProbeManager(ProbeConfig{
		Pool:        pool,
		Concurrency: 1,
		Fetcher: func(_ node.Hash, _ string) ([]byte, time.Duration, error) {
			calls.Add(1)
			return []byte("ip=198.51.100.1"), 10 * time.Millisecond, nil
		},
	})
	defer mgr.Stop()

	if ok := mgr.enqueueProbe(hash, probeTaskKindEgress, probePriorityNormal); !ok {
		t.Fatal("enqueue should succeed")
	}

	sub.SetEnabled(false)
	mgr.Start()
	time.Sleep(50 * time.Millisecond)

	if got := calls.Load(); got != 0 {
		t.Fatalf("expected disabled queued node to be skipped, calls=%d", got)
	}
}

func TestProbeManager_StopWaitsImmediateProbe(t *testing.T) {
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
	})

	hash := node.HashFromRawOptions([]byte(`{"type":"stop-immediate"}`))
	pool.AddNodeFromSub(hash, []byte(`{"type":"stop-immediate"}`), "sub1")
	entry, ok := pool.GetEntry(hash)
	if !ok {
		t.Fatal("entry not found")
	}
	storeOutbound(entry)

	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32

	mgr := NewProbeManager(ProbeConfig{
		Pool:        pool,
		Concurrency: 1,
		Fetcher: func(_ node.Hash, url string) ([]byte, time.Duration, error) {
			if calls.Add(1) == 1 {
				close(started)
				<-release
			}
			return []byte("ip=203.0.113.1"), 10 * time.Millisecond, nil
		},
	})
	mgr.Start()

	mgr.TriggerImmediateEgressProbe(hash)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("immediate probe did not start")
	}

	stopDone := make(chan struct{})
	go func() {
		mgr.Stop()
		close(stopDone)
	}()

	select {
	case <-stopDone:
		t.Fatal("Stop returned before in-flight immediate probe finished")
	case <-time.After(30 * time.Millisecond):
	}

	close(release)
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("Stop did not complete after in-flight immediate probe finished")
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 probe call, got %d", got)
	}
}

func TestProbeQueue_DequeueChoosesNormalWhenSelectorRequests(t *testing.T) {
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
	})

	hashHigh := node.HashFromRawOptions([]byte(`{"type":"queue-high"}`))
	hashNormal := node.HashFromRawOptions([]byte(`{"type":"queue-normal"}`))
	pool.AddNodeFromSub(hashHigh, []byte(`{"type":"queue-high"}`), "sub1")
	pool.AddNodeFromSub(hashNormal, []byte(`{"type":"queue-normal"}`), "sub1")

	entryHigh, ok := pool.GetEntry(hashHigh)
	if !ok {
		t.Fatal("high entry not found")
	}
	storeOutbound(entryHigh)
	entryNormal, ok := pool.GetEntry(hashNormal)
	if !ok {
		t.Fatal("normal entry not found")
	}
	storeOutbound(entryNormal)

	order := make(chan node.Hash, 2)
	mgr := NewProbeManager(ProbeConfig{
		Pool:        pool,
		Concurrency: 1,
		ChooseNormalWhenBoth: func() bool {
			return true
		},
		Fetcher: func(hash node.Hash, _ string) ([]byte, time.Duration, error) {
			order <- hash
			return []byte("ip=198.51.100.20"), 10 * time.Millisecond, nil
		},
	})
	defer mgr.Stop()

	if ok := mgr.enqueueProbe(hashHigh, probeTaskKindEgress, probePriorityHigh); !ok {
		t.Fatal("enqueue high should succeed")
	}
	if ok := mgr.enqueueProbe(hashNormal, probeTaskKindEgress, probePriorityNormal); !ok {
		t.Fatal("enqueue normal should succeed")
	}

	mgr.Start()

	select {
	case got := <-order:
		if got != hashNormal {
			t.Fatalf("first dequeued hash = %s, want normal %s", got.Hex(), hashNormal.Hex())
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first dequeued task")
	}
}

func TestProbeQueue_HighUpgradeOfQueuedNormalRunsFirstWithoutExtraRun(t *testing.T) {
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
	})

	hashOther := node.HashFromRawOptions([]byte(`{"type":"queue-upgrade-other"}`))
	hashTarget := node.HashFromRawOptions([]byte(`{"type":"queue-upgrade-target"}`))
	pool.AddNodeFromSub(hashOther, []byte(`{"type":"queue-upgrade-other"}`), "sub1")
	pool.AddNodeFromSub(hashTarget, []byte(`{"type":"queue-upgrade-target"}`), "sub1")

	entryOther, ok := pool.GetEntry(hashOther)
	if !ok {
		t.Fatal("other entry not found")
	}
	storeOutbound(entryOther)
	entryTarget, ok := pool.GetEntry(hashTarget)
	if !ok {
		t.Fatal("target entry not found")
	}
	storeOutbound(entryTarget)

	order := make(chan node.Hash, 3)
	mgr := NewProbeManager(ProbeConfig{
		Pool:        pool,
		Concurrency: 1,
		ChooseNormalWhenBoth: func() bool {
			return false
		},
		Fetcher: func(hash node.Hash, _ string) ([]byte, time.Duration, error) {
			order <- hash
			return []byte("ip=198.51.100.21"), 10 * time.Millisecond, nil
		},
	})
	defer mgr.Stop()

	if ok := mgr.enqueueProbe(hashOther, probeTaskKindEgress, probePriorityNormal); !ok {
		t.Fatal("enqueue other normal should succeed")
	}
	if ok := mgr.enqueueProbe(hashTarget, probeTaskKindEgress, probePriorityNormal); !ok {
		t.Fatal("enqueue target normal should succeed")
	}
	if ok := mgr.enqueueProbe(hashTarget, probeTaskKindEgress, probePriorityHigh); !ok {
		t.Fatal("enqueue target high upgrade should add a high-priority token")
	}

	mgr.Start()

	select {
	case got := <-order:
		if got != hashTarget {
			t.Fatalf("first executed hash = %s, want upgraded target %s", got.Hex(), hashTarget.Hex())
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upgraded task")
	}

	select {
	case got := <-order:
		if got != hashOther {
			t.Fatalf("second executed hash = %s, want other %s", got.Hex(), hashOther.Hex())
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for second executed task")
	}

	select {
	case got := <-order:
		t.Fatalf("unexpected extra probe execution for %s", got.Hex())
	case <-time.After(200 * time.Millisecond):
	}
}

func TestProbeQueue_FullDropsWithoutBlocking(t *testing.T) {
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
	})

	hash1 := node.HashFromRawOptions([]byte(`{"type":"queue-full-1"}`))
	hash2 := node.HashFromRawOptions([]byte(`{"type":"queue-full-2"}`))
	pool.AddNodeFromSub(hash1, []byte(`{"type":"queue-full-1"}`), "sub1")
	pool.AddNodeFromSub(hash2, []byte(`{"type":"queue-full-2"}`), "sub1")

	mgr := NewProbeManager(ProbeConfig{
		Pool:          pool,
		Concurrency:   1,
		QueueCapacity: 1,
	})

	if ok := mgr.enqueueProbe(hash1, probeTaskKindEgress, probePriorityNormal); !ok {
		t.Fatal("first enqueue should succeed")
	}
	if ok := mgr.enqueueProbe(hash2, probeTaskKindEgress, probePriorityNormal); ok {
		t.Fatal("second enqueue should be dropped when queue is full")
	}
}

func TestProbeSync_BypassesAsyncWorkerLimit(t *testing.T) {
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
	})

	hashAsync := node.HashFromRawOptions([]byte(`{"type":"sync-bypass-async"}`))
	hashSync := node.HashFromRawOptions([]byte(`{"type":"sync-bypass-sync"}`))
	pool.AddNodeFromSub(hashAsync, []byte(`{"type":"sync-bypass-async"}`), "sub1")
	pool.AddNodeFromSub(hashSync, []byte(`{"type":"sync-bypass-sync"}`), "sub1")

	entryAsync, ok := pool.GetEntry(hashAsync)
	if !ok {
		t.Fatal("async entry not found")
	}
	storeOutbound(entryAsync)
	entrySync, ok := pool.GetEntry(hashSync)
	if !ok {
		t.Fatal("sync entry not found")
	}
	storeOutbound(entrySync)

	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	mgr := NewProbeManager(ProbeConfig{
		Pool:        pool,
		Concurrency: 1,
		Fetcher: func(hash node.Hash, _ string) ([]byte, time.Duration, error) {
			if hash == hashAsync {
				startedOnce.Do(func() { close(started) })
				<-release
				return []byte("ip=198.51.100.31"), 20 * time.Millisecond, nil
			}
			return []byte("ip=198.51.100.32"), 5 * time.Millisecond, nil
		},
	})
	mgr.Start()
	defer mgr.Stop()

	mgr.TriggerImmediateEgressProbe(hashAsync)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("async worker did not start")
	}

	done := make(chan error, 1)
	go func() {
		_, err := mgr.ProbeEgressSync(hashSync)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ProbeEgressSync error: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("ProbeEgressSync blocked by async worker limit")
	}

	close(release)
}

func TestProbeLatencySync_ReturnsEWMAFromNormalizedDomain(t *testing.T) {
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
	})

	hash := node.HashFromRawOptions([]byte(`{"type":"latency-sync"}`))
	pool.AddNodeFromSub(hash, []byte(`{"type":"latency-sync"}`), "sub1")

	entry, ok := pool.GetEntry(hash)
	if !ok {
		t.Fatal("entry not found")
	}
	storeOutbound(entry)

	mgr := NewProbeManager(ProbeConfig{
		Pool: pool,
		Fetcher: func(_ node.Hash, url string) ([]byte, time.Duration, error) {
			return []byte("ok"), 80 * time.Millisecond, nil
		},
		LatencyTestURL: func() string {
			return "https://www.gstatic.com/generate_204"
		},
	})

	result, err := mgr.ProbeLatencySync(hash)
	if err != nil {
		t.Fatalf("ProbeLatencySync: %v", err)
	}
	if result.LatencyEwmaMs <= 0 {
		t.Fatalf("latency_ewma_ms = %f, want > 0", result.LatencyEwmaMs)
	}

	stats, ok := entry.LatencyTable.GetDomainStats("gstatic.com")
	if !ok {
		t.Fatal("expected normalized domain latency entry for gstatic.com")
	}
	if stats.Ewma != 80*time.Millisecond {
		t.Fatalf("stored EWMA = %v, want %v", stats.Ewma, 80*time.Millisecond)
	}
}

func TestProbeSync_EmitsProbeEvents(t *testing.T) {
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
	})

	hash := node.HashFromRawOptions([]byte(`{"type":"probe-sync-events"}`))
	pool.AddNodeFromSub(hash, []byte(`{"type":"probe-sync-events"}`), "sub1")

	entry, ok := pool.GetEntry(hash)
	if !ok {
		t.Fatal("entry not found")
	}
	storeOutbound(entry)

	var gotKinds []string
	mgr := NewProbeManager(ProbeConfig{
		Pool: pool,
		Fetcher: func(_ node.Hash, url string) ([]byte, time.Duration, error) {
			switch url {
			case egressTraceURL:
				return []byte("ip=198.51.100.10"), 20 * time.Millisecond, nil
			default:
				return []byte("ok"), 30 * time.Millisecond, nil
			}
		},
		LatencyTestURL: func() string {
			return "https://www.gstatic.com/generate_204"
		},
		OnProbeEvent: func(kind string) {
			gotKinds = append(gotKinds, kind)
		},
	})

	if _, err := mgr.ProbeEgressSync(hash); err != nil {
		t.Fatalf("ProbeEgressSync: %v", err)
	}
	if _, err := mgr.ProbeLatencySync(hash); err != nil {
		t.Fatalf("ProbeLatencySync: %v", err)
	}

	wantKinds := []string{"egress", "latency"}
	if len(gotKinds) != len(wantKinds) {
		t.Fatalf("probe event count: got %d, want %d (kinds=%v)", len(gotKinds), len(wantKinds), gotKinds)
	}
	for i := range wantKinds {
		if gotKinds[i] != wantKinds[i] {
			t.Fatalf("probe event kind[%d]: got %q, want %q", i, gotKinds[i], wantKinds[i])
		}
	}
}

func TestIsLatencyProbeDue_UsesAttemptTimestamps(t *testing.T) {
	mgr := NewProbeManager(ProbeConfig{})
	hash := node.HashFromRawOptions([]byte(`{"type":"due-check"}`))
	entry := node.NewNodeEntry(hash, []byte(`{"type":"due-check"}`), time.Now(), 16)
	now := time.Now()

	// Seed a very recent latency-table sample; due-check should ignore this and
	// rely on attempt timestamps.
	entry.LatencyTable.LoadEntry("example.com", node.DomainLatencyStats{
		Ewma:        20 * time.Millisecond,
		LastUpdated: now,
	})

	entry.LastLatencyProbeAttempt.Store(now.Add(-10 * time.Minute).UnixNano())
	entry.LastAuthorityLatencyProbeAttempt.Store(now.Add(-10 * time.Minute).UnixNano())
	if !mgr.isLatencyProbeDue(entry, now, 5*time.Minute, 1*time.Hour, []string{"example.com"}, 15*time.Second) {
		t.Fatal("expected due=true when last latency attempt is stale")
	}

	entry.LastLatencyProbeAttempt.Store(now.Add(-1 * time.Minute).UnixNano())
	entry.LastAuthorityLatencyProbeAttempt.Store(now.Add(-2 * time.Hour).UnixNano())
	if !mgr.isLatencyProbeDue(entry, now, 5*time.Minute, 1*time.Hour, []string{"example.com"}, 15*time.Second) {
		t.Fatal("expected due=true when authority attempt is stale")
	}
}

// TestParseCloudflareTrace_Success verifies IP extraction from trace body.
func TestParseCloudflareTrace_Success(t *testing.T) {
	body := []byte("fl=abc\nip=1.2.3.4\nloc=US\nts=12345")
	addr, loc, err := ParseCloudflareTrace(body)
	if err != nil {
		t.Fatal(err)
	}
	if addr != netip.MustParseAddr("1.2.3.4") {
		t.Fatalf("got %v, want 1.2.3.4", addr)
	}
	if loc == nil || *loc != "us" {
		t.Fatalf("loc: got %v, want %q", loc, "us")
	}
}

func TestParseCloudflareTrace_WithoutLoc(t *testing.T) {
	body := []byte("fl=abc\nip=1.2.3.4\nts=12345")
	addr, loc, err := ParseCloudflareTrace(body)
	if err != nil {
		t.Fatal(err)
	}
	if addr != netip.MustParseAddr("1.2.3.4") {
		t.Fatalf("got %v, want 1.2.3.4", addr)
	}
	if loc != nil {
		t.Fatalf("loc: got %v, want nil", loc)
	}
}

// TestParseCloudflareTrace_NoIP verifies error when ip= field is missing.
func TestParseCloudflareTrace_NoIP(t *testing.T) {
	body := []byte("fl=abc\nts=12345")
	_, _, err := ParseCloudflareTrace(body)
	if err == nil {
		t.Fatal("expected error when ip field is missing")
	}
}

func TestProbeTaskQueueRetireRequeuesOriginalPriority(t *testing.T) {
	q := newProbeTaskQueue(4, 4, func() bool { return false })
	q.workers = 2
	q.target = 2
	high := probeTask{key: probeTaskKey{kind: probeTaskKindEgress}}
	normal := probeTask{key: probeTaskKey{kind: probeTaskKindLatency}}
	if !q.Enqueue(high, probePriorityHigh) || !q.Enqueue(normal, probePriorityNormal) {
		t.Fatal("enqueue")
	}
	q.setTarget(1)
	if task, ok := q.Dequeue(); ok {
		t.Fatalf("retiring worker kept %+v", task)
	}
	if q.workers != 1 {
		t.Fatalf("workers after retire: got %d, want 1", q.workers)
	}
	task, ok := q.Dequeue()
	if !ok || task != high {
		t.Fatalf("remaining worker got %+v ok=%v, want high-priority task", task, ok)
	}
}

func TestSetConcurrencyBeforeStart(t *testing.T) {
	mgr := NewProbeManager(ProbeConfig{Concurrency: 1})
	mgr.SetConcurrency(3)
	mgr.startWorkers()
	defer mgr.Stop()
	if mgr.workerNum() != 3 || mgr.Concurrency() != 3 {
		t.Fatalf("workers=%d target=%d", mgr.workerNum(), mgr.Concurrency())
	}
}

func TestSetConcurrencyIgnoredAfterStop(t *testing.T) {
	mgr := NewProbeManager(ProbeConfig{Concurrency: 1})
	mgr.startWorkers()
	mgr.Stop()
	mgr.SetConcurrency(4)
	if mgr.workerNum() != 1 || mgr.Concurrency() != 1 {
		t.Fatalf("workers=%d target=%d", mgr.workerNum(), mgr.Concurrency())
	}
}

func TestSetConcurrencyScalesProbeWorkers(t *testing.T) {
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
	})
	var inflight atomic.Int32
	var maxInflight atomic.Int32
	entered := make(chan struct{}, 8)
	var holdMu sync.Mutex
	hold := make(chan struct{})
	waitHold := func() {
		holdMu.Lock()
		ch := hold
		holdMu.Unlock()
		if ch != nil {
			<-ch
		}
	}
	closeHold := func() {
		holdMu.Lock()
		ch := hold
		hold = nil
		holdMu.Unlock()
		if ch != nil {
			close(ch)
		}
	}
	swapHold := func(next chan struct{}) chan struct{} {
		holdMu.Lock()
		old := hold
		hold = next
		holdMu.Unlock()
		return old
	}

	mgr := NewProbeManager(ProbeConfig{
		Pool:        pool,
		Concurrency: 1,
		Fetcher: func(node.Hash, string) ([]byte, time.Duration, error) {
			n := inflight.Add(1)
			defer inflight.Add(-1)
			for {
				old := maxInflight.Load()
				if n <= old || maxInflight.CompareAndSwap(old, n) {
					break
				}
			}
			entered <- struct{}{}
			waitHold()
			return []byte("ip=203.0.113.9\nloc=US\n"), 5 * time.Millisecond, nil
		},
	})
	mgr.startWorkers()
	t.Cleanup(func() {
		closeHold()
		mgr.Stop()
	})

	raws := [][]byte{
		[]byte(`{"type":"scale-a"}`),
		[]byte(`{"type":"scale-b"}`),
		[]byte(`{"type":"scale-c"}`),
	}
	var hashes []node.Hash
	for _, raw := range raws {
		hash := node.HashFromRawOptions(raw)
		pool.AddNodeFromSub(hash, raw, "sub1")
		entry, ok := pool.GetEntry(hash)
		if !ok {
			t.Fatal("missing entry")
		}
		storeOutbound(entry)
		hashes = append(hashes, hash)
		mgr.TriggerImmediateEgressProbe(hash)
	}

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first worker did not start")
	}
	select {
	case <-entered:
		t.Fatal("concurrency 1 ran a second probe")
	case <-time.After(200 * time.Millisecond):
	}

	mgr.SetConcurrency(3)
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("worker %d did not start after scale up", i+2)
		}
	}
	if got := maxInflight.Load(); got != 3 {
		t.Fatalf("max inflight after scale up: got %d, want 3", got)
	}

	mgr.SetConcurrency(1)
	next := make(chan struct{})
	old := swapHold(next)
	close(old)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.workerNum() == 1 && inflight.Load() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if mgr.workerNum() != 1 || inflight.Load() != 0 {
		t.Fatalf("after scale down workers=%d inflight=%d", mgr.workerNum(), inflight.Load())
	}

	maxInflight.Store(0)
	for _, hash := range hashes {
		mgr.TriggerImmediateEgressProbe(hash)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("remaining worker did not start")
	}
	select {
	case <-entered:
		t.Fatal("scaled-down worker pool ran a second probe")
	case <-time.After(200 * time.Millisecond):
	}
	if got := maxInflight.Load(); got != 1 {
		t.Fatalf("max inflight after scale down: got %d, want 1", got)
	}
}
