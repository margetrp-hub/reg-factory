"use client";

import { useCallback, useEffect, useState } from "react";
import { api, type AccountRow } from "@/lib/api";
import { Badge, Card, Empty, Stat, timeAgo } from "@/components/ui";

const STATUS_LABEL: Record<string, { text: string; tone: "ok" | "warn" | "bad" | "neutral" | "info" }> = {
  ok: { text: "正常", tone: "ok" },
  expired: { text: "过期", tone: "warn" },
  banned: { text: "封禁", tone: "bad" },
  invalid: { text: "失效", tone: "bad" },
  unknown: { text: "未测", tone: "neutral" },
};

export default function AccountsPage() {
  const [rows, setRows] = useState<AccountRow[]>([]);
  const [kind, setKind] = useState("");
  const [platform, setPlatform] = useState("");
  const [status, setStatus] = useState("");
  const [busy, setBusy] = useState("");
  const [notice, setNotice] = useState("");
  const [error, setError] = useState("");

  const load = useCallback(async () => {
    try {
      const data = await api.accounts({ kind, platform, status, limit: 300 });
      setRows(data.accounts);
      setError("");
    } catch (err) {
      setError((err as Error).message);
    }
  }, [kind, platform, status]);

  useEffect(() => {
    load();
    const timer = setInterval(load, 8000);
    return () => clearInterval(timer);
  }, [load]);

  const sync = async () => {
    setBusy("sync");
    try {
      const data = await api.syncAccounts();
      setNotice(`已同步 ${data.synced} 条`);
      await load();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy("");
    }
  };

  const act = async (ref: string, plat: string, action: "liveness" | "refresh") => {
    setBusy(ref + action);
    setNotice("");
    try {
      if (action === "liveness") {
        await api.accountLiveness(ref, plat, false);
        setNotice(`已提交测活：${ref}`);
      } else {
        await api.accountRefresh(ref, plat, false);
        setNotice(`已提交刷新：${ref}`);
      }
      await load();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy("");
    }
  };

  const counts = rows.reduce<Record<string, number>>((acc, row) => {
    acc[row.status] = (acc[row.status] || 0) + 1;
    return acc;
  }, {});

  return (
    <div className="space-y-4">
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-5">
        <Stat label="账号总数" value={rows.length} hint="accounts 表" />
        <Stat label="正常" value={counts.ok ?? 0} tone="neutral" />
        <Stat label="过期/受限" value={counts.expired ?? 0} tone="warn" />
        <Stat label="封禁/失效" value={(counts.banned ?? 0) + (counts.invalid ?? 0)} tone="bad" />
        <Stat label="未测活" value={counts.unknown ?? 0} />
      </div>

      <Card
        title="账号主数据"
        actions={
          <>
            <select className="field w-[110px] py-1 text-[12px]" value={kind} onChange={(e) => setKind(e.target.value)}>
              <option value="">全部类型</option>
              <option value="emails">邮箱</option>
              <option value="cookies">Cookie</option>
              <option value="tokens">Token</option>
              <option value="outlook">Outlook</option>
            </select>
            <select className="field w-[110px] py-1 text-[12px]" value={platform} onChange={(e) => setPlatform(e.target.value)}>
              <option value="">全部平台</option>
              <option value="claude">claude</option>
              <option value="chatgpt">chatgpt</option>
              <option value="grok">grok</option>
              <option value="outlook">outlook</option>
            </select>
            <select className="field w-[110px] py-1 text-[12px]" value={status} onChange={(e) => setStatus(e.target.value)}>
              <option value="">全部状态</option>
              <option value="ok">正常</option>
              <option value="expired">过期</option>
              <option value="banned">封禁</option>
              <option value="unknown">未测</option>
            </select>
            <button className="btn-ghost" onClick={sync} disabled={busy === "sync"}>
              同步资产
            </button>
          </>
        }
        bodyClass="p-0"
      >
        {error && <div className="px-4 py-2 text-[12px] text-destructive">{error}</div>}
        {notice && <div className="px-4 py-2 text-[12px] text-success">{notice}</div>}
        {rows.length === 0 ? (
          <div className="p-4">
            <Empty>暂无账号主数据。点「同步资产」把邮箱池 / Cookie / Token 写进 accounts 表。</Empty>
          </div>
        ) : (
          <div className="max-h-[560px] overflow-auto">
            <table className="w-full text-[12px]">
              <thead className="sticky top-0 bg-card/95 text-[11px] uppercase tracking-[0.06em] text-muted-foreground">
                <tr className="border-b border-border">
                  <th className="px-4 py-2 text-left font-medium">账号</th>
                  <th className="px-2 py-2 text-left font-medium">平台</th>
                  <th className="px-2 py-2 text-left font-medium">状态</th>
                  <th className="px-2 py-2 text-left font-medium">测活</th>
                  <th className="px-2 py-2 text-left font-medium">领取</th>
                  <th className="px-4 py-2 text-right font-medium">操作</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((row) => {
                  const meta = STATUS_LABEL[row.status] || STATUS_LABEL.unknown;
                  return (
                    <tr key={row.ref} className="border-b border-border/60 last:border-0 hover:bg-accent/40">
                      <td className="max-w-[280px] truncate px-4 py-1.5 font-mono text-[11px] text-foreground" title={row.ref}>
                        {row.identity || row.ref}
                      </td>
                      <td className="px-2 py-1.5">
                        <Badge tone="info">{row.platform || "-"}</Badge>
                      </td>
                      <td className="px-2 py-1.5">
                        <Badge tone={meta.tone}>{meta.text}</Badge>
                      </td>
                      <td className="px-2 py-1.5 text-muted-foreground">
                        {row.health_checked_at ? timeAgo(row.health_checked_at) : "从未"}
                      </td>
                      <td className="px-2 py-1.5">
                        {row.claimed ? <Badge tone="neutral">已领</Badge> : <Badge tone="ok">可供应</Badge>}
                      </td>
                      <td className="px-4 py-1.5 text-right">
                        <button
                          className="btn-ghost mr-1 px-2 py-0.5 text-[11px]"
                          disabled={busy === row.ref + "liveness"}
                          onClick={() => act(row.ref, row.platform, "liveness")}
                        >
                          测活
                        </button>
                        <button
                          className="btn-ghost px-2 py-0.5 text-[11px]"
                          disabled={busy === row.ref + "refresh" || row.claimed}
                          onClick={() => act(row.ref, row.platform, "refresh")}
                        >
                          刷新
                        </button>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      <Card title="治理规则">
        <ul className="space-y-1.5 text-[11.5px] leading-relaxed text-muted-foreground">
          <li>· 测活/刷新会以任务形式进队列，受并发与内存闸门约束，不会绕过调度直接扫号。</li>
          <li>· 已被下游领取（claimed）的账号禁止自动刷新，避免影响已交付凭据。</li>
          <li>· 封禁类只标记不可修；超时/断连标 unknown，不自动重试。</li>
          <li>· 定时巡检开关与频率在「配置」页：REG_FACTORY_LIVENESS_*。</li>
        </ul>
      </Card>
    </div>
  );
}
