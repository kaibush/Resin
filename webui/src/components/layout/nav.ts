import {
  Activity,
  Cable,
  Database,
  LayoutDashboard,
  Logs,
  Network,
  Regex,
  Rss,
  Server,
  Settings,
  type LucideIcon,
} from "lucide-react";

export type NavItem = {
  label: string;
  path: string;
  icon: LucideIcon;
};

export type NavGroup = {
  id: string;
  label: string;
  items: NavItem[];
};

export const navGroups: NavGroup[] = [
  {
    id: "overview",
    label: "概览",
    items: [{ label: "总览看板", path: "/dashboard", icon: LayoutDashboard }],
  },
  {
    id: "proxy",
    label: "代理",
    items: [
      { label: "平台管理", path: "/platforms", icon: Server },
      { label: "订阅管理", path: "/subscriptions", icon: Rss },
      { label: "节点池", path: "/nodes", icon: Network },
      { label: "接入点", path: "/endpoints", icon: Cable },
    ],
  },
  {
    id: "traffic",
    label: "流量",
    items: [
      { label: "请求头规则", path: "/rules", icon: Regex },
      { label: "请求日志", path: "/request-logs", icon: Logs },
      { label: "探测日志", path: "/probe-logs", icon: Activity },
    ],
  },
  {
    id: "system",
    label: "系统",
    items: [
      { label: "资源", path: "/resources", icon: Database },
      { label: "系统配置", path: "/system-config", icon: Settings },
    ],
  },
];

export const navItems = navGroups.flatMap((group) => group.items);

export function isNavItemActive(pathname: string, path: string) {
  return pathname === path || pathname.startsWith(`${path}/`);
}
