"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { api, AccountRow } from "@/lib/api";
import { Badge, Empty, ErrorNote, StatusBadge } from "@/components/ui";
import { Drawer } from "@/components/drawer";
import { ConfigEditor } from "@/components/config-editor";
import { CodeLookup, ImportMailboxes, MailActionConfirm, MailboxEditor, PendingMailSync, PruneMail, SourceEditor, SourceSelect, SYNC_STORAGE_KEY, SyncMail } from "@/components/icmail-forms";
import { MAIL_STATES, MAIL_TAGS, Mailbox, MailConfig, MailMessage, MailRun, MailSource, mailRequest, mailTime, mailTone, useMailData } from "@/lib/icmail";

type Tab = "pool" | "overview" | "aliases" | "sources" | "messages" | "runs" | "settings";
const PAGES = [
  { id: "pool", label: "邮箱池", href: "/apps/icmail/", description: "管理邮箱地址、收件来源与取码状态。" },
  { id: "sources", label: "收件来源", href: "/apps/icmail/sources/", description: "连接收件箱，自动索引新邮件；配置保存后自动应用。" },
  { id: "messages", label: "收信与取码", href: "/apps/icmail/messages/", description: "按邮箱精确查信，查看分类、正文与验证码。" },
  { id: "runs", label: "同步记录", href: "/apps/icmail/runs/", description: "查看每次手工同步的进度与结果，处理失败或中断。" },
  { id: "settings", label: "邮箱设置", href: "/apps/icmail/settings/", description: "集中管理各类邮箱连接、公开取码与保留设置。" },
];

function MailIcon() {
  return <svg aria-hidden="true" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.4" className="h-6 w-6"><rect x="3" y="5" width="18" height="14" rx="3" /><path d="m4 7 6.4 5a2.6 2.6 0 0 0 3.2 0L20 7" /></svg>;
}
function PageFooter({ total, offset, setOffset }: { total?: number; offset: number; setOffset: (value: number) => void }) {
  return <footer className="sg-task-footer"><span>{total === undefined ? "正在读取…" : total ? `${offset + 1}–${Math.min(offset + 25, total)} / ${total}` : "0 条记录"}</span><div className="flex gap-2"><button className="btn-ghost" disabled={!offset} onClick={() => setOffset(Math.max(0, offset - 25))}>上一页</button><button className="btn-ghost" disabled={total === undefined || offset + 25 >= total} onClick={() => setOffset(offset + 25)}>下一页</button></div></footer>;
}
function ListEmpty({ loading, error, hasData, children }: { loading: boolean; error: string; hasData: boolean; children: string }) {
  return <Empty>{loading && !hasData ? "正在读取…" : error && !hasData ? "尚未读取到记录，请重试。" : children}</Empty>;
}
function Count({ total, loading }: { total?: number; loading?: boolean }) {
  return <span className="sg-description tabular-nums" aria-live="polite">{total === undefined ? "读取中…" : `共 ${total} 条`}{loading && total !== undefined ? " · 刷新中" : ""}</span>;
}

export function IcmailView({ tab }: { tab: Tab }) {
  const active = tab === "overview" || tab === "aliases" ? "pool" : tab;
  const page = PAGES.find(item => item.id === active)!;
  const [refresh, setRefresh] = useState(0);
  const [notice, setNotice] = useState("");
  const [sync, setSync] = useState<number | null>(null);
  const [pending, setPending] = useState<PendingMailSync | null>(null);
  const sourceData = useMailData<{ sources: MailSource[] }>("/sources", refresh);
  const sources = sourceData.data?.sources || [];
  useEffect(() => {
    try {
      const saved = JSON.parse(sessionStorage.getItem(SYNC_STORAGE_KEY) || "null");
      if (saved && typeof saved.request_id === "string" && Number.isInteger(saved.source_id)) setPending(saved);
    } catch { /* No persisted pending request. */ }
  }, []);
  const reload = () => setRefresh(n => n + 1);
  const saved = (message = "设置已保存。") => { setNotice(message); reload(); };
  return <div className="sg-workspace sg-mail-workspace">
    <header className="sg-page-head"><div className="flex items-center gap-4"><span className="sg-mail-emblem"><MailIcon /></span><div><p className="sg-eyebrow">供给 · 邮箱</p><h1>邮箱池管理</h1><p className="sg-description">{page.description}</p></div></div><button className="btn-ghost" onClick={reload}>刷新</button></header>
    <nav aria-label="邮箱池工作区" className="sg-account-nav sg-mail-nav">{PAGES.map(item => <Link key={item.id} href={item.href} aria-current={active === item.id ? "page" : undefined}>{item.label}</Link>)}</nav>
    <ErrorNote message={sourceData.error} />
    {notice && <p className="sg-mail-notice" role="status">{notice}</p>}
    {pending && <div className="sg-mail-recovery"><div><strong className="text-sm">有一项同步请求尚未确认</strong><p className="sg-description">原请求编号已保留，可以核对受理结果。</p></div><button className="btn-ghost" onClick={() => setSync(pending.source_id)}>核对原请求</button></div>}
    {active === "pool" && <PoolPage sources={sources} refresh={refresh} onSaved={saved} />}
    {active === "sources" && <SourcesPage sources={sources} loading={sourceData.loading} loaded={!!sourceData.data} error={sourceData.error} onSaved={saved} onSync={setSync} />}
    {active === "messages" && <MessagesPage sources={sources} refresh={refresh} onSaved={saved} />}
    {active === "runs" && <RunsPage sources={sources} refresh={refresh} onSync={setSync} />}
    {active === "settings" && <MailSettings refresh={refresh} />}
    {sync !== null && <SyncMail sources={sources} sourceId={sync} pending={pending} onPending={setPending} onClose={() => setSync(null)} />}
  </div>;
}

function PoolPage({ sources, refresh, onSaved }: { sources: MailSource[]; refresh: number; onSaved: (message?: string) => void }) {
  const [kind, setKind] = useState("ic");
  const [query, setQuery] = useState("");
  const [state, setState] = useState("");
  const [source, setSource] = useState(0);
  const [offset, setOffset] = useState(0);
  const [importing, setImporting] = useState(false);
  const [edit, setEdit] = useState<Mailbox | null>(null);
  const [code, setCode] = useState<string | null>(null);
  useEffect(() => { const p = new URLSearchParams(window.location.search); setSource(Number(p.get("source") || 0)); setQuery(p.get("q") || ""); if (["outlook", "emails"].includes(p.get("pool") || "")) setKind(p.get("pool")!); }, []);
  const changeKind = (value: string) => { setKind(value); setOffset(0); const url = new URL(window.location.href); url.searchParams.set("pool", value); window.history.replaceState(null, "", url); };
  return <>
    <div className="sg-mail-typebar"><div className="sg-mail-segments" role="group" aria-label="邮箱池类型">{[["ic", "IC 地址池"], ["outlook", "Outlook"], ["emails", "通用邮箱"]].map(([key, label]) => <button key={key} aria-pressed={kind === key} onClick={() => changeKind(key)}>{label}</button>)}</div><p className="sg-description">{kind === "ic" ? "先登记地址，再自动收信" : "沿用原账号记录与凭据"}</p></div>
    {kind === "ic" ? <ICAddresses sources={sources} refresh={refresh} query={query} state={state} source={source} offset={offset} onQuery={v => { setQuery(v); setOffset(0); }} onState={v => { setState(v); setOffset(0); }} onSource={v => { setSource(v); setOffset(0); }} setOffset={setOffset} onImport={() => setImporting(true)} onEdit={setEdit} onCode={setCode} /> : <AccountMailPool key={kind} kind={kind} refresh={refresh} />}
    {importing && <ImportMailboxes sources={sources} onClose={() => setImporting(false)} onSaved={onSaved} />}
    {edit && <MailboxEditor mailbox={edit} sources={sources} onClose={() => setEdit(null)} onSaved={() => onSaved()} />}
    {code !== null && <CodeLookup email={code} onClose={() => setCode(null)} />}
  </>;
}

function ICAddresses({ sources, refresh, query, state, source, offset, onQuery, onState, onSource, setOffset, onImport, onEdit, onCode }: { sources: MailSource[]; refresh: number; query: string; state: string; source: number; offset: number; onQuery: (v: string) => void; onState: (v: string) => void; onSource: (v: number) => void; setOffset: (v: number) => void; onImport: () => void; onEdit: (v: Mailbox) => void; onCode: (v: string) => void }) {
  const [search, setSearch] = useState(query);
  useEffect(() => setSearch(query), [query]);
  const params = new URLSearchParams({ q: query, state, source_id: String(source), limit: "25", offset: String(offset) });
  const { data, error, loading } = useMailData<{ items: Mailbox[]; total: number }>(`/pool?${params}`, refresh);
  useEffect(() => { if (data && offset > 0 && offset >= data.total) setOffset(Math.max(0, Math.ceil(data.total / 25) - 1) * 25); }, [data, offset, setOffset]);
  return <section className="sg-task-surface">
    <header className="sg-task-toolbar"><h2 className="text-sm font-semibold">IC 地址池</h2><Count total={data?.total} loading={loading} /><div className="sg-mail-actions"><button className="btn-ghost" onClick={() => onCode("")}>查验证码</button><button className="btn-primary" onClick={onImport}>导入地址</button></div></header>
    <form className="sg-mail-filters" onSubmit={e => { e.preventDefault(); onQuery(search.trim()); }} onKeyDown={e => { if (e.key === "Enter" && e.nativeEvent.isComposing) e.preventDefault(); }}>
      <input className="field" aria-label="搜索邮箱地址" placeholder="搜索邮箱地址" value={search} onChange={e => setSearch(e.target.value)} />
      <select className="field" aria-label="筛选取码状态" value={state} onChange={e => onState(e.target.value)}><option value="">全部状态</option>{Object.entries(MAIL_STATES).map(([key, label]) => <option value={key} key={key}>{label}</option>)}</select>
      <SourceSelect sources={sources} value={source} onChange={onSource} allLabel="全部收件来源" /><button className="btn-ghost" type="submit">搜索</button>
    </form>
    <div className="px-5"><ErrorNote message={error} /></div>
    <div className="overflow-auto"><table className="tbl sg-task-table sg-mail-table"><thead><tr><th>邮箱地址</th><th>收件来源</th><th>取码状态</th><th className="text-right">邮件 / 风险</th><th>最近收信</th><th>操作</th></tr></thead><tbody>{data?.items.map(row => <tr key={row.email}>
      <td><button className="sg-mail-identity" onClick={() => onEdit(row)}>{row.email}</button><p className="sg-description">{row.origin === "discovered" ? "收信发现" : row.origin === "manual" ? "手工登记" : "导入地址"}</p></td>
      <td>{row.sourceName || (row.sourceId ? "来源已移除" : "未绑定")}{row.sourceName && !row.sourceEnabled && <p className="text-xs text-warning">来源已停用</p>}</td>
      <td><Badge tone={mailTone(row.state)}>{MAIL_STATES[row.state] || row.state}</Badge></td><td className="tabular-nums">{row.total} / <span className={row.riskHits ? "text-destructive" : "text-muted-foreground"}>{row.riskHits}</span></td><td className="text-xs">{mailTime(row.lastAt)}</td>
      <td><div className="flex gap-2"><button className="btn-ghost" onClick={() => onCode(row.email)} disabled={row.state !== "active"}>取码</button><Link className="btn-ghost" href={`/apps/icmail/messages/?email=${encodeURIComponent(row.email)}`}>邮件</Link><button className="btn-ghost" onClick={() => onEdit(row)}>设置</button></div></td>
    </tr>)}</tbody></table>{!data?.items.length && <ListEmpty loading={loading} error={error} hasData={!!data}>{query || state || source ? "没有符合筛选条件的邮箱。" : "还没有邮箱地址。可先导入地址，也可由收信自动发现。"}</ListEmpty>}</div>
    <PageFooter total={data?.total} offset={offset} setOffset={setOffset} />
  </section>;
}

function AccountMailPool({ kind, refresh }: { kind: string; refresh: number }) {
  const [data, setData] = useState<{ accounts: AccountRow[]; total: number } | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [offset, setOffset] = useState(0);
  const [detail, setDetail] = useState<AccountRow | null>(null);
  const [closing, setClosing] = useState(false);
  useEffect(() => {
    const controller = new AbortController(); let timer: ReturnType<typeof setTimeout>;
    const load = async () => {
      setLoading(true);
      try {
        const next = await api.accounts({ ...(kind === "outlook" ? { platform: "outlook" } : { kind: "emails" }), limit: 25, offset }, AbortSignal.any([controller.signal, AbortSignal.timeout(15000)]));
        if (!controller.signal.aborted) { setData(next); setError(""); }
      } catch (err) { if (!controller.signal.aborted) setError("读取失败，保留上次内容：" + (err as Error).message); }
      finally { if (!controller.signal.aborted) { setLoading(false); timer = setTimeout(load, 25000); } }
    };
    void load(); return () => { controller.abort(); clearTimeout(timer); };
  }, [kind, refresh, offset]);
  const href = kind === "outlook" ? "/platform/outlook/accounts/" : "/accounts/";
  return <section className="sg-task-surface"><header className="sg-task-toolbar"><h2 className="text-sm font-semibold">{kind === "outlook" ? "Outlook 邮箱" : "通用邮箱池"}</h2><Count total={data?.total} loading={loading} /><Link className="btn-ghost sg-mail-actions" href={href}>管理账号</Link></header><div className="px-5"><ErrorNote message={error} /></div>
    <div className="overflow-auto"><table className="tbl sg-task-table sg-mail-table"><thead><tr><th>邮箱</th><th>平台</th><th>状态</th><th>最近检查</th><th>操作</th></tr></thead><tbody>{data?.accounts.map(row => <tr key={row.ref}><td><button className="sg-mail-identity" onClick={() => setDetail(row)}>{row.identity || row.ref}</button></td><td>{row.platform}</td><td><Badge tone={row.status === "ok" ? "ok" : row.status === "unknown" ? "neutral" : "warn"}>{row.status === "ok" ? "正常" : row.status === "unknown" ? "尚未确认" : row.status}</Badge></td><td>{mailTime(row.health_checked_at)}</td><td><button className="btn-ghost" onClick={() => setDetail(row)}>详情</button></td></tr>)}</tbody></table>{!data?.accounts.length && <ListEmpty loading={loading} error={error} hasData={!!data}>当前邮箱池没有记录。</ListEmpty>}</div><PageFooter total={data?.total} offset={offset} setOffset={setOffset} />
    {detail && <Drawer open={!closing} title="邮箱账号" subtitle={detail.identity} onClose={() => setClosing(true)} onClosed={() => { setDetail(null); setClosing(false); }} footer={<Link className="btn-primary" href={href}>进入账号管理</Link>}><dl className="space-y-4 text-sm"><div><dt className="sg-description">账号编号</dt><dd className="break-all font-mono mt-1">{detail.ref}</dd></div><div><dt className="sg-description">平台</dt><dd>{detail.platform}</dd></div><div><dt className="sg-description">最近检查</dt><dd>{mailTime(detail.health_checked_at)}</dd></div><div><dt className="sg-description">原账号状态</dt><dd>{detail.status}</dd></div></dl></Drawer>}
  </section>;
}

function SourcesPage({ sources, loading, loaded, error, onSaved, onSync }: { sources: MailSource[]; loading: boolean; loaded: boolean; error: string; onSaved: (message?: string) => void; onSync: (id: number) => void }) {
  const [edit, setEdit] = useState<MailSource | "new" | null>(null);
  const [remove, setRemove] = useState<MailSource | null>(null);
  const [busy, setBusy] = useState(0);
  const [actionError, setActionError] = useState("");
  const toggle = async (row: MailSource) => { setBusy(row.id); setActionError(""); try { await mailRequest(`/sources/${row.id}/toggle`, { method: "POST", body: JSON.stringify({ enabled: !row.enabled }) }); onSaved(row.enabled ? "来源已停用，原收信记录仍保留。" : "来源已启用，正在应用连接配置。"); } catch (err) { setActionError((err as Error).message); } finally { setBusy(0); } };
  return <section className="sg-task-surface"><header className="sg-task-toolbar"><h2 className="text-sm font-semibold">收件来源</h2><Count total={loaded ? sources.length : undefined} loading={loading} /><div className="sg-mail-actions"><button className="btn-ghost" disabled={!sources.some(s => s.enabled)} onClick={() => onSync(0)}>同步全部</button><button className="btn-primary" onClick={() => setEdit("new")}>新增来源</button></div></header><div className="px-5"><ErrorNote message={actionError} /></div>
    <div className="overflow-auto"><table className="tbl sg-task-table sg-mail-table"><thead><tr><th>来源 / 账号</th><th>连接</th><th>运行状态</th><th>最近成功收取</th><th>操作</th></tr></thead><tbody>{sources.map(row => <tr key={row.id}><td><button className="sg-mail-identity" onClick={() => setEdit(row)}>{row.name}</button><p className="sg-description">{row.username}</p></td><td><span>{row.host}:{row.port}</span><p className="sg-description">{row.folder} · {row.idle ? "推送优先" : `${row.heartbeatSeconds} 秒轮询`}</p></td><td><Badge tone={!row.enabled ? "neutral" : row.lastError ? "bad" : row.runtime === "running" && row.lastOKAt ? "ok" : "info"}>{!row.enabled ? "已停用" : row.lastError ? "连接异常" : row.runtime === "running" && row.lastOKAt ? "收件中" : "正在连接"}</Badge>{row.lastError && <p className="sg-mail-error-detail" title={row.lastError}>{row.lastError}</p>}</td><td className="text-xs">{mailTime(row.lastOKAt)}<p className="sg-description">游标 {row.lastUID}</p></td><td><div className="flex gap-2"><button className="btn-ghost" onClick={() => setEdit(row)}>编辑</button><button className="btn-ghost" disabled={!row.enabled} onClick={() => onSync(row.id)}>同步</button><button className="btn-ghost" disabled={!!busy} onClick={() => void toggle(row)}>{busy === row.id ? "处理中…" : row.enabled ? "停用" : "启用"}</button><button className="btn-danger" disabled={!!busy} onClick={() => setRemove(row)}>移除</button></div></td></tr>)}</tbody></table>{!sources.length && <ListEmpty loading={loading} error={error} hasData={loaded}>尚未添加收件来源。添加后即可自动收信。</ListEmpty>}</div>
    {edit && <SourceEditor source={edit === "new" ? undefined : edit} onClose={() => setEdit(null)} onSaved={() => onSaved("来源已保存，运行器正在应用配置。")} />}
    {remove && <MailActionConfirm title="移除收件来源" description={`移除「${remove.name}」将停止该连接，未执行的同步会取消。已收邮件、邮箱地址和运行证据保留；关联地址之后需要重新绑定来源。`} label="确认移除" onClose={() => setRemove(null)} action={async () => { await mailRequest(`/sources/${remove.id}`, { method: "DELETE" }); onSaved("来源已移除，历史记录仍保留。"); }} />}
  </section>;
}

function MessagesPage({ sources, refresh, onSaved }: { sources: MailSource[]; refresh: number; onSaved: (message?: string) => void }) {
  const [email, setEmail] = useState("");
  const [draft, setDraft] = useState("");
  const [tag, setTag] = useState("");
  const [source, setSource] = useState(0);
  const [offset, setOffset] = useState(0);
  const [detail, setDetail] = useState<MailMessage | null>(null);
  const [closing, setClosing] = useState(false);
  const [code, setCode] = useState<string | null>(null);
  const [prune, setPrune] = useState(false);
  useEffect(() => { const address = new URLSearchParams(window.location.search).get("email") || ""; setEmail(address); setDraft(address); }, []);
  const params = new URLSearchParams({ alias: email, tag, source_id: String(source), offset: String(offset), limit: "25" });
  const { data, error, loading } = useMailData<{ messages: MailMessage[]; total: number }>(`/messages?${params}`, refresh);
  useEffect(() => { if (data && offset && offset >= data.total) setOffset(Math.max(0, Math.ceil(data.total / 25) - 1) * 25); }, [data, offset]);
  return <section className="sg-task-surface"><header className="sg-task-toolbar"><h2 className="text-sm font-semibold">收信记录</h2><Count total={data?.total} loading={loading} /><div className="sg-mail-actions"><button className="btn-ghost" onClick={() => setPrune(true)}>清理旧邮件</button><button className="btn-primary" onClick={() => setCode(email)}>查验证码</button></div></header>
    <form className="sg-mail-filters" onSubmit={e => { e.preventDefault(); setEmail(draft.trim()); setOffset(0); }} onKeyDown={e => { if (e.key === "Enter" && e.nativeEvent.isComposing) e.preventDefault(); }}><input className="field" aria-label="精确邮箱地址" placeholder="精确邮箱地址" value={draft} onChange={e => setDraft(e.target.value)} /><select className="field" aria-label="邮件分类" value={tag} onChange={e => { setTag(e.target.value); setOffset(0); }}><option value="">全部分类</option>{Object.entries(MAIL_TAGS).map(([key, label]) => <option key={key} value={key}>{label}</option>)}</select><SourceSelect sources={sources} value={source} onChange={v => { setSource(v); setOffset(0); }} allLabel="全部收件来源" /><button type="submit" className="btn-ghost">查找</button></form>
    <div className="px-5"><ErrorNote message={error} /></div><div className="overflow-auto"><table className="tbl sg-task-table sg-mail-table"><thead><tr><th>邮件</th><th>收件地址</th><th>分类</th><th>收信时间</th><th>操作</th></tr></thead><tbody>{data?.messages.map(row => <tr key={row.id}><td className="sg-mail-subject"><button className="sg-mail-identity" onClick={() => setDetail(row)}>{row.subject || "无主题"}</button><p className="sg-description" title={row.from}>{row.from || "未知发件人"}</p></td><td className="sg-mail-recipient" title={row.to}>{row.alias || row.to || "—"}</td><td><Badge tone={mailTone(row.primaryTag)}>{MAIL_TAGS[row.primaryTag] || row.primaryTag}</Badge></td><td className="text-xs">{mailTime(row.sentAt)}</td><td><button className="btn-ghost" onClick={() => setDetail(row)}>查看邮件</button></td></tr>)}</tbody></table>{!data?.messages.length && <ListEmpty loading={loading} error={error} hasData={!!data}>没有符合条件的邮件。收信来源启用后会自动同步。</ListEmpty>}</div><PageFooter total={data?.total} offset={offset} setOffset={setOffset} />
    {detail && <Drawer open={!closing} width={720} title={detail.subject || "邮件详情"} subtitle={mailTime(detail.sentAt)} onClose={() => setClosing(true)} onClosed={() => { setDetail(null); setClosing(false); }}><dl className="sg-mail-meta"><dt>发件人</dt><dd>{detail.from}</dd><dt>收件人</dt><dd>{detail.to}</dd><dt>来源</dt><dd>{sources.find(s => s.id === detail.sourceId)?.name || "来源已移除"}</dd><dt>分类</dt><dd><Badge tone={mailTone(detail.primaryTag)}>{MAIL_TAGS[detail.primaryTag] || detail.primaryTag}</Badge></dd></dl><pre className="sg-mail-body">{detail.text || detail.snippet || "无可显示的正文"}</pre></Drawer>}
    {code !== null && <CodeLookup email={code} onClose={() => setCode(null)} />}
    {prune && <PruneMail onClose={() => setPrune(false)} onSaved={() => onSaved("旧邮件已清理，邮箱地址与状态保留。")} />}
  </section>;
}

function RunsPage({ sources, refresh, onSync }: { sources: MailSource[]; refresh: number; onSync: (id: number) => void }) {
  const [request, setRequest] = useState("");
  const [offset, setOffset] = useState(0);
  const [detail, setDetail] = useState<MailRun | null>(null);
  const [closing, setClosing] = useState(false);
  const [retryAfterClose, setRetryAfterClose] = useState(false);
  useEffect(() => { setRequest(new URLSearchParams(window.location.search).get("request") || ""); }, []);
  const params = new URLSearchParams({ request_id: request, limit: "25", offset: String(offset) });
  const { data, error, loading } = useMailData<{ items: MailRun[]; total: number }>(`/sync?${params}`, refresh, 5000);
  const currentDetail = data?.items.find(r => r.id === detail?.id) || detail;
  const stateLabel = (row: MailRun) => row.state === "pending" ? <Badge tone="info">排队中</Badge> : <StatusBadge status={row.state} />;
  const canRetry = (row: MailRun) => ["failed", "interrupted", "cancelled", "completed"].includes(row.state) && sources.some(s => s.id === row.sourceId && s.enabled);
  return <section className="sg-task-surface"><header className="sg-task-toolbar"><div><h2 className="text-sm font-semibold">{request ? "本次同步" : "同步记录"}</h2><Count total={data?.total} loading={loading} /></div><div className="sg-mail-actions">{request && <button className="btn-ghost" onClick={() => { setRequest(""); setOffset(0); window.history.replaceState(null, "", "/apps/icmail/runs/"); }}>全部记录</button>}<button className="btn-primary" disabled={!sources.some(s => s.enabled)} onClick={() => onSync(0)}>同步来源</button></div></header>
    {request && <p className="sg-description px-5 pb-4 break-all">请求编号 <span className="font-mono">{request}</span></p>}
    <div className="px-5"><ErrorNote message={error} /></div><div className="overflow-auto"><table className="tbl sg-task-table sg-mail-table"><thead><tr><th>来源 / 请求</th><th>状态</th><th>新增邮件</th><th>发起 / 完成</th><th>结果</th><th>操作</th></tr></thead><tbody>{data?.items.map(row => <tr key={row.id}><td><button className="sg-mail-identity" onClick={() => setDetail(row)}>{row.sourceName}</button><p className="sg-description font-mono" title={row.requestId}>{row.requestId ? row.requestId.slice(0, 12) : `历史记录 #${row.id}`}</p></td><td>{stateLabel(row)}</td><td className="tabular-nums">{row.indexed}{["running", "pending"].includes(row.state) ? " · 处理中" : " 封"}</td><td className="text-xs">{mailTime(row.requestedAt)}<p className="sg-description">{row.finishedAt ? mailTime(row.finishedAt) : "尚未结束"}</p></td><td className="sg-mail-result">{row.note || (row.state === "pending" ? "等待收件器领取" : "正在收取与索引")}</td><td><div className="flex gap-2"><button className="btn-ghost" onClick={() => setDetail(row)}>详情</button>{canRetry(row) && <button className="btn-ghost" onClick={() => onSync(row.sourceId)}>{row.state === "completed" ? "再次同步" : "重试"}</button>}{!canRetry(row) && ["failed", "interrupted", "cancelled"].includes(row.state) && <Link className="btn-ghost" href="/apps/icmail/sources/">处理来源</Link>}</div></td></tr>)}</tbody></table>{!data?.items.length && <ListEmpty loading={loading} error={error} hasData={!!data}>暂无手工同步记录。自动收件情况可在收件来源查看。</ListEmpty>}</div><PageFooter total={data?.total} offset={offset} setOffset={setOffset} />
    {currentDetail && <Drawer open={!closing} title="同步详情" subtitle={currentDetail.sourceName} onClose={() => setClosing(true)} onClosed={() => { setDetail(null); setClosing(false); if (retryAfterClose) onSync(currentDetail.sourceId); setRetryAfterClose(false); }} footer={canRetry(currentDetail) ? <button className="btn-primary" onClick={() => { setRetryAfterClose(true); setClosing(true); }}>再次同步</button> : <Link href="/apps/icmail/sources/" className="btn-ghost">查看收件来源</Link>}><div className="space-y-5">{stateLabel(currentDetail)}<dl className="sg-mail-meta"><dt>请求编号</dt><dd className="font-mono">{currentDetail.requestId || `历史记录 #${currentDetail.id}`}</dd><dt>发起</dt><dd>{mailTime(currentDetail.requestedAt)}</dd><dt>开始</dt><dd>{mailTime(currentDetail.startedAt)}</dd><dt>结束</dt><dd>{mailTime(currentDetail.finishedAt)}</dd><dt>新增邮件</dt><dd>{currentDetail.indexed} 封</dd></dl><p className="text-sm leading-6 break-words">{currentDetail.note || "等待本次同步结果"}</p></div></Drawer>}
  </section>;
}

function MailSettings({ refresh }: { refresh: number }) {
  const { data, error } = useMailData<MailConfig>("/config", refresh, 0);
  return <><ErrorNote message={error} /><section className="sg-task-surface"><header className="sg-task-toolbar"><h2 className="text-sm font-semibold">当前生效配置</h2><Link className="btn-ghost sg-mail-actions" href="/apps/icmail/sources/">收件来源</Link></header><dl className="sg-mail-settings"><div><dt>邮件保留</dt><dd>{data ? `${data.retentionDays} 天` : "—"}</dd></div><div><dt>公开取码验证</dt><dd><Badge tone={data?.turnstile.siteKey && data.turnstile.secretSet ? "ok" : "warn"}>{data?.turnstile.siteKey && data.turnstile.secretSet ? "已配置" : "未配置"}</Badge></dd></div><div><dt>首次同步范围</dt><dd>{data ? `最近 ${data.bootstrapLimit} 封` : "—"}</dd></div></dl><p className="sg-description px-5 pb-5">下方沿用系统设置中的邮箱配置。进程配置保存后需重启生效；收件来源的编辑、启用和停用会直接应用。</p></section><ConfigEditor scope="system" area="mail" /></>;
}
