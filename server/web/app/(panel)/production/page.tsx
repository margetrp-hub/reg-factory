"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { api, type AccountSummary, type TaskPage } from "@/lib/api";
import { EntryArrow, ErrorNote, StatusBadge } from "@/components/ui";
import { PLATFORM_TASKS, scriptsForPlatform, TASK_GROUPS } from "@/lib/task-catalog";

export default function ProductionIndexPage() {
  const [accounts, setAccounts] = useState<AccountSummary[] | null>(null);
  const [tasks, setTasks] = useState<Record<string,TaskPage>>({});
  const [error, setError] = useState("");
  const [reload, setReload] = useState(0);
  useEffect(() => {
    let active = true;
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    const load = async () => {
      try {
        const catalog = await api.scripts(controller.signal);
        const [summary, ...pages] = await Promise.all([
          api.accountSummary("",controller.signal),
          ...Object.keys(PLATFORM_TASKS).map(slug=>{
            const ids=scriptsForPlatform(catalog.scripts,slug).map(script=>script.id);
            return ids.length ? api.tasks(1,{scripts:ids},controller.signal) : Promise.resolve({tasks:[],stats:{},count:0,total:0,limit:1,offset:0} as TaskPage);
          }),
        ]);
        if (!active) return;
        setAccounts(summary.platforms);
        setTasks(Object.fromEntries(Object.keys(PLATFORM_TASKS).map((slug,index)=>[slug,pages[index]])));
        setError("");
      } catch (err) { if (active) setError(`生产概览读取失败，已保留上次结果。${(err as Error).message}`); }
      finally { if (active) timer = setTimeout(load,document.hidden ? 30000 : 15000); }
    };
    void load();
    return () => { active=false; controller.abort(); clearTimeout(timer); };
  },[reload]);
  const total = accounts?.reduce((sum,item)=>sum+item.total,0);
  const abnormal = accounts?.reduce((sum,item)=>sum+item.abnormal,0);
  return <div className="sg-workspace">
    <header className="sg-page-head"><div><p className="sg-eyebrow">生产工作台</p><h1>按平台执行</h1><p className="sg-description">进入对应平台，选择执行方式、核对任务结果与账号状态。</p></div><div className="flex gap-2"><button className="btn-ghost" onClick={()=>setReload(value=>value+1)}>刷新</button><Link className="btn-ghost" href="/tasks/">全部任务</Link></div></header>
    <ErrorNote message={error} />
    <div className="flex flex-wrap items-center gap-x-6 gap-y-2 border-b border-border px-1 pb-4 text-[13px] text-muted-foreground">
      <span>已索引账号 <strong className="ml-2 font-medium tabular-nums text-foreground">{total ?? "—"}</strong></span>
      <span>异常账号 <strong className="ml-2 font-medium tabular-nums text-destructive">{abnormal ?? "—"}</strong></span>
      <Link className="ml-auto underline underline-offset-4" href="/settings/">执行配置</Link>
      <Link className="underline underline-offset-4" href="/apps/icmail/">邮箱供给</Link>
    </div>
    <div className="grid gap-4 sm:grid-cols-2">
      {Object.entries(PLATFORM_TASKS).map(([slug,platform])=>{
        const summary=accounts?.find(item=>item.platform===slug);
        const page=tasks[slug], last=page?.tasks[0];
        const running=page ? (page.stats.running || 0)+(page.stats.queued || 0) : null;
        return <section key={slug} className="sg-task-surface">
          <header className="sg-task-toolbar"><div className="min-w-0 flex-1"><h2 className="text-lg font-semibold tracking-tight">{platform.name}</h2><p className="sg-description mt-1">注册 · 授权 · 状态核验</p></div><Link aria-label={`进入 ${platform.name} 流程`} className="btn-ghost" href={`/platform/${slug}/`}>查看流程<EntryArrow /></Link></header>
          <div className="flex flex-wrap items-baseline gap-4 px-5 pb-5"><span className="text-3xl font-semibold tracking-tight tabular-nums">{accounts ? summary?.available || 0 : "—"}</span><span className="sg-description">正常账号</span><span className="sg-description ml-auto">总数 {accounts ? summary?.total || 0 : "—"} · 异常 {accounts ? summary?.abnormal || 0 : "—"}</span></div>
          <div className="flex flex-wrap items-center gap-3 border-y border-border bg-muted/30 px-5 py-3 text-[13px]"><span className="text-muted-foreground">最近执行</span>{last ? <><StatusBadge status={last.status}/><Link className="min-w-0 flex-1 truncate underline-offset-4 hover:underline" href={`/tasks/?task=${encodeURIComponent(last.id)}`}>{last.title}</Link></> : <span>{page ? "暂无执行" : "读取中…"}</span>}<span className="ml-auto tabular-nums text-muted-foreground">进行中 {running ?? "—"}</span></div>
          <footer className="flex gap-2 p-4"><Link className="btn-primary" href={`/platform/${slug}/tasks/`}>发起任务</Link><Link className="btn-ghost" href={`/platform/${slug}/accounts/`}>账号记录</Link></footer>
        </section>;
      })}
    </div>
    <section className="sg-task-surface"><header className="sg-task-toolbar"><h2 className="text-sm font-semibold">跨平台能力</h2><span className="sg-description">组合流程、授权、导出和支撑服务保留独立入口</span></header><div className="flex flex-wrap gap-2 px-5 pb-5">{TASK_GROUPS.map(group=><Link className="btn-ghost" key={group.slug} href={`/tasks/group/${group.slug}/`}>{group.title}<EntryArrow /></Link>)}</div></section>
  </div>;
}
