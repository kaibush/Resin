package probe

import (
	"errors"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/state"
	"github.com/Resinat/Resin/internal/topology"
	"testing"
	"time"
)

func TestProbeLoggingActualResultsAndDrain(t *testing.T) {
	engine, closer, err := state.PersistenceBootstrap(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	pool := topology.NewGlobalNodePool(topology.PoolConfig{MaxLatencyTableEntries: 16, MaxConsecutiveFailures: func() int { return 3 }})
	h := node.HashFromRawOptions([]byte("log-node"))
	pool.AddNodeFromSub(h, []byte("log-node"), "sub")
	body := []byte("ip=203.0.113.1\nloc=US\n")
	var fetchErr error
	m := NewProbeManager(ProbeConfig{Pool: pool, LogRepo: engine.StateRepo, ObservedFetcher: func(node.Hash, string) ([]byte, time.Duration, int64, int64, error) {
		return body, 42 * time.Millisecond, 4000, 1000, fetchErr
	}})
	if _, _, err = m.performEgressProbe(h, "manual"); err != nil {
		t.Fatal(err)
	}
	body = []byte("bad trace")
	if _, _, err = m.performEgressProbe(h); err == nil {
		t.Fatal("expected parse failure")
	}
	fetchErr = errors.New("secret password https://user:password@host/")
	if err = m.performLatencyProbe(h, "https://gstatic.com/path?token=secret"); err == nil {
		t.Fatal("expected fetch failure")
	}
	m.Stop()
	m.Stop()
	rows, _, err := engine.StateRepo.ListProbeLogs(state.ProbeLogFilter{})
	if err != nil || len(rows) != 3 {
		t.Fatalf("drain: %v %+v", err, rows)
	}
	if rows[0].Success || rows[1].Success || !rows[2].Success || rows[2].EgressIP != "203.0.113.1" || rows[2].Reason != "manual" {
		t.Fatalf("results: %+v", rows)
	}
	if rows[0].TargetHost != "gstatic.com" || rows[0].Error != "probe failed" || rows[2].IngressBytes != 4000 || rows[2].LatencyMs != 42 {
		t.Fatalf("metadata: %+v", rows)
	}
}
func TestMeteredProbeLogNoDuplicateOnCacheHit(t *testing.T) {
	f := newMeteredFixture(t)
	f.m.logs = newLogWriter(f.c.repo)
	defer f.m.Stop()
	h := f.add(1)
	if err := f.m.VerifyRouteNode(h); err != nil {
		t.Fatal(err)
	}
	if err := f.m.VerifyRouteNode(h); err != nil {
		t.Fatal(err)
	}
	f.m.Stop()
	rows, _, err := f.c.repo.ListProbeLogs(state.ProbeLogFilter{})
	if err != nil || len(rows) != 1 || rows[0].Reason != "required" || rows[0].IngressBytes != 4000 {
		t.Fatalf("metered logs: %v %+v", err, rows)
	}
}

func TestProbeLogQueueOverflowVisible(t *testing.T) {
	w := &logWriter{queue: make(chan state.ProbeLog, 1)}
	w.emit(state.ProbeLog{})
	w.emit(state.ProbeLog{})
	if w.dropped.Load() != 1 {
		t.Fatal("missing dropped count")
	}
}
