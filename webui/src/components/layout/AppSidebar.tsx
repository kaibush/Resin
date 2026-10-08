import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { AlertTriangle, ChevronRight, LogOut, PanelLeft, X } from "lucide-react";
import { NavLink, useLocation, useNavigate } from "react-router-dom";
import { Button } from "../ui/Button";
import { LanguageSwitcher } from "../LanguageSwitcher";
import { cn } from "../../lib/cn";
import { apiRequest } from "../../lib/api-client";
import { useAuthStore } from "../../features/auth/auth-store";
import { getEnvConfig } from "../../features/systemConfig/api";
import { useI18n } from "../../i18n";
import { isNavItemActive, navGroups } from "./nav";

const CLOSED_GROUPS_KEY = "resin.sidebar.closed-groups";

type SystemInfoResponse = {
  version: string;
};


function useDesktopNav() {
  const [desktop, setDesktop] = useState(() => window.matchMedia("(min-width: 961px)").matches);
  useEffect(() => {
    const media = window.matchMedia("(min-width: 961px)");
    const onChange = (event: MediaQueryListEvent) => setDesktop(event.matches);
    media.addEventListener("change", onChange);
    return () => media.removeEventListener("change", onChange);
  }, []);
  return desktop;
}

type AppSidebarProps = {
  collapsed: boolean;
  mobileOpen: boolean;
  onNavigate: () => void;
  onClose: () => void;
  onToggleCollapsed: () => void;
};

function readClosedGroups() {
  try {
    const parsed: unknown = JSON.parse(localStorage.getItem(CLOSED_GROUPS_KEY) ?? "[]");
    if (!Array.isArray(parsed)) {
      return [];
    }
    return parsed.filter((item): item is string => typeof item === "string");
  } catch {
    return [];
  }
}

export function AppSidebar({ collapsed, mobileOpen, onNavigate, onClose, onToggleCollapsed }: AppSidebarProps) {
  const { t } = useI18n();
  const location = useLocation();
  const navigate = useNavigate();
  const token = useAuthStore((state) => state.token);
  const clearToken = useAuthStore((state) => state.clearToken);
  const [closedGroups, setClosedGroups] = useState(readClosedGroups);
  const desktop = useDesktopNav();
  const iconMode = collapsed && desktop;
  const envConfigQuery = useQuery({
    queryKey: ["system-config-env", "shell"],
    queryFn: getEnvConfig,
    staleTime: 30_000,
  });
  const systemInfoQuery = useQuery({
    queryKey: ["system-info", "shell"],
    queryFn: () => apiRequest<SystemInfoResponse>("/api/v1/system/info"),
    staleTime: 300_000,
  });
  const logoSrc = `${import.meta.env.BASE_URL}vite.svg`;
  const version = systemInfoQuery.data?.version?.trim();
  const envConfig = envConfigQuery.data;
  const authWarnings: string[] = [];
  if (envConfig && !envConfig.admin_token_set) {
    authWarnings.push(t("RESIN_ADMIN_TOKEN 为空，控制面 API 免认证"));
  }
  if (envConfig && !envConfig.proxy_token_set) {
    authWarnings.push(t("RESIN_PROXY_TOKEN 为空，正/反向代理免认证"));
  }
  if (envConfig && envConfig.admin_token_set && envConfig.admin_token_weak) {
    authWarnings.push(t("RESIN_ADMIN_TOKEN 强度较弱，建议更换为更高熵随机令牌"));
  }
  if (envConfig && envConfig.proxy_token_set && envConfig.proxy_token_weak) {
    authWarnings.push(t("RESIN_PROXY_TOKEN 强度较弱，建议更换为更高熵随机令牌"));
  }

  const toggleGroup = (id: string) => {
    setClosedGroups((current) => {
      const next = current.includes(id) ? current.filter((item) => item !== id) : [...current, id];
      try {
        localStorage.setItem(CLOSED_GROUPS_KEY, JSON.stringify(next));
      } catch {
        /* ignore private mode */
      }
      return next;
    });
  };

  const logout = () => {
    clearToken();
    onNavigate();
    navigate("/login", { replace: true });
  };

  return (
    <aside className={cn("sidebar app-sidebar", mobileOpen && "is-open")}>
      <div className="sidebar-head">
        <div className="brand" title="Resin">
          <div className="brand-logo" aria-hidden="true">
            <img src={logoSrc} alt="" />
          </div>
          <div className="brand-copy">
            <div className="brand-title-row">
              <p className="brand-title">Resin</p>
              {version ? <span className="brand-version" title={version}>{version}</span> : null}
            </div>
            <p className="brand-subtitle">{t("高性能粘性代理池 · 管理面板")}</p>
          </div>
        </div>
        <button
          type="button"
          className="sidebar-collapse"
          aria-pressed={collapsed}
          aria-label={collapsed ? t("展开导航") : t("收起导航")}
          title={collapsed ? t("展开导航") : t("收起导航")}
          onClick={onToggleCollapsed}
        >
          <PanelLeft size={16} />
        </button>
        <button type="button" className="sidebar-close" aria-label={t("关闭")} onClick={onClose}>
          <X size={16} />
        </button>
      </div>

      <div className="sidebar-main">
        <nav className="nav-groups" aria-label={t("主导航")}>
          {navGroups.map((group) => {
            const active = group.items.some((item) => isNavItemActive(location.pathname, item.path));
            const closed = !iconMode && !active && closedGroups.includes(group.id);
            return (
              <div key={group.id} className={cn("nav-group", closed && "is-closed")}>
                <button
                  type="button"
                  className="nav-group-label"
                  aria-expanded={!closed}
                  aria-hidden={iconMode || undefined}
                  tabIndex={iconMode ? -1 : 0}
                  onClick={() => {
                    if (!iconMode) {
                      toggleGroup(group.id);
                    }
                  }}
                >
                  <span>{t(group.label)}</span>
                  <ChevronRight size={14} className={cn("nav-group-chevron", !closed && "is-open")} />
                </button>
                <div className="nav-group-items">
                  {group.items.map((item) => {
                    const Icon = item.icon;
                    return (
                      <NavLink
                        key={item.path}
                        to={item.path}
                        title={t(item.label)}
                        className={({ isActive }) => cn("nav-item", isActive && "nav-item-active")}
                        onClick={onNavigate}
                      >
                        <Icon size={16} strokeWidth={1.75} />
                        <span>{t(item.label)}</span>
                      </NavLink>
                    );
                  })}
                </div>
              </div>
            );
          })}
        </nav>
      </div>

      <div className="sidebar-bottom">
        {authWarnings.length > 0 ? (
          <div className="callout callout-warning sidebar-warning" role="alert" title={authWarnings.join("\n")}>
            <AlertTriangle size={16} />
            <div className="sidebar-warning-copy">
              <strong>{t("安全警告")}</strong>
              <div className="sidebar-warning-list">
                {authWarnings.map((warning) => (
                  <span key={warning}>{warning}</span>
                ))}
              </div>
            </div>
          </div>
        ) : null}
        {!token ? <p className="sidebar-hint">{t("当前为免认证访问模式")}</p> : null}
        <div className="sidebar-tools">
          {token ? (
            <Button variant="secondary" size="sm" className="sidebar-icon-btn" onClick={logout} aria-label={t("退出登录")} title={t("退出登录")}>
              <LogOut size={16} />
            </Button>
          ) : (
            <span className="sidebar-tool-spacer" aria-hidden="true" />
          )}
          <LanguageSwitcher className="sidebar-locale desktop-locale" compact />
        </div>
      </div>
      <button
        type="button"
        className="sidebar-rail"
        aria-label={collapsed ? t("展开导航") : t("收起导航")}
        tabIndex={-1}
        onClick={onToggleCollapsed}
      />
    </aside>
  );
}
