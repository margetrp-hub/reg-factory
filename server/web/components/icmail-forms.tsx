"use client";

import { ReactNode, useEffect, useRef, useState } from "react";
import { Drawer } from "@/components/drawer";
import { Badge, ErrorNote, Field } from "@/components/ui";
import { MAIL_STATES, Mailbox, MailCode, MailSource, MailRequestError, mailRequest, mailTime } from "@/lib/icmail";

function MailForm({ title, subtitle, dirty, onClose, submit, label = "保存", children }: { title: string; subtitle?: string; dirty: boolean; onClose: () => void; submit: () => Promise<void>; label?: string; children: ReactNode }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [discard, setDiscard] = useState(false);
  const [open, setOpen] = useState(true);
  useEffect(() => {
    if (!dirty && !busy) return;
    const protect = (event: BeforeUnloadEvent) => { event.preventDefault(); event.returnValue = ""; };
    window.addEventListener("beforeunload", protect);
    return () => window.removeEventListener("beforeunload", protect);
  }, [dirty, busy]);
  const close = () => { if (busy) return; if (dirty) setDiscard(true); else setOpen(false); };
  const save = async () => {
    if (busy) return;
    setBusy(true); setError("");
    try { await submit(); setOpen(false); } catch (err) { setError((err as Error).message); } finally { setBusy(false); }
  };
  return <Drawer variant="dialog" width={640} open={open} onClose={close} onClosed={onClose} title={title} subtitle={subtitle} footer={discard ? <><span className="mr-auto text-sm">放弃未保存的内容？</span><button className="btn-ghost" onClick={() => setDiscard(false)}>继续编辑</button><button className="btn-danger" onClick={() => setOpen(false)}>放弃修改</button></> : <><button className="btn-ghost" disabled={busy} onClick={close}>取消</button><button className="btn-primary" disabled={busy} onClick={() => void save()}>{busy ? "处理中…" : label}</button></>}>
    <div className="space-y-4">{children}<ErrorNote message={error} /></div>
  </Drawer>;
}

export function SourceEditor({ source, onClose, onSaved }: { source?: MailSource; onClose: () => void; onSaved: () => void }) {
  const initial = { id: source?.id || 0, name: source?.name || "", host: source?.host || "", port: source?.port || 993, username: source?.username || "", password: "", folder: source?.folder || "INBOX", enabled: source?.enabled ?? true, idle: source?.idle ?? true, intervalSeconds: source?.intervalSeconds || 10, heartbeatSeconds: source?.heartbeatSeconds || 60 };
  const [draft, setDraft] = useState(initial);
  const change = (key: keyof typeof draft, value: string | number | boolean) => setDraft(old => ({ ...old, [key]: value }));
  return <MailForm title={source ? "编辑收件来源" : "新增收件来源"} subtitle="保存后自动应用连接设置。应用专用密码留空表示保留；已有收信的账号、服务器或文件夹需要通过新建来源更换。" dirty={JSON.stringify(draft) !== JSON.stringify(initial)} onClose={onClose} submit={async () => {
    if (!draft.name.trim() || !draft.host.trim() || !draft.username.trim()) throw new Error("请填写名称、服务器和登录账号");
    if (!source?.passwordSet && !draft.password) throw new Error("请填写应用专用密码");
    if (draft.port < 1 || draft.port > 65535 || draft.intervalSeconds < 5 || draft.heartbeatSeconds < 5) throw new Error("端口需要在 1–65535 之间，间隔至少为 5 秒");
    await mailRequest("/sources", { method: "POST", body: JSON.stringify({ ...draft, name: draft.name.trim(), host: draft.host.trim(), username: draft.username.trim() }) }); onSaved();
  }}>
    <Field label="来源名称"><input autoFocus className="field" value={draft.name} onChange={e => change("name", e.target.value)} placeholder="例如：业务收件箱" /></Field>
    <div className="grid gap-4 sm:grid-cols-[1fr_110px]"><Field label="IMAP 服务器"><input className="field" value={draft.host} onChange={e => change("host", e.target.value)} placeholder="imap.example.com" /></Field><Field label="端口"><input className="field" type="number" min={1} max={65535} value={draft.port} onChange={e => change("port", Number(e.target.value))} /></Field></div>
    <Field label="登录账号"><input className="field" value={draft.username} onChange={e => change("username", e.target.value)} autoComplete="off" /></Field>
    <Field label="应用专用密码" hint={source?.passwordSet ? "已设置，留空保留原密码" : "使用邮箱提供商的应用专用密码"}><input className="field" type="password" value={draft.password} onChange={e => change("password", e.target.value)} autoComplete="new-password" /></Field>
    <Field label="文件夹"><input className="field" value={draft.folder} onChange={e => change("folder", e.target.value)} /></Field>
    <div className="grid gap-4 sm:grid-cols-2"><Field label="失败重连间隔（秒）"><input className="field" type="number" min={5} value={draft.intervalSeconds} onChange={e => change("intervalSeconds", Number(e.target.value))} /></Field><Field label="轮询间隔（秒）"><input className="field" type="number" min={5} value={draft.heartbeatSeconds} onChange={e => change("heartbeatSeconds", Number(e.target.value))} /></Field></div>
    <div className="flex flex-wrap gap-5 text-sm"><label className="flex items-center gap-2"><input type="checkbox" checked={draft.enabled} onChange={e => change("enabled", e.target.checked)} />启用收件</label><label className="flex items-center gap-2"><input type="checkbox" checked={draft.idle} onChange={e => change("idle", e.target.checked)} />优先使用实时推送（IDLE）</label></div>
  </MailForm>;
}

export function SourceSelect({ sources, value, onChange, allLabel = "暂不绑定来源", disabled = false }: { sources: MailSource[]; value: number; onChange: (id: number) => void; allLabel?: string; disabled?: boolean }) {
  return <select className="field" value={value} onChange={e => onChange(Number(e.target.value))} disabled={disabled} aria-label="收件来源"><option value={0}>{allLabel}</option>{value > 0 && !sources.some(s => s.id === value) && <option value={value}>原来源已移除（{value}）</option>}{sources.map(s => <option key={s.id} value={s.id}>{s.name}{s.enabled ? "" : " · 已停用"}</option>)}</select>;
}

export function ImportMailboxes({ sources, onClose, onSaved }: { sources: MailSource[]; onClose: () => void; onSaved: (message: string) => void }) {
  const [text, setText] = useState("");
  const [source, setSource] = useState(0);
  return <MailForm title="导入邮箱地址" subtitle="每行一个地址，最多 1000 个。地址不需要先收到邮件；已有地址的来源与状态会保留。这里只登记地址，转发需在邮箱服务商处设置。" dirty={!!text || source !== 0} onClose={onClose} label="导入地址" submit={async () => {
    const emails = text.split(/\r?\n/).map(s => s.trim()).filter(Boolean);
    const result = await mailRequest<{ added: number }>("/pool/import", { method: "POST", body: JSON.stringify({ source_id: source, emails }) });
    onSaved(`已导入 ${result.added} 个新地址，已有地址保持原设置。`);
  }}><Field label="收件来源"><SourceSelect sources={sources} value={source} onChange={setSource} /></Field><Field label="邮箱地址"><textarea autoFocus rows={9} className="field font-mono" value={text} onChange={e => setText(e.target.value)} spellCheck={false} placeholder={"first@example.com\nsecond@example.com"} /></Field></MailForm>;
}

export function MailboxEditor({ mailbox, sources, onClose, onSaved }: { mailbox: Mailbox; sources: MailSource[]; onClose: () => void; onSaved: () => void }) {
  const [state, setState] = useState(mailbox.state);
  const [source, setSource] = useState(mailbox.sourceId);
  return <MailForm title="邮箱设置" subtitle={mailbox.email} dirty={state !== mailbox.state || source !== mailbox.sourceId} onClose={onClose} submit={async () => { await mailRequest("/pool/mailbox", { method: "POST", body: JSON.stringify({ email: mailbox.email, state, source_id: source }) }); onSaved(); }}>
    <Field label="收件来源"><SourceSelect sources={sources} value={source} onChange={setSource} /></Field>
    <Field label="取码状态" hint="暂停或阻止后停止提供验证码，收信与历史仍保留。"><select className="field" value={state} onChange={e => setState(e.target.value)}>{Object.entries(MAIL_STATES).map(([key, label]) => <option key={key} value={key}>{label}</option>)}</select></Field>
  </MailForm>;
}

export function CodeLookup({ email, onClose }: { email: string; onClose: () => void }) {
  const [open, setOpen] = useState(true);
  const [address, setAddress] = useState(email);
  const [result, setResult] = useState<MailCode | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [copied, setCopied] = useState(false);
  const request = useRef<AbortController | null>(null);
  useEffect(() => () => request.current?.abort(), []);
  const close = () => { request.current?.abort(); setOpen(false); };
  const lookup = async () => {
    if (busy) return;
    const controller = new AbortController(); request.current = controller;
    setBusy(true); setError(""); setCopied(false); setResult(null);
    try {
      const code = await mailRequest<MailCode>("/code?email=" + encodeURIComponent(address.trim()), { signal: controller.signal });
      if (!controller.signal.aborted) setResult(code);
    } catch (err) { if (!controller.signal.aborted) setError((err as Error).message); }
    finally { if (!controller.signal.aborted) setBusy(false); }
  };
  return <Drawer variant="dialog" width={520} open={open} title="查找验证码" subtitle="读取该地址最近的收信，暂停或阻止的地址需先恢复。" onClose={close} onClosed={onClose} footer={<><button className="btn-ghost" onClick={close}>关闭</button><button className="btn-primary" onClick={() => void lookup()} disabled={busy || !address.trim()}>{busy ? "查找中…" : "查找验证码"}</button></>}>
    <Field label="邮箱地址"><input autoFocus disabled={busy} type="email" className="field" value={address} onChange={e => { setAddress(e.target.value); setResult(null); setCopied(false); setError(""); }} /></Field>
    <div aria-live="polite" className="mt-4 space-y-3"><ErrorNote message={error} />{result && <div className="rounded-xl bg-muted/60 p-5">{result.code ? <><div className="flex items-center justify-between gap-3"><strong className="font-mono text-2xl tracking-[.12em]">{result.code}</strong><button className="btn-ghost" onClick={async () => { try { await navigator.clipboard.writeText(result.code); setCopied(true); } catch { setError("复制失败，可选中验证码手动复制"); } }}>{copied ? "已复制" : "复制"}</button></div><p className="sg-description mt-3">收信于 {mailTime(result.receivedAt)}</p></> : <p className="text-sm">最近 {result.scanned} 封邮件中没有验证码。</p>}</div>}</div>
  </Drawer>;
}

export function MailActionConfirm({ title, description, label, onClose, action }: { title: string; description: string; label: string; onClose: () => void; action: () => Promise<void> }) {
  return <MailForm title={title} dirty={false} onClose={onClose} label={label} submit={action}><p className="text-sm leading-6">{description}</p></MailForm>;
}

export function PruneMail({ onClose, onSaved }: { onClose: () => void; onSaved: () => void }) {
  const [days, setDays] = useState(90);
  return <MailForm title="清理旧邮件" subtitle="只删除本地邮件索引和正文，不删除邮箱地址、手工状态或上游邮件。清理后的正文无法在这里恢复。" dirty={days !== 90} label="确认清理" onClose={onClose} submit={async () => { if (!Number.isInteger(days) || days < 1 || days > 3650) throw new Error("请输入 1–3650 天"); await mailRequest(`/prune?days=${days}`, { method: "POST" }); onSaved(); }}><Field label="保留最近多少天"><input autoFocus className="field" type="number" min={1} max={3650} value={days} onChange={e => setDays(Number(e.target.value))} /></Field><p className="sg-description">将清理 {new Date(Date.now() - days * 86400000).toLocaleDateString()} 之前的邮件。</p></MailForm>;
}

export type PendingMailSync = { request_id: string; source_id: number };
export const SYNC_STORAGE_KEY = "sg-mail-pending-sync";
export function SyncMail({ sources, sourceId, pending, onPending, onClose }: { sources: MailSource[]; sourceId: number; pending: PendingMailSync | null; onPending: (value: PendingMailSync | null) => void; onClose: () => void }) {
  const [open, setOpen] = useState(true);
  const [source, setSource] = useState(pending?.source_id ?? sourceId);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const savePending = (value: PendingMailSync | null) => { onPending(value); try { if (value) sessionStorage.setItem(SYNC_STORAGE_KEY, JSON.stringify(value)); else sessionStorage.removeItem(SYNC_STORAGE_KEY); } catch { /* Current page still retains the same request ID. */ } };
  const submit = async () => {
    if (busy) return;
    setBusy(true); setError("");
    const request = pending || { request_id: crypto.randomUUID(), source_id: source };
    savePending(request);
    try {
      const original = await mailRequest<{ total: number }>(`/sync?request_id=${encodeURIComponent(request.request_id)}`);
      if (!original.total) await mailRequest("/sync", { method: "POST", body: JSON.stringify(request) });
      savePending(null);
      window.location.assign(`/apps/icmail/runs/?request=${encodeURIComponent(request.request_id)}`);
    } catch (err) {
      if (err instanceof MailRequestError && err.status >= 400 && err.status < 500) { savePending(null); setError(err.message); }
      else setError("尚未确认受理结果。请求编号已保留，请核对原请求，避免重复发起。" + (err as Error).message);
    } finally { setBusy(false); }
  };
  return <Drawer variant="dialog" width={560} open={open} title={pending ? "核对同步请求" : "同步收件来源"} subtitle="自动收件始终按来源配置运行；手工同步会在运行记录中显示真实结果。" onClose={() => { if (!busy) setOpen(false); }} onClosed={onClose} footer={<><button className="btn-ghost" disabled={busy} onClick={() => setOpen(false)}>关闭</button><button className="btn-primary" disabled={busy || (!pending && !sources.some(s => s.enabled))} onClick={() => void submit()}>{busy ? "核对中…" : pending ? "核对并继续原请求" : "开始同步"}</button></>}>
    <div className="space-y-4"><Field label="同步范围"><SourceSelect sources={sources.filter(s => s.enabled)} value={source} onChange={setSource} allLabel="所有启用的收件来源" disabled={!!pending || busy} /></Field>{pending && <div className="rounded-lg bg-muted/60 p-3"><Badge tone="warn">等待核对</Badge><p className="mt-2 break-all font-mono text-xs">{pending.request_id}</p><p className="sg-description mt-2">先查询原编号；尚未受理时使用同一编号提交。</p></div>}<ErrorNote message={error} /></div>
  </Drawer>;
}
