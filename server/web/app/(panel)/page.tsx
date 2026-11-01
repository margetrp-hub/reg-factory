"use client";

import Link from "next/link";
import { useCallback, useEffect, useState } from "react";
import { api, type SystemInfo, type Task } from "@/lib/api";
import { Badge, Card, Empty, Stat, StatusBadge, duration, timeAgo } from "@/components/ui";

type AssetSummary = { kind: string; total: number; claimed: number; available: number };

const KIND_LABEL: Record<string, string> = {
  emails: "邮箱池",
  cookies: "平台 Cookie",
  tokens: "Token",
  outlook: "Outlook 账号",
};

export default function OverviewPage() {
  const [sys, setSys] = useState<SystemInfo | null>(null);
  const [tasks, setTasks] = useState<Task[]>([]);
  const [assets, setAssets] = useState<AssetSummary[]>([]);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      const [system, taskList, assetList] = await Promise.all([api.system(), api.tasks(8), api.assetSummary()]);
      setSys(system);
      setTasks(taskList.tasks);
      setAssets(assetList.assets);
      setError("");
    } catch (err) {
      setError((err as Error).message);
    }
  }, []);

  useEffect(() => {
    load();
    const timer = setInterval(load, 8000);
    return () => clearInterval(timer);
  }, [load]);

  const stopAll = async () => {
    setBusy(true);
    try {
      await api.stopAll();
      await load();
    } finally {
      setBusy(false);
    }
  };

  const available = assets.reduce((sum, item) => sum + item.available, 0);
  const totalAssets = assets.reduce((sum, item) => sum + item.total, 0);

  return (
    <div className="space-y-4">
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Stat label="运行中任务" value={sys?.running ?? 0} hint={`槽位 ${sys?.slots_used ?? 0}/${sys?.slots_total ?? sys?.max_parallel ?? "-"}`} tone={sys?.guarded ? "bad" : "neutral"} />
        <Stat label="排队" value={sys?.queued ?? 0} hint="内存闸门触发时会滞留" tone={(sys?.queued ?? 0) > 0 ? "warn" : "neutral"} />
        <Stat label="可用资产" value={available} hint={`总计 ${totalAssets} 条`} />
        <Stat
          label="CPU"
          value={`${sys?.cpu_pct ?? 0}%`}
          hint={`${sys?.cpu_cores ?? 0} / ${sys?.cpu_quota ?? 0} 核`}
          tone={(sys?.cpu_pct ?? 0) > 85 ? "warn" : "neutral"}
        />
        <Stat
          label="内存水位"
          value={`${sys?.memory.used_pct ?? 0}%`}
          hint={`${sys?.memory.current_mb ?? 0} / ${sys?.memory.max_mb ?? 0} MB`}
          tone={(sys?.memory.used_pct ?? 0) >= (sys?.guard_pct ?? 85) ? "bad" : "neutral"}
        />
      </div>

      {error && <div className="rounded-lg border border-destructive/30 bg-destructive/10 px-3 py-2 text-[12px] text-destructive">{error}</div>}

      <div className="grid gap-4 lg:grid-cols-[1.6fr_1fr]">
        <Card
          title="最近任务"
          actions={
            <>
              <Link href="/tasks/" className="btn-ghost">
                全部任务
              </Link>
              <button className="btn-danger" onClick={stopAll} disabled={busy || !sys?.running}>
                停止全部
              </button>
            </>
          }
          bodyClass="p-0"
        >
          {tasks.length === 0 ? (
            <div className="p-4">
              <Empty>还没有任务。到「任务」页选择脚本并启动。</Empty>
            </div>
          ) : (
            <table className="w-full text-[12.5px]">
              <thead className="text-[11px] uppercase tracking-[0.08em] text-muted-foreground">
                <tr className="border-b border-border">
                  <th className="px-4 py-2 text-left font-medium">任务</th>
                  <th className="px-2 py-2 text-left font-medium">状态</th>
                  <th className="px-2 py-2 text-left font-medium">创建</th>
                  <th className="px-4 py-2 text-right font-medium">退出码</th>
                </tr>
              </thead>
              <tbody>
                {tasks.map((task) => (
                  <tr key={task.id} className="border-b border-border/60 last:border-0 hover:bg-accent">
                    <td className="px-4 py-2">
                      <Link href={`/tasks/?id=${task.id}`} className="hover:text-brand">
                        <span className="text-foreground">{task.title || task.script}</span>
                        <span className="ml-2 font-mono text-[11px] text-muted-foreground">{task.id}</span>
                      </Link>
                    </td>
                    <td className="px-2 py-2">
                      <StatusBadge status={task.status} />
                    </td>
                    <td className="px-2 py-2 text-muted-foreground">{timeAgo(task.created_at)}</td>
                    <td className="px-4 py-2 text-right font-mono text-muted-foreground">{task.exit_code >= 0 ? task.exit_code : "-"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Card>

        <div className="space-y-4">
          <Card title="主机与运行时">
            <div className="space-y-1">
              <div className="kv">
                <span className="kv-k">版本</span>
                <span className="kv-v">{sys?.version ?? "-"}</span>
              </div>
              <div className="kv">
                <span className="kv-k">运行时长</span>
                <span className="kv-v">{duration(sys?.uptime_sec ?? 0)}</span>
              </div>
              <div className="kv">
                <span className="kv-k">监听</span>
                <span className="kv-v">{sys?.addr ?? "-"}</span>
              </div>
              <div className="kv">
                <span className="kv-k">数据目录</span>
                <span className="kv-v truncate" title={sys?.data_dir}>
                  {sys?.data_dir ?? "-"}
                </span>
              </div>
              <div className="kv">
                <span className="kv-k">Python</span>
                <span className="kv-v truncate" title={sys?.python}>
                  {sys?.python ?? "-"}
                </span>
              </div>
              <div className="kv">
                <span className="kv-k">磁盘</span>
                <span className="kv-v">
                  {(sys?.disk_free_gb ?? 0).toFixed(0)}G 可用 / {(sys?.disk_total_gb ?? 0).toFixed(0)}G
                </span>
              </div>
              <div className="kv">
                <span className="kv-k">内存闸门</span>
                <span className="kv-v">
                  {sys?.guarded ? <Badge tone="bad">已触发</Badge> : <Badge tone="ok">正常</Badge>}
                  <span className="ml-2 text-muted-foreground">阈值 {sys?.guard_pct ?? 85}%</span>
                </span>
              </div>
            </div>
          </Card>

          <Card title="资产池" actions={<Link href="/assets/" className="btn-ghost">管理</Link>}>
            <div className="space-y-1">
              {assets.length === 0 && <Empty>暂无资产</Empty>}
              {assets.map((item) => (
                <div key={item.kind} className="kv">
                  <span className="kv-k">{KIND_LABEL[item.kind] || item.kind}</span>
                  <span className="kv-v">
                    <span className="text-success">{item.available}</span>
                    <span className="text-muted-foreground"> / {item.total}</span>
                  </span>
                </div>
              ))}
            </div>
          </Card>
        </div>
      </div>
    </div>
  );
}
