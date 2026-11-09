"use client";

import { Suspense, useEffect, useRef, useState, useTransition } from "react";
import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { api, ApiError, type AccountRow } from "@/lib/api";
import { Badge, ErrorNote } from "@/components/ui";
import { TaskGroupView } from "@/components/task-group-view";
import { PLATFORM_TASKS } from "@/lib/task-catalog";
import { accountAction, pendingCredentialCheck, startCredentialCheck } from "@/lib/account-actions";
import { AccountNavigation, CHECK_LABELS, LIBRARY_LABELS, checkTone } from "@/components/account-navigation";

export function PlatformSubview({ slug, tab }: { slug: string; tab: "tasks" | "accounts" }) {
  return tab === "tasks" ? <TaskGroupView key={slug} platform={slug} /> :
    <Suspense fallback={<div className="sg-table-skeleton">正在读取账号…</div>}><PlatformAccounts key={slug} slug={slug} /></Suspense>;
}

function PlatformAccounts({ slug }: { slug: string }) {
  const name = PLATFORM_TASKS[slug]?.name || slug;
  const router = useRouter();
  const [navigating, startNavigation] = useTransition();
  const params = useSearchParams();
  const status = params.get("status") || "";
  const offset = Math.max(0, Number(params.get("offset")) || 0);
  const [page, setPage] = useState<{accounts: AccountRow[]; total: number} | null>(null);
  const [reload, setReload] = useState(0);
  const [busy, setBusy] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [actionError, setActionError] = useState("");
  const [pending, setPending] = useState(false);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const operating = useRef(false);
  const labels: Record<string,string> = {...CHECK_LABELS, ok:"正常", unknown:"待核验"};
  const checksParams = new URLSearchParams();
  if (status) checksParams.set("account_status",status);
  if (offset) checksParams.set("account_offset",String(offset));
  const checksHref = `/platform/${slug}/checks/${checksParams.size ? "?" + checksParams : ""}`;
  const accountsURL = (nextStatus: string, nextOffset: number) => {
    const query = new URLSearchParams();
    if (nextStatus) query.set("status",nextStatus);
    if (nextOffset) query.set("offset",String(nextOffset));
    return `/platform/${slug}/accounts/${query.size ? "?" + query : ""}`;
  };
  const navigateAccounts = (nextStatus: string, nextOffset: number) => startNavigation(()=>router.replace(accountsURL(nextStatus,nextOffset),{scroll:false}));
  const openChecks = (id: string, action: "check" | "refresh" = "check") => {
    checksParams.set(action === "check" ? "batch" : "task",id);
    if (action === "refresh") checksParams.set("mode","refresh");
    router.push(`/platform/${slug}/runs/?${checksParams}`);
  };
  useEffect(() => { setSelected(new Set()); }, [offset,status]);
  useEffect(() => { setPending(!!pendingCredentialCheck(slug)); }, [slug]);
  useEffect(() => {
    let active = true;
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    const load = async () => {
      if (active) setLoading(true);
      try {
        const result = await api.accounts({platform:slug,status,offset,limit:25},controller.signal);
        if (active) {
          setPage(result); setError("");
          if (offset > 0 && offset >= result.total) router.replace(accountsURL(status,Math.max(0,Math.ceil(result.total/25)-1)*25),{scroll:false});
        }
      } catch (err) { if (active) setError(`账号读取失败，保留上次结果。${(err as Error).message}`); }
      finally { if (active) {setLoading(false);timer=setTimeout(load,document.hidden?30000:10000);} }
    };
    void load();
    return () => {active=false;controller.abort();clearTimeout(timer);};
    // URL state owns filtering; refresh never resets it.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  },[slug,status,offset,reload,router]);

  const check = async (refs: string[], recover = false) => {
    if (operating.current) return;
    operating.current=true;setBusy("check");setActionError("");
    try {
      const previous = recover ? pendingCredentialCheck(slug) : null;
      const result = await startCredentialCheck(slug,previous?.refs || refs,previous?.parent_id);
      setPending(false);openChecks(result.task_id);
    } catch (err) {
      setActionError(err instanceof ApiError ? err.detail || err.message : (err as Error).message);
      setPending(!!pendingCredentialCheck(slug));
    } finally {operating.current=false;setBusy("");}
  };
  const refresh = async (row: AccountRow, action: "refresh" | "authorize" = "refresh") => {
    if (operating.current) return;
    operating.current=true;setBusy(row.ref);setActionError("");
    try {
      const result=await accountAction(row.ref,row.platform || slug,action);
      if (result.task_id) openChecks(result.task_id,"refresh");
    } catch (err) {setActionError(`刷新请求未确认：${err instanceof ApiError ? err.detail || err.message : (err as Error).message}。再次点击会先核对原请求。`);}
    finally {operating.current=false;setBusy("");}
  };
  const selectedRefs=page?.accounts.filter(row=>selected.has(row.ref)).map(row=>row.ref) || [];
  return <div className="sg-workspace sg-enter">
    <header className="sg-page-head"><div><p className="sg-eyebrow">{name}</p><h1>账号管理</h1><p className="sg-description">选择账号并发起操作，受理后进入独立运行页查看进度与结果。</p></div>
      <div className="flex gap-2"><button className="btn-ghost" disabled={loading} onClick={()=>setReload(value=>value+1)}>{loading ? "更新中…" : "更新列表"}</button><Link className="btn-ghost" href={`/platform/${slug}/`}>平台概览</Link></div>
    </header>
    <AccountNavigation slug={slug} active="accounts" checksHref={checksHref} />
    <ErrorNote message={error || actionError} />
    {pending && <div className="sg-inline-notice" role="status"><span>有一批检查的提交结果尚未确认。</span><button className="btn-ghost" disabled={!!busy} onClick={()=>check([],true)}>核对原批次</button></div>}
    <section className="sg-task-surface">
      <header className="sg-account-toolbar">
        <div className="flex items-center gap-3"><h2 className="text-sm font-semibold">账号记录</h2><span className="sg-description">{page ? `共 ${page.total} 条` : "读取中…"}</span></div>
        <div className="sg-account-tools"><select aria-label="账号状态" className="field" value={status} disabled={navigating} onChange={event=>navigateAccounts(event.target.value,0)}><option value="">全部状态</option>{Object.entries(labels).filter(([key])=>!["pending","running"].includes(key)).map(([key,label])=><option key={key} value={key}>{label}</option>)}</select>
          <Link className="btn-ghost" href={`/accounts/?platform=${slug}`}>导入与批量管理</Link></div>
      </header>
      <div className="sg-selection-bar">
        <label className="flex items-center gap-2"><input type="checkbox" aria-label="选择本页" disabled={!!busy || !page?.accounts.length} checked={!!page?.accounts.length && selectedRefs.length===page.accounts.length} onChange={event=>setSelected(event.target.checked ? new Set(page?.accounts.map(row=>row.ref)) : new Set())}/><span>本页已选 {selectedRefs.length} 项</span></label>
        <button className="btn-primary" disabled={!!busy || !selectedRefs.length} onClick={()=>check(selectedRefs)}>{busy === "check" ? "提交中…" : slug === "chatgpt" ? "批量测试 AT" : "批量核对报告"}</button>
        <span className="sg-description">{slug === "chatgpt" ? "只读测试，不使用 RT" : "读取历史检查报告，不刷新凭据"}</span>
      </div>
      <div className="overflow-auto"><table className="tbl sg-task-table whitespace-nowrap" aria-label="账号列表"><thead><tr><th>账号</th><th>凭据状态</th><th>领取</th><th>最近核验</th><th>操作</th></tr></thead><tbody>
        {page?.accounts.map(row=><tr key={row.ref}><td><label className="flex items-center gap-3"><input type="checkbox" aria-label={`选择 ${row.identity || row.ref}`} disabled={!!busy} checked={selected.has(row.ref)} onChange={event=>setSelected(previous=>{const next=new Set(previous);if(event.target.checked)next.add(row.ref);else next.delete(row.ref);return next;})}/><span className="block max-w-72 truncate" title={row.identity || row.ref}>{row.identity || row.ref}</span></label></td>
          <td><Badge tone={checkTone(row.status)}>{labels[row.status] || row.status}</Badge>{slug === "chatgpt" && <span className="mt-1 block text-xs text-muted-foreground">{LIBRARY_LABELS[row.library_status || "pending"] || "待准备"}</span>}</td><td>{row.claimed ? "已领取" : "未领取"}</td><td className="text-[13px] text-muted-foreground">{row.health_checked_at ? new Date(row.health_checked_at).toLocaleString() : "尚未核验"}</td>
          <td><div className="flex gap-2"><button className="btn-ghost" disabled={!!busy} onClick={()=>check([row.ref])}>{slug === "chatgpt" ? "测试 AT" : "核对报告"}</button><button className="btn-ghost" disabled={row.claimed || !!busy} title={row.claimed ? "已领取账号在主系统原任务中修复" : undefined} onClick={()=>refresh(row)}>{slug === "chatgpt" ? "RT 刷新" : "刷新凭据"}</button>{slug === "chatgpt" && <button className="btn-ghost" disabled={row.claimed || !!busy} title="使用已保存的密码/2FA重新授权，获取完整凭据" onClick={()=>refresh(row,"authorize")}>修复凭证</button>}</div></td></tr>)}
      </tbody></table>{!page && <div className="sg-table-skeleton">正在读取账号…</div>}{page?.total===0 && <p className="p-8 text-center text-sm text-muted-foreground">没有匹配账号</p>}</div>
      <footer className="sg-task-footer"><span>{page ? `${page.total ? offset+1 : 0}–${Math.min(offset+25,page.total)} / ${page.total}` : "—"}</span><div className="flex gap-2"><button className="btn-ghost" disabled={!page || offset===0 || !!busy || navigating || loading} onClick={()=>navigateAccounts(status,Math.max(0,offset-25))}>上一页</button><button className="btn-ghost" disabled={!page || offset+25>=page.total || !!busy || navigating || loading} onClick={()=>navigateAccounts(status,offset+25)}>下一页</button></div></footer>
    </section>
    <p className="sg-description">AT 测试只读，RT 刷新使用原刷新凭据；修复凭证使用已保存的密码/2FA重新授权。已领取账号由主系统原任务负责。</p>
  </div>;
}
