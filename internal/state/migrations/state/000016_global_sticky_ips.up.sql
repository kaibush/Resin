-- Recent bindings survive platform deletion/recreation. Consolidate scoped
-- rows by newest binding, without retaining platform IDs or a foreign key.
CREATE TABLE sticky_ips_global (
    account_hash BLOB PRIMARY KEY NOT NULL CHECK(length(account_hash) = 32),
    egress_ip BLOB NOT NULL CHECK(length(egress_ip) IN (4, 16)),
    updated_at_ns INTEGER NOT NULL
) WITHOUT ROWID;

INSERT INTO sticky_ips_global (account_hash, egress_ip, updated_at_ns)
SELECT account_hash, egress_ip, updated_at_ns FROM (
    SELECT account_hash, egress_ip, updated_at_ns,
           ROW_NUMBER() OVER (
               PARTITION BY account_hash ORDER BY updated_at_ns DESC, platform_id ASC
           ) AS rank
    FROM sticky_ips
) WHERE rank = 1;

DROP TABLE sticky_ips;
ALTER TABLE sticky_ips_global RENAME TO sticky_ips;
