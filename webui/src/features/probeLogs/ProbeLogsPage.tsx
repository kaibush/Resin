import { useEffect, useMemo, useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { createColumnHelper } from "@tanstack/react-table";
import { AlertTriangle, Eraser, RefreshCw, Sparkles, X } from "lucide-react";
import { DataTable } from "../../components/ui/DataTable";
import { CursorPagination } from "../../components/ui/CursorPagination";
import { Button } from "../../components/ui/Button";
import { Badge } from "../../components/ui/Badge";
import { Card } from "../../components/ui/Card";
import { Input } from "../../components/ui/Input";
import { Select } from "../../components/ui/Select";
import { useI18n } from "../../i18n";
import { apiRequest } from "../../lib/api-client";
import { formatBytes } from "../../lib/bytes";
import { formatDateTime } from "../../lib/time";
import { formatApiErrorMessage } from "../../lib/error-message";
import { getCurrentLocale, isEnglishLocale } from "../../i18n/locale";

type ProbeLog = {
  id: string; ts: string; node_hash: string; subscriptions: string;
  kind: string; reason: string; target_host: string; success: boolean;
  error: string; egress_ip: string; duration_ms: number; latency_ms: number;
  ingress_bytes: number; egress_bytes: number; bytes_measured: boolean;
};
type Page = {
  items: ProbeLog[]; has_more: boolean; next_cursor: string;
  dropped_since_start: number; retention_max_rows: number; retention_days: number;
};
type Filters = {
  subscription: string; node_hash: string; target_host: string;
  kind: string; reason: string; success: string; from: string; to: string;
};

const emptyFilters: Filters = { subscription: "", node_hash: "", target_host: "", kind: "", reason: "", success: "", from: "", to: "" };
const reasons: Record<string, string> = { automatic: "自动检测", periodic: "周期检测", retry: "故障复测", required: "分配验证", manual: "手动检测" };
const FILTER_DEBOUNCE_MS = 100;
const controlStyle = { width: "100%", padding: "4px 8px", fontSize: "0.875rem", minHeight: "32px", height: "32px" };
const col = createColumnHelper<ProbeLog>();

function toISO(local: string): string {
  if (!local) return "";
  const date = new Date(local);
  return Number.isNaN(date.getTime()) ? "" : date.toISOString();
}

function dateLocale(): string {
  return isEnglishLocale(getCurrentLocale()) ? "en-US" : "zh-CN";
}

function splitDateTime(input: string): { date: string; time: string } {
  const value = new Date(input);
  if (Number.isNaN(value.getTime())) return { date: input || "—", time: "—" };
  return {
    date: new Intl.DateTimeFormat(dateLocale(), { year: "numeric", month: "2-digit", day: "2-digit" }).format(value),
    time: new Intl.DateTimeFormat(dateLocale(), { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false }).format(value),
  };
}

function splitSubscription(raw: string): { title: string; detail: string } {
  const parts = raw.split(",").map((part) => part.trim()).filter(Boolean);
  if (!parts.length) return { title: "—", detail: "" };
  const named = parts.map((part) => {
    const match = part.match(/^(.*) \(([^)]+)\)$/);
    return match ? { name: match[1], id: match[2] } : { name: part, id: "" };
  });
  const extra = named.length > 1 ? ` +${named.length - 1}` : "";
  return { title: `${named[0].name}${extra}`, detail: named.map((item) => item.id).filter(Boolean).join(", ") };
}

function FilterField({ id, label, children }: { id: string; label: string; children: ReactNode }) {
  return (
    <div style={{ flex: "1 1 180px", minWidth: 0, display: "flex", flexDirection: "column", gap: "0.25rem" }}>
      <label htmlFor={id} style={{ fontSize: "0.75rem", color: "var(--text-secondary)" }}>{label}</label>
      {children}
    </div>
  );
}

export function ProbeLogsPage() {
  const { t } = useI18n();
  const [draft, setDraft] = useState(emptyFilters);
  const [textFilters, setTextFilters] = useState({ subscription: "", node_hash: "", target_host: "" });
  const [cursors, setCursors] = useState<string[]>([""]);
  const [limit, setLimit] = useState(100);
  const [autoRefresh, setAutoRefresh] = useState(false);
  const [selected, setSelected] = useState<ProbeLog | null>(null);
  const cursor = cursors[cursors.length - 1];

  useEffect(() => {
    const timeoutID = window.setTimeout(() => {
      setTextFilters({ subscription: draft.subscription, node_hash: draft.node_hash, target_host: draft.target_host });
    }, FILTER_DEBOUNCE_MS);
    return () => window.clearTimeout(timeoutID);
  }, [draft.node_hash, draft.subscription, draft.target_host]);

  const rangeInvalid = useMemo(() => {
    const from = toISO(draft.from);
    const to = toISO(draft.to);
    return Boolean(from && to && from >= to);
  }, [draft.from, draft.to]);

  const filters = useMemo<Filters>(() => ({
    ...textFilters,
    kind: draft.kind,
    reason: draft.reason,
    success: draft.success,
    from: toISO(draft.from),
    to: rangeInvalid ? "" : toISO(draft.to),
  }), [draft.kind, draft.from, draft.reason, draft.success, draft.to, rangeInvalid, textFilters]);

  const query = useQuery({
    queryKey: ["probe-logs", filters, cursor, limit],
    queryFn: () => {
      const params = new URLSearchParams({ limit: String(limit) });
      Object.entries(filters).forEach(([key, value]) => { if (value) params.set(key, value); });
      if (cursor) params.set("cursor", cursor);
      return apiRequest<Page>(`/api/v1/probe-logs?${params}`);
    },
    placeholderData: (previous) => previous,
    refetchInterval: autoRefresh && !cursor ? 5000 : false,
  });

  useEffect(() => {
    if (!selected) return;
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") setSelected(null);
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [selected]);

  const resetPage = () => {
    setCursors([""]);
    setSelected(null);
  };
  const update = (patch: Partial<Filters>) => {
    setDraft((prev) => ({ ...prev, ...patch }));
    resetPage();
  };
  const reset = () => {
    setDraft(emptyFilters);
    setTextFilters({ subscription: "", node_hash: "", target_host: "" });
    resetPage();
  };

  const items = query.data?.items ?? [];
  const transitioning = query.isFetching && query.isPlaceholderData;
  const visible = transitioning ? [] : items;
  const columns = useMemo(() => [
    col.accessor("ts", {
      header: t("时间"),
      cell: (info) => {
        const parts = splitDateTime(info.getValue());
        return <div className="logs-cell-stack logs-time-cell"><span>{parts.time}</span><small>{parts.date}</small></div>;
      },
    }),
    col.accessor("subscriptions", {
      header: t("订阅"),
      cell: (info) => {
        const parts = splitSubscription(info.getValue());
        return (
          <div className="logs-cell-stack probe-logs-sub" title={info.getValue()}>
            <span>{parts.title}</span>
            {parts.detail ? <small>{parts.detail}</small> : null}
          </div>
        );
      },
    }),
    col.accessor("node_hash", {
      header: t("节点"),
      cell: (info) => {
        const log = info.row.original;
        return (
          <div className="logs-cell-stack">
            <code title={log.node_hash}>{log.node_hash.slice(0, 12)}</code>
            <small title={log.egress_ip}>{log.egress_ip || "—"}</small>
          </div>
        );
      },
    }),
    col.accessor("kind", {
      header: t("探测类型"),
      cell: (info) => {
        const kind = info.getValue();
        const label = kind === "egress" ? "出口检测" : kind === "latency" ? "延迟检测" : kind;
        return <Badge variant={kind === "egress" ? "info" : "accent"}>{t(label)}</Badge>;
      },
    }),
    col.accessor("reason", {
      header: t("触发原因"),
      cell: (info) => t(reasons[info.getValue()] ?? info.getValue()),
    }),
    col.accessor("target_host", {
      header: t("目标域名"),
      cell: (info) => <span className="probe-logs-host" title={info.getValue()}>{info.getValue() || "—"}</span>,
    }),
    col.accessor("success", {
      header: t("结果"),
      cell: (info) => <Badge variant={info.getValue() ? "success" : "danger"}>{t(info.getValue() ? "成功" : "失败")}</Badge>,
    }),
    col.accessor("duration_ms", { header: t("耗时"), cell: (info) => `${info.getValue()} ms` }),
    col.display({
      id: "traffic",
      header: t("流量"),
      cell: (info) => {
        const log = info.row.original;
        if (!log.bytes_measured) return "—";
        return (
          <div className="logs-cell-stack">
            <span>↑ {formatBytes(log.egress_bytes)}</span>
            <small>↓ {formatBytes(log.ingress_bytes)}</small>
          </div>
        );
      },
    }),
  ], [t]);

  return (
    <section className="nodes-page">
      <header className="module-header">
        <div>
          <h2>{t("探测日志")}</h2>
          <p className="module-description">{t("查看 Resin 自身的出口和延迟检测，点击记录查看详情。")}</p>
          <p className="probe-logs-note">
            {t("仅记录实际发起的检测；缓存命中、预算阻止及被动业务反馈不产生记录。")}
            {" "}
            {t("保留最近 7 天，最多 100,000 条。流量包含目标 TLS，不等同于供应商账单；历史探测无法补录。")}
          </p>
        </div>
      </header>

      <Card className="filter-card platform-list-card platform-directory-card">
        <div className="logs-inline-filters">
          <FilterField id="probe-subscription" label={t("订阅名称或 ID")}>
            <Input id="probe-subscription" value={draft.subscription} onChange={(event) => update({ subscription: event.target.value.trim() })} style={controlStyle} />
          </FilterField>
          <FilterField id="probe-node" label={t("完整节点哈希")}>
            <Input id="probe-node" value={draft.node_hash} onChange={(event) => update({ node_hash: event.target.value.trim() })} style={controlStyle} />
          </FilterField>
          <FilterField id="probe-host" label={t("目标域名")}>
            <Input id="probe-host" value={draft.target_host} onChange={(event) => update({ target_host: event.target.value.trim() })} style={controlStyle} />
          </FilterField>
          <FilterField id="probe-kind" label={t("探测类型")}>
            <Select id="probe-kind" value={draft.kind} onChange={(event) => update({ kind: event.target.value })} style={controlStyle}>
              <option value="">{t("全部")}</option>
              <option value="egress">{t("出口检测")}</option>
              <option value="latency">{t("延迟检测")}</option>
            </Select>
          </FilterField>
          <FilterField id="probe-reason" label={t("触发原因")}>
            <Select id="probe-reason" value={draft.reason} onChange={(event) => update({ reason: event.target.value })} style={controlStyle}>
              <option value="">{t("全部")}</option>
              {Object.entries(reasons).map(([value, label]) => <option key={value} value={value}>{t(label)}</option>)}
            </Select>
          </FilterField>
          <FilterField id="probe-success" label={t("结果")}>
            <Select id="probe-success" value={draft.success} onChange={(event) => update({ success: event.target.value })} style={controlStyle}>
              <option value="">{t("全部")}</option>
              <option value="true">{t("成功")}</option>
              <option value="false">{t("失败")}</option>
            </Select>
          </FilterField>
          <FilterField id="probe-from" label={t("开始时间")}>
            <Input id="probe-from" type="datetime-local" value={draft.from} onChange={(event) => update({ from: event.target.value })} style={controlStyle} />
          </FilterField>
          <FilterField id="probe-to" label={t("结束时间")}>
            <Input id="probe-to" type="datetime-local" value={draft.to} onChange={(event) => update({ to: event.target.value })} style={controlStyle} />
          </FilterField>
          <div className="probe-logs-filter-actions">
            <label className="probe-logs-auto">
              <input type="checkbox" checked={autoRefresh} onChange={(event) => setAutoRefresh(event.target.checked)} />
              {t("首页每 5 秒自动刷新")}
            </label>
            <Button size="sm" variant="secondary" onClick={() => { setCursors([""]); void query.refetch(); }} disabled={query.isFetching} style={{ minHeight: 32, height: 32 }}>
              <RefreshCw size={14} className={query.isFetching ? "spin" : undefined} />{t("刷新")}
            </Button>
            <Button size="sm" variant="secondary" onClick={reset} style={{ minHeight: 32, height: 32 }}>
              <Eraser size={14} />{t("重置")}
            </Button>
          </div>
        </div>
        {rangeInvalid ? <div className="callout callout-warning probe-logs-alert">{t("时间范围错误：开始时间必须早于结束时间，已暂不应用结束时间筛选。")}</div> : null}
        {(query.data?.dropped_since_start ?? 0) > 0 ? (
          <div className="callout callout-warning probe-logs-alert" role="status">{t("本次启动以来丢失日志条数")}: {query.data?.dropped_since_start}</div>
        ) : null}
      </Card>

      <Card className="nodes-table-card platform-cards-container subscriptions-table-card">
        {query.isPending || transitioning ? <p className="muted">{t("正在加载日志...")}</p> : null}
        {query.isError ? (
          <div className="callout callout-error" role="alert"><AlertTriangle size={14} /><span>{formatApiErrorMessage(query.error, t)}</span></div>
        ) : null}
        {!query.isPending && !transitioning && !query.isError && !visible.length ? (
          <div className="empty-box"><Sparkles size={16} /><p>{t("暂无探测日志")}</p></div>
        ) : null}
        {visible.length ? (
          <DataTable
            data={visible}
            columns={columns}
            getRowId={(row) => row.id}
            onRowClick={setSelected}
            selectedRowId={selected?.id}
            className="data-table-logs"
            wrapClassName="data-table-wrap-logs"
          />
        ) : null}
        <CursorPagination
          pageIndex={cursors.length - 1}
          hasMore={Boolean(query.data?.has_more && query.data.next_cursor)}
          pageSize={limit}
          disabled={query.isFetching || query.isError}
          onPageSizeChange={(size) => { setLimit(size); resetPage(); }}
          onPrev={() => { setCursors((prev) => prev.slice(0, -1)); setSelected(null); }}
          onNext={() => {
            if (query.data?.next_cursor) {
              setCursors((prev) => [...prev, query.data.next_cursor]);
              setSelected(null);
            }
          }}
        />
      </Card>

      {selected ? (
        <div className="drawer-overlay" role="dialog" aria-modal="true" aria-label={t("探测详情")} onClick={() => setSelected(null)}>
          <Card className="drawer-panel" onClick={(event) => event.stopPropagation()}>
            <div className="drawer-header">
              <div>
                <h3>{selected.target_host || t("探测详情")}</h3>
                <p>{selected.id}</p>
              </div>
              <div className="drawer-header-actions">
                <Badge variant={selected.success ? "success" : "danger"}>{t(selected.success ? "成功" : "失败")}</Badge>
                <Button variant="ghost" size="sm" aria-label={t("关闭详情面板")} onClick={() => setSelected(null)}><X size={16} /></Button>
              </div>
            </div>
            <div className="platform-drawer-layout">
              <section className="platform-drawer-section">
                <div className="platform-drawer-section-head">
                  <h4>{t("日志摘要")}</h4>
                  <p>{t("检测时间、类型、结果与流量。")}</p>
                </div>
                <div className="stats-grid">
                  <div><span>{t("时间")}</span><p>{formatDateTime(selected.ts)}</p></div>
                  <div><span>{t("探测类型")}</span><p>{t(selected.kind === "egress" ? "出口检测" : selected.kind === "latency" ? "延迟检测" : selected.kind)}</p></div>
                  <div><span>{t("触发原因")}</span><p>{t(reasons[selected.reason] ?? selected.reason)}</p></div>
                  <div><span>{t("耗时")}</span><p>{selected.duration_ms} ms</p></div>
                  <div><span>{t("TLS 握手延迟")}</span><p>{selected.latency_ms > 0 ? `${selected.latency_ms} ms` : "—"}</p></div>
                  <div><span>{t("目标域名")}</span><p>{selected.target_host || "—"}</p></div>
                  <div><span>{t("出口 IP")}</span><p>{selected.egress_ip || "—"}</p></div>
                  <div><span>{t("上行流量")}</span><p>{selected.bytes_measured ? formatBytes(selected.egress_bytes) : "—"}</p></div>
                  <div><span>{t("下行流量")}</span><p>{selected.bytes_measured ? formatBytes(selected.ingress_bytes) : "—"}</p></div>
                </div>
                <p className="probe-logs-note">{t("流量包含目标 TLS，不等同于供应商账单。")}</p>
                {selected.error ? <div className="callout callout-error" role="status"><AlertTriangle size={14} /><span>{selected.error}</span></div> : null}
              </section>
              <section className="platform-drawer-section">
                <div className="platform-drawer-section-head">
                  <h4>{t("订阅与节点")}</h4>
                </div>
                <div className="stats-grid">
                  <div><span>{t("订阅")}</span><p>{selected.subscriptions || "—"}</p></div>
                  <div><span>{t("节点")}</span><p><code>{selected.node_hash}</code></p></div>
                </div>
              </section>
            </div>
          </Card>
        </div>
      ) : null}
    </section>
  );
}
