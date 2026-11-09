"use client";

import Link from "next/link";
import { api, type Task } from "@/lib/api";
import { useState } from "react";
import { Empty, StatusBadge, timeAgo } from "@/components/ui";

/** 任务表格：任务中心 / 平台工作台 / 分组页共用，统一列与交互。 */
export function TaskTable({
  tasks,
  onSelect,
  activeId,
  onStop,
  emptyText = "暂无任务",
  limit,
  showScript = false,
}: {
  tasks: Task[];
  onSelect?: (id: string) => void;
  activeId?: string;
  onStop?: (id: string) => void;
  emptyText?: string;
  limit?: number;
  showScript?: boolean;
}) {
  const [productionError, setProductionError] = useState("");
  const [busy, setBusy] = useState("");
  const rows = limit ? tasks.slice(0, limit) : tasks;
  const isProduction = (task: Task) => {
    try { return task.script === "production.team_json" && JSON.parse(task.args_json || "{}").mode === "produce"; }
    catch { return false; }
  };
  const showActions = Boolean(onStop) || rows.some(isProduction);
  async function productionControl(task: Task, action: "continue" | "stop") {
    const message = action === "stop"
      ? "终止这一项生产，保留原账号、凭据和执行记录。系统会核对已发生的加入结果，其他数量继续执行。"
      : "继续原账号的生产并重置自动重试次数。已保存的凭据会直接核验，不会重复生成。";
    if (!window.confirm(message)) return;
    setBusy(task.id); setProductionError("");
    try { await api.productionControl(task.id, action); }
    catch (error) { setProductionError(error instanceof Error ? error.message : "操作失败，请查看原任务日志"); }
    finally { setBusy(""); }
  }
  if (rows.length === 0) {
    return (
      <div className="p-4">
        <Empty>{emptyText}</Empty>
      </div>
    );
  }
  return (
    <>
    {productionError && <p role="alert" className="px-3 py-2 text-red-500">{productionError}</p>}
    <table className="tbl sg-task-table whitespace-nowrap">
      <thead>
        <tr className="border-b border-border">
          <th className="px-3 py-1.5 text-left font-medium">状态</th>
          <th className="px-2 py-1.5 text-left font-medium">任务</th>
          {showScript && <th className="px-2 py-1.5 text-left font-medium">脚本</th>}
          <th className="px-2 py-1.5 text-right font-medium">耗时</th>
          <th className="px-3 py-1.5 text-right font-medium">创建</th>
          <th className="px-3 py-1.5 text-right font-medium">退出码</th>
          {showActions && <th className="px-3 py-1.5 text-right font-medium">操作</th>}
        </tr>
      </thead>
      <tbody>
        {rows.map((task) => {
          const start = task.started_at ? new Date(task.started_at).getTime() : 0;
          const end = task.finished_at ? new Date(task.finished_at).getTime() : 0;
          const seconds = start && end ? Math.max(0, Math.round((end - start) / 1000)) : 0;
          const running = task.status === "running" || task.status === "queued";
          return (
            <tr
              key={task.id}
              onClick={() => onSelect?.(task.id)}
              className={`border-b border-border/60 last:border-0 ${
                onSelect ? "cursor-pointer hover:bg-accent/40" : ""
              } ${activeId === task.id ? "bg-brand/10" : ""}`}
            >
              <td className="px-3 py-1.5"><StatusBadge status={task.status} /></td>
              <td className="max-w-[260px] px-2 py-1.5">
                {onSelect ? (
                  <button type="button" className="block max-w-full truncate text-left font-medium" title={task.title || task.script}
                    aria-pressed={activeId === task.id} onClick={event => { event.stopPropagation(); onSelect(task.id); }}>{task.title || task.script}</button>
                ) : (
                  <Link href={`/tasks/?task=${encodeURIComponent(task.id)}`} className="block truncate hover:text-brand">
                    {task.title || task.script}
                  </Link>
                )}
              </td>
              {showScript && (
                <td className="px-2 py-1.5 font-mono text-[11px] text-muted-foreground">{task.script}</td>
              )}
              <td className="px-2 py-1.5 text-right font-mono text-[11px] text-muted-foreground">
                {task.status === "queued" ? "等待执行" : running ? "运行中" : seconds ? `${seconds}s` : "-"}
              </td>
              <td className="px-3 py-1.5 text-right text-[11px] text-muted-foreground">{timeAgo(task.created_at)}</td>
              <td className="px-3 py-1.5 text-right font-mono text-[11px] text-muted-foreground">
                {task.exit_code >= 0 ? task.exit_code : "-"}
              </td>
              {showActions && (
                <td className="px-3 py-1.5 text-right">
                  {isProduction(task) ? (<>
                    {task.status === "waiting_user" || task.status === "unknown" ? (
                      <button className="btn-ghost px-2 py-0.5 text-[11px]" disabled={busy === task.id}
                        onClick={async (event) => {
                          event.stopPropagation();
                          await productionControl(task, "continue");
                        }}>继续原生产</button>
                    ) : null}
                    {["running", "queued", "unknown", "waiting_user"].includes(task.status) ? (
                      <button className="btn-danger px-2 py-0.5 text-[11px]" disabled={busy === task.id}
                        onClick={async (event) => { event.stopPropagation(); await productionControl(task, "stop"); }}>终止本项</button>
                    ) : <span className="text-[11px] text-muted-foreground">在生产中心补发此项</span>}
                  </>) : running && onStop ? (
                    <button
                      className="btn-danger px-2 py-0.5 text-[11px]"
                      onClick={(event) => {
                        event.stopPropagation();
                        onStop(task.id);
                      }}
                    >
                      停止
                    </button>
                  ) : (
                    <span className="text-[11px] text-muted-foreground">—</span>
                  )}
                </td>
              )}
            </tr>
          );
        })}
      </tbody>
    </table>
    </>
  );
}
