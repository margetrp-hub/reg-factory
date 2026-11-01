"use client";

import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useCallback, useEffect, useState } from "react";
import { api, type SystemInfo } from "@/lib/api";

const NAV = [
  { href: "/", label: "概览", hint: "overview" },
  { href: "/platform/claude/", label: "Claude", hint: "claude" },
  { href: "/platform/chatgpt/", label: "ChatGPT", hint: "chatgpt" },
  { href: "/platform/grok/", label: "Grok", hint: "grok" },
  { href: "/platform/outlook/", label: "Outlook", hint: "outlook" },
  { href: "/tasks/", label: "全部任务", hint: "tasks" },
  { href: "/assets/", label: "资产", hint: "assets" },
  { href: "/apps/icmail/", label: "IC 邮箱", hint: "icmail" },
  { href: "/accounts/", label: "账号", hint: "accounts" },
  { href: "/settings/", label: "配置", hint: "settings" },
  { href: "/audit/", label: "审计", hint: "audit" },
];

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

  const memPct = sys?.memory.used_pct ?? 0;
  const activeLabel =
    NAV.find((item) =>
      item.href === "/" ? pathname === "/" : pathname.startsWith(item.href.replace(/\/$/, "")),
    )?.label || "概览";

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
            const active =
              item.href === "/" ? pathname === "/" : pathname.startsWith(item.href.replace(/\/$/, ""));
            return (
              <Link
                key={item.href}
                href={item.href}
                onClick={() => setOpen(false)}
                className={`nav-item ${active ? "nav-item-active" : ""}`}
              >
                <span className="min-w-0 flex-1 truncate">{item.label}</span>
                <span className="shrink-0 font-mono text-[10px] lowercase tracking-wide text-muted-foreground/60">
                  {item.hint}
                </span>
              </Link>
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
        <header className="sticky top-0 z-20 flex h-14 flex-wrap items-center gap-x-4 gap-y-1 border-b border-border bg-background/90 px-4 backdrop-blur">
          <button className="btn-ghost md:hidden" onClick={() => setOpen((v) => !v)}>
            菜单
          </button>
          <div className="flex items-center gap-2 text-[13px]">
            <span className="text-muted-foreground">控制面</span>
            <span className="text-muted-foreground/40">/</span>
            <span className="font-medium">{activeLabel}</span>
          </div>

          <div className="flex items-center gap-1.5 text-[11px] text-muted-foreground">
            <span className="rounded-md bg-muted px-1.5 py-0.5 font-mono text-foreground">
              {sys?.running ?? 0}/{sys?.max_parallel ?? "-"}
            </span>
            <span>运行</span>
            {!!sys?.queued && (
              <span className="rounded-md bg-warning/15 px-1.5 py-0.5 font-mono text-warning">
                排队 {sys.queued}
              </span>
            )}
            {sys?.guarded && (
              <span className="rounded-md bg-destructive/10 px-1.5 py-0.5 font-mono text-destructive">
                内存闸门
              </span>
            )}
          </div>

          <div className="hidden min-w-[180px] flex-1 items-center gap-2 text-[11px] text-muted-foreground sm:flex">
            <span className="shrink-0 font-mono">
              {sys?.memory.current_mb ?? 0}/{sys?.memory.max_mb ?? 0}MB
            </span>
            <div className="h-1.5 w-full max-w-[190px] overflow-hidden rounded-full bg-muted">
              <div
                className={`h-full rounded-full transition-all ${
                  sys?.guarded ? "bg-destructive" : memPct > 70 ? "bg-warning" : "bg-success"
                }`}
                style={{ width: `${Math.min(100, memPct)}%` }}
              />
            </div>
            <span className="shrink-0 font-mono">{memPct}%</span>
            <span className="shrink-0 font-mono text-muted-foreground">CPU {sys?.cpu_pct ?? 0}%</span>
          </div>

          <div className="ml-auto flex items-center gap-2 text-[11px] text-muted-foreground">
            <span className="hidden md:inline">
              版本 <span className="font-mono text-foreground">{sys?.version ?? "-"}</span>
            </span>
            <span className="hidden md:inline">
              磁盘 <span className="font-mono text-foreground">{sys?.disk_free_gb?.toFixed(0) ?? "-"}G</span>
            </span>
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

        <main className="flex-1 p-4 lg:p-6">
          {ready ? children : <div className="text-[13px] text-muted-foreground">正在验证会话…</div>}
        </main>
      </div>
    </div>
  );
}
