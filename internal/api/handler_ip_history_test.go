package api

import (
	"fmt"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/testutil"
)

func TestIPHistoryAPIIncludesZeroCountsFiltersAndReset(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)
	id := mustCreatePlatform(t, srv, "ip-history-api")
	p, _ := cp.Pool.GetPlatform(id)
	p.IPGovernanceEnabled = true
	p.IPGovernanceMaxAccountsPerIP = 1
	sub := subscription.NewSubscription("history-sub", "History", "https://example.com/sub", true, false)
	cp.SubMgr.Register(sub)
	for i, ip := range []string{"192.0.2.1", "192.0.2.1", "192.0.2.2"} {
		raw := []byte(fmt.Sprintf(`{"type":"ss","server":"1.1.1.1","port":%d}`, 443+i))
		hash := node.HashFromRawOptions(raw)
		cp.Pool.AddNodeFromSub(hash, raw, sub.ID)
		sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Tags: []string{"seed"}})
		entry, _ := cp.Pool.GetEntry(hash)
		entry.SetEgressIP(netip.MustParseAddr(ip))
		entry.LatencyTable.Update("example.com", time.Millisecond, time.Minute)
		ob := testutil.NewNoopOutbound()
		entry.Outbound.Store(&ob)
		cp.Pool.RecordResult(hash, true)
	}
	cp.Pool.RebuildPlatform(p)
	cp.Router.RestoreIPHistory([]model.IPHistory{{EgressIP: "192.0.2.1", AccountHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FirstSeenNs: 1}})
	base := "/api/v1/platforms/" + id + "/ip-history"
	rec := doJSONRequest(t, srv, http.MethodGet, base+"?limit=1", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d %s", rec.Code, rec.Body)
	}
	body := decodeJSONMap(t, rec)
	summary := body["summary"].(map[string]any)
	if body["total"] != float64(2) || summary["available_ips"] != float64(2) || summary["unused_ips"] != float64(1) || summary["reached_ips"] != float64(1) {
		t.Fatalf("summary=%v", body)
	}
	items := body["items"].([]any)
	row := items[0].(map[string]any)
	if len(items) != 1 || row["egress_ip"] != "192.0.2.1" || row["node_count"] != float64(2) || row["threshold_status"] != "reached" {
		t.Fatalf("rows=%v", items)
	}
	for _, query := range []string{"?status=unused", "?egress_ip=192.0.2.2", "?offset=1&limit=1"} {
		rec = doJSONRequest(t, srv, http.MethodGet, base+query, nil, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("query=%s status=%d", query, rec.Code)
		}
		rows := decodeJSONMap(t, rec)["items"].([]any)
		if len(rows) != 1 || rows[0].(map[string]any)["historical_accounts"] != float64(0) {
			t.Fatalf("unused missing: %v", rows)
		}
	}
	p.IPGovernanceEnabled = false
	rec = doJSONRequest(t, srv, http.MethodGet, base, nil, true)
	rows := decodeJSONMap(t, rec)["items"].([]any)
	if rows[0].(map[string]any)["threshold_status"] != "disabled" {
		t.Fatal("disabled threshold not shown")
	}
	rec = doJSONRequest(t, srv, http.MethodDelete, base+"?egress_ip=192.0.2.1", nil, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated reset status=%d", rec.Code)
	}
	rec = doJSONRequest(t, srv, http.MethodDelete, base+"?egress_ip=192.0.2.1", nil, true)
	if rec.Code != http.StatusNoContent || cp.Router.HistoricalIPCount(netip.MustParseAddr("192.0.2.1")) != 0 {
		t.Fatalf("reset=%d %s", rec.Code, rec.Body)
	}
}
