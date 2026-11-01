"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { api, type Script, type Task } from "@/lib/api";
import { Badge, Card, Empty, StatusBadge, timeAgo } from "@/components/ui";
import { TaskForm } from "@/components/task-form";
import { LogView } from "@/components/log-view";

export default function TasksPage() {
  const [scripts, setScripts] = useState<Script[]>([]);
  const [activeId, setActiveId] = useState("");
  const [tasks, setTasks] = useState<Task[]>([]);
  const [selected, setSelected] = useState("");
  const [filter, setFilter] = useState("");
  const [error, setError] = useState("");

  const loadTasks = useCallback(async () => {
    try {
      const data = await api.tasks(120);
      setTasks(data.tasks);
    } catch (err) {
      setError((err as Error).message);
    }
  }, []);

  useEffect(() => {
    api
      .scripts()
      .then((data) => {
        setScripts(data.scripts);
        if (data.scripts.length) setActiveId((prev) => prev || data.scripts[0].id);
      })
      .catch((err) => setError((err as Error).message));
    loadTasks();
    const timer = setInterval(loadTasks, 5000);
    return () => clearInterval(timer);
  }, [loadTasks]);

  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    const id = params.get("id");
    if (id) setSelected(id);
  }, []);

  const groups = useMemo(() => {
    const map = new Map<string, Script[]>();
    for (const script of scripts) {
      const key = script.category || "其他";
      map.set(key, [...(map.get(key) || []), script]);
    }
    return Array.from(map.entries());
  }, [scripts]);

  const active = scripts.find((item) => item.id === activeId);
  const visible = filter ? tasks.filter((task) => task.status === filter) : tasks;
  const selectedTask = tasks.find((task) => task.id === selected);

  return (
    <div className="grid gap-4 xl:grid-cols-[220px_1fr_1.15fr]">
      <Card title="任务库" bodyClass="p-2" className="h-fit xl:sticky xl:top-[56px]">
        <div className="max-h-[calc(100vh-160px)] space-y-3 overflow-auto pr-1">
          {groups.map(([category, list]) => (
            <div key={category}>
              <div className="px-2 pb-1 text-[10px] uppercase tracking-[0.12em] text-muted-foreground">{category}</div>
              {list.map((script) => (
                <button
                  key={script.id}
                  onClick={() => setActiveId(script.id)}
                  className={`w-full rounded-lg px-2 py-1.5 text-left text-[12.5px] transition-colors ${
                    activeId === script.id ? "bg-brand/20 text-primary-foreground" : "text-muted-foreground hover:bg-accent hover:text-foreground"
                  }`}
                >
                  {script.title || script.id}
                </button>
              ))}
            </div>
          ))}
          {scripts.length === 0 && <Empty>未加载到任务 schema</Empty>}
        </div>
      </Card>

      <Card title={active?.title || "选择任务"} className="h-fit">
        {active ? (
          <TaskForm script={active} onStarted={(id) => { setSelected(id); loadTasks(); }} />
        ) : (
          <Empty>左侧选择一个脚本</Empty>
        )}
      </Card>

      <div className="space-y-4">
        <Card
          title="任务队列"
          bodyClass="p-0"
          actions={
            <select className="field w-[130px] py-1 text-[12px]" value={filter} onChange={(e) => setFilter(e.target.value)}>
              <option value="">全部状态</option>
              <option value="queued">排队</option>
              <option value="running">运行中</option>
              <option value="succeeded">成功</option>
              <option value="failed">失败</option>
              <option value="stopped">已停止</option>
            </select>
          }
        >
          {error && <div className="px-4 py-2 text-[12px] text-destructive">{error}</div>}
          <div className="max-h-[280px] overflow-auto">
            {visible.length === 0 ? (
              <div className="p-4">
                <Empty>暂无任务</Empty>
              </div>
            ) : (
              visible.map((task) => (
                <div
                  key={task.id}
                  onClick={() => setSelected(task.id)}
                  className={`flex cursor-pointer items-center gap-2 border-b border-border/60 px-4 py-2 text-[12.5px] last:border-0 hover:bg-accent ${
                    selected === task.id ? "bg-accent" : ""
                  }`}
                >
                  <StatusBadge status={task.status} />
                  <span className="min-w-0 flex-1 truncate text-foreground">{task.title || task.script}</span>
                  <span className="font-mono text-[11px] text-muted-foreground">{timeAgo(task.created_at)}</span>
                  {(task.status === "running" || task.status === "queued") && (
                    <button
                      className="btn-danger px-2 py-0.5 text-[11px]"
                      onClick={async (event) => {
                        event.stopPropagation();
                        await api.stopTask(task.id).catch(() => null);
                        loadTasks();
                      }}
                    >
                      停止
                    </button>
                  )}
                </div>
              ))
            )}
          </div>
        </Card>

        <Card
          title={selectedTask ? `运行日志 · ${selectedTask.title}` : "运行日志"}
          bodyClass="p-3"
          actions={
            selectedTask ? (
              <>
                <Badge tone={selectedTask.status === "failed" ? "bad" : selectedTask.status === "succeeded" ? "ok" : "info"}>
                  {selectedTask.status}
                </Badge>
                <span className="font-mono text-[11px] text-muted-foreground">exit {selectedTask.exit_code}</span>
              </>
            ) : null
          }
        >
          {selected ? <LogView taskId={selected} /> : <Empty>点上方任务查看实时日志</Empty>}
        </Card>
      </div>
    </div>
  );
}
