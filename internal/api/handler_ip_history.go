package api

import (
	"cmp"
	"net/http"
	"slices"
	"strings"

	"github.com/Resinat/Resin/internal/service"
)

func HandleIPHistory(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := requireUUIDPathParam(w, r, "id", "platform_id")
		if !ok {
			return
		}
		pg, ok := parsePaginationOrWriteInvalid(w, r)
		if !ok {
			return
		}
		sorting, ok := parseSortingOrWriteInvalid(w, r, []string{"egress_ip", "historical_accounts", "active_accounts"}, "historical_accounts", "desc")
		if !ok {
			return
		}
		status := r.URL.Query().Get("status")
		if status != "" && status != "unused" && status != "reached" {
			writeInvalidArgument(w, "status: must be unused or reached")
			return
		}
		entries, summary, err := cp.GetIPHistory(id)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		search := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("egress_ip")))
		entries = slices.DeleteFunc(entries, func(e service.IPHistoryEntry) bool {
			return (search != "" && !strings.Contains(e.EgressIP, search)) ||
				(status == "unused" && e.HistoricalAccounts != 0) ||
				(status == "reached" && e.ThresholdStatus != "reached")
		})
		slices.SortFunc(entries, func(a, b service.IPHistoryEntry) int {
			var order int
			switch sorting.SortBy {
			case "historical_accounts":
				order = cmp.Compare(a.HistoricalAccounts, b.HistoricalAccounts)
			case "active_accounts":
				order = cmp.Compare(a.ActiveAccounts, b.ActiveAccounts)
			default:
				order = strings.Compare(a.EgressIP, b.EgressIP)
			}
			if order == 0 {
				order = strings.Compare(a.EgressIP, b.EgressIP)
			}
			return applySortOrder(order, sorting.SortOrder)
		})
		WriteJSON(w, http.StatusOK, map[string]any{
			"items": PaginateSlice(entries, pg), "total": len(entries), "limit": pg.Limit, "offset": pg.Offset, "summary": summary,
		})
	}
}

func HandleResetIPHistory(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := requireUUIDPathParam(w, r, "id", "platform_id")
		if !ok {
			return
		}
		if err := cp.ResetIPHistory(id, r.URL.Query().Get("egress_ip")); err != nil {
			writeServiceError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
