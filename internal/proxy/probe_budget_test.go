package proxy

import (
	"fmt"
	"github.com/Resinat/Resin/internal/probepolicy"
	"net/http"
	"testing"
)

func TestMapRouteProbeBudgetError(t *testing.T) {
	err := mapRouteError(fmt.Errorf("verify node: %w", probepolicy.ErrBudget))
	if err.HTTPCode != http.StatusServiceUnavailable || err.ResinError != "PROBE_BUDGET_EXHAUSTED" {
		t.Fatalf("budget error hidden: %+v", err)
	}
}
