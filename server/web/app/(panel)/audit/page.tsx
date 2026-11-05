"use client";

import { useEffect, useState } from "react";
import { api } from "@/lib/api";
import { Badge, Card, Empty } from "@/components/ui";

type AuditRow = { id: number; ts: string; actor: string; action: string; detail: string; ip: string };

export default function AuditPage() {
  const [rows, setRows] = useState<AuditRow[]>([]);
  const [error, setError] = useState("");

  useEffect(() => {
    api
      .audit(300)
      .then((data) => setRows(data.audit))
      .catch((err) => setError((err as Error).message));
  }, []);

  return (
    <Card title="操作审计" bodyClass="p-0" actions={<Badge tone="neutral">{rows.length} 条</Badge>}>
      {error && <div className="px-4 py-2 text-[12px] text-destructive">{error}</div>}
      {rows.length === 0 ? (
        <div className="p-4">
          <Empty>暂无记录</Empty>
        </div>
      ) : (
        <div className="max-h-[calc(100vh-180px)] overflow-auto">
          <table className="tbl">
            <thead className="sticky top-0 bg-card/95 text-[11px] uppercase tracking-[0.08em] text-muted-foreground">
              <tr className="border-b border-border">
                <th className="px-4 py-2 text-left font-medium">时间</th>
                <th className="px-2 py-2 text-left font-medium">操作者</th>
                <th className="px-2 py-2 text-left font-medium">动作</th>
                <th className="px-2 py-2 text-left font-medium">详情</th>
                <th className="px-4 py-2 text-left font-medium">来源 IP</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => (
                <tr key={row.id} className="border-b border-border/60 last:border-0 hover:bg-accent">
                  <td className="whitespace-nowrap px-4 py-1.5 font-mono text-[11px] text-muted-foreground">
                    {new Date(row.ts).toLocaleString("zh-CN", { hour12: false })}
                  </td>
                  <td className="px-2 py-1.5 text-foreground/85">{row.actor || "-"}</td>
                  <td className="px-2 py-1.5">
                    <Badge tone={row.action.startsWith("task") ? "info" : row.action.startsWith("config") ? "warn" : "neutral"}>
                      {row.action}
                    </Badge>
                  </td>
                  <td className="max-w-[420px] truncate px-2 py-1.5 font-mono text-[11px] text-muted-foreground" title={row.detail}>
                    {row.detail || "-"}
                  </td>
                  <td className="px-4 py-1.5 font-mono text-[11px] text-muted-foreground">{row.ip}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  );
}
