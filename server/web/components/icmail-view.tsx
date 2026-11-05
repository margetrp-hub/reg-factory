"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { Badge, Card, Empty, Stat, timeAgo } from "@/components/ui";
import { Drawer } from "@/components/drawer";

const BASE = "/api/apps/icmail";

type Stats = { indexed: number; lastUid: number; sources: number; byTag: Record<string, number> };
type SourceRow = {
  id: number; name: string; host: string; port: number; username: string; folder: string;
  enabled: boolean; idle: boolean; intervalSeconds: number; heartbeatSeconds: number;
  lastUID: number; passwordSet: boolean; password?: string;
};
type AliasRow = { hme: string; state: string; manual: boolean; total: number; riskHits: number; lastTag: string; lastTs: number };
type MessageRow = { uid: string; subject: string; from: string; to?: string; primaryTag: string; alias: string; sentAt: number; text?: string };
type ConfigSnap = {
  dbPath: string; retentionDays: number; bootstrapLimit: number; fetchChunk: number; adminKeySet: boolean;
  turnstile: { siteKey: string; secretSet: boolean; expectedHostname: string; verifyURL: string };
  imap: { hostEnvSet: boolean; userEnvSet: boolean; passEnvSet: boolean; folder: string; intervalSec: number; heartbeatSec: number };
  sources: SourceRow[];
};

type Tab = "overview" | "sources" | "aliases" | "messages";

async function call<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(BASE + path, {
    credentials: "same-origin",
    headers: { "content-type": "application/json" },
    ...init,
  });
  const body = await res.json().catch(() => ({}));
  if (!res.ok || body.ok === false) {
    throw new Error(body?.error?.message || body?.error?.code || `HTTP ${res.status}`);
  }
  return body.data as T;
}

const TAG_TONE: Record<string, "ok" | "warn" | "bad" | "info" | "neutral"> = {
  otp: "info", risk: "bad", banned: "bad", payment: "warn", promo: "neutral", other: "neutral",
};

const TABS: { id: Tab; label: string; href: string }[] = [
  { id: "overview", label: "概览", href: "/apps/icmail/" },
  { id: "sources", label: "源", href: "/apps/icmail/sources/" },
  { id: "aliases", label: "别名", href: "/apps/icmail/aliases/" },
  { id: "messages", label: "邮件", href: "/apps/icmail/messages/" },
];

const EMPTY_SOURCE = { name: "", host: "", port: 993, username: "", password: "", folder: "INBOX" };

export function IcmailView({ tab }: { tab: Tab }) {
  const [stats, setStats] = useState<Stats | null>(null);
  const [config, setConfig] = useState<ConfigSnap | null>(null);
  const [aliases, setAliases] = useState<AliasRow[]>([]);
  const [messages, setMessages] = useState<MessageRow[]>([]);
  const [tag, setTag] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState("");
  const [codeEmail, setCodeEmail] = useState("");
  const [codeResult, setCodeResult] = useState<{ code?: string; email?: string; ageSeconds?: number; scanned?: number } | null>(null);
  const [newSource, setNewSource] = useState(EMPTY_SOURCE);
  const [openMsg, setOpenMsg] = useState<MessageRow | null>(null);
  const [editSource, setEditSource] = useState<SourceRow | null>(null);
  const [draft, setDraft] = useState<SourceRow | null>(null);

  const load = useCallback(async () => {
    try {
      const [health, cfg, aliasData, msgData] = await Promise.all([
        fetch(`${BASE}/health`, { credentials: "same-origin" }).then((r) => r.json()).catch(() => null),
        call<ConfigSnap>("/config"),
        call<{ aliases: AliasRow[] }>("/aliases"),
        call<{ messages: MessageRow[] }>(`/messages?limit=60${tag ? `&tag=${tag}` : ""}`),
      ]);
      if (health?.ok) setStats(health.data.stats);
      setConfig(cfg);
      setAliases(aliasData.aliases || []);
      setMessages(msgData.messages || []);
      setError("");
    } catch (err) {
      setError((err as Error).message);
    }
  }, [tag]);

  useEffect(() => {
    load();
    const timer = setInterval(load, 25000);
    return () => clearInterval(timer);
  }, [load]);

  const act = async (label: string, fn: () => Promise<unknown>) => {
    setBusy(label);
    setNotice("");
    setError("");
    try {
      await fn();
      setNotice(`${label} 完成`);
      await load();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy("");
    }
  };

  const toggleSource = (row: SourceRow) =>
    act(row.enabled ? "停用源" : "启用源", () =>
      call(`/sources/${row.id}/toggle`, { method: "POST", body: JSON.stringify({ enabled: !row.enabled }) }));
  const removeSource = (row: SourceRow) => act("删除源", () => call(`/sources/${row.id}`, { method: "DELETE" }));
  const addSource = () =>
    act("新增源", () =>
      call("/sources", { method: "POST", body: JSON.stringify({ ...newSource, enabled: true, idle: true }) })
        .then(() => setNewSource(EMPTY_SOURCE)));
  const setAliasState = (row: AliasRow, state: string) =>
    act("更新别名状态", () => call("/aliases/state", { method: "POST", body: JSON.stringify({ hme: row.hme, state }) }));
  const sync = () => act("触发同步", () => call("/sync", { method: "POST", body: JSON.stringify({}) }));
  const prune = () => act("清理旧邮件", () => call(`/prune?days=${config?.retentionDays || 90}`));
  const lookupCode = async () => {
    if (!codeEmail.trim()) return;
    setBusy("查码");
    setError("");
    try {
      setCodeResult(await call(`/code?email=${encodeURIComponent(codeEmail.trim())}`));
    } catch (err) {
      setError((err as Error).message);
      setCodeResult(null);
    } finally {
      setBusy("");
    }
  };

  const tagCounts = stats?.byTag || {};
  const flow = useMemo(
    () => [
      { id: "source", title: "① IMAP 源", ready: (config?.sources?.filter((s) => s.enabled).length ?? 0) > 0, text: `${config?.sources?.filter((s) => s.enabled).length ?? 0} 个启用` },
      { id: "index", title: "② 索引", ready: (stats?.indexed ?? 0) > 0, text: `${stats?.indexed ?? 0} 封` },
      { id: "classify", title: "③ 分类", ready: Object.keys(tagCounts).length > 0, text: `${Object.keys(tagCounts).length} 类` },
      { id: "lookup", title: "④ 取码", ready: true, text: "管理端直读" },
    ],
    [config, stats, tagCounts],
  );

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-3">
        <span className="grid h-9 w-9 place-items-center rounded-md bg-foreground text-[13px] font-semibold text-primary-foreground">I</span>
        <div className="min-w-0 flex-1">
          <h1 className="text-[16px] font-semibold leading-tight">IC 邮箱</h1>
          <p className="text-[11px] text-muted-foreground">
            IMAP 多源收件 → SQLite 索引 → 分类（banned/risk/otp/payment/promo）；协议注册与测活都从这里取码。
          </p>
        </div>
        <div className="flex gap-1.5">
          {TABS.map((item) => (
            <Link
              key={item.id}
              href={item.href}
              className={tab === item.id ? "btn-primary text-[11px]" : "btn-ghost text-[11px]"}
            >
              {item.label}
            </Link>
          ))}
        </div>
      </div>

      {error && <div className="rounded-lg border border-destructive/30 bg-destructive/10 px-3 py-2 text-[12px] text-destructive">{error}</div>}
      {notice && <div className="rounded-lg border border-success/30 bg-success/10 px-3 py-2 text-[12px] text-success">{notice}</div>}

      {tab === "overview" && (
        <>
          <div className="grid grid-cols-2 gap-3 lg:grid-cols-5">
            <Stat label="已索引" value={stats?.indexed ?? 0} hint={`lastUID ${stats?.lastUid ?? "-"}`} />
            <Stat label="IMAP 源" value={config?.sources?.length ?? 0} />
            <Stat label="验证码类" value={tagCounts.otp ?? 0} tone="warn" />
            <Stat label="风险/封禁" value={(tagCounts.risk ?? 0) + (tagCounts.banned ?? 0)} tone="bad" />
            <Stat label="保留期" value={`${config?.retentionDays ?? "-"} 天`} />
          </div>

          <Card title="流程" bodyClass="p-0">
            <div className="divide-y divide-border">
              {flow.map((step, index) => (
                <div key={step.id} className="flex items-center gap-3 px-4 py-2.5">
                  <span className="grid h-6 w-6 shrink-0 place-items-center rounded-full border border-border text-[11px] text-muted-foreground">
                    {index + 1}
                  </span>
                  <span className="text-[13px] font-medium">{step.title}</span>
                  <Badge tone={step.ready ? "ok" : "warn"}>{step.text}</Badge>
                </div>
              ))}
            </div>
          </Card>

          <div className="grid gap-4 xl:grid-cols-2">
            <Card title="查验证码（管理端）" actions={<Badge tone="info">无 Turnstile</Badge>}>
              <div className="flex gap-2">
                <input className="field" placeholder="邮箱地址" value={codeEmail} onChange={(e) => setCodeEmail(e.target.value)} />
                <button className="btn-primary shrink-0" onClick={lookupCode} disabled={busy === "查码"}>查询</button>
              </div>
              {codeResult && (
                <div className="mt-3 rounded-lg border border-border bg-muted/40 px-3 py-2 font-mono text-[13px]">
                  {codeResult.code ? <span className="text-success">{codeResult.code}</span> : (
                    <span className="text-muted-foreground">未找到验证码（扫了 {codeResult.scanned ?? 0} 封）</span>
                  )}
                  {codeResult.ageSeconds != null && (
                    <span className="ml-2 text-[11px] text-muted-foreground">{Math.round(codeResult.ageSeconds)} 秒前</span>
                  )}
                </div>
              )}
              <div className="mt-3 flex gap-2">
                <button className="btn-ghost text-[11px]" onClick={sync} disabled={!!busy}>触发同步</button>
                <button className="btn-ghost text-[11px]" onClick={prune} disabled={!!busy}>清理旧邮件</button>
              </div>
            </Card>

            <Card title="最近邮件" bodyClass="p-0" actions={<Link href="/apps/icmail/messages/" className="btn-ghost px-2 py-1 text-[11px]">全部</Link>}>
              <div className="max-h-[260px] overflow-auto">
                {messages.length === 0 ? (
                  <div className="p-4"><Empty>暂无邮件</Empty></div>
                ) : (
                  <table className="tbl">
                    <thead>
                      <tr className="border-b border-border">
                        <th className="px-3 py-1.5 text-left font-medium">分类</th>
                        <th className="px-2 py-1.5 text-left font-medium">主题</th>
                        <th className="px-3 py-1.5 text-right font-medium">时间</th>
                      </tr>
                    </thead>
                    <tbody>
                      {messages.slice(0, 10).map((row) => (
                        <tr
                          key={row.uid + row.alias}
                          onClick={() => setOpenMsg(row)}
                          className="cursor-pointer border-b border-border/60 last:border-0 hover:bg-accent/40"
                        >
                          <td className="px-3 py-1.5"><Badge tone={TAG_TONE[row.primaryTag] || "neutral"}>{row.primaryTag || "other"}</Badge></td>
                          <td className="max-w-[260px] truncate px-2 py-1.5" title={row.subject}>{row.subject || "(无主题)"}</td>
                          <td className="px-3 py-1.5 text-right text-[11px] text-muted-foreground">
                            {row.sentAt ? timeAgo(new Date(row.sentAt * 1000).toISOString()) : "-"}
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                )}
              </div>
            </Card>
          </div>

          <Card title="配置快照" bodyClass="p-0">
            <table className="tbl">
              <tbody>
                {[
                  { k: "库文件", v: config?.dbPath ?? "-" },
                  { k: "保留期", v: `${config?.retentionDays ?? "-"} 天` },
                  { k: "Bootstrap 上限", v: String(config?.bootstrapLimit ?? "-") },
                  { k: "Fetch Chunk", v: String(config?.fetchChunk ?? "-") },
                  { k: "AdminKey", v: config?.adminKeySet ? "已设置" : "空（靠外层登录）" },
                  { k: "Turnstile", v: config?.turnstile?.siteKey ? "已配置" : "未配置" },
                  { k: "IMAP env", v: config?.imap?.hostEnvSet ? "已配置" : "未配置" },
                  { k: "Heartbeat", v: `${config?.imap?.heartbeatSec ?? "-"}s` },
                ].map((row) => (
                  <tr key={row.k} className="border-b border-border/60 last:border-0">
                    <td className="w-[160px] px-4 py-1.5 text-[11px] text-muted-foreground">{row.k}</td>
                    <td className="px-2 py-1.5 font-mono text-[11px]">{row.v}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </Card>
        </>
      )}

      {tab === "sources" && (
        <>
          <Card title="IMAP 源" bodyClass="p-0">
            <div className="max-h-[420px] overflow-auto">
              {!config?.sources?.length ? (
                <div className="p-4"><Empty>没有源，用下面表单新增</Empty></div>
              ) : (
                <table className="tbl">
                  <thead>
                    <tr className="border-b border-border">
                      <th className="px-3 py-1.5 text-left font-medium">状态</th>
                      <th className="px-2 py-1.5 text-left font-medium">名称</th>
                      <th className="px-2 py-1.5 text-left font-medium">服务器</th>
                      <th className="px-2 py-1.5 text-left font-medium">账号</th>
                      <th className="px-2 py-1.5 text-left font-medium">文件夹</th>
                      <th className="px-2 py-1.5 text-right font-medium">interval</th>
                      <th className="px-2 py-1.5 text-right font-medium">heartbeat</th>
                      <th className="px-2 py-1.5 text-right font-medium">lastUID</th>
                      <th className="px-3 py-1.5 text-right font-medium">操作</th>
                    </tr>
                  </thead>
                  <tbody>
                    {config.sources.map((row) => (
                      <tr
                        key={row.id}
                        onClick={() => { setEditSource(row); setDraft({ ...row }); }}
                        className="cursor-pointer border-b border-border/60 last:border-0 hover:bg-accent/40"
                      >
                        <td className="px-3 py-1.5"><Badge tone={row.enabled ? "ok" : "neutral"}>{row.enabled ? "启用" : "停用"}</Badge></td>
                        <td className="px-2 py-1.5">{row.name}</td>
                        <td className="px-2 py-1.5 font-mono text-[11px] text-muted-foreground">{row.host}:{row.port}</td>
                        <td className="max-w-[180px] truncate px-2 py-1.5 font-mono text-[11px]">{row.username || "-"}</td>
                        <td className="px-2 py-1.5 text-[11px] text-muted-foreground">{row.folder}</td>
                        <td className="px-2 py-1.5 text-right font-mono text-[11px]">{row.intervalSeconds}s</td>
                        <td className="px-2 py-1.5 text-right font-mono text-[11px]">{row.heartbeatSeconds}s</td>
                        <td className="px-2 py-1.5 text-right font-mono text-[11px] text-muted-foreground">{row.lastUID}</td>
                        <td className="px-3 py-1.5 text-right">
                          <button className="btn-ghost px-2 py-0.5 text-[11px]" onClick={() => toggleSource(row)} disabled={!!busy}>
                            {row.enabled ? "停用" : "启用"}
                          </button>
                          <button className="btn-danger ml-1 px-2 py-0.5 text-[11px]" onClick={() => removeSource(row)} disabled={!!busy}>删除</button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </div>
          </Card>

          <Card
            title="新增源"
            actions={<span className="text-[11px] text-muted-foreground">defaults: 993 / INBOX / idle</span>}
          >
            <div className="grid gap-2 sm:grid-cols-3">
              <input className="field" placeholder="名称（唯一）" value={newSource.name} onChange={(e) => setNewSource({ ...newSource, name: e.target.value })} />
              <input className="field" placeholder="IMAP host" value={newSource.host} onChange={(e) => setNewSource({ ...newSource, host: e.target.value })} />
              <input className="field" placeholder="端口 993" value={newSource.port} onChange={(e) => setNewSource({ ...newSource, port: Number(e.target.value) || 993 })} />
              <input className="field" placeholder="用户名" value={newSource.username} onChange={(e) => setNewSource({ ...newSource, username: e.target.value })} />
              <input className="field" type="password" placeholder="密码 / App 专用密码" value={newSource.password} onChange={(e) => setNewSource({ ...newSource, password: e.target.value })} />
              <input className="field" placeholder="文件夹 INBOX" value={newSource.folder} onChange={(e) => setNewSource({ ...newSource, folder: e.target.value })} />
            </div>
            <button className="btn-primary mt-2" onClick={addSource} disabled={busy === "新增源" || !newSource.name || !newSource.host}>
              保存
            </button>
            <p className="mt-2 text-[11px] text-muted-foreground">
              也可以在 <span className="font-mono">/data/.env</span> 用 <span className="font-mono">ICMAIL_IMAP_*</span> 配默认源（首次启动时 seed）。
            </p>
          </Card>
        </>
      )}

      {tab === "aliases" && (
        <Card title="别名" bodyClass="p-0" actions={<span className="text-[11px] text-muted-foreground">以实际收件人聚合</span>}>
          <div className="max-h-[560px] overflow-auto">
            {aliases.length === 0 ? (
              <div className="p-4"><Empty>还没有收信记录</Empty></div>
            ) : (
              <table className="tbl">
                <thead>
                  <tr className="border-b border-border">
                    <th className="px-3 py-1.5 text-left font-medium">状态</th>
                    <th className="px-2 py-1.5 text-left font-medium">别名</th>
                    <th className="px-2 py-1.5 text-right font-medium">收件数</th>
                    <th className="px-2 py-1.5 text-right font-medium">风险</th>
                    <th className="px-2 py-1.5 text-left font-medium">最后分类</th>
                    <th className="px-2 py-1.5 text-right font-medium">最后收信</th>
                    <th className="px-3 py-1.5 text-right font-medium">切换</th>
                  </tr>
                </thead>
                <tbody>
                  {aliases.map((row) => (
                    <tr key={row.hme} className="border-b border-border/60 last:border-0">
                      <td className="px-3 py-1.5">
                        <Badge tone={row.state === "active" ? "ok" : row.state === "blocked" ? "bad" : "neutral"}>{row.state}</Badge>
                      </td>
                      <td className="max-w-[260px] truncate px-2 py-1.5 font-mono text-[11px]">{row.hme}</td>
                      <td className="px-2 py-1.5 text-right font-mono text-[11px]">{row.total}</td>
                      <td className="px-2 py-1.5 text-right">
                        {row.riskHits > 0 ? <Badge tone="bad">{row.riskHits}</Badge> : <span className="text-[11px] text-muted-foreground">0</span>}
                      </td>
                      <td className="px-2 py-1.5"><Badge tone={TAG_TONE[row.lastTag] || "neutral"}>{row.lastTag || "-"}</Badge></td>
                      <td className="px-2 py-1.5 text-right text-[11px] text-muted-foreground">
                        {row.lastTs ? timeAgo(new Date(row.lastTs * 1000).toISOString()) : "-"}
                      </td>
                      <td className="px-3 py-1.5 text-right">
                        <select className="field w-[92px] py-0.5 text-[11px]" value={row.state} onChange={(e) => setAliasState(row, e.target.value)}>
                          <option value="active">active</option>
                          <option value="paused">paused</option>
                          <option value="blocked">blocked</option>
                        </select>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        </Card>
      )}

      {tab === "messages" && (
        <Card
          title="邮件"
          bodyClass="p-0"
          actions={
            <select className="field w-[130px] py-1 text-[12px]" value={tag} onChange={(e) => setTag(e.target.value)}>
              <option value="">全部分类</option>
              {Object.keys(tagCounts).map((key) => (
                <option key={key} value={key}>{key}（{tagCounts[key]}）</option>
              ))}
            </select>
          }
        >
          <div className="max-h-[620px] overflow-auto">
            {messages.length === 0 ? (
              <div className="p-4"><Empty>暂无邮件</Empty></div>
            ) : (
              <table className="tbl">
                <thead>
                  <tr className="border-b border-border">
                    <th className="px-3 py-1.5 text-left font-medium">分类</th>
                    <th className="px-2 py-1.5 text-left font-medium">主题</th>
                    <th className="px-2 py-1.5 text-left font-medium">收件人</th>
                    <th className="px-2 py-1.5 text-left font-medium">发件人</th>
                    <th className="px-3 py-1.5 text-right font-medium">时间</th>
                  </tr>
                </thead>
                <tbody>
                  {messages.map((row) => (
                    <tr
                      key={row.uid + row.alias}
                      onClick={() => setOpenMsg(row)}
                      className={`cursor-pointer border-b border-border/60 last:border-0 hover:bg-accent/40 ${
                        openMsg?.uid === row.uid ? "bg-brand/10" : ""
                      }`}
                    >
                      <td className="px-3 py-1.5"><Badge tone={TAG_TONE[row.primaryTag] || "neutral"}>{row.primaryTag || "other"}</Badge></td>
                      <td className="max-w-[320px] truncate px-2 py-1.5" title={row.subject}>{row.subject || "(无主题)"}</td>
                      <td className="max-w-[180px] truncate px-2 py-1.5 font-mono text-[11px] text-muted-foreground">{row.alias || "-"}</td>
                      <td className="max-w-[180px] truncate px-2 py-1.5 text-[11px] text-muted-foreground">{row.from || "-"}</td>
                      <td className="px-3 py-1.5 text-right text-[11px] text-muted-foreground">
                        {row.sentAt ? timeAgo(new Date(row.sentAt * 1000).toISOString()) : "-"}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        </Card>
      )}

      <Drawer
        open={!!editSource}
        title={editSource?.name || "编辑源"}
        subtitle={editSource ? `${editSource.host}:${editSource.port} · ${editSource.username || "未填账号"}` : ""}
        onClose={() => { setEditSource(null); setDraft(null); }}
        footer={
          <>
            <button
              className="btn-danger px-2 py-1 text-[11px]"
              disabled={!editSource || !!busy}
              onClick={async () => { if (editSource) { await removeSource(editSource); setEditSource(null); setDraft(null); } }}
            >
              删除
            </button>
            <button
              className="btn-primary px-3 py-1 text-[11px]"
              disabled={!draft || !!busy}
              onClick={async () => {
                if (!draft) return;
                await act("保存源", () => call("/sources", {
                  method: "POST",
                  body: JSON.stringify({
                    id: draft.id, name: draft.name, host: draft.host, port: draft.port,
                    username: draft.username, password: draft.password || "",
                    folder: draft.folder, enabled: draft.enabled, idle: draft.idle,
                    intervalSeconds: draft.intervalSeconds, heartbeatSeconds: draft.heartbeatSeconds,
                  }),
                }));
                setEditSource(null);
                setDraft(null);
              }}
            >
              保存
            </button>
          </>
        }
      >
        {draft && (
          <div className="space-y-2">
            {[
              { k: "name", label: "名称（唯一）", type: "text" },
              { k: "host", label: "IMAP host", type: "text" },
              { k: "username", label: "账号", type: "text" },
              { k: "folder", label: "文件夹", type: "text" },
            ].map((field) => (
              <div key={field.k}>
                <label className="label">{field.label}</label>
                <input
                  className="field py-1.5 text-[12.5px]"
                  value={String((draft as unknown as Record<string, unknown>)[field.k] ?? "")}
                  onChange={(e) => setDraft({ ...draft, [field.k]: e.target.value } as SourceRow)}
                />
              </div>
            ))}
            <div className="grid grid-cols-2 gap-2">
              <div>
                <label className="label">端口</label>
                <input className="field py-1.5 text-[12.5px]" value={draft.port}
                       onChange={(e) => setDraft({ ...draft, port: Number(e.target.value) || 993 })} />
              </div>
              <div>
                <label className="label">interval（秒）</label>
                <input className="field py-1.5 text-[12.5px]" value={draft.intervalSeconds}
                       onChange={(e) => setDraft({ ...draft, intervalSeconds: Number(e.target.value) || 10 })} />
              </div>
            </div>
            <div>
              <label className="label">密码（留空 = 保持原密码）</label>
              <input className="field py-1.5 text-[12.5px]" type="password" placeholder={draft.passwordSet ? "已保存，留空不改" : "未设置"}
                     onChange={(e) => setDraft({ ...draft, password: e.target.value } as SourceRow)} />
            </div>
            <div className="grid grid-cols-2 gap-2 pt-1">
              <label className="flex items-center gap-2 text-[12px]">
                <input type="checkbox" checked={draft.enabled} onChange={(e) => setDraft({ ...draft, enabled: e.target.checked })} />
                启用
              </label>
              <label className="flex items-center gap-2 text-[12px]">
                <input type="checkbox" checked={draft.idle} onChange={(e) => setDraft({ ...draft, idle: e.target.checked })} />
                IDLE 推送
              </label>
            </div>
            <p className="pt-1 text-[11px] leading-relaxed text-muted-foreground">
              保存后源立即生效；正在运行的 worker 会在下一轮读取。
            </p>
          </div>
        )}
      </Drawer>

      {openMsg && (
        <Card
          title={openMsg.subject || "(无主题)"}
          actions={
            <>
              <Badge tone={TAG_TONE[openMsg.primaryTag] || "neutral"}>{openMsg.primaryTag || "other"}</Badge>
              <button className="btn-ghost px-2 py-1 text-[11px]" onClick={() => setOpenMsg(null)}>关闭</button>
            </>
          }
          bodyClass="p-0"
        >
          <table className="tbl">
            <tbody>
              {[
                { k: "From", v: openMsg.from || "-" },
                { k: "To", v: openMsg.alias || openMsg.to || "-" },
                { k: "时间", v: openMsg.sentAt ? new Date(openMsg.sentAt * 1000).toLocaleString() : "-" },
                { k: "UID", v: openMsg.uid },
              ].map((row) => (
                <tr key={row.k} className="border-b border-border/60 last:border-0">
                  <td className="w-[70px] px-4 py-1.5 text-[11px] text-muted-foreground">{row.k}</td>
                  <td className="truncate px-2 py-1.5 font-mono text-[11px]">{row.v}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <div className="max-h-[420px] overflow-auto border-t border-border p-4">
            {openMsg.text ? (
              <pre className="whitespace-pre-wrap break-words font-sans text-[12.5px] leading-relaxed text-foreground/90">
                {openMsg.text}
              </pre>
            ) : (
              <Empty>这封邮件没有索引到正文（可能是纯 HTML 或超长）</Empty>
            )}
          </div>
        </Card>
      )}
    </div>
  );
}
