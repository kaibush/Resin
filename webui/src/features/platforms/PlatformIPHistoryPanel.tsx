import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import { AlertTriangle, RefreshCw, Search } from "lucide-react";
import { useState } from "react";
import { Badge } from "../../components/ui/Badge";
import { Button } from "../../components/ui/Button";
import { Card } from "../../components/ui/Card";
import { DataTable } from "../../components/ui/DataTable";
import { Input } from "../../components/ui/Input";
import { OffsetPagination } from "../../components/ui/OffsetPagination";
import { Select } from "../../components/ui/Select";
import { useI18n } from "../../i18n";
import { formatApiErrorMessage } from "../../lib/error-message";
import { listPlatformIPHistory, resetPlatformIPHistory } from "./api";
import type { ListIPHistoryInput, Platform, PlatformIPHistory } from "./types";

const PAGE_SIZES = [25, 50, 100] as const;

export function PlatformIPHistoryPanel({ platform, onConfigure }: { platform: Platform; onConfigure: () => void }) {
  const { t } = useI18n();
  const client = useQueryClient();
  const [page, setPage] = useState(0);
  const [pageSize, setPageSize] = useState<number>(25);
  const [search, setSearch] = useState("");
  const [submittedSearch, setSubmittedSearch] = useState("");
  const [status, setStatus] = useState<ListIPHistoryInput["status"]>("");
  const [order, setOrder] = useState<"asc" | "desc">("desc");
  const query = useQuery({
    queryKey: ["platform-ip-history", platform.id, page, pageSize, submittedSearch, status, order, platform.updated_at],
    queryFn: () => listPlatformIPHistory(platform.id, {
      limit: pageSize, offset: page * pageSize, egress_ip: submittedSearch,
      status, sort_by: "historical_accounts", sort_order: order,
    }),
    refetchInterval: 30_000,
    refetchIntervalInBackground: false,
  });
  const reset = useMutation({
    mutationFn: (ip: string) => resetPlatformIPHistory(platform.id, ip),
    onSuccess: () => client.invalidateQueries({ queryKey: ["platform-ip-history"] }),
  });
  const handleReset = (ip: string) => {
    if (window.confirm(t("确认清零 {{ip}} 的历史账号计数？此操作对所有平台生效且不可撤销，不会释放现有租约。后续使用将重新累计。", { ip }))) {
      reset.mutate(ip);
    }
  };
  const summary = query.data?.summary;
  const columns: ColumnDef<PlatformIPHistory>[] = [
    { accessorKey: "egress_ip", header: t("出口 IP") },
    { accessorKey: "historical_accounts", header: t("历史不同账号数"), cell: ({ row }) => <strong>{row.original.historical_accounts.toLocaleString()}</strong> },
    { accessorKey: "active_accounts", header: t("当前活跃账号数") },
    { accessorKey: "node_count", header: t("可用节点数") },
    { id: "limit", header: t("软门限"), cell: () => summary?.enabled && summary.soft_limit > 0 ? summary.soft_limit : "—" },
    {
      accessorKey: "threshold_status", header: t("门限状态"),
      cell: ({ row }) => {
        const value = row.original.threshold_status;
        return <Badge variant={value === "reached" ? "warning" : value === "below" ? "success" : "muted"}>
          {t({ disabled: "未启用", unlimited: "未设门限", below: "低于门限", reached: "已达门限" }[value])}
        </Badge>;
      },
    },
    {
      id: "actions", header: t("操作"),
      cell: ({ row }) => <Button variant="ghost" size="sm" disabled={reset.isPending || row.original.historical_accounts === 0}
        aria-label={t("清零 {{ip}} 的历史计数", { ip: row.original.egress_ip })}
        onClick={() => handleReset(row.original.egress_ip)}>{t("清零历史")}</Button>,
    },
  ];
  const total = query.data?.total ?? 0;
  const error = query.error ?? reset.error;
  return <section className="platform-ip-history">
    <div className="platform-drawer-section-head platform-lease-head">
      <div>
        <h4>{t("出口 IP 使用历史")}</h4>
        <p>{t("展示当前平台全部可用出口 IP，包含历史计数为 0 的 IP；多个节点共用同一出口时合并展示。")}</p>
      </div>
      <Button variant="secondary" onClick={onConfigure}>{t("配置历史均衡")}</Button>
    </div>
    <div className="callout">
      <div>
        <Badge variant={platform.ip_governance_enabled ? "success" : "muted"}>{platform.ip_governance_enabled ? t("历史均衡已开启") : t("历史均衡已关闭")}</Badge>
        <p>{t("历史计数按出口 IP 去重粘性标记，跨平台合并。同一标记再次使用同一 IP 不增加，租约过期不减少。关闭开关后停止记录和历史均衡，已有历史保留。")}</p>
        <p>{t("软门限全部达到后仍会选择历史账号最少的 IP。历史从功能启用后累计，升级时仅补入仍保留的绑定，不能还原更早已丢失的记录。")}</p>
      </div>
    </div>
    <div className="platform-monitor-kpi-grid" aria-live="polite">
      {[
        ["当前池可用 IP", summary?.available_ips],
        ["已有历史的 IP", summary?.used_ips],
        ["历史计数为零的 IP", summary?.unused_ips],
        ["已达门限的 IP", summary?.reached_ips],
      ].map(([label, value]) => <Card className="platform-monitor-kpi-card" key={String(label)}>
        <div><p className="platform-monitor-kpi-label">{t(String(label))}</p>
          <p className="platform-monitor-kpi-value">{typeof value === "number" ? value.toLocaleString() : "—"}</p></div>
      </Card>)}
    </div>
    <p className="muted">{t("其中 {{count}} 个可用 IP 当前有活跃租约；活跃数只供观察，不用于历史均衡。", { count: summary?.active_ips ?? "—" })}</p>
    <form className="platform-ip-history-toolbar" onSubmit={(event) => { event.preventDefault(); setSubmittedSearch(search.trim()); setPage(0); }}>
      <label className="search-box" htmlFor="ip-history-search"><Search size={16} />
        <Input id="ip-history-search" type="search" value={search} onChange={(event) => setSearch(event.target.value)} placeholder={t("搜索出口 IP")} aria-label={t("搜索出口 IP")} />
      </label>
      <Button variant="secondary" type="submit">{t("搜索")}</Button>
      <Select aria-label={t("筛选 IP 使用状态")} value={status} onChange={(event) => { setStatus(event.target.value as ListIPHistoryInput["status"]); setPage(0); }}>
        <option value="">{t("全部可用 IP")}</option><option value="unused">{t("历史计数为零")}</option><option value="reached">{t("已达门限")}</option>
      </Select>
      <Select aria-label={t("历史账号数排序")} value={order} onChange={(event) => { setOrder(event.target.value as "asc" | "desc"); setPage(0); }}>
        <option value="desc">{t("历史账号数：从多到少")}</option><option value="asc">{t("历史账号数：从少到多")}</option>
      </Select>
      <Button type="button" variant="secondary" size="sm" disabled={query.isFetching} onClick={() => void query.refetch()}>
        <RefreshCw size={16} className={query.isFetching ? "spin" : undefined} />{t("刷新")}
      </Button>
    </form>
    {error ? <div className="callout callout-error" role="alert"><AlertTriangle size={14} /><span>{formatApiErrorMessage(error, t)}</span></div> : null}
    {query.isPending ? <p className="muted">{t("正在加载 IP 使用历史...")}</p> : null}
    {query.data ? <>
      {query.data.items.length ? <DataTable data={query.data.items} columns={columns} getRowId={(row) => row.egress_ip} /> : <div className="empty-box">{t("没有匹配的可用出口 IP")}</div>}
      <OffsetPagination page={page} totalPages={Math.max(1, Math.ceil(total / pageSize))} totalItems={total} pageSize={pageSize}
        pageSizeOptions={PAGE_SIZES} disabled={query.isFetching} onPageChange={setPage} onPageSizeChange={(size) => { setPageSize(size); setPage(0); }} />
    </> : null}
  </section>;
}
