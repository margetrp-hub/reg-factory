"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import Link from "next/link";
import { api, type Script, type TaskPage } from "@/lib/api";
import { Empty, EntryArrow, ErrorNote } from "@/components/ui";
import { TaskForm } from "@/components/task-form";
import { TaskTable } from "@/components/task-table";
import { LogView } from "@/components/log-view";
import { Drawer } from "@/components/drawer";
import { groupBySlug, groupOf, PLATFORM_TASKS, scriptsForPlatform, EXTERNAL_TOOLS } from "@/lib/task-catalog";

export function TaskGroupView({ group: slug = "other", platform }: { group?: string; platform?: string }) {
  const platformInfo = platform ? PLATFORM_TASKS[platform] : undefined;
  const group = platformInfo ? { title: `${platformInfo.name} 任务`, desc: "选择本次执行模式；参数、原始结果和恢复操作保持在同一任务中。" }
    : groupBySlug(slug) || { slug: "other", title: "其他任务", desc: "已配置的其他执行流程。" };
  const [scripts, setScripts] = useState<Script[]>([]);
  const [page, setPage] = useState<TaskPage | null>(null);
  const [mode, setMode] = useState<Script | null>(null);
  const [open, setOpen] = useState(false);
  const [visited, setVisited] = useState<Script[]>([]);
  const [activeRun, setActiveRun] = useState("");
  const [offset, setOffset] = useState(0);
  const [status, setStatus] = useState("");
  const [error, setError] = useState("");
  const [actionError, setActionError] = useState("");
  const [refresh, setRefresh] = useState(0);
  const [schemaReady, setSchemaReady] = useState(false);
  const [schemaReload, setSchemaReload] = useState(0);
  const [schemaError, setSchemaError] = useState("");
  const [k12URL, setK12URL] = useState("");
  const [toolError, setToolError] = useState("");
  const preselected = useRef(false);
  const ids = useMemo(() => scripts.map(script => script.id), [scripts]);

  useEffect(() => {
    if (slug !== "helper" || platform) return;
    const controller = new AbortController();
    api.config(controller.signal).then(data=>{
      const saved=Object.values(data.groups).flat().find(item=>item.key === "K12_CONSOLE_URL")?.value || "";
      if (!saved) return;
      try { const url=new URL(saved); if(["https:","http:"].includes(url.protocol) && !url.username && !url.password) setK12URL(url.href); else setToolError("K12 访问地址需要使用 HTTP(S)，请在执行配置中调整。"); }
      catch { setToolError("K12 访问地址无效，请在执行配置中调整。"); }
    }).catch(error=>{if(!controller.signal.aborted)setToolError(error.message);});
    return ()=>controller.abort();
  }, [slug,platform,schemaReload]);

  useEffect(() => {
    let active = true;
    const controller = new AbortController();
    api.scripts(controller.signal).then(data => {
      if (!active) return;
      const mine = platform ? scriptsForPlatform(data.scripts,platform) : data.scripts.filter(script => groupOf(script).slug === slug);
      setScripts(mine);
      setSchemaReady(true);
      setSchemaError("");
      const selected = mine.find(script => script.id === new URLSearchParams(window.location.search).get("script"));
      if (selected && !preselected.current) { setMode(selected); setVisited([selected]); setOpen(true); }
      preselected.current = true;
    }).catch(err => { if (active) setSchemaError(`模式读取失败：${err.message}`); });
    return () => { active = false; controller.abort(); };
  }, [slug, platform, schemaReload]);

  useEffect(() => {
    if (!schemaReady) return;
    if (!ids.length) { setPage({ tasks: [], count: 0, total: 0, stats: {}, limit: 25, offset: 0 }); return; }
    let active = true;
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    const load = async () => {
      try {
        const data = await api.tasks(25, { scripts: ids, offset, status }, controller.signal);
        if (active) {
          setPage(data); setError("");
          if (offset > 0 && data.total <= offset) setOffset(Math.max(0, Math.ceil(data.total / 25) - 1) * 25);
        }
      } catch (err) { if (active) setError(`执行记录读取失败，已有结果已保留。${(err as Error).message}`); }
      finally { if (active) timer = setTimeout(load, document.hidden ? 30000 : 10000); }
    };
    void load();
    return () => { active = false; clearTimeout(timer); controller.abort(); };
  }, [schemaReady, ids, offset, status, refresh]);

  return <div className="sg-workspace">
    <header className="sg-page-head">
      <div><p className="sg-eyebrow">发起任务</p><h1>{group.title}</h1><p className="sg-description">{group.desc}</p></div>
      <div className="flex gap-2"><button className="btn-ghost" onClick={() => setRefresh(value => value + 1)}>刷新</button><Link className="btn-ghost" href={platform ? `/platform/${platform}/` : "/tasks/"}>{platform ? "返回平台概览" : "返回任务中心"}</Link></div>
    </header>
    <ErrorNote message={error} />
    <ErrorNote message={actionError} />
    {schemaError && <div><ErrorNote message={schemaError} /><button className="btn-ghost mt-2" onClick={() => setSchemaReload(value => value + 1)}>重试读取模式</button></div>}
    <div className="sg-mode-grid">
      {scripts.map(script => <button key={script.id} className="sg-mode-card" onClick={() => {
        setVisited(previous => previous.some(item => item.id === script.id) ? previous : [...previous, script]);
        setMode(script); setOpen(true);
      }}>
        <span className="sg-mode-title">{script.title}<EntryArrow /></span>
        <span className="sg-description">{script.desc || "设置本次执行参数并查看原任务结果"}</span>
        <span className="text-[12px] text-muted-foreground">{script.platform || "通用"} · {script.args?.length || 0} 项设置</span>
      </button>)}
      {!scripts.length && <Empty>{schemaReady ? "该分类暂无可用模式" : "正在读取执行模式…"}</Empty>}
    </div>
    {slug === "helper" && !platform && <section className="sg-task-surface"><header className="sg-task-toolbar"><h2 className="text-sm font-semibold">配套工具</h2></header><div className="flex flex-wrap gap-3 px-5 pb-5">{EXTERNAL_TOOLS.map(tool=><a className="btn-ghost" key={tool.id} href={tool.url} target="_blank" rel="noreferrer">{tool.title}<EntryArrow /></a>)}</div></section>}
    {slug === "helper" && !platform && <section className="sg-task-surface"><header className="sg-task-toolbar"><h2 className="text-sm font-semibold">Codex K12</h2>{k12URL && <a className="btn-primary ml-auto" href={k12URL} target="_blank" rel="noreferrer">打开独立控制台</a>}<Link className="btn-ghost" href="/settings/?q=K12">配置访问地址</Link></header><div className="px-5 pb-5"><p className="sg-description">保留 Workspace、邮箱池、协议重登、2FA、格式转换、卡密兑换、备份恢复与任务重试。K12 使用自己的进程和配置；此处登记入口不会替它启动或修改任务。</p>{!k12URL && <p className="sg-description mt-2">尚未填写访问地址。部署完成后在执行配置中填写 K12_CONSOLE_URL。</p>}<ErrorNote message={toolError}/></div></section>}
    <section className="sg-task-surface" aria-label="本分类记录">
      <header className="sg-task-toolbar"><h2 className="text-sm font-semibold">执行记录</h2>
        <select aria-label="记录状态" className="field ml-auto max-w-40" value={status} onChange={event => { setStatus(event.target.value); setOffset(0); }}>
          <option value="">全部状态</option><option value="running">运行中</option><option value="queued">排队</option><option value="succeeded">成功</option><option value="completed">已完成</option><option value="failed">失败</option><option value="unknown">待核对</option><option value="waiting_user">待人工</option><option value="stopped">已停止</option><option value="interrupted">中断</option>
        </select>
      </header>
      <div className="overflow-auto"><TaskTable tasks={page?.tasks || []} activeId={activeRun} onSelect={setActiveRun}
        onStop={async id => { setActionError(""); try { await api.stopTask(id); setRefresh(value => value + 1); } catch (err) { setActionError(`停止未确认，请查看原任务：${(err as Error).message}`); } }} /></div>
      <footer className="sg-task-footer"><span>共 {page?.total ?? "—"} 次执行</span><div className="flex gap-2">
        <button className="btn-ghost" disabled={!page || offset === 0} onClick={() => setOffset(value => Math.max(0, value - 25))}>上一页</button>
        <button className="btn-ghost" disabled={!page || offset + 25 >= page.total} onClick={() => setOffset(value => value + 25)}>下一页</button>
      </div></footer>
    </section>
    {activeRun && <section className="sg-task-surface sg-enter"><header className="sg-task-toolbar"><h2 className="text-sm font-semibold">原任务日志</h2><Link className="btn-ghost ml-auto" href={`/tasks/?task=${encodeURIComponent(activeRun)}`}>查看任务详情</Link></header><div className="p-4"><LogView key={activeRun} taskId={activeRun} /></div></section>}
    <Drawer open={open} title={mode?.title || "执行参数"} subtitle="关闭或切换模式会保留本页输入。受理后自动运行，结果留在原任务。" onClose={() => setOpen(false)} width={560}>
      {visited.map(script => <div key={script.id} hidden={mode?.id !== script.id}>
        <TaskForm script={script} onStarted={id => {
          setActiveRun(id); if (mode?.id === script.id) setOpen(false); setOffset(0); setStatus(""); setRefresh(value => value + 1);
        }} />
      </div>)}
    </Drawer>
  </div>;
}
