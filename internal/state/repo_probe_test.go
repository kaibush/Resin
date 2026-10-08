package state

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/probepolicy"
)

func TestProbeBudgetConcurrentReservationAndSettlement(t *testing.T) {
	r := newTestStateRepo(t)
	var wg sync.WaitGroup
	var admitted atomic.Int64
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := r.ReserveProbe("sub", "2026-10-08", "periodic", 1000, 100, true)
			if err == nil {
				admitted.Add(1)
			} else if !errors.Is(err, ErrProbeBudget) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 10 {
		t.Fatalf("admitted %d, want 10", admitted.Load())
	}
	if err := r.FinishProbe("sub", "2026-10-08", "periodic", 100, 20, 30, false); err != nil {
		t.Fatal(err)
	}
	if err := r.ReserveProbe("sub", "2026-10-08", "required", 1000, 100, true); !errors.Is(err, ErrProbeBudget) {
		t.Fatalf("strict required bypassed budget: %v", err)
	}
	if err := r.ReserveProbe("sub", "2026-10-08", "required", 1000, 100, false); err != nil {
		t.Fatal(err)
	}
	if err := r.ReserveProbe("sub", "2026-11-01", "periodic", 1000, 100, true); err != nil {
		t.Fatalf("month rollover: %v", err)
	}
	rows, err := r.ProbeUsage("sub", time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].IngressBytes+rows[0].EgressBytes != 50 {
		t.Fatalf("usage: %+v", rows)
	}
}
func TestProbeBudgetAndNodeStateSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	engine, closer, err := PersistenceBootstrap(dir, dir+"/cache")
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.ReserveProbe("sub", "2026-10-08", "periodic", 100, 100, true); err != nil {
		t.Fatal(err)
	}
	want := ProbeNodeState{LastUsedNs: 1, VerifiedNs: 2, NextRetryNs: 3, Failures: 4}
	if err = engine.SaveProbeNodeState("hash", want); err != nil {
		t.Fatal(err)
	}
	closer.Close()
	engine, closer, err = PersistenceBootstrap(dir, dir+"/cache")
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	if err = engine.ReserveProbe("sub", "2026-10-08", "periodic", 100, 100, true); !errors.Is(err, ErrProbeBudget) {
		t.Fatalf("restart reset reservation: %v", err)
	}
	got, err := engine.LoadProbeNodeStates()
	if err != nil || got["hash"] != want {
		t.Fatalf("state: %+v %v", got, err)
	}
}
func TestSubscriptionProbePolicyRoundTrip(t *testing.T) {
	r := newTestStateRepo(t)
	p := probepolicy.DefaultMeteredProbePolicy()
	p.StrictBudget = true
	s := model.Subscription{ID: "s", Name: "paid", UpdateIntervalNs: int64(time.Hour), ProbePolicy: p}
	if err := r.UpsertSubscription(s); err != nil {
		t.Fatal(err)
	}
	rows, err := r.ListSubscriptions()
	if err != nil || len(rows) != 1 || rows[0].ProbePolicy != p {
		t.Fatalf("roundtrip %+v %v", rows, err)
	}
	s.ProbePolicy = probepolicy.ProbePolicy{}
	if err = r.UpsertSubscription(s); err != nil {
		t.Fatal(err)
	}
	rows, err = r.ListSubscriptions()
	if err != nil || rows[0].ProbePolicy.Mode != "" {
		t.Fatalf("inherit %+v %v", rows, err)
	}
}

func TestPlatformVerificationAgeRoundTrip(t *testing.T) {
	r := newTestStateRepo(t)
	p := model.Platform{ID: "paid", Name: "paid", StickyTTLNs: int64(time.Hour), ReverseProxyMissAction: "TREAT_AS_EMPTY", ReverseProxyEmptyAccountBehavior: "RANDOM", AllocationPolicy: "BALANCED", EgressVerificationMaxAgeNs: int64(time.Hour)}
	if err := r.UpsertPlatform(p); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetPlatform(p.ID)
	if err != nil || got.EgressVerificationMaxAgeNs != p.EgressVerificationMaxAgeNs {
		t.Fatalf("get %+v %v", got, err)
	}
	all, err := r.ListPlatforms()
	if err != nil || len(all) != 1 || all[0].EgressVerificationMaxAgeNs != p.EgressVerificationMaxAgeNs {
		t.Fatalf("list %+v %v", all, err)
	}
}
