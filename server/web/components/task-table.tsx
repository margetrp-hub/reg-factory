"use client";

import Link from "next/link";
import type { Task } from "@/lib/api";
import { Empty, StatusBadge, timeAgo } from "@/components/ui";
import { groupOf } from "@/lib/task-catalog";

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
  const rows = limit ? tasks.slice(0, limit) : tasks;
  if (rows.length === 0) {
    return (
      <div className="p-4">
        <Empty>{emptyText}</Empty>
      </div>
    );
  }
  return (
    <table className="tbl whitespace-nowrap">
      <thead>
        <tr className="border-b border-border">
          <th className="px-3 py-1.5 text-left font-medium">状态</th>
          <th className="px-2 py-1.5 text-left font-medium">任务</th>
          {showScript && <th className="px-2 py-1.5 text-left font-medium">脚本</th>}
          <th className="px-2 py-1.5 text-right font-medium">耗时</th>
          <th className="px-3 py-1.5 text-right font-medium">创建</th>
          <th className="px-3 py-1.5 text-right font-medium">退出码</th>
          {onStop && <th className="px-3 py-1.5 text-right font-medium">操作</th>}
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
                  <span className="block truncate" title={task.title || task.script}>{task.title || task.script}</span>
                ) : (
                  <Link href={`/tasks/group/${groupOf(task.script).slug}/?script=${task.script}`} className="block truncate hover:text-brand">
                    {task.title || task.script}
                  </Link>
                )}
              </td>
              {showScript && (
                <td className="px-2 py-1.5 font-mono text-[11px] text-muted-foreground">{task.script}</td>
              )}
              <td className="px-2 py-1.5 text-right font-mono text-[11px] text-muted-foreground">
                {running ? "运行中" : seconds ? `${seconds}s` : "-"}
              </td>
              <td className="px-3 py-1.5 text-right text-[11px] text-muted-foreground">{timeAgo(task.created_at)}</td>
              <td className="px-3 py-1.5 text-right font-mono text-[11px] text-muted-foreground">
                {task.exit_code >= 0 ? task.exit_code : "-"}
              </td>
              {onStop && (
                <td className="px-3 py-1.5 text-right">
                  {task.script === "production.team_json" ? (
                    <span className="text-[11px] text-muted-foreground">生产中心管理</span>
                  ) : running ? (
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
  );
}
