package probepolicy

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestProbePolicyPriceIsConfigurable(t *testing.T) {
	var missing ProbePolicy
	if err := json.Unmarshal([]byte(`{"mode":"metered","egress_interval":"24h","active_window":"24h","max_egress_age":"24h","monthly_budget_bytes":1000000000,"strict_budget":false}`), &missing); err != nil {
		t.Fatal(err)
	}
	if missing.PricePerGB != 0 {
		t.Fatalf("missing price = %v, want 0", missing.PricePerGB)
	}
	if err := missing.Validate(); err != nil {
		t.Fatal(err)
	}

	policy := DefaultMeteredProbePolicy()
	policy.PricePerGB = 2.5
	raw, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	var got ProbePolicy
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got != policy {
		t.Fatalf("round trip %+v", got)
	}
	if got.EgressInterval != Duration(24*time.Hour) {
		t.Fatalf("interval = %v", got.EgressInterval)
	}

	for _, price := range []float64{-0.01, math.NaN(), math.Inf(1), 1_000_001} {
		bad := policy
		bad.PricePerGB = price
		if err := bad.Validate(); err == nil {
			t.Fatalf("price %v accepted", price)
		}
	}
}
