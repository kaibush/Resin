package state

import (
	"fmt"
	"net/netip"

	"github.com/Resinat/Resin/internal/model"
)

// SaveStickyIP is called only for a new identity or changed exit. Renewals do
// not write. The primary key bounds storage to one row per global identity.
func (r *StateRepo) SaveStickyIP(entry model.StickyIP) error {
	if !entry.EgressIP.IsValid() || entry.EgressIP.Zone() != "" {
		return fmt.Errorf("invalid sticky IP")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.db.Exec(`INSERT INTO sticky_ips (account_hash, egress_ip, updated_at_ns)
		VALUES (?, ?, ?) ON CONFLICT(account_hash) DO UPDATE SET
		egress_ip = excluded.egress_ip, updated_at_ns = excluded.updated_at_ns WHERE sticky_ips.egress_ip != excluded.egress_ip
		AND excluded.updated_at_ns > sticky_ips.updated_at_ns`,
		entry.AccountHash[:], entry.EgressIP.Unmap().AsSlice(), entry.UpdatedAtNs)
	return err
}

// WalkStickyIPs streams bootstrap data directly into the routing index, without
// holding a second full copy. Routing never queries this table.
func (r *StateRepo) WalkStickyIPs(visit func(model.StickyIP)) error {
	rows, err := r.db.Query("SELECT account_hash, egress_ip, updated_at_ns FROM sticky_ips")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var entry model.StickyIP
		var digest, rawIP []byte
		if err := rows.Scan(&digest, &rawIP, &entry.UpdatedAtNs); err != nil {
			return err
		}
		ip, ok := netip.AddrFromSlice(rawIP)
		if !ok || len(digest) != len(entry.AccountHash) {
			return fmt.Errorf("invalid persisted sticky IP")
		}
		copy(entry.AccountHash[:], digest)
		entry.EgressIP = ip.Unmap()
		visit(entry)
	}
	return rows.Err()
}
