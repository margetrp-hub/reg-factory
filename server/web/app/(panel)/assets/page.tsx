"use client";

import { useCallback, useEffect, useState } from "react";
import { api, type AssetItem } from "@/lib/api";
import { Badge, Card, Empty, Stat } from "@/components/ui";
import { AssetResults } from "@/components/asset-results";

const TABS: { kind: string; label: string; hint: string }[] = [
  { kind: "emails", label: "邮箱池", hint: "emails.txt" },
  { kind: "cookies", label: "平台 Cookie", hint: "cookies/**" },
  { kind: "tokens", label: "Token", hint: "tokens/**" },
  { kind: "outlook", label: "Outlook", hint: "outlook_accounts / _outlook_pool" },
];

type Summary = { kind: string; total: number; claimed: number; available: number };

export default function AssetsPage() {
  const [kind, setKind] = useState("emails");
  const [items, setItems] = useState<AssetItem[]>([]);
  const [summary, setSummary] = useState<Summary[]>([]);
  const [reveal, setReveal] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const load = useCallback(async (target: string) => {
    try {
      const [list, sum] = await Promise.all([api.assetList(target, 300), api.assetSummary()]);
      setItems(list.items || []);
      setSummary(sum.assets || []);
      setError("");
    } catch (err) {
      setError((err as Error).message);
    }
  }, []);

  useEffect(() => {
    load(kind);
  }, [kind, load]);

  const current = summary.find((item) => item.kind === kind);

  const claim = async () => {
    setBusy(true);
    setNotice("");
    try {
      const data = await api.claim(kind);
      setNotice(`已领取：${data.item.ref}`);
      await load(kind);
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const reset = async () => {
    setBusy(true);
    try {
      const data = await api.resetClaims(kind);
      setNotice(`已重置 ${data.removed} 条领取记录`);
      await load(kind);
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const mask = (value: string) => {
    if (reveal) return value;
    if (value.length <= 24) return value.replace(/./g, "•");
    return `${value.slice(0, 12)}${"•".repeat(16)}${value.slice(-8)}`;
  };

  return (
    <div className="space-y-4">
      <AssetResults />
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        {TABS.map((tab) => {
          const item = summary.find((entry) => entry.kind === tab.kind);
          return (
            <button key={tab.kind} onClick={() => setKind(tab.kind)} className="text-left">
              <Stat
                label={tab.label}
                value={
                  <span>
                    {item?.available ?? 0}
                    <span className="text-[13px] text-muted-foreground"> / {item?.total ?? 0}</span>
                  </span>
                }
                hint={tab.hint}
                tone={kind === tab.kind ? "warn" : "neutral"}
              />
            </button>
          );
        })}
      </div>

      <Card
        title={`${TABS.find((tab) => tab.kind === kind)?.label || kind} · ${items.length} 条`}
        actions={
          <>
            <label className="flex items-center gap-1.5 text-[11px] text-muted-foreground">
              <input type="checkbox" checked={reveal} onChange={(e) => setReveal(e.target.checked)} /> 显示明文
            </label>
            <button className="btn-primary" onClick={claim} disabled={busy || (current?.available ?? 0) === 0}>
              领取一条
            </button>
            <button className="btn-danger" onClick={reset} disabled={busy || (current?.claimed ?? 0) === 0}>
              重置领取
            </button>
          </>
        }
        bodyClass="p-0"
      >
        {error && <div className="px-4 py-2 text-[12px] text-destructive">{error}</div>}
        {notice && <div className="px-4 py-2 text-[12px] text-success">{notice}</div>}
        {items.length === 0 ? (
          <div className="p-4">
            <Empty>该分类暂无资产。任务跑成功后会自动写入数据目录。</Empty>
          </div>
        ) : (
          <div className="max-h-[560px] overflow-auto">
            <table className="tbl">
              <thead className="sticky top-0 bg-card/95 text-[11px] uppercase tracking-[0.08em] text-muted-foreground">
                <tr className="border-b border-border">
                  <th className="px-4 py-2 text-left font-medium">来源</th>
                  <th className="px-2 py-2 text-left font-medium">平台</th>
                  <th className="px-2 py-2 text-left font-medium">状态</th>
                  <th className="px-4 py-2 text-left font-medium">内容</th>
                </tr>
              </thead>
              <tbody>
                {items.map((item) => (
                  <tr key={item.ref} className="border-b border-border/60 last:border-0 hover:bg-accent">
                    <td className="px-4 py-2 font-mono text-[11px] text-muted-foreground">{item.source}</td>
                    <td className="px-2 py-2">
                      <Badge tone="info">{item.platform}</Badge>
                    </td>
                    <td className="px-2 py-2">
                      {item.claimed ? <Badge tone="neutral">已领取</Badge> : <Badge tone="ok">可用</Badge>}
                    </td>
                    <td className="px-4 py-2">
                      <details>
                        <summary className="cursor-pointer font-mono text-[11px] text-muted-foreground">{mask(item.value)}</summary>
                        <pre className="mono-log mt-1 max-h-[220px] overflow-auto rounded bg-muted/40 p-2 text-foreground/85">{item.value}</pre>
                      </details>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    </div>
  );
}
