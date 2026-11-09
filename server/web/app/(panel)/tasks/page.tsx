"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { api, type Task, type TaskPage } from "@/lib/api";
import { EntryArrow, ErrorNote, StatusBadge } from "@/components/ui";
import { TASK_GROUPS } from "@/lib/task-catalog";
import { TaskTable } from "@/components/task-table";
import { LogView } from "@/components/log-view";

const FILTERS = [
  ["", "全部"], ["running", "运行中"], ["queued", "排队"], ["waiting_user", "待人工"],
  ["unknown", "待核对"], ["failed", "失败"], ["succeeded", "成功"], ["completed", "已完成"],
  ["stopped", "已停止"], ["interrupted", "中断"], ["cancelled", "已取消"],
];
const PAGE_SIZE = 25;

export default function TaskCenterPage() {
  const [page, setPage] = useState<TaskPage | null>(null);
  const [filter, setFilter] = useState({ status: "", q: "", offset: 0 });
  const [search, setSearch] = useState("");
  const [selected, setSelected] = useState<Task | null>(null);
  const [selectedID, setSelectedID] = useState("");
  const [allLogs, setAllLogs] = useState(false);
  const [error, setError] = useState("");
  const [actionError, setActionError] = useState("");
  const [detailError, setDetailError] = useState("");
  const [refreshing, setRefreshing] = useState(false);
  const [reload, setReload] = useState(0);
  const [ready, setReady] = useState(false);
  const [launchers, setLaunchers] = useState(false);

  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    const status = params.get("status") || "";
    const q = (params.get("q") || "").slice(0, 128);
    const offset = Number(params.get("offset"));
    setFilter({ status: FILTERS.some(([key]) => key === status) ? status : "", q,
      offset: Number.isInteger(offset) && offset >= 0 && offset <= 10000000 ? offset : 0 });
    setSearch(q);
    setSelectedID(params.get("task") || "");
    setReady(true);
  }, []);

  useEffect(() => {
    if (!ready) return;
    const params = new URLSearchParams();
    if (filter.status) params.set("status", filter.status);
    if (filter.q) params.set("q", filter.q);
    if (filter.offset) params.set("offset", String(filter.offset));
    if (selectedID) params.set("task", selectedID);
    window.history.replaceState(null, "", `${window.location.pathname}${params.size ? `?${params}` : ""}`);
  }, [ready, filter, selectedID]);

  useEffect(() => {
    if (!ready) return;
    let active = true;
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    const load = async () => {
      if (active) setRefreshing(true);
      try {
        const data = await api.tasks(PAGE_SIZE, filter, controller.signal);
        if (!active) return;
        setPage(data);
        setError("");
        if (filter.offset > 0 && data.total <= filter.offset) {
          setFilter(previous => ({ ...previous, offset: Math.max(0, Math.ceil(data.total / PAGE_SIZE) - 1) * PAGE_SIZE }));
        }
      } catch (err) {
        if (active) setError(`任务读取失败，已保留上次结果。${(err as Error).message}`);
      } finally {
        if (active) {
          setRefreshing(false);
          timer = setTimeout(load, document.hidden ? 30000 : 10000);
        }
      }
    };
    void load();
    return () => { active = false; clearTimeout(timer); controller.abort(); };
  }, [ready, filter, reload]);

  useEffect(() => {
    if (!selectedID) return;
    let active = true;
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    const load = async () => {
      try {
        const task = await api.task(selectedID, controller.signal);
        if (active) { setSelected(task); setDetailError(""); }
      } catch (err) {
        if (active) setDetailError(`原任务暂时不可读：${(err as Error).message}`);
      } finally {
        if (active) timer = setTimeout(load, document.hidden ? 30000 : 10000);
      }
    };
    void load();
    return () => { active = false; clearTimeout(timer); controller.abort(); };
  }, [selectedID, reload]);

  const total = Object.values(page?.stats || {}).reduce((sum, count) => sum + count, 0);
  const select = (id: string) => {
    setDetailError("");
    setSelectedID(id);
    setSelected(page?.tasks.find(task => task.id === id) || null);
    setAllLogs(false);
  };

  return (
    <div className="sg-workspace">
      <header className="sg-page-head">
        <div><p className="sg-eyebrow">执行工作台</p><h1>任务中心</h1>
          <p className="sg-description">查看每次执行、原始结果与下一步处理。</p></div>
        <div className="flex flex-wrap items-center gap-2">
          <button className="btn-ghost" onClick={() => setAllLogs(value => !value)}>{allLogs ? "返回任务日志" : "系统日志"}</button>
          <button className="btn-ghost" disabled={refreshing} onClick={() => setReload(value => value + 1)}>{refreshing ? "刷新中…" : "刷新"}</button>
          <button className="btn-primary" aria-expanded={launchers} onClick={() => setLaunchers(value => !value)}>发起任务</button>
        </div>
      </header>
      <ErrorNote message={actionError} />

      {launchers && <nav aria-label="任务类型" className="sg-mode-grid sg-enter">
        {TASK_GROUPS.map(group => <Link key={group.slug} className="sg-mode-card" href={`/tasks/group/${group.slug}/`}>
          <span className="sg-mode-title">{group.title}<EntryArrow /></span>
          <span className="sg-description">{group.desc}</span>
        </Link>)}
      </nav>}

      <section className="sg-task-surface" aria-label="任务记录">
        <div className="sg-task-toolbar">
          <form onSubmit={event => { event.preventDefault(); setFilter(previous => ({ ...previous, q: search.trim(), offset: 0 })); }} className="flex min-w-0 flex-1 gap-2">
            <input aria-label="搜索任务" className="field max-w-sm" placeholder="搜索任务名称或编号" maxLength={128} value={search} onChange={event => setSearch(event.target.value)} />
            <button className="btn-ghost" type="submit">搜索</button>
          </form>
          <span className="sg-description tabular-nums">{page ? `共 ${total} 条记录` : "正在读取…"}</span>
        </div>
        <nav aria-label="任务状态筛选" className="sg-status-tabs">
          {FILTERS.filter(([status]) => !status || (page?.stats[status] || 0) > 0 || filter.status === status).map(([status, label]) => (
            <button key={status} aria-pressed={filter.status === status} onClick={() => setFilter(previous => ({ ...previous, status, offset: 0 }))}>
              {label}<span className="tabular-nums">{page ? status ? page.stats[status] || 0 : total : "—"}</span>
            </button>
          ))}
        </nav>
        {error && <div className="px-5 py-3"><ErrorNote message={error} /><button className="btn-ghost mt-2" onClick={() => setReload(value => value + 1)}>重试读取</button></div>}
        <div className="overflow-x-auto" aria-busy={refreshing}>
          {page ? <TaskTable tasks={page.tasks} activeId={selectedID} onSelect={select} emptyText="没有匹配的任务，调整筛选后再试" onStop={async id => {
            setActionError(""); try { await api.stopTask(id); setReload(value => value + 1); } catch (err) { setActionError(`停止未确认，请核对原任务：${(err as Error).message}`); }
          }} /> : <div className="sg-table-skeleton" role="status">正在读取任务记录…</div>}
        </div>
        <footer className="sg-task-footer">
          <span>{page ? `${page.total ? filter.offset + 1 : 0}–${Math.min(filter.offset + page.count, page.total)} / ${page.total}` : "—"}</span>
          <div className="flex gap-2">
            <button className="btn-ghost" disabled={!page || filter.offset === 0 || refreshing} onClick={() => setFilter(previous => ({ ...previous, offset: Math.max(0, previous.offset - PAGE_SIZE) }))}>上一页</button>
            <button className="btn-ghost" disabled={!page || filter.offset + PAGE_SIZE >= page.total || refreshing} onClick={() => setFilter(previous => ({ ...previous, offset: previous.offset + PAGE_SIZE }))}>下一页</button>
          </div>
        </footer>
      </section>

      {(allLogs || selectedID) && <section className="sg-task-surface sg-enter" aria-label="执行详情">
        <header className="sg-task-toolbar">
          <div className="min-w-0"><h2 className="text-sm font-semibold break-words">{allLogs ? "系统与全部任务日志" : selected?.title || "原任务详情"}</h2>
            {!allLogs && <p className="mt-1 break-all text-[12px] text-muted-foreground">{selectedID}</p>}</div>
          {!allLogs && selected && <StatusBadge status={selected.status} />}
          <button className="btn-ghost ml-auto" onClick={() => { setSelectedID(""); setSelected(null); setAllLogs(false); }}>关闭详情</button>
        </header>
        {!allLogs && <div className="px-5 pt-3"><ErrorNote message={detailError} />
          {selected?.error && <ErrorNote message={selected.error} />}
          {selected?.id.startsWith("work-") && ["failed", "unknown", "waiting_user"].includes(selected.status) && <p className="sg-description py-2">在主系统的原任务中处理；明确失败可重新发起，结果待核对时先读取原结果。</p>}
        </div>}
        <div className="p-3 sm:p-5"><LogView key={allLogs ? "system" : selectedID} taskId={allLogs ? "" : selectedID} height={380} /></div>
      </section>}
    </div>
  );
}
