import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { createColumnHelper } from "@tanstack/react-table";
import { RefreshCw } from "lucide-react";
import { DataTable } from "../../components/ui/DataTable";
import { CursorPagination } from "../../components/ui/CursorPagination";
import { Button } from "../../components/ui/Button";
import { Card } from "../../components/ui/Card";
import { Input } from "../../components/ui/Input";
import { Select } from "../../components/ui/Select";
import { useI18n } from "../../i18n";
import { apiRequest } from "../../lib/api-client";
import { formatBytes } from "../../lib/bytes";
import { formatDateTime } from "../../lib/time";
import { formatApiErrorMessage } from "../../lib/error-message";

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
const emptyFilters = { subscription: "", node_hash: "", target_host: "", kind: "", reason: "", success: "", from: "", to: "" };
const reasons: Record<string, string> = { automatic: "自动检测", periodic: "周期检测", retry: "故障复测", required: "分配验证", manual: "手动检测" };
const col = createColumnHelper<ProbeLog>();

export function ProbeLogsPage() {
  const { t } = useI18n();
  const [draft, setDraft] = useState(emptyFilters);
  const [filters, setFilters] = useState(emptyFilters);
  const [cursors, setCursors] = useState<string[]>([""]);
  const [limit, setLimit] = useState(100);
  const [autoRefresh, setAutoRefresh] = useState(false);
  const [selected, setSelected] = useState<ProbeLog | null>(null);
  const [validation, setValidation] = useState("");
  const cursor = cursors[cursors.length - 1];
  const query = useQuery({
    queryKey: ["probe-logs", filters, cursor, limit],
    queryFn: () => {
      const params = new URLSearchParams({ limit: String(limit) });
      Object.entries(filters).forEach(([key, value]) => { if (value) params.set(key, value); });
      if (cursor) params.set("cursor", cursor);
      return apiRequest<Page>(`/api/v1/probe-logs?${params}`);
    },
    refetchInterval: autoRefresh && !cursor ? 5000 : false,
  });
  const columns = [
    col.accessor("ts", { header: t("时间"), cell: (c) => formatDateTime(c.getValue()) }),
    col.accessor("subscriptions", { header: t("订阅"), cell: (c) => <span title={c.getValue()}>{c.getValue() || "—"}</span> }),
    col.accessor("node_hash", { header: t("节点"), cell: (c) => <code title={c.getValue()}>{c.getValue().slice(0, 12)}</code> }),
    col.accessor("kind", { header: t("探测类型"), cell: (c) => t(c.getValue() === "egress" ? "出口检测" : "延迟检测") }),
    col.accessor("reason", { header: t("触发原因"), cell: (c) => t(reasons[c.getValue()] ?? c.getValue()) }),
    col.accessor("target_host", { header: t("目标域名") }),
    col.accessor("success", { header: t("结果"), cell: (c) => t(c.getValue() ? "成功" : "失败") }),
    col.accessor("duration_ms", { header: t("耗时"), cell: (c) => `${c.getValue()} ms` }),
    col.accessor("egress_bytes", { header: t("上行流量"), cell: (c) => c.row.original.bytes_measured ? formatBytes(c.getValue()) : "—" }),
    col.accessor("ingress_bytes", { header: t("下行流量"), cell: (c) => c.row.original.bytes_measured ? formatBytes(c.getValue()) : "—" }),
  ];
  function apply() {
    const next = { ...draft };
    for (const key of ["from", "to"] as const) {
      if (next[key]) {
        const date = new Date(next[key]);
        if (Number.isNaN(date.getTime())) { setValidation(t("时间格式无效")); return; }
        next[key] = date.toISOString();
      }
    }
    if (next.from && next.to && next.from >= next.to) { setValidation(t("开始时间必须早于结束时间")); return; }
    setValidation(""); setFilters(next); setCursors([""]); setSelected(null);
  }
  return <section className="probe-logs-page">
    <div className="page-header"><div><h1>{t("探测日志")}</h1><p>{t("查看 Resin 自身的出口和延迟检测，点击记录查看详情。")}</p></div>
      <Button variant="secondary" onClick={() => { setCursors([""]); void query.refetch(); }} disabled={query.isFetching}><RefreshCw size={16} />{t("刷新")}</Button>
    </div>
    <Card>
      <p>{t("仅记录实际发起的检测；缓存命中、预算阻止及被动业务反馈不产生记录。")}</p>
      <p>{t("保留最近 7 天，最多 100,000 条。流量包含目标 TLS，不等同于供应商账单；历史探测无法补录。")}</p>
      {(query.data?.dropped_since_start ?? 0) > 0 && <p role="status">{t("本次启动以来丢失日志条数")}: {query.data?.dropped_since_start}</p>}
      <form onSubmit={(event) => { event.preventDefault(); apply(); }}>
        <div className="form-grid">
          {(["subscription", "node_hash", "target_host"] as const).map((key) => <label key={key}><span className="field-label">{t({ subscription: "订阅名称或 ID", node_hash: "完整节点哈希", target_host: "目标域名" }[key])}</span><Input value={draft[key]} onChange={(e) => setDraft({ ...draft, [key]: e.target.value.trim() })} /></label>)}
          <label><span className="field-label">{t("探测类型")}</span><Select value={draft.kind} onChange={(e) => setDraft({ ...draft, kind: e.target.value })}><option value="">{t("全部")}</option><option value="egress">{t("出口检测")}</option><option value="latency">{t("延迟检测")}</option></Select></label>
          <label><span className="field-label">{t("触发原因")}</span><Select value={draft.reason} onChange={(e) => setDraft({ ...draft, reason: e.target.value })}><option value="">{t("全部")}</option>{Object.entries(reasons).map(([value, label]) => <option key={value} value={value}>{t(label)}</option>)}</Select></label>
          <label><span className="field-label">{t("结果")}</span><Select value={draft.success} onChange={(e) => setDraft({ ...draft, success: e.target.value })}><option value="">{t("全部")}</option><option value="true">{t("成功")}</option><option value="false">{t("失败")}</option></Select></label>
          {(["from", "to"] as const).map((key) => <label key={key}><span className="field-label">{t(key === "from" ? "开始时间" : "结束时间")}</span><Input type="datetime-local" value={draft[key]} onChange={(e) => setDraft({ ...draft, [key]: e.target.value })} /></label>)}
        </div>
        <div className="actions"><Button type="submit">{t("筛选")}</Button><Button type="button" variant="secondary" onClick={() => { setDraft(emptyFilters); setFilters(emptyFilters); setCursors([""]); setSelected(null); setValidation(""); }}>{t("重置")}</Button>
          <label><input type="checkbox" checked={autoRefresh} onChange={(e) => setAutoRefresh(e.target.checked)} />{t("首页每 5 秒自动刷新")}</label>
        </div>
      </form>
      {validation && <p role="alert">{validation}</p>}
    </Card>
    <Card>
      {query.isError ? <p role="alert">{formatApiErrorMessage(query.error, t)}</p> : query.isPending ? <p>{t("加载中...")}</p> : <>
        <DataTable data={query.data.items} columns={columns} getRowId={(row) => row.id} onRowClick={setSelected} selectedRowId={selected?.id} />
        {query.data.items.length === 0 && <p>{t("暂无探测日志")}</p>}
      </>}
      <CursorPagination pageIndex={cursors.length - 1} hasMore={query.data?.has_more ?? false} pageSize={limit} disabled={query.isFetching || query.isError} onPageSizeChange={(size) => { setLimit(size); setCursors([""]); }} onPrev={() => setCursors(cursors.slice(0, -1))} onNext={() => { if (query.data?.next_cursor) setCursors([...cursors, query.data.next_cursor]); }} />
    </Card>
    {selected && <Card><div className="page-header"><h2>{t("探测详情")}</h2><Button variant="secondary" onClick={() => setSelected(null)}>{t("关闭")}</Button></div>
      <p>{t("时间")}: {formatDateTime(selected.ts)} · ID: {selected.id}</p>
      <p>{t("节点")}: <code>{selected.node_hash}</code></p><p>{t("订阅")}: {selected.subscriptions}</p>
      <p>{t("目标域名")}: {selected.target_host} · {t("出口 IP")}: {selected.egress_ip || "—"}</p>
      <p>{t("触发原因")}: {t(reasons[selected.reason] ?? selected.reason)} · {t("结果")}: {t(selected.success ? "成功" : "失败")}</p>
      {selected.error && <p role="status">{t("错误")}: {selected.error}</p>}
      <p>{t("耗时")}: {selected.duration_ms} ms · {t("TLS 握手延迟")}: {selected.latency_ms} ms</p>
      <p>{t("上行流量")}: {selected.bytes_measured ? formatBytes(selected.egress_bytes) : "—"} · {t("下行流量")}: {selected.bytes_measured ? formatBytes(selected.ingress_bytes) : "—"}</p>
    </Card>}
  </section>;
}
