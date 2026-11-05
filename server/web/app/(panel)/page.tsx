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
  const attention = tasks.filter((task) => ["failed", "interrupted", "running", "queued"].includes(task.status));
  const failed = tasks.filter((task) => ["failed", "interrupted"].includes(task.status));
  const resourceTone = sys?.guarded ? "bad" : (sys?.queued ?? 0) > 0 ? "warn" : "neutral";

  return (
    <div className="space-y-5">
      <div className="page-head">
        <div className="min-w-0 flex-1">
          <div className="section-kicker">SG REG · 控制面</div>
          <h1 className="page-title">工作台</h1>
          <p className="page-desc">先处理阻塞和异常，再继续生产；运行、资产和供给都从这里进入。</p>
        </div>
        <Link href="/production/" className="btn-primary">开始生产</Link>
        <Link href="/tasks/" className="btn-ghost">查看运行</Link>
      </div>

      {error && <div className="rounded-lg border border-destructive/30 bg-destructive/10 px-3 py-2 text-[12px] text-destructive">{error}</div>}

      <div className="grid gap-4 lg:grid-cols-[1.45fr_1fr]">
        <Card title="需要处理" actions={<Badge tone={failed.length ? "bad" : attention.length ? "warn" : "ok"}>{failed.length ? `${failed.length} 项异常` : attention.length ? `${attention.length} 项运行中` : "当前清爽"}</Badge>}>
          {attention.length === 0 ? (
            <div className="flex min-h-[132px] flex-col justify-center rounded-lg border border-dashed border-success/30 bg-success/5 px-4 py-3">
              <div className="text-[13px] font-medium text-success">没有待处理的运行项</div>
              <p className="mt-1 text-[12px] text-muted-foreground">可以从生产总览选择平台，或到供给中心检查邮箱与代理。</p>
            </div>
          ) : (
            <div className="space-y-1.5">
              {attention.slice(0, 5).map((task) => (
                <Link key={task.id} href={`/tasks/?id=${task.id}`} className="flex items-center gap-3 rounded-lg border border-border/70 px-3 py-2 transition-colors hover:border-brand/40 hover:bg-accent/40">
                  <StatusBadge status={task.status} />
                  <span className="min-w-0 flex-1 truncate text-[12.5px]">{task.title || task.script}</span>
                  <span className="shrink-0 text-[11px] text-muted-foreground">{timeAgo(task.created_at)}</span>
                </Link>
              ))}
              {attention.length > 5 && <Link href="/tasks/" className="block pt-1 text-[11px] text-brand">查看全部 {attention.length} 项运行 →</Link>}
            </div>
          )}
        </Card>

        <Card title="资源健康" actions={<Link href="/materials/" className="btn-ghost px-2 py-1 text-[11px]">检查供给</Link>}>
          <div className="grid grid-cols-2 gap-2">
            <Stat label="运行 / 排队" value={`${sys?.running ?? 0} / ${sys?.queued ?? 0}`} hint={`槽位 ${sys?.slots_used ?? 0}/${sys?.slots_total ?? sys?.max_parallel ?? "-"}`} tone={resourceTone} />
            <Stat label="可用资产" value={available} hint={`总计 ${totalAssets} 条`} />
            <Stat label="内存" value={`${sys?.memory.used_pct ?? 0}%`} hint={`${sys?.memory.current_mb ?? 0} / ${sys?.memory.max_mb ?? 0} MB`} tone={(sys?.memory.used_pct ?? 0) >= (sys?.guard_pct ?? 85) ? "bad" : "neutral"} />
            <Stat label="CPU" value={`${sys?.cpu_pct ?? 0}%`} hint={`${sys?.cpu_cores ?? 0} 核`} tone={(sys?.cpu_pct ?? 0) > 85 ? "warn" : "neutral"} />
          </div>
        </Card>
      </div>

      <div className="grid gap-4 lg:grid-cols-[1.6fr_1fr]">
        <Card
          title="最近运行"
          actions={<><Link href="/tasks/" className="btn-ghost">运行中心</Link><button className="btn-danger" onClick={stopAll} disabled={busy || !sys?.running}>停止全部</button></>}
          bodyClass="p-0"
        >
          {tasks.length === 0 ? (
            <div className="p-4"><Empty>还没有运行记录。进入生产总览选择一个平台开始。</Empty></div>
          ) : (
            <table className="tbl">
              <thead><tr><th>任务</th><th>状态</th><th>创建</th><th className="text-right">退出码</th></tr></thead>
              <tbody>
                {tasks.map((task) => (
                  <tr key={task.id}>
                    <td><Link href={`/tasks/?id=${task.id}`} className="hover:text-brand"><span className="text-foreground">{task.title || task.script}</span><span className="ml-2 font-mono text-[11px] text-muted-foreground">{task.id}</span></Link></td>
                    <td><StatusBadge status={task.status} /></td>
                    <td className="text-muted-foreground">{timeAgo(task.created_at)}</td>
                    <td className="text-right font-mono text-muted-foreground">{task.exit_code >= 0 ? task.exit_code : "-"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Card>

        <div className="space-y-4">
          <Card title="快速进入">
            <div className="space-y-1.5">
              {[
                { title: "生产总览", desc: "按平台流程继续", href: "/production/" },
                { title: "资产中心", desc: "查看账号、测活和领取", href: "/accounts/" },
                { title: "供给中心", desc: "邮箱、代理、接码与 IC", href: "/materials/" },
                { title: "系统设置", desc: "配置、审计与运行参数", href: "/settings/" },
              ].map((item) => (
                <Link key={item.href} href={item.href} className="flex items-center gap-3 rounded-lg border border-border/70 px-3 py-2 transition-colors hover:border-brand/40 hover:bg-accent/40">
                  <span className="min-w-0 flex-1 text-[12.5px] font-medium">{item.title}</span><span className="text-[11px] text-muted-foreground">{item.desc}</span><span className="text-brand">→</span>
                </Link>
              ))}
            </div>
          </Card>

          <Card title="运行环境">
            <div className="space-y-1">
              <div className="kv"><span className="kv-k">版本</span><span className="kv-v">{sys?.version ?? "-"}</span></div>
              <div className="kv"><span className="kv-k">运行时长</span><span className="kv-v">{duration(sys?.uptime_sec ?? 0)}</span></div>
              <div className="kv"><span className="kv-k">监听</span><span className="kv-v">{sys?.addr ?? "-"}</span></div>
              <div className="kv"><span className="kv-k">数据目录</span><span className="kv-v truncate" title={sys?.data_dir}>{sys?.data_dir ?? "-"}</span></div>
              <div className="kv"><span className="kv-k">磁盘</span><span className="kv-v">{(sys?.disk_free_gb ?? 0).toFixed(0)}G / {(sys?.disk_total_gb ?? 0).toFixed(0)}G</span></div>
              <div className="kv"><span className="kv-k">内存闸门</span><span className="kv-v">{sys?.guarded ? <Badge tone="bad">已触发</Badge> : <Badge tone="ok">正常</Badge>}</span></div>
            </div>
          </Card>

          <Card title="资产池" actions={<Link href="/accounts/" className="btn-ghost">资产中心</Link>}>
            <div className="space-y-1">
              {assets.length === 0 && <Empty>暂无资产</Empty>}
              {assets.map((item) => (
                <div key={item.kind} className="kv"><span className="kv-k">{KIND_LABEL[item.kind] || item.kind}</span><span className="kv-v"><span className="text-success">{item.available}</span><span className="text-muted-foreground"> / {item.total}</span></span></div>
              ))}
            </div>
          </Card>
        </div>
      </div>
    </div>
  );
}
