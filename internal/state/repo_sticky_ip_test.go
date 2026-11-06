package state

import (
	"crypto/sha256"
	"net/netip"
	"testing"

	"github.com/Resinat/Resin/internal/model"
)

func TestStickyIPCompactStorageOverwriteRestartAndPlatformDeletion(t *testing.T) {
	stateDir, cacheDir := t.TempDir(), t.TempDir()
	engine, closer, err := PersistenceBootstrap(stateDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closer.Close() })
	for _, id := range []string{"one", "two"} {
		if err := engine.UpsertPlatform(model.Platform{ID: id, Name: id, StickyTTLNs: 1000, ReverseProxyMissAction: "TREAT_AS_EMPTY", AllocationPolicy: "BALANCED", StickyReuseIP: true}); err != nil {
			t.Fatal(err)
		}
	}
	entry := model.StickyIP{AccountHash: sha256.Sum256([]byte("a-very-long-secret-account-marker")), EgressIP: netip.MustParseAddr("::ffff:192.0.2.1"), UpdatedAtNs: 1}
	for i := 0; i < 5; i++ {
		if err := engine.SaveStickyIP(entry); err != nil {
			t.Fatal(err)
		}
	}
	var count, hashLen, ipLen int
	var hashType, ipType string
	if err := engine.StateRepo.db.QueryRow("SELECT count(*), length(account_hash), length(egress_ip), typeof(account_hash), typeof(egress_ip) FROM sticky_ips").Scan(&count, &hashLen, &ipLen, &hashType, &ipType); err != nil {
		t.Fatal(err)
	}
	if count != 1 || hashLen != 32 || ipLen != 4 || hashType != "blob" || ipType != "blob" {
		t.Fatalf("storage: rows=%d hash=%s/%d ip=%s/%d", count, hashType, hashLen, ipType, ipLen)
	}
	entry.EgressIP = netip.MustParseAddr("2001:db8::1")
	entry.UpdatedAtNs = 2
	if err := engine.SaveStickyIP(entry); err != nil {
		t.Fatal(err)
	}
	other := entry
	other.AccountHash = sha256.Sum256([]byte("other-marker"))
	if err := engine.SaveStickyIP(other); err != nil {
		t.Fatal(err)
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	engine, closer, err = PersistenceBootstrap(stateDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	var got []model.StickyIP
	if err := engine.WalkStickyIPs(func(e model.StickyIP) { got = append(got, e) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !(got[0] == entry && got[1] == other || got[0] == other && got[1] == entry) {
		t.Fatalf("restart=%+v", got)
	}
	p, err := engine.GetPlatform("one")
	if err != nil || !p.StickyReuseIP {
		t.Fatalf("setting not persisted: %+v %v", p, err)
	}
	p.StickyReuseIP = false
	if err := engine.UpsertPlatform(*p); err != nil {
		t.Fatal(err)
	}
	got = nil
	if err := engine.WalkStickyIPs(func(e model.StickyIP) { got = append(got, e) }); err != nil || len(got) != 2 {
		t.Fatalf("disable lost bindings: %+v %v", got, err)
	}
	if err := engine.DeletePlatform("one"); err != nil {
		t.Fatal(err)
	}
	got = nil
	if err := engine.WalkStickyIPs(func(e model.StickyIP) { got = append(got, e) }); err != nil || len(got) != 2 {
		t.Fatalf("platform deletion removed global bindings: %+v %v", got, err)
	}
	if err := engine.SaveStickyIP(entry); err != nil {
		t.Fatal("global binding incorrectly depends on a platform", err)
	}
}

func TestStickyIPMigrationDefaultsOffForExistingPlatform(t *testing.T) {
	repo := newTestStateRepo(t)
	if err := repo.UpsertPlatform(model.Platform{ID: "existing", Name: "Existing", StickyTTLNs: 123, ReverseProxyMissAction: "TREAT_AS_EMPTY", AllocationPolicy: "BALANCED"}); err != nil {
		t.Fatal(err)
	}
	// Recreate the preceding schema while preserving the existing platform.
	if _, err := repo.db.Exec("DROP TABLE sticky_ips; ALTER TABLE platforms DROP COLUMN sticky_reuse_ip; UPDATE schema_migrations SET version=14"); err != nil {
		t.Fatal(err)
	}
	if err := MigrateStateDB(repo.db); err != nil {
		t.Fatal(err)
	}
	p, err := repo.GetPlatform("existing")
	if err != nil || p.StickyReuseIP || p.StickyTTLNs != 123 {
		t.Fatalf("migration=%+v %v", p, err)
	}
}

func TestStickyIPMigrationConsolidatesLatestGlobalBinding(t *testing.T) {
	repo := newTestStateRepo(t)
	for _, id := range []string{"old-a", "old-b"} {
		if err := repo.UpsertPlatform(model.Platform{ID: id, Name: id, StickyTTLNs: 123, ReverseProxyMissAction: "TREAT_AS_EMPTY", AllocationPolicy: "BALANCED"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.db.Exec("DROP TABLE sticky_ips; ALTER TABLE platforms DROP COLUMN sticky_reuse_ip; UPDATE schema_migrations SET version=15"); err != nil {
		t.Fatal(err)
	}
	previous, err := migrationsFS.ReadFile("migrations/state/000015_sticky_reuse_ip.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.Exec(string(previous)); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("alice"))
	for i, id := range []string{"old-a", "old-b"} {
		ip := netip.MustParseAddr([]string{"192.0.2.1", "192.0.2.2"}[i])
		if _, err := repo.db.Exec("INSERT INTO sticky_ips VALUES (?,?,?,?)", id, hash[:], ip.AsSlice(), i+1); err != nil {
			t.Fatal(err)
		}
	}
	if err := MigrateStateDB(repo.db); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old-a", "old-b"} {
		if err := repo.DeletePlatform(id); err != nil {
			t.Fatal(err)
		}
	}
	var got []model.StickyIP
	if err := repo.WalkStickyIPs(func(e model.StickyIP) { got = append(got, e) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].AccountHash != hash || got[0].EgressIP.String() != "192.0.2.2" || got[0].UpdatedAtNs != 2 {
		t.Fatalf("global migration=%+v", got)
	}
	older := got[0]
	older.EgressIP = netip.MustParseAddr("192.0.2.1")
	older.UpdatedAtNs = 1
	if err := repo.SaveStickyIP(older); err != nil {
		t.Fatal(err)
	}
	if err := repo.WalkStickyIPs(func(e model.StickyIP) {
		if e != got[0] {
			t.Error("older update replaced newest binding")
		}
	}); err != nil {
		t.Fatal(err)
	}
}
