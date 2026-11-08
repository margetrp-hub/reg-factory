"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { api, type Task } from "@/lib/api";
import { Badge, Card, Stat } from "@/components/ui";
import { TASK_GROUPS, groupOf } from "@/lib/task-catalog";
import { TaskTable } from "@/components/task-table";
import { LogView } from "@/components/log-view";

export default function TaskCenterPage() {
  const [tasks, setTasks] = useState<Task[]>([]);
  const [error, setError] = useState("");
  const [selected, setSelected] = useState("");
  const [allLogs, setAllLogs] = useState(true);

  const load = useCallback(async () => {
    try {
      const data = await api.tasks(120);
      setTasks(data.tasks);
      setSelected((prev) => (data.tasks.some((task) => task.id === prev) ? prev : data.tasks[0]?.id || ""));
      setError("");
    } catch (err) {
      setError((err as Error).message);
    }
  }, []);

  useEffect(() => {
    load();
    const timer = setInterval(load, 5000);
    return () => clearInterval(timer);
  }, [load]);

  const counts = useMemo(() => {
    const latest = new Map<string, Task>();
    for (const task of tasks) if (!latest.has(task.script)) latest.set(task.script, task);
    const perGroup = new Map<string, Task>();
    for (const [scriptId, task] of latest) {
      const slug = groupOf(scriptId).slug;
      if (!perGroup.has(slug)) perGroup.set(slug, task);
    }
    return {
      perGroup,
      running: tasks.filter((task) => task.status === "running").length,
      queued: tasks.filter((task) => task.status === "queued").length,
      succeeded: tasks.filter((task) => ["succeeded", "completed"].includes(task.status)).length,
      unknown: tasks.filter((task) => task.status === "unknown").length,
      failed: tasks.filter((task) => task.status === "failed").length,
    };
  }, [tasks]);

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-3">
        <div className="min-w-0 flex-1">
          <h1 className="text-[16px] font-semibold leading-tight">任务中心</h1>
          <p className="text-[11px] text-muted-foreground">
            所有任务、历史输出和实时流统一在这里查看；分类页只负责发起业务流程。
          </p>
        </div>
        <Badge tone={counts.running > 0 ? "warn" : "info"}>
          运行中 {counts.running} · 排队 {counts.queued}
        </Badge>
      </div>

      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Stat label="运行 / 排队" value={`${counts.running}/${counts.queued}`} tone={counts.queued > 0 ? "warn" : "neutral"} />
        <Stat label="成功" value={counts.succeeded} />
        <Stat label="失败" value={counts.failed} tone={counts.failed > 0 ? "bad" : "neutral"} />
        <Stat label="结果待核对" value={counts.unknown} tone={counts.unknown > 0 ? "warn" : "neutral"} hint="查看原任务步骤和处理提示" />
      </div>

      {error && (
        <div className="rounded-lg border border-destructive/30 bg-destructive/10 px-3 py-2 text-[12px] text-destructive">
          {error}
        </div>
      )}

      <div className="grid min-w-0 grid-cols-1 gap-4">
        <Card title="全局任务流" bodyClass="p-0">
          <div className="max-h-[520px] overflow-auto">
            <TaskTable tasks={tasks} onSelect={(id) => { setSelected(id); setAllLogs(false); }} activeId={allLogs ? "" : selected} onStop={async (id) => {
              try { await api.stopTask(id); await load(); } catch (err) { setError((err as Error).message); }
            }} showScript />
          </div>
        </Card>
        <Card title="终端日志" bodyClass="p-2" actions={
          <>
            <button className={allLogs ? "btn-primary" : "btn-ghost"} onClick={() => setAllLogs(true)}>系统与全部任务</button>
            <button className={!allLogs ? "btn-primary" : "btn-ghost"} disabled={!selected} onClick={() => setAllLogs(false)}>选中任务</button>
          </>
        }>
          <LogView taskId={allLogs ? "" : selected} height={480} />
        </Card>
      </div>

      <div className="grid gap-3 sm:grid-cols-2">
        {TASK_GROUPS.map((group) => {
          const last = counts.perGroup.get(group.slug);
          return (
            <Link
              key={group.slug}
              href={`/tasks/group/${group.slug}/`}
              className="group rounded-[var(--radius)] border border-border bg-card p-4 transition-colors hover:border-brand/50 hover:bg-accent/30"
            >
              <div className="flex items-center justify-between">
                <span className="text-[14px] font-semibold">{group.title}</span>
                <span className="font-mono text-[11px] text-muted-foreground">{group.ids.length} 个</span>
              </div>
              <p className="mt-1 text-[11px] leading-snug text-muted-foreground">{group.desc}</p>
              <div className="mt-3 flex items-center justify-between text-[11px]">
                <span className="text-muted-foreground">
                  {last ? `最近：${last.title || last.script}` : "尚未运行"}
                </span>
                <span className="text-brand opacity-0 transition-opacity group-hover:opacity-100">进入 →</span>
              </div>
            </Link>
          );
        })}
      </div>

      <Card title="其他入口" bodyClass="p-0">
        <table className="tbl">
          <tbody>
            {[
              { name: "Claude 协议注册", desc: "零浏览器，IC / Outlook / magic link 取码", href: "/platform/claude/" },
              { name: "批量测活", desc: "按号池扫描缓存刷新健康状态", href: "/accounts/" },
              { name: "IC 邮箱", desc: "IMAP 多源收件与索引", href: "/apps/icmail/" },
              { name: "物料补给", desc: "邮箱 / 代理 / 接码 / 验证码", href: "/materials/" },
            ].map((row) => (
              <tr key={row.name} className="border-b border-border/60 last:border-0 hover:bg-accent/40">
                <td className="w-[220px] px-4 py-2 text-[12.5px] font-medium">
                  <Link href={row.href} className="hover:text-brand">{row.name}</Link>
                </td>
                <td className="px-2 py-2 text-[11px] text-muted-foreground">{row.desc}</td>
                <td className="w-[80px] px-4 py-2 text-right">
                  <Link href={row.href} className="text-[11px] text-brand">进入 →</Link>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </Card>
    </div>
  );
}
