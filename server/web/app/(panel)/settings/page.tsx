"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { api } from "@/lib/api";
import { Badge, Card, Empty, ErrorNote } from "@/components/ui";

type Entry = { key: string; value: string; secret: boolean; line: number };

export default function SettingsPage() {
  const [groups, setGroups] = useState<Record<string, Entry[]>>({});
  const [path, setPath] = useState("");
  const [dirty, setDirty] = useState<Record<string, string>>({});
  const [query, setQuery] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [rawOpen, setRawOpen] = useState(false);
  const [raw, setRaw] = useState("");
  const [reveal, setReveal] = useState(false);
  const [proxy, setProxy] = useState("");
  const [target, setTarget] = useState("https://claude.ai");
  const [probe, setProbe] = useState<string>("");

  const load = useCallback(async () => {
    try {
      const data = await api.config();
      setGroups(data.groups);
      setPath(data.path);
      setError("");
    } catch (err) {
      setError((err as Error).message);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const filtered = useMemo(() => {
    if (!query.trim()) return groups;
    const needle = query.trim().toLowerCase();
    const out: Record<string, Entry[]> = {};
    for (const [name, list] of Object.entries(groups)) {
      const hits = list.filter((entry) => entry.key.toLowerCase().includes(needle));
      if (hits.length) out[name] = hits;
    }
    return out;
  }, [groups, query]);

  const save = async () => {
    if (Object.keys(dirty).length === 0) return;
    setBusy(true);
    setNotice("");
    setError("");
    try {
      const data = await api.saveConfig(dirty);
      setNotice(`已保存 ${data.changed.length} 项：${data.changed.slice(0, 6).join(", ")}${data.changed.length > 6 ? "…" : ""}`);
      setDirty({});
      await load();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const openRaw = async () => {
    setRawOpen(true);
    try {
      const data = await api.configRaw();
      setRaw(data.content);
    } catch (err) {
      setError((err as Error).message);
    }
  };

  const saveRaw = async () => {
    setBusy(true);
    try {
      await api.saveRaw(raw);
      setNotice("原始配置已保存");
      setRawOpen(false);
      await load();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const runProbe = async () => {
    setProbe("检测中…");
    try {
      const result = await api.proxyCheck(proxy, target);
      setProbe(
        result.error
          ? `失败：${result.error}（${result.ms}ms）`
          : `HTTP ${result.status} · ${result.ms}ms${result.title ? ` · ${result.title}` : ""}`,
      );
    } catch (err) {
      setProbe((err as Error).message);
    }
  };

  return (
    <div className="space-y-4">
      <div className="grid gap-4 lg:grid-cols-[1.5fr_1fr]">
        <Card
          title="环境配置"
          actions={
            <>
              <input
                className="field w-[170px] py-1 text-[12px]"
                placeholder="搜索键名"
                value={query}
                onChange={(e) => setQuery(e.target.value)}
              />
              <label className="flex items-center gap-1.5 text-[11px] text-muted-foreground">
                <input type="checkbox" checked={reveal} onChange={(e) => setReveal(e.target.checked)} /> 明文
              </label>
              <button className="btn-ghost" onClick={openRaw}>
                原始编辑
              </button>
              <button className="btn-primary" onClick={save} disabled={busy || Object.keys(dirty).length === 0}>
                保存 {Object.keys(dirty).length > 0 ? `(${Object.keys(dirty).length})` : ""}
              </button>
            </>
          }
        >
          <div className="mb-2 text-[11px] text-muted-foreground">
            文件 <span className="font-mono">{path}</span> · 修改后对新任务立即生效（面板进程内的值需重启容器）
          </div>
          <ErrorNote message={error} />
          {notice && <div className="mb-2 rounded border border-success/30 bg-success/10 px-2 py-1 text-[11px] text-success">{notice}</div>}
          {Object.keys(filtered).length === 0 && <Empty>没有匹配的配置项</Empty>}
          <div className="max-h-[calc(100vh-260px)] space-y-4 overflow-auto pr-1">
            {Object.entries(filtered).map(([group, list]) => (
              <section key={group}>
                <div className="mb-1.5 flex items-center gap-2">
                  <span className="text-[12px] font-medium text-foreground/85">{group}</span>
                  <Badge tone="neutral">{list.length}</Badge>
                </div>
                <div className="space-y-1.5">
                  {list.map((entry) => {
                    const value = dirty[entry.key] ?? entry.value;
                    const changed = entry.key in dirty;
                    return (
                      <div key={entry.key} className="grid grid-cols-[minmax(0,1fr)_minmax(0,1.3fr)] items-center gap-2">
                        <div className="truncate font-mono text-[11px] text-muted-foreground" title={entry.key}>
                          {entry.key}
                          {changed && <span className="ml-1 text-warning">*</span>}
                        </div>
                        <input
                          className={`field py-1 text-[12px] ${changed ? "border-warning/50" : ""}`}
                          type={entry.secret && !reveal ? "password" : "text"}
                          value={value}
                          onChange={(e) => setDirty((prev) => ({ ...prev, [entry.key]: e.target.value }))}
                        />
                      </div>
                    );
                  })}
                </div>
              </section>
            ))}
          </div>
        </Card>

        <div className="space-y-4">
          <Card title="代理体检">
            <p className="mb-3 text-[11px] leading-relaxed text-muted-foreground">
              SG 机房 IP 直连 claude.ai / chatgpt.com / grok.com 会拿 403（Cloudflare）。填一条住宅代理试真实出口。
            </p>
            <label className="label">代理（可空 = 直连）</label>
            <input className="field mb-2" placeholder="http://user:pass@host:port" value={proxy} onChange={(e) => setProxy(e.target.value)} />
            <label className="label">目标站点</label>
            <input className="field mb-3" value={target} onChange={(e) => setTarget(e.target.value)} />
            <button className="btn-primary" onClick={runProbe}>
              测试
            </button>
            {probe && <div className="mt-3 rounded-lg border border-border bg-muted/30 px-3 py-2 font-mono text-[11.5px] text-foreground/85">{probe}</div>}
          </Card>

          <Card title="说明">
            <ul className="space-y-1.5 text-[11.5px] leading-relaxed text-muted-foreground">
              <li>· 配置写入 <span className="font-mono">/data/.env</span>，与 Python worker 共用同一份。</li>
              <li>· 同一键只认第一次出现；保存时替换首个匹配、其余的保持原样。</li>
              <li>· 含 KEY / TOKEN / PASSWORD 的项默认打码，勾选「明文」可查看。</li>
              <li>· 原始编辑会做 KEY=VALUE 校验，拒绝任何非注释的其它行。</li>
            </ul>
          </Card>
        </div>
      </div>

      {rawOpen && (
        <Card
          title="原始 .env 编辑"
          actions={
            <>
              <button className="btn-ghost" onClick={() => setRawOpen(false)}>
                取消
              </button>
              <button className="btn-primary" onClick={saveRaw} disabled={busy}>
                保存
              </button>
            </>
          }
        >
          <textarea
            className="field h-[420px] font-mono text-[12px] leading-relaxed"
            value={raw}
            onChange={(e) => setRaw(e.target.value)}
            spellCheck={false}
          />
        </Card>
      )}
    </div>
  );
}
