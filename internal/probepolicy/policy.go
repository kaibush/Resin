package probepolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var ErrBudget = errors.New("probe traffic budget exhausted")

// ProbePolicy is opt-in: the zero value preserves legacy global scheduling.
type ProbePolicy struct {
	Mode               string   `json:"mode"`
	EgressInterval     Duration `json:"egress_interval"`
	ActiveWindow       Duration `json:"active_window"`
	MaxEgressAge       Duration `json:"max_egress_age"`
	MonthlyBudgetBytes int64    `json:"monthly_budget_bytes"`
	StrictBudget       bool     `json:"strict_budget"`
}

func DefaultMeteredProbePolicy() ProbePolicy {
	return ProbePolicy{Mode: "metered", EgressInterval: Duration(24 * time.Hour), ActiveWindow: Duration(24 * time.Hour), MaxEgressAge: Duration(24 * time.Hour), MonthlyBudgetBytes: 1_000_000_000}
}

func (p ProbePolicy) Validate() error {
	if p.Mode != "" && p.Mode != "inherit" && p.Mode != "metered" {
		return fmt.Errorf("probe_policy.mode: must be inherit or metered")
	}
	if p.MonthlyBudgetBytes < 0 {
		return fmt.Errorf("probe_policy.monthly_budget_bytes: must be non-negative")
	}
	if p.Mode == "metered" {
		if p.EgressInterval < Duration(30*time.Second) || p.ActiveWindow < Duration(30*time.Second) || p.MaxEgressAge < Duration(30*time.Second) {
			return fmt.Errorf("probe_policy intervals: must be >= 30s")
		}
		if p.MonthlyBudgetBytes == 0 {
			return fmt.Errorf("probe_policy.monthly_budget_bytes: must be positive")
		}
	}
	return nil
}

// Reject misspelled options rather than silently falling back to global probing.
func (p *ProbePolicy) UnmarshalJSON(data []byte) error {
	type wire ProbePolicy
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if fields == nil {
		return fmt.Errorf("probe_policy: must be an object")
	}
	for key, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("probe_policy.%s: null is not allowed", key)
		}
	}
	var next wire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&next); err != nil {
		return err
	}
	*p = ProbePolicy(next)
	return nil
}
