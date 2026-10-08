import { useEffect, useState } from "react";
import { Menu } from "lucide-react";
import { Outlet, useLocation } from "react-router-dom";
import { motion } from "framer-motion";
import { LanguageSwitcher } from "./LanguageSwitcher";
import { AppSidebar } from "./layout/AppSidebar";
import { isNavItemActive, navItems } from "./layout/nav";
import { cn } from "../lib/cn";
import { useI18n } from "../i18n";

const COLLAPSED_KEY = "resin.sidebar.collapsed";

function readCollapsed() {
  try {
    return localStorage.getItem(COLLAPSED_KEY) === "1";
  } catch {
    return false;
  }
}

export function AppShell() {
  const { t } = useI18n();
  const location = useLocation();
  const routeKey = `${location.pathname}${location.search}`;
  const [openOn, setOpenOn] = useState<string | null>(null);
  const [collapsed, setCollapsed] = useState(readCollapsed);
  const navOpen = openOn === routeKey;
  const current = navItems.find((item) => isNavItemActive(location.pathname, item.path));

  useEffect(() => {
    if (!navOpen) {
      return;
    }
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        setOpenOn(null);
      }
    };
    const previous = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    window.addEventListener("keydown", onKeyDown);
    return () => {
      document.body.style.overflow = previous;
      window.removeEventListener("keydown", onKeyDown);
    };
  }, [navOpen]);

  const toggleCollapsed = () => {
    setCollapsed((value) => {
      const next = !value;
      try {
        localStorage.setItem(COLLAPSED_KEY, next ? "1" : "0");
      } catch {
        /* ignore private mode */
      }
      return next;
    });
  };

  return (
    <div className={cn("app-layout", collapsed && "is-sidebar-collapsed")}>
      <header className="mobile-topbar">
        <button type="button" className="mobile-menu-btn" aria-label={t("主导航")} aria-expanded={navOpen} onClick={() => setOpenOn(routeKey)}>
          <Menu size={18} />
        </button>
        <div className="mobile-topbar-copy">
          <strong>{t(current?.label ?? "总览看板")}</strong>
          <span>Resin</span>
        </div>
        <LanguageSwitcher className="sidebar-locale" compact />
      </header>

      {navOpen ? <button type="button" className="nav-backdrop" aria-label={t("关闭")} onClick={() => setOpenOn(null)} /> : null}

      <AppSidebar
        collapsed={collapsed}
        mobileOpen={navOpen}
        onNavigate={() => setOpenOn(null)}
        onClose={() => setOpenOn(null)}
        onToggleCollapsed={toggleCollapsed}
      />

      <main className="main">
        <motion.div
          key={location.pathname}
          initial={{ opacity: 0, y: 6 }}
          animate={{ opacity: 1, y: 0 }}
          transition={{ duration: 0.2, ease: "easeOut" }}
          className="content"
        >
          <Outlet />
        </motion.div>
      </main>
    </div>
  );
}
