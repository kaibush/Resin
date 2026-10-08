export type ProbePolicy = {
  mode: "inherit" | "metered";
  egress_interval: string;
  active_window: string;
  max_egress_age: string;
  monthly_budget_bytes: number;
  strict_budget: boolean;
};
export const defaultProbePolicy: ProbePolicy = {
  mode: "inherit", egress_interval: "24h", active_window: "24h",
  max_egress_age: "24h", monthly_budget_bytes: 1_000_000_000, strict_budget: false,
};
export type ProbeUsage = {
  day: string; reason: string; ingress_bytes: number; egress_bytes: number;
  attempts: number; failures: number; reserved_bytes: number;
};
export type Subscription = {
  probe_policy: ProbePolicy;
  probe_usage: ProbeUsage[];
  probe_usage_error?: string;
  id: string;
  name: string;
  source_type: "remote" | "local";
  url: string;
  content: string;
  update_interval: string;
  node_count: number;
  healthy_node_count: number;
  ephemeral: boolean;
  incremental_alive_nodes: boolean;
  ephemeral_node_evict_delay: string;
  enabled: boolean;
  created_at: string;
  last_checked?: string;
  last_updated?: string;
  last_error?: string;
};

export type PageResponse<T> = {
  items: T[];
  total: number;
  limit: number;
  offset: number;
};

export type SubscriptionCreateInput = {
 probe_policy?: ProbePolicy;
  name: string;
  source_type?: "remote" | "local";
  url?: string;
  content?: string;
  update_interval?: string;
  enabled?: boolean;
  ephemeral?: boolean;
  incremental_alive_nodes?: boolean;
  ephemeral_node_evict_delay?: string;
};

export type SubscriptionUpdateInput = {
 probe_policy?: ProbePolicy;
  name?: string;
  url?: string;
  content?: string;
  update_interval?: string;
  enabled?: boolean;
  ephemeral?: boolean;
  incremental_alive_nodes?: boolean;
  ephemeral_node_evict_delay?: string;
};
