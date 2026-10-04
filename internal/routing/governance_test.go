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
