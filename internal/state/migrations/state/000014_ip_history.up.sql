CREATE TABLE ip_history (
    egress_ip TEXT NOT NULL,
    account_hash TEXT NOT NULL,
    first_seen_ns INTEGER NOT NULL,
    PRIMARY KEY (egress_ip, account_hash)
) WITHOUT ROWID;

CREATE TABLE ip_history_bootstrap (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    seeded_at_ns INTEGER NOT NULL
);
