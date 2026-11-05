"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { api, type AccountRow, type Task } from "@/lib/api";
import { Badge, Card, Stat } from "@/components/ui";

const PLATFORMS = [
  { slug: "claude", name: "Claude", note: "邮箱取码 → 注册 → sessionKey", accent: "bg-[#d97757]" },
  { slug: "chatgpt", name: "ChatGPT", note: "注册 → Codex 授权 → 导入", accent: "bg-[#10a37f]" },
  { slug: "grok", name: "Grok", note: "住宅代理 → 注册 → SSO", accent: "bg-foreground" },
  { slug: "outlook", name: "Outlook", note: "常驻注册 → Graph 授权", accent: "bg-[#0078d4]" },
];

export default function ProductionIndexPage() {
  const [accounts, setAccounts] = useState<AccountRow[]>([]);
  const [tasks, setTasks] = useState<Task[]>([]);
  const [ic, setIc] = useState<{ indexed: number; sources: number } | null>(null);

  const load = useCallback(async () => {
    const [accountData, taskData, health] = await Promise.all([
      api.accounts({ limit: 500 }),
      api.tasks(80),
      fetch("/api/apps/icmail/health", { credentials: "same-origin" }).then((r) => r.json()).catch(() => null),
    ]);
    setAccounts(accountData.accounts);
    setTasks(taskData.tasks);
    if (health?.ok) setIc(health.data.stats);
  }, []);

  useEffect(() => {
    load();
    const timer = setInterval(load, 10000);
    return () => clearInterval(timer);
  }, [load]);

  const perPlatform = useMemo(() => {
    const map = new Map<string, { total: number; ok: number; bad: number; last?: Task }>();
    for (const row of accounts) {
      const key = row.platform || "other";
      const entry = map.get(key) || { total: 0, ok: 0, bad: 0 };
      entry.total += 1;
      if (row.status === "ok") entry.ok += 1;
      if (row.status === "banned" || row.status === "invalid") entry.bad += 1;
      map.set(key, entry);
    }
    return map;
  }, [accounts]);

  const totals = useMemo(
    () => ({
      ok: accounts.filter((row) => row.status === "ok").length,
      bad: accounts.filter((row) => row.status === "banned" || row.status === "invalid").length,
      running: tasks.filter((task) => task.status === "running" || task.status === "queued").length,
      failed: tasks.filter((task) => task.status === "failed").length,
    }),
    [accounts, tasks],
  );

  return (
    <div className="space-y-4">
      <div className="page-head">
        <div className="min-w-0 flex-1">
          <h1 className="page-title">生产</h1>
          <p className="page-desc">按平台进入流程：每个平台是一条独立的注册/授权链，产出统一进资产。</p>
        </div>
        <Link href="/tasks/" className="btn-ghost text-[11px]">任务中心</Link>
        <Link href="/accounts/" className="btn-ghost text-[11px]">账号资产</Link>
      </div>

      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Stat label="可用账号" value={totals.ok} />
        <Stat label="封禁/失效" value={totals.bad} tone={totals.bad > 0 ? "bad" : "neutral"} />
        <Stat label="运行 / 排队" value={totals.running} tone={totals.running > 0 ? "warn" : "neutral"} />
        <Stat label="IC 索引" value={ic?.indexed ?? 0} hint={`源 ${ic?.sources ?? 0}`} />
      </div>

      <div className="grid gap-3 sm:grid-cols-2">
        {PLATFORMS.map((platform) => {
          const stat = perPlatform.get(platform.slug) || { total: 0, ok: 0, bad: 0 };
          const last = tasks.find((task) =>
            (task.script || "").toLowerCase().includes(platform.slug) ||
            (task.title || "").toLowerCase().includes(platform.name.toLowerCase()),
          );
          return (
            <div key={platform.slug} className="card p-4">
              <div className="flex items-center gap-2.5">
                <span className={`grid h-7 w-7 place-items-center rounded-md ${platform.accent} text-[12px] font-semibold text-primary-foreground`}>
                  {platform.name.slice(0, 1)}
                </span>
                <div className="min-w-0 flex-1">
                  <div className="text-[13.5px] font-semibold">{platform.name}</div>
                  <div className="text-[11px] text-muted-foreground">{platform.note}</div>
                </div>
                <Link href={`/platform/${platform.slug}/`} className="btn-ghost px-2 py-1 text-[11px]">流程 →</Link>
              </div>
              <div className="mt-3 grid grid-cols-4 gap-2 border-t border-border/60 pt-2 text-center">
                {[
                  { k: "账号", v: stat.total },
                  { k: "可用", v: stat.ok },
                  { k: "异常", v: stat.bad },
                  { k: "任务", v: last ? last.status.slice(0, 4) : "-" },
                ].map((cell) => (
                  <div key={cell.k}>
                    <div className="font-mono text-[12.5px] text-foreground">{cell.v}</div>
                    <div className="text-[10px] uppercase tracking-[0.06em] text-muted-foreground">{cell.k}</div>
                  </div>
                ))}
              </div>
              <div className="mt-2 flex gap-1.5">
                <Link href={`/platform/${platform.slug}/tasks/`} className="btn-ghost flex-1 px-2 py-1 text-[11px]">任务</Link>
                <Link href={`/platform/${platform.slug}/accounts/`} className="btn-ghost flex-1 px-2 py-1 text-[11px]">账号</Link>
              </div>
            </div>
          );
        })}
      </div>

      <Card title="IC 邮箱（供给方）" bodyClass="p-0">
        <table className="tbl">
          <tbody>
            <tr>
              <td className="w-[150px] font-medium">IC 邮箱</td>
              <td className="text-[11.5px] text-muted-foreground">
                索引 {ic?.indexed ?? 0} 封 · {ic?.sources ?? 0} 个 IMAP 源；协议注册与测活从这里取码
              </td>
              <td className="w-[150px] text-right">
                <Link href="/apps/icmail/" className="btn-ghost px-2 py-1 text-[11px]">工作台 →</Link>
              </td>
            </tr>
          </tbody>
        </table>
      </Card>
    </div>
  );
}
