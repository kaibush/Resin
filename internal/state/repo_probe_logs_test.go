package state

import (
	"strconv"
	"testing"
	"time"
)

func TestProbeLogsCursorFiltersAndRetention(t *testing.T) {
	r := newTestStateRepo(t)
	now := time.Now().UTC()
	logs := []ProbeLog{
		{Ts: now.Add(-8 * 24 * time.Hour), NodeHash: "old"},
		{Ts: now, NodeHash: "a", Subscriptions: "paid (id)", Kind: "egress", Reason: "manual", TargetHost: "cloudflare.com", Success: true, IngressBytes: 4000, BytesMeasured: true},
		{Ts: now, NodeHash: "b", Kind: "latency", Success: false},
		{Ts: now, NodeHash: "c", Kind: "egress", Success: true},
	}
	if err := r.AppendProbeLogs(logs); err != nil {
		t.Fatal(err)
	}
	rows, more, err := r.ListProbeLogs(ProbeLogFilter{Limit: 2})
	if err != nil || !more || len(rows) != 2 {
		t.Fatalf("page: %v %v %+v", err, more, rows)
	}
	cursor, _ := strconv.ParseInt(rows[1].ID, 10, 64)
	if err = r.AppendProbeLogs([]ProbeLog{{Ts: now, NodeHash: "new"}}); err != nil {
		t.Fatal(err)
	}
	rows, more, err = r.ListProbeLogs(ProbeLogFilter{Limit: 2, Cursor: cursor})
	if err != nil || more || len(rows) != 1 || rows[0].NodeHash != "a" {
		t.Fatalf("unstable cursor: %v %v %+v", err, more, rows)
	}
	success := true
	rows, _, err = r.ListProbeLogs(ProbeLogFilter{Subscription: "paid", NodeHash: "a", Kind: "egress", Reason: "manual", TargetHost: "cloudflare.com", Success: &success, From: now.Add(-time.Second), To: now.Add(time.Second)})
	if err != nil || len(rows) != 1 || rows[0].IngressBytes != 4000 || !rows[0].BytesMeasured {
		t.Fatalf("filter: %v %+v", err, rows)
	}
	rows, _, err = r.ListProbeLogs(ProbeLogFilter{Subscription: "' OR 1=1 --"})
	if err != nil || len(rows) != 0 {
		t.Fatalf("filter escaping: %v %+v", err, rows)
	}
	// Simulate the ID boundary without inserting 100,000 diagnostic records.
	if _, err = r.db.Exec(`UPDATE sqlite_sequence SET seq=? WHERE name='probe_logs'`, ProbeLogRetention+10); err != nil {
		t.Fatal(err)
	}
	if err = r.AppendProbeLogs([]ProbeLog{{Ts: now, NodeHash: "last"}}); err != nil {
		t.Fatal(err)
	}
	rows, _, err = r.ListProbeLogs(ProbeLogFilter{})
	if err != nil || len(rows) != 1 || rows[0].NodeHash != "last" {
		t.Fatalf("retention: %v %+v", err, rows)
	}
}

func TestProbeLogsSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	cache := t.TempDir()
	engine, closer, err := PersistenceBootstrap(dir, cache)
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.StateRepo.AppendProbeLogs([]ProbeLog{{Ts: time.Now(), NodeHash: "persisted", IngressBytes: 4096}}); err != nil {
		t.Fatal(err)
	}
	if err = closer.Close(); err != nil {
		t.Fatal(err)
	}
	engine, closer, err = PersistenceBootstrap(dir, cache)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	rows, _, err := engine.StateRepo.ListProbeLogs(ProbeLogFilter{})
	if err != nil || len(rows) != 1 || rows[0].NodeHash != "persisted" || rows[0].IngressBytes != 4096 {
		t.Fatalf("reopen: %v %+v", err, rows)
	}
}
