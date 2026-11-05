"use client";

import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useCallback, useEffect, useState } from "react";
import { api, type SystemInfo } from "@/lib/api";

type NavSection = "workspace" | "production" | "runtime" | "assets" | "supply" | "nodes" | "system";

const NAV: { href: string; label: string; section: NavSection }[] = [
  { href: "/", label: "工作台", section: "workspace" },
  { href: "/production/", label: "生产", section: "production" },
  { href: "/tasks/", label: "运行", section: "runtime" },
  { href: "/accounts/", label: "资产", section: "assets" },
  { href: "/materials/", label: "供给", section: "supply" },
  { href: "/nodes/", label: "节点", section: "nodes" },
  { href: "/settings/", label: "系统", section: "system" },
];

const CONTEXT_LINKS: Record<NavSection, { href: string; label: string }[]> = {
  workspace: [],
  production: [
    { href: "/production/", label: "生产总览" },
    { href: "/platform/claude/", label: "Claude" },
    { href: "/platform/chatgpt/", label: "ChatGPT" },
    { href: "/platform/grok/", label: "Grok" },
    { href: "/platform/outlook/", label: "Outlook" },
  ],
  runtime: [
    { href: "/tasks/", label: "任务队列" },
    { href: "/audit/", label: "操作审计" },
  ],
  assets: [{ href: "/accounts/", label: "账号主数据" }],
  supply: [
    { href: "/materials/", label: "物料总览" },
    { href: "/apps/icmail/", label: "IC 邮箱" },
  ],
  nodes: [
    { href: "/nodes/", label: "节点总览" },
    { href: "/settings/?focus=node", label: "节点配置" },
    { href: "/tasks/", label: "节点运行日志" },
  ],
  system: [
    { href: "/settings/", label: "配置" },
    { href: "/audit/", label: "审计" },
  ],
};

type Theme = "light" | "dark";

function currentTheme(): Theme {
  if (typeof document === "undefined") return "light";
  return document.documentElement.classList.contains("dark") ? "dark" : "light";
}

export function Shell({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  const router = useRouter();
  const [ready, setReady] = useState(false);
  const [user, setUser] = useState("");
  const [sys, setSys] = useState<SystemInfo | null>(null);
  const [open, setOpen] = useState(false);
  const [theme, setTheme] = useState<Theme>("light");

  const loadSystem = useCallback(async () => {
    try {
      setSys(await api.system());
    } catch {
      /* 轮询失败保留旧值 */
    }
  }, []);

  useEffect(() => {
    setTheme(currentTheme());
    let alive = true;
    api
      .session()
      .then((data) => {
        if (!alive) return;
        setUser(data.user);
        setReady(true);
      })
      .catch(() => {
        if (alive) router.replace("/login/");
      });
    return () => {
      alive = false;
    };
  }, [router]);

  useEffect(() => {
    if (!ready) return;
    loadSystem();
    const timer = setInterval(loadSystem, 10_000);
    return () => clearInterval(timer);
  }, [ready, loadSystem]);

  const toggleTheme = () => {
    const next: Theme = theme === "dark" ? "light" : "dark";
    setTheme(next);
    document.documentElement.classList.toggle("dark", next === "dark");
    document.documentElement.style.colorScheme = next;
    try {
      localStorage.setItem("rf-theme", next);
    } catch {
      /* 隐私模式下忽略 */
    }
  };

  const logout = async () => {
    await api.logout().catch(() => null);
    router.replace("/login/");
  };

  const activeItem = NAV.find((item) =>
    item.href === "/" ? pathname === "/" : pathname.startsWith(item.href.replace(/\/$/, "")),
  );
  const nodeConfigFocused = pathname.startsWith("/settings") &&
    typeof window !== "undefined" && new URLSearchParams(window.location.search).get("focus") === "node";
  const activeSection: NavSection = nodeConfigFocused ? "nodes" : activeItem?.section ||
    (pathname.startsWith("/platform/") ? "production" :
      pathname.startsWith("/apps/icmail") || pathname.startsWith("/materials") ? "supply" :
      pathname.startsWith("/nodes") ? "nodes" :
        pathname.startsWith("/audit") ? "runtime" : "workspace");
  const activeContext = CONTEXT_LINKS[activeSection].find((item) => {
    const [itemPath, itemQuery] = item.href.split("?");
    if (itemQuery === "focus=node") return nodeConfigFocused;
    return itemPath === "/" ? pathname === "/" : pathname.startsWith(itemPath.replace(/\/$/, ""));
  });
  const activeLabel = activeContext?.label || activeItem?.label || "工作台";

  return (
    <div className="flex min-h-screen bg-background text-foreground">
      <aside
        className={`fixed inset-y-0 left-0 z-30 flex w-[228px] flex-col border-r border-sidebar-border bg-sidebar text-sidebar-foreground transition-transform duration-200 md:static md:translate-x-0 ${
          open ? "translate-x-0" : "-translate-x-full"
        }`}
      >
        <div className="flex h-14 items-center gap-2.5 border-b border-sidebar-border px-4">
          <div className="grid h-7 w-7 place-items-center rounded-md bg-primary text-[13px] font-semibold text-primary-foreground">
            R
          </div>
          <div className="min-w-0">
            <div className="truncate text-[13px] font-semibold leading-tight">reg-factory</div>
            <div className="truncate text-[10.5px] text-muted-foreground">Control Plane</div>
          </div>
        </div>

        <nav className="flex-1 space-y-1 overflow-y-auto p-3">
          {NAV.map((item) => {
            const active = item.section === activeSection;
            return (
              <div key={item.href}>
                <Link
                  href={item.href}
                  onClick={() => setOpen(false)}
                  className={`nav-item ${active ? "nav-item-active" : ""}`}
                >
                  <span className="min-w-0 flex-1 truncate">{item.label}</span>
                  {active && item.section !== "workspace" && <span className="h-1.5 w-1.5 rounded-full bg-brand" />}
                </Link>
                {active && CONTEXT_LINKS[item.section].length > 0 && (
                  <div className="ml-3 mt-1 space-y-0.5 border-l border-sidebar-border pl-3">
                    {CONTEXT_LINKS[item.section].map((link) => {
                      const selected = pathname === link.href || pathname.startsWith(link.href);
                      return (
                        <Link
                          key={link.href}
                          href={link.href}
                          onClick={() => setOpen(false)}
                          className={`block rounded-md px-2.5 py-1.5 text-[12px] transition-colors ${
                            selected ? "bg-sidebar-accent font-medium text-sidebar-accent-foreground" : "text-muted-foreground hover:bg-sidebar-accent hover:text-sidebar-accent-foreground"
                          }`}
                        >
                          {link.label}
                        </Link>
                      );
                    })}
                  </div>
                )}
              </div>
            );
          })}
        </nav>

        <div className="border-t border-sidebar-border p-3">
          <div className="mb-2 flex items-center justify-between px-1 text-[11px] text-muted-foreground">
            <span>当前会话</span>
            <span className="font-mono text-foreground">{user || "-"}</span>
          </div>
          <button
            onClick={logout}
            className="w-full rounded-md border border-sidebar-border px-2 py-1.5 text-[12px] text-muted-foreground transition-colors hover:bg-sidebar-accent hover:text-sidebar-accent-foreground"
          >
            退出登录
          </button>
        </div>
      </aside>

      <div className="flex min-w-0 flex-1 flex-col">
        <header className="sticky top-0 z-20 flex h-14 items-center gap-3 border-b border-border bg-background/90 px-4 backdrop-blur lg:px-6">
          <button className="btn-ghost md:hidden" onClick={() => setOpen((v) => !v)}>
            导航
          </button>
          <div className="flex items-center gap-2 text-[13px]">
            <span className="text-muted-foreground">SG REG</span>
            <span className="text-muted-foreground/40">/</span>
            <span className="font-medium">{activeLabel}</span>
          </div>

          <Link href="/tasks/" className="hidden items-center gap-1.5 rounded-full border border-border bg-muted/50 px-2.5 py-1 text-[11px] text-muted-foreground transition-colors hover:border-brand/40 hover:text-foreground sm:flex">
            <span className="font-mono text-foreground">{sys?.running ?? 0}/{sys?.max_parallel ?? "-"}</span>
            <span>运行</span>
            {!!sys?.queued && (
              <span className="text-warning">· 排队 {sys.queued}</span>
            )}
            {sys?.guarded && (
              <span className="text-destructive">· 资源受限</span>
            )}
          </Link>

          <div className="ml-auto flex items-center gap-2 text-[11px] text-muted-foreground">
            <span className="hidden lg:inline">{user || "管理员"}</span>
            <button
              onClick={toggleTheme}
              className="btn-ghost px-2 py-1"
              title={theme === "dark" ? "切换到亮色" : "切换到暗色"}
              aria-label="切换主题"
            >
              {theme === "dark" ? "☀" : "☾"}
            </button>
          </div>
        </header>

        <main className="flex-1 p-4 lg:px-8 lg:py-6">
          {ready ? children : <div className="text-[13px] text-muted-foreground">正在验证会话…</div>}
        </main>
      </div>
    </div>
  );
}
