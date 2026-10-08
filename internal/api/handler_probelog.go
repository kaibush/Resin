package api

import (
	"github.com/Resinat/Resin/internal/service"
	"github.com/Resinat/Resin/internal/state"
	"net/http"
	"strconv"
	"time"
)

func HandleListProbeLogs(cp *service.ControlPlaneService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cp.Engine == nil || cp.Engine.StateRepo == nil {
			WriteError(w, 503, "UNAVAILABLE", "probe log storage unavailable")
			return
		}
		limit, ok := parseRequestLogLimitQuery(w, r)
		if !ok {
			return
		}
		if limit > 2000 {
			writeInvalidArgument(w, "limit: must be <= 2000")
			return
		}
		q := r.URL.Query()
		if q.Get("offset") != "" {
			writeInvalidArgument(w, "offset: use cursor")
			return
		}
		f := state.ProbeLogFilter{Limit: limit, NodeHash: q.Get("node_hash"), Subscription: q.Get("subscription"), Kind: q.Get("kind"), Reason: q.Get("reason"), TargetHost: q.Get("target_host")}
		if v := q.Get("cursor"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n <= 0 {
				writeInvalidArgument(w, "cursor: invalid")
				return
			}
			f.Cursor = n
		}
		for _, p := range []struct {
			key string
			dst *time.Time
		}{{"from", &f.From}, {"to", &f.To}} {
			if v := q.Get(p.key); v != "" {
				t, err := time.Parse(time.RFC3339Nano, v)
				if err != nil {
					writeInvalidArgument(w, p.key+": invalid RFC3339 timestamp")
					return
				}
				*p.dst = t
			}
		}
		if !f.From.IsZero() && !f.To.IsZero() && !f.From.Before(f.To) {
			writeInvalidArgument(w, "from: must be before to")
			return
		}
		f.Success, ok = parseStrictBoolQuery(w, r, "success")
		if !ok {
			return
		}
		if f.Kind != "" && f.Kind != "egress" && f.Kind != "latency" {
			writeInvalidArgument(w, "kind: must be egress or latency")
			return
		}
		rows, more, err := cp.Engine.StateRepo.ListProbeLogs(f)
		if err != nil {
			WriteError(w, 500, "INTERNAL", "cannot read probe logs")
			return
		}
		cursor := ""
		if more {
			cursor = rows[len(rows)-1].ID
		}
		var dropped int64
		if cp.ProbeMgr != nil {
			dropped = cp.ProbeMgr.DroppedProbeLogs()
		}
		WriteJSON(w, 200, map[string]any{"items": rows, "has_more": more, "next_cursor": cursor, "limit": limit, "dropped_since_start": dropped, "retention_max_rows": state.ProbeLogRetention, "retention_days": 7})
	})
}
