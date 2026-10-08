package state

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Probe logs are diagnostic history, independent from the durable budget ledger.
const ProbeLogRetention = 100000

type ProbeLog struct {
	ID            string    `json:"id"`
	Ts            time.Time `json:"ts"`
	NodeHash      string    `json:"node_hash"`
	Subscriptions string    `json:"subscriptions"`
	Kind          string    `json:"kind"`
	Reason        string    `json:"reason"`
	TargetHost    string    `json:"target_host"`
	Success       bool      `json:"success"`
	Error         string    `json:"error"`
	EgressIP      string    `json:"egress_ip"`
	DurationMs    int64     `json:"duration_ms"`
	LatencyMs     int64     `json:"latency_ms"`
	IngressBytes  int64     `json:"ingress_bytes"`
	EgressBytes   int64     `json:"egress_bytes"`
	BytesMeasured bool      `json:"bytes_measured"`
}
type ProbeLogFilter struct {
	Cursor                                           int64
	Limit                                            int
	From, To                                         time.Time
	NodeHash, Subscription, Kind, Reason, TargetHost string
	Success                                          *bool
}

func (r *StateRepo) AppendProbeLogs(logs []ProbeLog) error {
	if len(logs) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO probe_logs(ts,node_hash,subscriptions,kind,reason,target_host,success,record) VALUES(?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, v := range logs {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err = stmt.Exec(v.Ts.UnixNano(), v.NodeHash, v.Subscriptions, v.Kind, v.Reason, v.TargetHost, v.Success, string(b)); err != nil {
			return err
		}
	}
	// IDs are monotonic, so pruning stays indexed even at millions of probes/day.
	if _, err = tx.Exec(`DELETE FROM probe_logs WHERE id <= (SELECT COALESCE(MAX(id),0)-? FROM probe_logs) OR ts < ?`, ProbeLogRetention, time.Now().Add(-7*24*time.Hour).UnixNano()); err != nil {
		return err
	}
	return tx.Commit()
}
func (r *StateRepo) ListProbeLogs(f ProbeLogFilter) ([]ProbeLog, bool, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 2000 {
		limit = 2000
	}
	clauses := []string{"ts >= ?"}
	args := []any{time.Now().Add(-7 * 24 * time.Hour).UnixNano()}
	if f.Cursor > 0 {
		clauses = append(clauses, "id < ?")
		args = append(args, f.Cursor)
	}
	for _, p := range []struct{ k, v string }{{"node_hash", f.NodeHash}, {"kind", f.Kind}, {"reason", f.Reason}, {"target_host", f.TargetHost}} {
		if p.v != "" {
			clauses = append(clauses, p.k+" = ?")
			args = append(args, p.v)
		}
	}
	if f.Subscription != "" {
		clauses = append(clauses, "instr(subscriptions, ?) > 0")
		args = append(args, f.Subscription)
	}
	if f.Success != nil {
		clauses = append(clauses, "success = ?")
		args = append(args, *f.Success)
	}
	if !f.From.IsZero() {
		clauses = append(clauses, "ts >= ?")
		args = append(args, f.From.UnixNano())
	}
	if !f.To.IsZero() {
		clauses = append(clauses, "ts < ?")
		args = append(args, f.To.UnixNano())
	}
	args = append(args, limit+1)
	rows, err := r.db.Query(`SELECT id,record FROM probe_logs WHERE `+strings.Join(clauses, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	result := []ProbeLog{}
	for rows.Next() {
		var id int64
		var raw string
		if err = rows.Scan(&id, &raw); err != nil {
			return nil, false, err
		}
		var v ProbeLog
		if err = json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, false, err
		}
		v.ID = strconv.FormatInt(id, 10)
		result = append(result, v)
	}
	if err = rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(result) > limit
	if more {
		result = result[:limit]
	}
	return result, more, nil
}
