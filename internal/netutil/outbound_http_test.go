package netutil

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Resinat/Resin/internal/testutil"
)

func TestHTTPGetViaOutbound_RequireStatusOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}))
	defer srv.Close()

	ob, err := (&testutil.StubOutboundBuilder{}).Build(nil)
	if err != nil {
		t.Fatalf("build outbound: %v", err)
	}
	_, _, err = HTTPGetViaOutbound(context.Background(), ob, srv.URL, OutboundHTTPOptions{
		RequireStatusOK: true,
	})
	if err == nil {
		t.Fatal("expected non-200 status to return error")
	}
	if !strings.Contains(err.Error(), "unexpected status 404") {
		t.Fatalf("expected status error, got: %v", err)
	}
}

func TestHTTPGetViaOutbound_AllowNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("probe-body"))
	}))
	defer srv.Close()

	ob, err := (&testutil.StubOutboundBuilder{}).Build(nil)
	if err != nil {
		t.Fatalf("build outbound: %v", err)
	}
	body, _, err := HTTPGetViaOutbound(context.Background(), ob, srv.URL, OutboundHTTPOptions{
		RequireStatusOK: false,
	})
	if err != nil {
		t.Fatalf("expected non-200 response to pass through, got: %v", err)
	}
	if string(body) != "probe-body" {
		t.Fatalf("unexpected body %q", string(body))
	}
}

func TestConnCloseHook_CloseIsIdempotentAndConcurrentSafe(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	var onCloseCount atomic.Int32
	hook := &connCloseHook{
		Conn: client,
		onClose: func() {
			onCloseCount.Add(1)
		},
	}

	const closers = 32
	var wg sync.WaitGroup
	wg.Add(closers)
	for i := 0; i < closers; i++ {
		go func() {
			defer wg.Done()
			_ = hook.Close()
		}()
	}
	wg.Wait()

	if got := onCloseCount.Load(); got != 1 {
		t.Fatalf("onClose called %d times, want 1", got)
	}
}

func TestProbeResponseAndTransferLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte(strings.Repeat("x", 1<<20)))
	}))
	defer srv.Close()
	ob, err := (&testutil.StubOutboundBuilder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name           string
		body, transfer int64
	}{{"body", 1024, 65536}, {"wire", 0, 2048}} {
		t.Run(tc.name, func(t *testing.T) {
			var in, out int64
			_, _, err := HTTPGetViaOutbound(context.Background(), ob, srv.URL, OutboundHTTPOptions{MaxBodyBytes: tc.body, MaxTransferBytes: tc.transfer, OnBytes: func(i, o int64) { in = i; out = o }})
			if err == nil {
				t.Fatal("oversized response accepted")
			}
			if in == 0 || out == 0 || in+out > tc.transfer {
				t.Fatalf("invalid accounting in=%d out=%d cap=%d", in, out, tc.transfer)
			}
		})
	}
}
func TestProbeRedirectDoesNotConnectToNextTarget(t *testing.T) {
	var targetCalls atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer source.Close()
	ob, err := (&testutil.StubOutboundBuilder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = HTTPGetViaOutbound(context.Background(), ob, source.URL, OutboundHTTPOptions{MaxRedirects: 1, MaxTransferBytes: 65536})
	if err == nil || targetCalls.Load() != 0 {
		t.Fatalf("redirect followed: calls=%d err=%v", targetCalls.Load(), err)
	}
}

func TestProbeCountsTLSHandshakeFailure(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("untrusted TLS request reached handler") }))
	defer server.Close()
	ob, err := (&testutil.StubOutboundBuilder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	var in, out int64
	_, _, err = HTTPGetViaOutbound(context.Background(), ob, server.URL, OutboundHTTPOptions{MaxTransferBytes: 65536, OnBytes: func(i, o int64) { in = i; out = o }})
	if err == nil {
		t.Fatal("untrusted certificate accepted")
	}
	if in == 0 || out == 0 || in+out > 65536 {
		t.Fatalf("TLS bytes not accounted: %d/%d", in, out)
	}
}
