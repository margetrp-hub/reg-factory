"use client";

import { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { api, type NodeCapabilities, type NodeReady } from "@/lib/api";
import { Badge, Card, Empty, Stat } from "@/components/ui";

export default function NodesPage() {
  const [ready, setReady] = useState<NodeReady | null>(null);
  const [node, setNode] = useState<NodeCapabilities | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const [readyState, capabilities] = await Promise.all([api.nodeReady(), api.nodeCapabilities()]);
      setReady(readyState);
      setNode(capabilities);
      setError("");
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
    const timer = setInterval(load, 10000);
    return () => clearInterval(timer);
  }, [load]);

  return (
    <div className="space-y-4">
      <div className="page-head">
        <div className="min-w-0 flex-1">
          <h1 className="page-title">节点</h1>
          <p className="page-desc">运行时身份、协议能力和密钥指纹统一从当前控制面读取。</p>
        </div>
        <button className="btn-ghost" onClick={load} disabled={loading}>{loading ? "读取中…" : "刷新状态"}</button>
        <Badge tone={error || !ready ? "neutral" : ready.execution_ready ? "ok" : "warn"}>
          {error ? "状态未确认" : !ready ? "读取中" : ready.execution_ready ? "执行就绪" : "未就绪"}
        </Badge>
      </div>

      {error && <div className="rounded-lg border border-destructive/30 bg-destructive/10 px-3 py-2 text-[12px] text-destructive">{error}{ready ? "；以下保留上次读取的状态。" : ""}</div>}

      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Stat label="节点" value={node?.node_id || "-"} />
        <Stat label="平台" value={node?.config || "-"} />
        <Stat label="支持动作" value={node?.actions?.supported?.length ?? 0} />
        <Stat label="协议" value={node?.detail?.receipt ? "v1" : "-"} />
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        <Card title="运行状态">
          {ready ? (
            <div className="space-y-1">
              <div className="kv"><span className="kv-k">ready</span><span className="kv-v">{String(ready.ready)}</span></div>
              <div className="kv"><span className="kv-k">execution_ready</span><span className="kv-v">{String(ready.execution_ready)}</span></div>
              <div className="kv"><span className="kv-k">schema</span><span className="kv-v">{node?.schema || "-"}</span></div>
              <div className="kv"><span className="kv-k">工作密钥指纹</span><span className="kv-v">{node?.work_key_fp || "-"}</span></div>
              <div className="kv"><span className="kv-k">回执密钥指纹</span><span className="kv-v">{node?.receipt_key_fp || "-"}</span></div>
              <div className="kv"><span className="kv-k">业务密钥指纹</span><span className="kv-v">{node?.business_key_fp || "-"}</span></div>
            </div>
          ) : <Empty>{error ? "节点状态读取失败，请刷新重试。" : "正在读取节点状态"}</Empty>}
        </Card>

        <Card title="能力清单">
          <div className="mb-3 flex flex-wrap gap-1.5">
            {(node?.actions?.supported || []).map((action) => <Badge key={action} tone="ok">{action}</Badge>)}
          </div>
          <div className="text-[11px] text-muted-foreground">节点不支持的动作不会伪装成可执行任务，协议能力在这里集中核对。</div>
        </Card>
      </div>

      <Card
        title="节点操作"
        actions={
          <div className="flex items-center gap-2">
            <Link className="btn-ghost" href="/nodes/config/">节点配置</Link>
            <Link className="btn-primary" href="/tasks/">统一终端日志</Link>
          </div>
        }
      >
        <p className="text-[12px] leading-relaxed text-muted-foreground">
          节点执行仍由同一个任务队列和日志流记录。配置、任务输出和审计分别进入统一控制面，不再维护独立的节点面板。
        </p>
      </Card>
    </div>
  );
}
