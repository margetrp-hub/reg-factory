"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { api, type AccountSummary, type Script, type TaskPage } from "@/lib/api";
import { EntryArrow, ErrorNote } from "@/components/ui";
import { TaskTable } from "@/components/task-table";
import { PLATFORM_TASKS, TASK_GROUPS, groupOf } from "@/lib/task-catalog";

export default function ProductionIndexPage() {
  const [accounts, setAccounts] = useState<AccountSummary[] | null>(null);
  const [scripts, setScripts] = useState<Script[]>([]);
  const [page, setPage] = useState<TaskPage | null>(null);
  const [error, setError] = useState("");
  const [schemaError, setSchemaError] = useState("");
  const [reload, setReload] = useState(0);
  useEffect(() => {
    const controller = new AbortController();
    api.scripts(controller.signal).then(data => {
      if (!controller.signal.aborted) { setScripts(data.scripts); setSchemaError(""); }
    }).catch(err => { if (!controller.signal.aborted) setSchemaError(`模式目录读取失败：${err.message}`); });
    return () => controller.abort();
  }, [reload]);
  useEffect(() => {
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    const load = async () => {
      try {
        const [summary, tasks] = await Promise.all([api.accountSummary("", controller.signal), api.tasks(6, {}, controller.signal)]);
        if (controller.signal.aborted) return;
        setAccounts(summary.platforms); setPage(tasks); setError("");
      } catch (err) { if (!controller.signal.aborted) setError(`读取失败，已保留上次结果。${(err as Error).message}`); }
      finally { if (!controller.signal.aborted) timer = setTimeout(load, document.hidden ? 30000 : 15000); }
    };
    void load();
    return () => { controller.abort(); clearTimeout(timer); };
  }, [reload]);
  const total = accounts?.reduce((sum, item) => sum + item.total, 0);
  const available = accounts?.reduce((sum, item) => sum + item.available, 0);
  return <div className="sg-workspace">
    <header className="sg-page-head"><div><p className="sg-eyebrow">生产工作台</p><h1>生产中心</h1><p className="sg-description">先选业务，再选执行模式。连接与供给共用配置。</p></div><div className="flex flex-wrap gap-2"><button className="btn-ghost" onClick={() => setReload(value => value + 1)}>刷新</button><Link className="btn-ghost" href="/settings/">配置中心</Link><Link className="btn-ghost" href="/tasks/">运行与日志</Link></div></header>
    <ErrorNote message={error || schemaError} />
    <div className="flex flex-wrap items-center gap-x-6 gap-y-2 border-b border-border px-1 pb-4 text-sm text-muted-foreground">
      <span>账号 <strong className="ml-2 text-2xl font-medium tabular-nums text-foreground">{total ?? "—"}</strong></span>
      <span>可用 <strong className="ml-2 text-2xl font-medium tabular-nums text-success">{available ?? "—"}</strong></span>
      <span>运行中 <strong className="ml-2 text-2xl font-medium tabular-nums text-foreground">{page ? (page.stats.running || 0) + (page.stats.queued || 0) : "—"}</strong></span>
      <Link className="ml-auto underline underline-offset-4" href="/apps/icmail/">统一邮箱池</Link>
    </div>
    <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3" aria-label="业务流程">
      {TASK_GROUPS.filter(group => group.slug !== "other" || scripts.some(script => groupOf(script).slug === "other")).map(group => <Link className="sg-mode-card" key={group.slug} href={`/tasks/group/${group.slug}/`}>
        <span className="sg-mode-title">{group.title}<EntryArrow /></span><span className="sg-description">{group.desc}</span><span className="text-xs text-muted-foreground">{scripts.filter(script => groupOf(script).slug === group.slug).length || "—"} 种执行模式</span>
      </Link>)}
      <Link className="sg-mode-card" href="/accounts/"><span className="sg-mode-title">凭据维护<EntryArrow /></span><span className="sg-description">AT 测试、RT 刷新、登录授权与修复，从原账号发起。</span><span className="text-xs text-muted-foreground">密码＋2FA协议授权 · 浏览器与邮箱授权</span></Link>
    </div>
    <section className="sg-task-surface"><header className="sg-task-toolbar"><h2 className="text-sm font-semibold">平台账号</h2><span className="sg-description">按平台查看原账号、流程和处理记录</span></header><div className="grid gap-2 px-5 pb-5 sm:grid-cols-3 xl:grid-cols-6">{Object.entries(PLATFORM_TASKS).map(([slug, platform]) => {
      const summary = accounts?.find(item => item.platform === slug);
      return <Link key={slug} className="rounded-xl border border-border px-3 py-3 transition-colors hover:bg-accent" href={`/platform/${slug}/`}><span className="text-sm font-medium">{platform.name}</span><p className="mt-2 text-xs text-muted-foreground">可用 <span className="tabular-nums text-success">{accounts ? summary?.available || 0 : "—"}</span> / {accounts ? summary?.total || 0 : "—"}</p></Link>;
    })}</div></section>
    <section className="sg-task-surface"><header className="sg-task-toolbar"><h2 className="text-sm font-semibold">最近运行</h2><Link className="btn-ghost ml-auto" href="/tasks/">全部运行与日志</Link></header><div className="overflow-auto"><TaskTable tasks={page?.tasks || []} /></div></section>
  </div>;
}
