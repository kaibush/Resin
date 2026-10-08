CREATE TABLE probe_logs (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 ts INTEGER NOT NULL,
 node_hash TEXT NOT NULL,
 subscriptions TEXT NOT NULL,
 kind TEXT NOT NULL,
 reason TEXT NOT NULL,
 target_host TEXT NOT NULL,
 success INTEGER NOT NULL,
 record TEXT NOT NULL
);
CREATE INDEX probe_logs_ts ON probe_logs(ts);
CREATE INDEX probe_logs_node ON probe_logs(node_hash,id);
