package routing

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGovernanceHTTPReusesConnectionAfterSuccessAndError(t *testing.T) {
	var connections, calls atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_, _ = io.WriteString(w, `{"mode":"strict","status":"enforced"}`)
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, strings.Repeat(" ", 4096))
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	t.Setenv("RESIN_GOVERNANCE_URL", server.URL)
	t.Setenv("RESIN_GOVERNANCE_TOKEN", "test")
	router := &Router{governance: newGovernanceClient()}
	defer router.governance.client.CloseIdleConnections()
	for i := 0; i < 3; i++ {
		if ready := router.GovernanceReady(); ready != (i != 1) {
			t.Fatalf("call %d readiness=%v", i+1, ready)
		}
	}
	if connections.Load() != 1 {
		t.Fatalf("3 callbacks used %d connections, want 1", connections.Load())
	}
}

func TestGovernanceHTTPRejectsOversizedAndTrailingData(t *testing.T) {
	for _, payload := range []string{`{"mode":"strict","status":"enforced"}{}`, `{"mode":"strict","status":"enforced"}` + strings.Repeat(" ", 1<<20)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, payload)
		}))
		t.Setenv("RESIN_GOVERNANCE_URL", server.URL)
		t.Setenv("RESIN_GOVERNANCE_TOKEN", "test")
		router := &Router{governance: newGovernanceClient()}
		ready := router.GovernanceReady()
		router.governance.client.CloseIdleConnections()
		server.Close()
		if ready {
			t.Fatal("invalid response accepted")
		}
	}
}
