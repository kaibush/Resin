package service

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/probepolicy"
	"github.com/Resinat/Resin/internal/state"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/testutil"
)

func TestSubscriptionMeteredPolicyCreatePatchAndPersistence(t *testing.T) {
	cp, _, _ := newCleanupSubscriptionTestService()
	engine, closer, err := state.PersistenceBootstrap(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	cp.Engine = engine
	name, source, content := "paid", "local", "socks5://127.0.0.1:8080"
	policy := probepolicy.DefaultMeteredProbePolicy()
	res, err := cp.CreateSubscription(CreateSubscriptionRequest{Name: &name, SourceType: &source, Content: &content, ProbePolicy: &policy})
	if err != nil {
		t.Fatal(err)
	}
	policy.StrictBudget = true
	policy.EgressInterval = probepolicy.Duration(12 * time.Hour)
	body, _ := json.Marshal(map[string]any{"probe_policy": policy})
	res, err = cp.UpdateSubscription(res.ID, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.ProbePolicy != policy {
		t.Fatalf("response %+v", res.ProbePolicy)
	}
	stored, err := engine.ListSubscriptions()
	if err != nil || stored[0].ProbePolicy != policy {
		t.Fatalf("persistence %+v %v", stored, err)
	}
	for _, bad := range []string{`{"probe_policy":null}`, `{"probe_policy":{"mode":"typo"}}`, `{"probe_policy":{"mode":"metered","egress_interval":"0s"}}`, `{"probe_policy":{"mode":"inherit","unexpected":true}}`} {
		if _, err = cp.UpdateSubscription(res.ID, []byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if cp.SubMgr.Lookup(res.ID).ProbePolicy() != policy {
		t.Fatal("invalid patch mutated runtime")
	}
	res, err = cp.UpdateSubscription(res.ID, []byte(`{"probe_policy":{"mode":"inherit"}}`))
	if err != nil || res.ProbePolicy.Mode != "inherit" {
		t.Fatalf("inherit: %v", err)
	}
}
func TestMeteredCleanupPreservesUnverifiedNodes(t *testing.T) {
	cp, subs, pool := newCleanupSubscriptionTestService()
	sub := subscription.NewSubscription("paid", "paid", "", true, true)
	sub.SetProbePolicy(probepolicy.DefaultMeteredProbePolicy())
	subs.Register(sub)
	raw := []byte(`{"type":"socks","server":"localhost","server_port":1234}`)
	h := node.HashFromRawOptions(raw)
	pool.AddNodeFromSub(h, raw, sub.ID)
	sub.ManagedNodes().StoreNode(h, subscription.ManagedNode{})
	entry, _ := pool.GetEntry(h)
	ob := testutil.NewNoopOutbound()
	entry.Outbound.Store(&ob)
	defer entry.LatencyTable.Close()
	count, err := cp.CleanupSubscriptionCircuitOpenNodes(sub.ID)
	if err != nil || count != 0 {
		t.Fatalf("pending node removed %d %v", count, err)
	}
	entry.FailureCount.Store(3)
	count, err = cp.CleanupSubscriptionCircuitOpenNodes(sub.ID)
	if err != nil || count != 1 {
		t.Fatalf("failed node not removed %d %v", count, err)
	}
}
