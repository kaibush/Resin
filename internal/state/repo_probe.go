package state

import (
	"database/sql"
	"github.com/Resinat/Resin/internal/probepolicy"
	"time"
)

var ErrProbeBudget = probepolicy.ErrBudget

// ProbeUsage includes durable reservations: a crash cannot reset the budget.
type ProbeUsage struct {
	Day           string `json:"day"`
	Reason        string `json:"reason"`
	IngressBytes  int64  `json:"ingress_bytes"`
	EgressBytes   int64  `json:"egress_bytes"`
	Attempts      int64  `json:"attempts"`
	Failures      int64  `json:"failures"`
	ReservedBytes int64  `json:"reserved_bytes"`
}

type ProbeNodeState struct {
	VerifiedIP  string
	LastUsedNs  int64
	VerifiedNs  int64
	NextRetryNs int64
	Failures    int
}

func (r *StateRepo) LoadProbeNodeStates() (map[string]ProbeNodeState, error) {
	rows, err := r.db.Query(`SELECT node_hash,last_used_ns,verified_ns,next_retry_ns,failures,verified_ip FROM probe_node_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ProbeNodeState{}
	for rows.Next() {
		var h string
		var s ProbeNodeState
		if err = rows.Scan(&h, &s.LastUsedNs, &s.VerifiedNs, &s.NextRetryNs, &s.Failures, &s.VerifiedIP); err != nil {
			return nil, err
		}
		out[h] = s
	}
	return out, rows.Err()
}
func (r *StateRepo) SaveProbeNodeState(hash string, s ProbeNodeState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.db.Exec(`INSERT INTO probe_node_state(node_hash,last_used_ns,verified_ns,next_retry_ns,failures,verified_ip) VALUES(?,?,?,?,?,?) ON CONFLICT(node_hash) DO UPDATE SET last_used_ns=excluded.last_used_ns,verified_ns=excluded.verified_ns,next_retry_ns=excluded.next_retry_ns,failures=excluded.failures,verified_ip=excluded.verified_ip`, hash, s.LastUsedNs, s.VerifiedNs, s.NextRetryNs, s.Failures, s.VerifiedIP)
	return err
}
func (r *StateRepo) ProbeUsage(id string, now time.Time) ([]ProbeUsage, error) {
	rows, err := r.db.Query(`SELECT day,reason,ingress_bytes,egress_bytes,attempts,failures,reserved_bytes FROM probe_usage WHERE subscription_id=? AND day>=? AND day<=? ORDER BY day,reason`, id, now.UTC().Format("2006-01")+"-01", now.UTC().Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ProbeUsage{}
	for rows.Next() {
		var u ProbeUsage
		if err = rows.Scan(&u.Day, &u.Reason, &u.IngressBytes, &u.EgressBytes, &u.Attempts, &u.Failures, &u.ReservedBytes); err != nil {
			return nil, err
		}
		result = append(result, u)
	}
	return result, rows.Err()
}

// ReserveProbe serializes the monthly check and reservation across all workers.
func (r *StateRepo) ReserveProbe(id, day, reason string, limit, reserve int64, enforce bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var used int64
	if err = tx.QueryRow(`SELECT COALESCE(SUM(ingress_bytes+egress_bytes+reserved_bytes),0) FROM probe_usage WHERE subscription_id=? AND day>=? AND day<=?`, id, day[:7]+"-01", day).Scan(&used); err != nil {
		return err
	}
	if enforce && (used > limit || reserve > limit-used) {
		return ErrProbeBudget
	}
	_, err = tx.Exec(`INSERT INTO probe_usage(subscription_id,day,reason,attempts,reserved_bytes) VALUES(?,?,?,1,?) ON CONFLICT(subscription_id,day,reason) DO UPDATE SET attempts=attempts+1,reserved_bytes=reserved_bytes+excluded.reserved_bytes`, id, day, reason, reserve)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (r *StateRepo) FinishProbe(id, day, reason string, reserve, ingress, egress int64, failed bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	result, err := r.db.Exec(`UPDATE probe_usage SET reserved_bytes=reserved_bytes-?,ingress_bytes=ingress_bytes+?,egress_bytes=egress_bytes+?,failures=failures+? WHERE subscription_id=? AND day=? AND reason=? AND reserved_bytes>=?`, reserve, ingress, egress, failed, id, day, reason, reserve)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return err
}
