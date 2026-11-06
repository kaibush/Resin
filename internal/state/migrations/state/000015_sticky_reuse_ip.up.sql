ALTER TABLE platforms ADD COLUMN sticky_reuse_ip INTEGER NOT NULL DEFAULT 0;

-- One latest exit per platform/identity, independent of expiring cache leases.
-- Binary values and WITHOUT ROWID avoid hex strings and a duplicate PK index.
CREATE TABLE sticky_ips (
    platform_id TEXT NOT NULL REFERENCES platforms(id) ON DELETE CASCADE,
    account_hash BLOB NOT NULL CHECK(length(account_hash) = 32),
    egress_ip BLOB NOT NULL CHECK(length(egress_ip) IN (4, 16)),
    updated_at_ns INTEGER NOT NULL,
    PRIMARY KEY (platform_id, account_hash)
) WITHOUT ROWID;
