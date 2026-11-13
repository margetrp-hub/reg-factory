"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { api, type AccountRow, type AccountSummary, type TaskPage } from "@/lib/api";
import { Badge, Card, Empty, ErrorNote } from "@/components/ui";
import { TaskTable } from "@/components/task-table";
import { PLATFORM_TASKS, scriptsForPlatform } from "@/lib/task-catalog";
import { AccountSummaryStrip } from "@/components/account-summary-strip";

export function PlatformView({ slug }: { slug: string }) {
  const spec = PLATFORM_TASKS[slug];
  const [accounts, setAccounts] = useState<AccountRow[]>([]);
  const [taskPage, setTaskPage] = useState<TaskPage | null>(null);
  const [summary, setSummary] = useState<AccountSummary | null>(null);
  const [error, setError] = useState("");
  const [reload, setReload] = useState(0);

  const load = useCallback(async (signal: AbortSignal) => {
    try {
      const catalog = await api.scripts(signal);
      const ids = scriptsForPlatform(catalog.scripts,spec.slug).map(script=>script.id);
      const [accountData, taskData, accountTotals] = await Promise.all([
        api.accounts({ platform: spec.slug, limit: 15 },signal),
        ids.length ? api.tasks(12,{scripts:ids},signal) : Promise.resolve({tasks:[],stats:{},count:0,total:0,limit:12,offset:0} as TaskPage),
        api.accountSummary(spec.slug,signal),
      ]);
      if (signal.aborted) return;
      setAccounts(accountData.accounts);
      setTaskPage(taskData);
      setSummary(accountTotals.platforms[0] || {platform:spec.slug,total:0,available:0,abnormal:0,unknown:0,used:0,leased:0,source_count:0});
      setError("");
    } catch (err) {
      if (!signal.aborted) setError(`平台读取失败，已保留上次结果。${(err as Error).message}`);
    }
  }, [spec.slug]);

  useEffect(() => {
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => { await load(controller.signal); if (!controller.signal.aborted) timer=setTimeout(poll,document.hidden?30000:15000); };
    void poll();
    return () => { controller.abort(); clearTimeout(timer); };
  }, [load,reload]);

  const stats = useMemo(() => {
    return {
      ok:summary?.available, bad:summary?.abnormal,
      running:taskPage ? (taskPage.stats.running || 0)+(taskPage.stats.queued || 0) : undefined,
      last:taskPage?.tasks[0],
    };
  }, [summary,taskPage]);

  return (
    <div className="sg-workspace">
      <div className="flex flex-wrap items-center gap-3">
        <span className="grid h-9 w-9 place-items-center rounded-md bg-primary text-[13px] font-semibold text-primary-foreground">
          {spec.name.slice(0, 1)}
        </span>
        <div className="min-w-0 flex-1">
          <h1 className="text-[16px] font-semibold leading-tight">{spec.name}</h1>
          <p className="text-[11px] leading-relaxed text-muted-foreground">{spec.summary}</p>
        </div>
        <div className="flex gap-2">
          <button className="btn-ghost" onClick={()=>setReload(value=>value+1)}>刷新</button>
          <Link href={`/platform/${spec.slug}/tasks/`} className="btn-ghost text-[11px]">任务</Link>
          <Link href={`/platform/${spec.slug}/accounts/`} className="btn-ghost text-[11px]">账号</Link>
          <Link href={`/platform/${spec.slug}/checks/`} className="btn-ghost text-[11px]">凭据检查</Link>
        </div>
      </div>

      <ErrorNote message={error}/>
      <AccountSummaryStrip summary={summary}/>

      <div className="grid gap-4 xl:grid-cols-2">
        <Card
          title="最近任务"
          bodyClass="p-0"
          actions={<Link href={`/platform/${spec.slug}/tasks/`} className="btn-ghost px-2 py-1 text-[11px]">全部</Link>}
        >
          <div className="max-h-[280px] overflow-auto">
            <TaskTable tasks={taskPage?.tasks || []} limit={12} showScript emptyText="还没有任务" />
          </div>
        </Card>

        <Card
          title={`${spec.name} 账号`}
          bodyClass="p-0"
          actions={<Link href={`/platform/${spec.slug}/accounts/`} className="btn-ghost px-2 py-1 text-[11px]">全部</Link>}
        >
          <div className="max-h-[280px] overflow-auto">
            {accounts.length === 0 ? (
              <div className="p-4"><Empty>暂无账号，可从任务页创建或导入</Empty></div>
            ) : (
              <table className="tbl">
                <thead>
                  <tr className="border-b border-border">
                    <th className="px-3 py-1.5 text-left font-medium">账号</th>
                    <th className="px-2 py-1.5 text-left font-medium">状态</th>
                    <th className="px-3 py-1.5 text-right font-medium">测活</th>
                  </tr>
                </thead>
                <tbody>
                  {accounts.slice(0, 15).map((row) => (
                    <tr key={row.ref} className="border-b border-border/60 last:border-0">
                      <td className="max-w-[220px] truncate px-3 py-1.5 font-mono text-[11px]" title={row.ref}>
                        {row.identity || row.ref}
                      </td>
                      <td className="px-2 py-1.5">
                        <Badge tone={row.status === "ok" ? "ok" : row.status === "unknown" ? "neutral" : "bad"}>{row.status}</Badge>
                      </td>
                      <td className="px-3 py-1.5 text-right text-[11px] text-muted-foreground">
                        {row.health_checked_at ? new Date(row.health_checked_at).toLocaleDateString() : "从未"}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        </Card>
      </div>
    </div>
  );
}
