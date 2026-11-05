"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { api, type Script, type Task } from "@/lib/api";
import { Badge, Card, Empty } from "@/components/ui";
import { TaskForm } from "@/components/task-form";
import { TaskTable } from "@/components/task-table";
import { LogView } from "@/components/log-view";
import { Tabs } from "@/components/tabs";
import { Drawer } from "@/components/drawer";
import { TASK_GROUPS, groupBySlug } from "@/lib/task-catalog";

export function TaskGroupView({ group: slug }: { group: string }) {
  const group = groupBySlug(slug) || groupBySlug("register")!;
  const [tab, setTab] = useState("cards");
  const [scripts, setScripts] = useState<Script[]>([]);
  const [selected, setSelected] = useState("");
  const [tasks, setTasks] = useState<Task[]>([]);
  const [activeRun, setActiveRun] = useState("");
  const [drawer, setDrawer] = useState<Script | null>(null);

  const load = useCallback(async () => {
    try {
      const [scriptData, taskData] = await Promise.all([api.scripts(), api.tasks(120)]);
      const ids = new Set(group.ids);
      const mine = scriptData.scripts.filter((item) => ids.has(item.id));
      setScripts(mine);
      setSelected((prev) => (mine.some((item) => item.id === prev) ? prev : mine[0]?.id || ""));
      setTasks(taskData.tasks.filter((task) => ids.has(task.script)));
    } catch {
      /* 下一次轮询再试 */
    }
  }, [group.ids]);

  useEffect(() => {
    const preselect = new URLSearchParams(window.location.search).get("script");
    if (preselect && group.ids.includes(preselect)) {
      setSelected(preselect);
      setTab("form");
    }
    load();
    const timer = setInterval(load, 5000);
    return () => clearInterval(timer);
  }, [load, group.ids]);

  const active = scripts.find((item) => item.id === selected);
  const selectedTask = tasks.find((task) => task.id === activeRun);
  const stats = useMemo(
    () => ({
      running: tasks.filter((task) => task.status === "running" || task.status === "queued").length,
      succeeded: tasks.filter((task) => task.status === "succeeded").length,
      failed: tasks.filter((task) => task.status === "failed").length,
    }),
    [tasks],
  );

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-3">
        <Link href="/tasks/" className="btn-ghost px-2 py-1 text-[11px]">← 任务中心</Link>
        <div className="min-w-0 flex-1">
          <h1 className="text-[16px] font-semibold leading-tight">{group.title}</h1>
          <p className="text-[11px] text-muted-foreground">{group.desc}</p>
        </div>
        <Badge tone={stats.running > 0 ? "warn" : "info"}>
          运行 {stats.running} · 成功 {stats.succeeded} · 失败 {stats.failed}
        </Badge>
      </div>

      <Tabs
        items={[
          { id: "cards", label: "任务", count: scripts.length },
          { id: "form", label: active ? `下单 · ${active.title}` : "下单" },
          { id: "queue", label: "队列", count: tasks.length },
          { id: "log", label: "日志" },
        ]}
        value={tab}
        onChange={setTab}
        right={
          <div className="flex gap-1.5">
            {TASK_GROUPS.filter((item) => item.slug !== group.slug).map((item) => (
              <Link key={item.slug} href={`/tasks/group/${item.slug}/`} className="btn-ghost px-2 py-1 text-[11px]">
                {item.title}
              </Link>
            ))}
          </div>
        }
      />

      {tab === "cards" && (
        <div className="grid gap-2 sm:grid-cols-2 xl:grid-cols-3">
          {scripts.map((script) => (
            <button
              key={script.id}
              onClick={() => {
                setSelected(script.id);
                setDrawer(script);
              }}
              className="rounded-[var(--radius)] border border-border bg-card p-3 text-left transition-colors hover:border-brand/40 hover:bg-accent/40"
            >
              <div className="flex items-center gap-2">
                <span className="min-w-0 flex-1 truncate text-[13px] font-medium">{script.title}</span>
                {script.platform && <Badge tone="info">{script.platform}</Badge>}
              </div>
              <div className="mt-1 line-clamp-2 text-[11px] leading-snug text-muted-foreground">
                {script.desc || script.file}
              </div>
            </button>
          ))}
          {scripts.length === 0 && <Empty>该分类暂无任务模板</Empty>}
        </div>
      )}

      {tab === "form" && (
        <Card
          title={active ? `下单 · ${active.title}` : "下单"}
          actions={
            <select className="field w-[220px] py-1 text-[12px]" value={selected} onChange={(e) => setSelected(e.target.value)}>
              {scripts.map((item) => (
                <option key={item.id} value={item.id}>{item.title}</option>
              ))}
            </select>
          }
        >
          {active ? (
            <TaskForm script={active} onStarted={(id) => { setActiveRun(id); setTab("log"); load(); }} />
          ) : (
            <Empty>先在「任务」标签选一个任务</Empty>
          )}
        </Card>
      )}

      {tab === "queue" && (
        <Card title="本分类任务" bodyClass="p-0">
          <div className="max-h-[600px] overflow-auto">
            <TaskTable
              tasks={tasks}
              activeId={activeRun}
              onSelect={(id) => { setActiveRun(id); setTab("log"); }}
              onStop={async (id) => { await api.stopTask(id).catch(() => null); load(); }}
              emptyText="该分类还没有任务"
            />
          </div>
        </Card>
      )}

      <Drawer
        open={!!drawer}
        title={drawer ? `下单 · ${drawer.title}` : ""}
        subtitle={drawer?.desc || drawer?.file}
        onClose={() => setDrawer(null)}
        width={520}
      >
        {drawer && (
          <TaskForm
            script={drawer}
            onStarted={(id) => {
              setActiveRun(id);
              setDrawer(null);
              setTab("log");
              load();
            }}
          />
        )}
      </Drawer>

      {tab === "log" && (
        <Card
          title={selectedTask ? `运行日志 · ${selectedTask.title}` : "运行日志"}
          bodyClass="p-3"
          actions={
            selectedTask ? (
              <span className="font-mono text-[11px] text-muted-foreground">exit {selectedTask.exit_code}</span>
            ) : null
          }
        >
          {activeRun ? <LogView taskId={activeRun} /> : <Empty>在「队列」里点一个任务查看日志</Empty>}
        </Card>
      )}
    </div>
  );
}
