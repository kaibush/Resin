package state

import (
	"time"

	"github.com/Resinat/Resin/internal/model"
)

func (r *StateRepo) IPHistoryInitialized() (bool, error) {
	var n int
	err := r.db.QueryRow("SELECT COUNT(*) FROM ip_history_bootstrap WHERE id = 1").Scan(&n)
	return n > 0, err
}

func (r *StateRepo) MarkIPHistoryInitialized() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.db.Exec("INSERT INTO ip_history_bootstrap (id, seeded_at_ns) VALUES (1, ?) ON CONFLICT DO NOTHING", time.Now().UnixNano())
	return err
}

// LoadIPHistory is used only at startup; routing reads in-memory counters.
func (r *StateRepo) LoadIPHistory() ([]model.IPHistory, error) {
	rows, err := r.db.Query("SELECT egress_ip, account_hash, first_seen_ns FROM ip_history")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []model.IPHistory
	for rows.Next() {
		var entry model.IPHistory
		if err := rows.Scan(&entry.EgressIP, &entry.AccountHash, &entry.FirstSeenNs); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// SaveIPHistory durably records new markers before a governed route is used.
// Duplicate requests never write; startup seeding uses one batch transaction.
func (r *StateRepo) SaveIPHistory(entries []model.IPHistory) error {
	if len(entries) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare("INSERT INTO ip_history (egress_ip, account_hash, first_seen_ns) VALUES (?, ?, ?) ON CONFLICT DO NOTHING")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, entry := range entries {
		if _, err := stmt.Exec(entry.EgressIP, entry.AccountHash, entry.FirstSeenNs); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *StateRepo) DeleteIPHistory(ip string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.db.Exec("DELETE FROM ip_history WHERE egress_ip = ?", ip)
	return err
}
