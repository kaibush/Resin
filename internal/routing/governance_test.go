package routing

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/platform"
)

func TestGovernanceFiltersFullViewAndRechecksExistingLease(t *testing.T) {
	pool := newRouterTestPool()
	plat := platform.NewPlatform("p", "Platform", nil, nil)
	plat.StickyTTLNs = int64(time.Hour)
	plat.IPGovernanceEnabled = true
	pool.addPlatform(plat)
	a, ea := newRoutableEntry(t, `{"id":"a"}`, "192.0.2.1")
	b, eb := newRoutableEntry(t, `{"id":"b"}`, "192.0.2.2")
	pool.addEntry(a, ea)
	pool.addEntry(b, eb)
	pool.rebuildPlatformView(plat)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Error("missing callback authorization")
		}
		var in struct {
			Identity   string                `json:"identity"`
			Candidates []governanceCandidate `json:"candidates"`
			CurrentIP  string                `json:"currentIP"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
		}
		if len(in.Candidates) != 2 || in.Identity != "account" {
			t.Errorf("incomplete group/view: %+v", in)
		}
		if calls.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(governanceDecision{Mode: "strict", Status: "allowed", NodeHash: b.Hex(), IP: "192.0.2.2", ValidUntil: time.Now().Add(time.Minute)})
			return
		}
		if in.CurrentIP != "192.0.2.2" {
			t.Errorf("lost original binding: %+v", in)
		}
		_ = json.NewEncoder(w).Encode(governanceDecision{Mode: "strict", Status: "waiting"})
	}))
	defer server.Close()
	router := newTestRouter(pool, nil)
	router.governance = &governanceClient{endpoint: server.URL, token: "test", client: server.Client()}
	result, err := router.RouteRequest("Platform", "account", "example.com")
	if err != nil || result.NodeHash != b {
		t.Fatalf("route: %+v %v", result, err)
	}
	if _, err := router.RouteRequest("Platform", "account", "example.com"); !errors.Is(err, ErrNoAvailableNodes) {
		t.Fatalf("existing lease bypassed isolation: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("callback calls: %d", calls.Load())
	}
}

func TestGovernanceAuditAndCallbackFailure(t *testing.T) {
	pool := newRouterTestPool()
	plat := platform.NewPlatform("p", "Platform", nil, nil)
	plat.IPGovernanceEnabled = true
	pool.addPlatform(plat)
	hash, entry := newRoutableEntry(t, `{"id":"a"}`, "192.0.2.1")
	pool.addEntry(hash, entry)
	pool.rebuildPlatformView(plat)
	var unavailable atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unavailable.Load() {
			w.WriteHeader(503)
			return
		}
		_ = json.NewEncoder(w).Encode(governanceDecision{Mode: "audit", Status: "audit"})
	}))
	defer server.Close()
	router := newTestRouter(pool, nil)
	router.governance = &governanceClient{endpoint: server.URL, token: "test", client: server.Client()}
	if _, err := router.RouteRequest("Platform", "", "example.com"); err != nil {
		t.Fatalf("audit changed anonymous routing: %v", err)
	}
	if _, err := router.RouteRequest("Platform", "account", "example.com"); err != nil {
		t.Fatal(err)
	}
	unavailable.Store(true)
	if _, err := router.RouteRequest("Platform", "account", "example.com"); !errors.Is(err, ErrNoAvailableNodes) {
		t.Fatalf("callback failure fell back: %v", err)
	}
	if err := router.UpsertLease(model.Lease{PlatformID: "p", Account: "imported", NodeHash: hash.Hex(), EgressIP: "192.0.2.1", ExpiryNs: time.Now().Add(time.Hour).UnixNano()}); !errors.Is(err, ErrNoAvailableNodes) {
		t.Fatalf("inheritance bypassed governance: %v", err)
	}
}

func TestGovernanceRejectsExpiredAdmissionAndChangedIP(t *testing.T) {
	pool := newRouterTestPool()
	plat := platform.NewPlatform("p", "Platform", nil, nil)
	plat.IPGovernanceEnabled = true
	pool.addPlatform(plat)
	hash, entry := newRoutableEntry(t, `{"id":"a"}`, "192.0.2.1")
	pool.addEntry(hash, entry)
	pool.rebuildPlatformView(plat)
	for _, test := range []struct {
		name, ip string
		expiry   time.Time
	}{{"expired", "192.0.2.1", time.Now().Add(-time.Second)}, {"wrong-ip", "192.0.2.2", time.Now().Add(time.Minute)}} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(governanceDecision{Mode: "strict", Status: "allowed", NodeHash: hash.Hex(), IP: test.ip, ValidUntil: test.expiry})
			}))
			defer server.Close()
			router := newTestRouter(pool, nil)
			router.governance = &governanceClient{endpoint: server.URL, token: "test", client: server.Client()}
			if _, err := router.RouteRequest("Platform", "account", "example.com"); !errors.Is(err, ErrNoAvailableNodes) {
				t.Fatalf("invalid admission accepted: %v", err)
			}
		})
	}
}

func TestGovernancePlatformTTLStillRechecksAdmission(t *testing.T) {
	for _, mode := range []string{"soft", "strict"} {
		for _, tc := range []struct {
			name      string
			follow    bool
			ttl, want time.Duration
		}{
			{"legacy", false, 24 * time.Hour, time.Minute},
			{"platform", true, 24 * time.Hour, 24 * time.Hour},
			{"short platform", true, 10 * time.Second, 10 * time.Second},
			{"legacy short platform", false, 10 * time.Second, 10 * time.Second},
			{"default platform", true, 0, 24 * time.Hour},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				pool := newRouterTestPool()
				plat := platform.NewPlatform("p", "Platform", nil, nil)
				plat.StickyTTLNs = int64(tc.ttl)
				plat.IPGovernanceEnabled = true
				pool.addPlatform(plat)
				hash, entry := newRoutableEntry(t, `{"id":"a"}`, "192.0.2.1")
				pool.addEntry(hash, entry)
				pool.rebuildPlatformView(plat)
				var expired, denied atomic.Bool
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					until := time.Now().Add(time.Minute)
					if expired.Load() {
						until = time.Now().Add(-time.Second)
					}
					status := "allowed"
					if denied.Load() {
						status = "waiting"
					}
					_ = json.NewEncoder(w).Encode(governanceDecision{Mode: mode, Status: status, NodeHash: hash.Hex(), IP: "192.0.2.1", ValidUntil: until, FollowPlatformTTL: tc.follow})
				}))
				defer server.Close()
				router := newTestRouter(pool, nil)
				router.governance = &governanceClient{endpoint: server.URL, token: "test", client: server.Client()}
				before := time.Now()
				if _, err := router.RouteRequest("Platform", "account", "example.com"); err != nil {
					t.Fatal(err)
				}
				state, ok := router.states.Load(plat.ID)
				if !ok {
					t.Fatal("missing routing state")
				}
				lease, ok := state.Leases.GetLease("account")
				if !ok || lease.ExpiryNs < before.Add(tc.want).UnixNano() || lease.ExpiryNs > time.Now().Add(tc.want).UnixNano() {
					t.Fatalf("lease expiry does not match %s: %+v", tc.want, lease)
				}
				// Even an unexpired 24-hour lease requires a fresh, valid admission.
				expired.Store(true)
				if _, err := router.RouteRequest("Platform", "account", "example.com"); !errors.Is(err, ErrNoAvailableNodes) {
					t.Fatalf("expired admission accepted: %v", err)
				}
				expired.Store(false)
				denied.Store(true)
				if _, err := router.RouteRequest("Platform", "account", "example.com"); !errors.Is(err, ErrNoAvailableNodes) {
					t.Fatalf("denied admission accepted: %v", err)
				}
				if calls.Load() != 3 {
					t.Fatalf("lease bypassed admission: %d calls", calls.Load())
				}
			})
		}
	}
}

func TestGovernanceScopeIsolatesOrdinaryPlatforms(t *testing.T) {
	pool := newRouterTestPool()
	ordinary := platform.NewPlatform("ordinary", "Ordinary", nil, nil)
	governed := platform.NewPlatform("governed", "Governed", nil, nil)
	governed.IPGovernanceEnabled = true
	hash, entry := newRoutableEntry(t, `{"id":"a"}`, "192.0.2.1")
	pool.addEntry(hash, entry)
	for _, p := range []*platform.Platform{ordinary, governed} {
		pool.addPlatform(p)
		pool.rebuildPlatformView(p)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) }))
	defer server.Close()
	router := newTestRouter(pool, nil)
	router.governance = &governanceClient{endpoint: server.URL, token: "test", client: server.Client()}
	for _, account := range []string{"", "sticky", "sticky"} {
		result, err := router.RouteRequest("Ordinary", account, "example.com")
		if err != nil || result.NodeHash != hash || result.Governed {
			t.Fatalf("ordinary route affected: %+v %v", result, err)
		}
	}
	if err := router.UpsertLease(model.Lease{PlatformID: "ordinary", Account: "imported", NodeHash: hash.Hex(), EgressIP: "192.0.2.1", ExpiryNs: time.Now().Add(time.Hour).UnixNano()}); err != nil {
		t.Fatal(err)
	}
	if router.GovernanceEnforced("Ordinary") {
		t.Fatal("ordinary bypass affected")
	}
	if calls.Load() != 0 {
		t.Fatalf("ordinary traffic called governance %d times", calls.Load())
	}
	for _, account := range []string{"", "sticky"} {
		if _, err := router.RouteRequest("Governed", account, "example.com"); !errors.Is(err, ErrNoAvailableNodes) {
			t.Fatalf("governed failure fell open: %v", err)
		}
	}
	if err := router.UpsertLease(model.Lease{PlatformID: "governed", Account: "imported"}); !errors.Is(err, ErrNoAvailableNodes) {
		t.Fatalf("governed import fell open: %v", err)
	}
	if !router.GovernanceEnforced("Governed") {
		t.Fatal("governed bypass fell open")
	}
	if calls.Load() != 4 {
		t.Fatalf("expected 4 governed checks, got %d", calls.Load())
	}
}

func TestGovernanceEnabledWithoutCallbackFailsClosed(t *testing.T) {
	pool := newRouterTestPool()
	p := platform.NewPlatform("p", "Platform", nil, nil)
	p.IPGovernanceEnabled = true
	pool.addPlatform(p)
	r := newTestRouter(pool, nil)
	r.governance = nil
	if _, err := r.RouteRequest("Platform", "", "example.com"); !errors.Is(err, ErrNoAvailableNodes) {
		t.Fatalf("missing callback allowed route: %v", err)
	}
	if !r.GovernanceEnforced("Platform") {
		t.Fatal("missing callback allowed bypass")
	}
}
