import { zodResolver } from "@hookform/resolvers/zod";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { RefreshCw, X } from "lucide-react";
import { useEffect, useRef } from "react";
import { useForm, type UseFormReturn } from "react-hook-form";
import { z } from "zod";
import { Button } from "../../components/ui/Button";
import { Card } from "../../components/ui/Card";
import { Input } from "../../components/ui/Input";
import { useI18n } from "../../i18n";
import { formatApiErrorMessage } from "../../lib/error-message";
import { getSubscription, updateSubscription } from "./api";
import type { ProbePolicy, Subscription } from "./types";

const BYTES_PER_MB = 1_000_000;
const BYTES_PER_GB = 1_000_000_000;

const policySchema = z.object({
  probe_policy: z.object({
    mode: z.enum(["inherit", "metered"]),
    egress_interval: z.string().trim().min(1),
    active_window: z.string().trim().min(1),
    max_egress_age: z.string().trim().min(1),
    monthly_budget_bytes: z.number().int().min(65536),
    price_per_gb: z.number().finite().min(0).max(1_000_000),
    strict_budget: z.boolean(),
  }),
});

function formatMB(bytes: number): string {
  return Number.isFinite(bytes) ? (bytes / BYTES_PER_MB).toFixed(2) : "—";
}

function formatGB(bytes: number): string {
  return Number.isFinite(bytes) ? (bytes / BYTES_PER_GB).toFixed(3) : "—";
}

function formatPrice(value: number): string {
  return Number.isFinite(value) ? value.toLocaleString("zh-CN", { maximumFractionDigits: 4 }) : "—";
}

function estimateCost(bytes: number, pricePerGb: number): string {
  return Number.isFinite(bytes) && Number.isFinite(pricePerGb) ? (bytes / BYTES_PER_GB * pricePerGb).toFixed(2) : "—";
}
type PolicyForm = z.infer<typeof policySchema>;

type Props = {
  subscription: Subscription;
  mode: "policy" | "usage";
  onClose: () => void;
  onSaved: () => void;
};

export function SubscriptionProbeDialog({ subscription, mode, onClose, onSaved }: Props) {
  const { t } = useI18n();
  const queryClient = useQueryClient();
  const dialogRef = useRef<HTMLDivElement>(null);
  const query = useQuery({
    queryKey: ["subscription-probe", subscription.id, mode],
    queryFn: () => getSubscription(subscription.id),
    staleTime: 0,
    refetchOnMount: "always",
    refetchOnWindowFocus: mode === "usage",
    refetchInterval: mode === "usage" ? 30_000 : false,
  });
  const save = useMutation({
    mutationFn: (policy: ProbePolicy) => updateSubscription(subscription.id, { probe_policy: policy }),
    onSuccess: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["subscriptions"] }),
        queryClient.invalidateQueries({ queryKey: ["subscription-probe", subscription.id] }),
      ]);
      onSaved();
      onClose();
    },
  });
  useEffect(() => {
    const previous = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    dialogRef.current?.focus();
    return () => previous?.focus();
  }, []);
  const close = () => { if (!save.isPending) onClose(); };
  return <div className="modal-overlay" role="dialog" aria-modal="true" aria-labelledby="subscription-probe-title"
    ref={dialogRef} tabIndex={-1}
    onKeyDown={(event) => {
      if (event.key === "Escape") { event.stopPropagation(); close(); }
      if (event.key === "Tab") {
        const elements = dialogRef.current?.querySelectorAll<HTMLElement>('button:not(:disabled), input:not(:disabled), select:not(:disabled), a[href]');
        if (!elements?.length) { event.preventDefault(); return; }
        const first = elements[0]; const last = elements[elements.length - 1];
        if (event.shiftKey && (document.activeElement === first || document.activeElement === dialogRef.current)) { event.preventDefault(); last.focus(); }
        if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
      }
    }}>
    <Card className="modal-card">
      <div className="modal-header">
        <h3 id="subscription-probe-title">{t(mode === "policy" ? "探测策略" : "探测流量")} · {subscription.name}</h3>
        <Button variant="ghost" size="sm" aria-label={t("关闭")} disabled={save.isPending} onClick={close}><X size={16} /></Button>
      </div>
      {(query.isPending || !query.isFetchedAfterMount) ? <p>{t("加载中...")}</p> : query.isError ? <div role="alert"><p>{formatApiErrorMessage(query.error, t)}</p><Button variant="secondary" onClick={() => void query.refetch()}>{t("重试")}</Button></div> : mode === "policy" ?
        <PolicyEditor subscription={query.data} pending={save.isPending} error={save.error} onSubmit={(policy) => save.mutate(policy)} onClose={close} /> : <>
          <ProbeUsagePanel subscription={query.data} />
          <div className="detail-actions">
            <Button variant="secondary" disabled={query.isFetching} onClick={() => void query.refetch()}><RefreshCw size={14} />{t("刷新")}</Button>
            <Button variant="secondary" onClick={close}>{t("关闭")}</Button>
          </div>
        </>}
    </Card>
  </div>;
}

function PolicyEditor({ subscription, pending, error, onSubmit, onClose }: {
  subscription: Subscription; pending: boolean; error: Error | null;
  onSubmit: (policy: ProbePolicy) => void; onClose: () => void;
}) {
  const { t } = useI18n();
  const form = useForm<PolicyForm>({ resolver: zodResolver(policySchema), defaultValues: { probe_policy: { ...subscription.probe_policy } } });
  return <form className="form-grid" onSubmit={form.handleSubmit((values) => onSubmit(values.probe_policy))}>
    <fieldset className="field-span-2" disabled={pending} style={{ border: 0, padding: 0, margin: 0, minWidth: 0 }}>
      <ProbePolicyFields form={form} />
    </fieldset>
    {error && <p className="field-error field-span-2" role="alert">{formatApiErrorMessage(error, t)}</p>}
    <div className="detail-actions field-span-2">
      <Button variant="secondary" type="button" disabled={pending} onClick={onClose}>{t("取消")}</Button>
      <Button type="submit" disabled={pending}>{t(pending ? "保存中..." : "保存")}</Button>
    </div>
  </form>;
}

function ProbePolicyFields({ form }: { form: UseFormReturn<PolicyForm> }) {
  const { t } = useI18n();
  const mode = form.watch("probe_policy.mode");
  const budgetBytes = form.watch("probe_policy.monthly_budget_bytes");
  const pricePerGb = form.watch("probe_policy.price_per_gb");
  return <div className="field-group field-span-2">
    <label className="field-label">{t("探测策略")}</label>
    <select className="input" aria-label={t("探测策略")} {...form.register("probe_policy.mode")}>
      <option value="inherit">{t("继承全局")}</option>
      <option value="metered">{t("按流量计费")}</option>
    </select>
    {mode === "metered" && <>
      <p>{t("首次使用时验证；空闲节点不主动探测；出口检测同时提供延迟结果。失败后按 5 分钟、30 分钟、2 小时、12 小时退避。")}</p>
      <div className="form-grid">
        <label>{t("活跃节点出口检测间隔")}<Input {...form.register("probe_policy.egress_interval")} /></label>
        <label>{t("近期使用窗口")}<Input {...form.register("probe_policy.active_window")} /></label>
        <label>{t("使用前出口信息最大年龄")}<Input {...form.register("probe_policy.max_egress_age")} /></label>
        <label>{t("每月探测预算（字节，1 GB = 1000000000）")}<Input type="number" min={65536} step={1} {...form.register("probe_policy.monthly_budget_bytes", { valueAsNumber: true })} /></label>
        <label>{t("每 GB 流量费用（元）")}<Input type="number" min={0} max={1_000_000} step="any" {...form.register("probe_policy.price_per_gb", { valueAsNumber: true })} /></label>
      </div>
      <label><input type="checkbox" {...form.register("probe_policy.strict_budget")} /> {t("严格预算：首次验证和手动检测也受预算限制")}</label>
      <p>{t("默认只限制后台检测；严格模式到限后，需验证的新分配会失败。预算按 UTC 月统计。缩短出口有效期会增加费用；检查间隔不能保证上游 IP 不变。")}</p>
      <p>{t("按供应商实际单价填写。探测流量里的估算费用使用这个值，不会固定按 3 元/GB 计算。")}</p>
      <BudgetPreview bytes={budgetBytes} pricePerGb={pricePerGb} />
    </>}
    {form.formState.errors.probe_policy && <p className="field-error">{t("请检查探测策略的间隔、预算和单价。")}</p>}
  </div>;
}
function BudgetPreview({ bytes, pricePerGb }: { bytes: number; pricePerGb: number }) {
  const { t } = useI18n();
  const valid = Number.isFinite(bytes) && Number.isFinite(pricePerGb);
  return <div className="probe-budget-preview" aria-live="polite">
    <p>{t("预算换算")}：{valid ? `${bytes.toLocaleString("zh-CN")} ${t("字节")} = ${formatMB(bytes)} MB = ${formatGB(bytes)} GB` : t("无法换算")}</p>
    <p>{t("按当前单价，预算用满约")} ¥{valid ? estimateCost(bytes, pricePerGb) : "—"}（¥{formatPrice(pricePerGb)}/GB）</p>
  </div>;
}

function ProbeUsagePanel({ subscription }: { subscription: Subscription }) {
  const { t } = useI18n();
  const rows = subscription.probe_usage ?? [];
  const bytes = rows.reduce((sum, row) => sum + row.ingress_bytes + row.egress_bytes, 0);
  const reserved = rows.reduce((sum, row) => sum + (Number.isFinite(row.reserved_bytes) ? row.reserved_bytes : 0), 0);
  const today = new Date().toISOString().slice(0, 10);
  const todayBytes = rows.filter(row => row.day === today).reduce((sum, row) => sum + row.ingress_bytes + row.egress_bytes, 0);
  const metered = subscription.probe_policy.mode === "metered";
  const limit = metered ? subscription.probe_policy.monthly_budget_bytes : 0;
  const price = Number.isFinite(subscription.probe_policy.price_per_gb) ? subscription.probe_policy.price_per_gb : 0;
  const remaining = Math.max(0, limit - bytes - reserved);
  const blocked = metered && bytes + reserved + 65536 > limit;
  return <div className="field-group field-span-2">
    <strong>{t("探测流量（UTC，估算）")}</strong>
    {subscription.probe_policy.mode !== "metered" && <p>{t("此处显示按流量计费策略的用量账本；继承全局期间的检测请在探测日志查看。")}</p>}
    {subscription.probe_usage_error ? <p className="field-error">{t("探测统计暂不可用")}</p> : <>
      {metered && <p>{t("月预算")} {formatMB(limit)} MB（{formatGB(limit)} GB） · {t("已用")} {formatMB(bytes)} MB · {t("剩余")} {formatMB(remaining)} MB</p>}
      <p>{t("今日")} {formatMB(todayBytes)} MB · {t("本月")} {formatMB(bytes)} MB{metered ? ` · ¥${estimateCost(bytes, price)}（¥${formatPrice(price)}/GB）` : ""}</p>
      {metered && <p>{t("未结算预留")} {formatMB(reserved)} MB {blocked ? t("后台探测预算不足，周期检测已暂停") : ""}</p>}
      {metered && price === 0 && <p>{t("单价为 0，估算费用为 0。请在探测策略中填写每 GB 费用。")}</p>}
      <p>{t("统计包含目标 TLS 流量，不等同于供应商账单。异常退出未结算的预留额度在当月继续占用预算。")}</p>
      <div className="data-table-wrap"><table className="data-table"><thead><tr><th>{t("原因")}</th><th>{t("次数")}</th><th>{t("失败")}</th><th>{t("上行 MB")}</th><th>{t("下行 MB")}</th></tr></thead><tbody>
        {([ ["required", "使用前验证"], ["periodic", "周期检测"], ["retry", "故障复测"], ["manual", "手动检测"] ] as const).map(([reason, label]) => {
          const group = rows.filter(row => row.reason === reason);
          return <tr key={reason}>
            <td data-label={t("原因")}>{t(label)}</td>
            <td data-label={t("次数")}>{group.reduce((s,r)=>s+r.attempts,0)}</td>
            <td data-label={t("失败")}>{group.reduce((s,r)=>s+r.failures,0)}</td>
            <td data-label={t("上行 MB")}>{(group.reduce((s,r)=>s+r.egress_bytes,0)/1e6).toFixed(2)}</td>
            <td data-label={t("下行 MB")}>{(group.reduce((s,r)=>s+r.ingress_bytes,0)/1e6).toFixed(2)}</td>
          </tr>;
        })}
      </tbody></table></div>
    </>}
  </div>;
}
