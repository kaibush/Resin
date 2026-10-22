package probe

import (
	"errors"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/topology"
)

func TestEgressRetryRateLimitAndRecovery(t *testing.T) {
	pool := topology.NewGlobalNodePool(topology.PoolConfig{MaxLatencyTableEntries: 16, MaxConsecutiveFailures: func() int { return 3 }})
	h := node.HashFromRawOptions([]byte(`{"type":"retry"}`))
	pool.AddNodeFromSub(h, []byte(`{"type":"retry"}`), "sub")
	e, _ := pool.GetEntry(h)
	storeOutbound(e)
	enabled := true
	calls := 0
	fail := true
	m := NewProbeManager(ProbeConfig{Pool: pool, ProbeFailureRetryEnabled: func() bool { return enabled }, Fetcher: func(node.Hash, string) ([]byte, time.Duration, error) {
		calls++
		if fail {
			return nil, 0, errors.New("Get trace: unexpected status: 429 Account exceeded the allowed rate limits")
		}
		return []byte("ip=203.0.113.1\nloc=US\n"), time.Millisecond, nil
	}})
	task := probeTask{key: probeTaskKey{hash: h, kind: probeTaskKindEgress}}
	m.executeTask(task)
	if calls != 1 || e.FailureCount.Load() != 0 || e.LastEgressUpdateAttempt.Load() == 0 {
		t.Fatal("429 must record attempt without marking node broken")
	}
	due := m.egressRetryDue(h, e.LastEgressUpdateAttempt.Load())
	if time.Until(due) < 59*time.Second || time.Until(due) > 77*time.Second {
		t.Fatalf("unexpected retry deadline %v", due)
	}
	m.executeTask(task)
	if calls != 1 {
		t.Fatal("cooldown must suppress queued probes")
	}
	// Other subscriptions can still make progress during the cooldown.
	other := node.HashFromRawOptions([]byte(`{"type":"other"}`))
	pool.AddNodeFromSub(other, []byte(`{"type":"other"}`), "other")
	oe, _ := pool.GetEntry(other)
	if !m.admitAutomaticProbe(oe, time.Now()) {
		t.Fatal("unrelated subscription blocked")
	}
	enabled = false
	m.executeTask(task)
	if calls != 2 || e.FailureCount.Load() != 1 {
		t.Fatal("disabled switch must restore legacy behavior")
	}
	enabled = true
	fail = false
	m.probeGroups["sub"] = probeGroup{}
	m.executeTask(task)
	if calls != 2 {
		t.Fatal("queued task bypassed failure backoff")
	}
	// Explicit manual probes are allowed to verify recovery immediately.
	m.probeEgress(h, e)
	if !e.GetEgressIP().IsValid() || len(m.retries) != 0 {
		t.Fatal("success must clear retry state and restore normal interval")
	}
	m.scanEgress()
	if m.taskQueue.normal.len() != 0 {
		t.Fatal("successful node should wait for normal interval")
	}
}

func TestRetryBackoffAndRestart(t *testing.T) {
	m := NewProbeManager(ProbeConfig{ProbeFailureRetryEnabled: func() bool { return true }})
	h := node.Hash{42}
	last := time.Now().Add(-2 * time.Minute).UnixNano()
	if m.egressRetryDue(h, last).After(time.Now()) {
		t.Fatal("persisted failure must retry after restart without waiting 24h")
	}
	for i := 1; i <= 10; i++ {
		m.recordEgressRetry(h, false)
		d := time.Until(m.egressRetryDue(h, last))
		expected := egressFailureRetryDelay(i)
		if d < expected-time.Second || d > expected+16*time.Second {
			t.Fatalf("failure %d: %v", i, d)
		}
	}
	if egressFailureRetryDelay(10) != 30*time.Minute {
		t.Fatal("backoff must be capped")
	}
}

func TestSubscriptionAdmissionAndCooldown(t *testing.T) {
	pool := topology.NewGlobalNodePool(topology.PoolConfig{MaxConsecutiveFailures: func() int { return 3 }})
	h := node.Hash{1}
	pool.AddNodeFromSub(h, []byte(`{}`), "shared")
	e, _ := pool.GetEntry(h)
	m := NewProbeManager(ProbeConfig{Pool: pool, ProbeFailureRetryEnabled: func() bool { return true }})
	now := time.Now()
	if !m.admitAutomaticProbe(e, now) || m.admitAutomaticProbe(e, now.Add(999*time.Millisecond)) || !m.admitAutomaticProbe(e, now.Add(time.Second)) {
		t.Fatal("expected one start per second")
	}
	err := errors.New("unexpected status: 429 Account exceeded the allowed rate limits")
	m.noteRateLimit(h, err)
	first := m.probeGroups["shared"]
	m.noteRateLimit(h, err)
	if m.probeGroups["shared"].strikes != first.strikes {
		t.Fatal("concurrent 429s must not multiply cooldown")
	}
	g := first
	g.cooldown = time.Now().Add(-time.Second)
	m.probeGroups["shared"] = g
	m.noteRateLimit(h, err)
	if m.probeGroups["shared"].strikes != 2 || time.Until(m.probeGroups["shared"].cooldown) < 119*time.Second {
		t.Fatal("repeated throttling must increase cooldown")
	}
}

func TestRateLimitRecognition(t *testing.T) {
	for _, s := range []string{"unexpected status: 429 Account exceeded", "unexpected status 429", "HTTP/1.1 429 Too Many Requests"} {
		if !isRateLimited(errors.New(s)) {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"dial tcp :4290 failed", "https://example.com/429 timeout", "unexpected status 502"} {
		if isRateLimited(errors.New(s)) {
			t.Fatal(s)
		}
	}
}
