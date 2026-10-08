package api

import (
	"encoding/json"
	"github.com/Resinat/Resin/internal/state"
	"net/http"
	"testing"
	"time"
)

func TestProbeLogEndpointAuthValidationAndPagination(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)
	if err := cp.Engine.StateRepo.AppendProbeLogs([]state.ProbeLog{{Ts: time.Now(), Kind: "egress", NodeHash: "a", Success: true}, {Ts: time.Now(), Kind: "latency", NodeHash: "b"}}); err != nil {
		t.Fatal(err)
	}
	if rr := doJSONRequest(t, srv, http.MethodGet, "/api/v1/probe-logs", nil, false); rr.Code != 401 {
		t.Fatalf("auth: %d", rr.Code)
	}
	for _, query := range []string{"cursor=bad", "cursor=-1", "limit=-1", "limit=2001", "success=yes", "kind=bogus", "from=invalid", "from=2026-01-02T00:00:00Z&to=2026-01-01T00:00:00Z", "offset=1"} {
		rr := doJSONRequest(t, srv, http.MethodGet, "/api/v1/probe-logs?"+query, nil, true)
		if rr.Code != 400 {
			t.Errorf("%s: %d %s", query, rr.Code, rr.Body.String())
		}
	}
	rr := doJSONRequest(t, srv, http.MethodGet, "/api/v1/probe-logs?limit=1", nil, true)
	var page struct {
		Items  []state.ProbeLog `json:"items"`
		More   bool             `json:"has_more"`
		Cursor string           `json:"next_cursor"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil || rr.Code != 200 || !page.More || len(page.Items) != 1 || page.Cursor == "" {
		t.Fatalf("page: %v %s", err, rr.Body.String())
	}
	rr = doJSONRequest(t, srv, http.MethodGet, "/api/v1/probe-logs?limit=1&cursor="+page.Cursor, nil, true)
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil || page.More || len(page.Items) != 1 || page.Items[0].NodeHash != "a" {
		t.Fatalf("next page: %v %s", err, rr.Body.String())
	}
	rr = doJSONRequest(t, srv, http.MethodGet, "/api/v1/probe-logs?success=true&kind=egress&node_hash=a", nil, true)
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil || len(page.Items) != 1 || !page.Items[0].Success {
		t.Fatalf("filter: %v %s", err, rr.Body.String())
	}
}
