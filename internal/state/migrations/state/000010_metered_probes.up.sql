ALTER TABLE subscriptions ADD COLUMN probe_policy_json TEXT NOT NULL DEFAULT '{}';
CREATE TABLE probe_usage (
 subscription_id TEXT NOT NULL,
 day TEXT NOT NULL,
 reason TEXT NOT NULL,
 ingress_bytes INTEGER NOT NULL DEFAULT 0,
 egress_bytes INTEGER NOT NULL DEFAULT 0,
 attempts INTEGER NOT NULL DEFAULT 0,
 failures INTEGER NOT NULL DEFAULT 0,
 reserved_bytes INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(subscription_id, day, reason)
);
CREATE TABLE probe_node_state (
 node_hash TEXT PRIMARY KEY,
 last_used_ns INTEGER NOT NULL DEFAULT 0,
 verified_ns INTEGER NOT NULL DEFAULT 0,
 verified_ip TEXT NOT NULL DEFAULT '',
 next_retry_ns INTEGER NOT NULL DEFAULT 0,
 failures INTEGER NOT NULL DEFAULT 0
);

ALTER TABLE platforms ADD COLUMN egress_verification_max_age_ns INTEGER NOT NULL DEFAULT 0;
