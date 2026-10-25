export type PlatformMissAction = "TREAT_AS_EMPTY" | "REJECT";
export type PlatformEmptyAccountBehavior = "RANDOM" | "FIXED_HEADER" | "ACCOUNT_HEADER_RULE";
export type PlatformAllocationPolicy = "BALANCED" | "PREFER_LOW_LATENCY" | "PREFER_IDLE_IP";

export type Platform = {
  id: string;
  name: string;
  sticky_ttl: string;
  regex_filters: string[];
  region_filters: string[];
  routable_node_count: number;
  reverse_proxy_miss_action: PlatformMissAction;
  reverse_proxy_empty_account_behavior: PlatformEmptyAccountBehavior;
  reverse_proxy_fixed_account_header: string;
  allocation_policy: PlatformAllocationPolicy;
  passive_circuit_breaker_disabled: boolean;
  ip_governance_enabled: boolean;
  ip_governance_max_accounts_per_ip: number;
  egress_verification_max_age: string;
  updated_at: string;
};

export type PageResponse<T> = {
  items: T[];
  total: number;
  limit: number;
  offset: number;
};

export type PlatformCreateInput = {
  name: string;
  sticky_ttl?: string;
  regex_filters?: string[];
  region_filters?: string[];
  reverse_proxy_miss_action?: PlatformMissAction;
  reverse_proxy_empty_account_behavior?: PlatformEmptyAccountBehavior;
  reverse_proxy_fixed_account_header?: string;
  allocation_policy?: PlatformAllocationPolicy;
  passive_circuit_breaker_disabled?: boolean;
  ip_governance_enabled?: boolean;
  ip_governance_max_accounts_per_ip?: number;
  egress_verification_max_age?: string;
};

export type PlatformUpdateInput = {
  name?: string;
  sticky_ttl?: string;
  regex_filters?: string[];
  region_filters?: string[];
  reverse_proxy_miss_action?: PlatformMissAction;
  reverse_proxy_empty_account_behavior?: PlatformEmptyAccountBehavior;
  reverse_proxy_fixed_account_header?: string;
  allocation_policy?: PlatformAllocationPolicy;
  passive_circuit_breaker_disabled?: boolean;
  ip_governance_enabled?: boolean;
  ip_governance_max_accounts_per_ip?: number;
  egress_verification_max_age?: string;
};

export type PlatformLease = {
  egress_verified_at?: string;
  platform_id: string;
  account: string;
  node_hash: string;
  node_tag: string;
  egress_ip: string;
  expiry: string;
  last_accessed: string;
};

export type PlatformLeaseSortBy = "account" | "expiry" | "last_accessed";
export type SortOrder = "asc" | "desc";

export type PlatformIPHistory = {
  egress_ip: string;
  historical_accounts: number;
  active_accounts: number;
  node_count: number;
  threshold_status: "disabled" | "unlimited" | "below" | "reached";
};

export type IPHistoryPage = PageResponse<PlatformIPHistory> & {
  summary: {
    available_ips: number;
    used_ips: number;
    unused_ips: number;
    active_ips: number;
    reached_ips: number;
    enabled: boolean;
    soft_limit: number;
  };
};

export type ListIPHistoryInput = {
  limit: number;
  offset: number;
  egress_ip: string;
  status: "" | "unused" | "reached";
  sort_by: "historical_accounts" | "active_accounts" | "egress_ip";
  sort_order: SortOrder;
};

export type ListPlatformLeasesInput = {
  limit?: number;
  offset?: number;
  account?: string;
  fuzzy?: boolean;
  sort_by?: PlatformLeaseSortBy;
  sort_order?: SortOrder;
};
