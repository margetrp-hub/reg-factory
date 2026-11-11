"use client";

import { useEffect, useRef, useState } from "react";
import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { api, ApiError, type CredentialCheckDetail, type CredentialCheckHistory, type RefreshDetail } from "@/lib/api";
import { pendingCredentialCheck, startCredentialCheck, type CredentialUpdateMethod } from "@/lib/account-actions";
import { CredentialUpdateDialog, type UpdateTarget } from "@/components/credential-update-dialog";
import { PLATFORM_TASKS } from "@/lib/task-catalog";
import { AccountNavigation, CHECK_LABELS, LIBRARY_LABELS, checkTone } from "@/components/account-navigation";
import { Badge, ErrorNote, StatusBadge } from "@/components/ui";
import { TaskTable } from "@/components/task-table";
import { LogView } from "@/components/log-view";
import { Drawer } from "@/components/drawer";

const activeStatus = (status: string) => ["queued","running","stopping"].includes(status);
const datetime = (value: string) => value ? new Date(value).toLocaleString() : "—";
const errorText = (err: unknown) => err instanceof ApiError ? err.detail || err.message : (err as Error).message;
const filters = [["","全部状态"],["running","检查中"],["queued","排队"],["succeeded","检查完成"],["unknown","待核对"],["failed","失败"],["stopped","已停止"],["interrupted","中断"]];

function BatchStatus({status}: {status: string}) {
  return status === "succeeded" ? <Badge tone="ok">检查完成</Badge> : <StatusBadge status={status}/>;
}

export function CredentialChecks({slug, view="history"}: {slug: string; view?: "history" | "run"}) {
  const router=useRouter();
  const params=useSearchParams();
  const mode=params.get("mode") === "refresh" ? "refresh" : "check";
  const batch=mode === "check" ? params.get("batch") || "" : "";
  const taskID=mode === "refresh" ? params.get("task") || "" : "";
  const status=params.get("status") || "";
  const offset=Math.max(0,Number(params.get("offset")) || 0);
  const [history,setHistory]=useState<{key: string; value: CredentialCheckHistory} | null>(null);
  const [detail,setDetail]=useState<CredentialCheckDetail | null>(null);
  const [refreshDetail,setRefreshDetail]=useState<RefreshDetail | null>(null);
  const [resolveOpen,setResolveOpen]=useState(false);
  const [updateRequest,setUpdateRequest]=useState<{targets:UpdateTarget[]; method?:CredentialUpdateMethod; retryOf?:string} | null>(null);
  const [reason,setReason]=useState("");
  const [resultStatus,setResultStatus]=useState("");
  const [resultOffset,setResultOffset]=useState(0);
  const [reload,setReload]=useState(0);
  const [loading,setLoading]=useState(false);
  const [error,setError]=useState("");
  const [actionError,setActionError]=useState("");
  const [busy,setBusy]=useState("");
  const [pending,setPending]=useState(false);
  const operating=useRef(false);
  const key=[slug,mode,status,offset].join(":");
  const page=history?.key === key ? history.value.page : null;
  const current=detail?.task.id === batch ? detail : null;
  const refresh=refreshDetail?.task.id === taskID ? refreshDetail.task : null;
  const accountQuery=new URLSearchParams();
  if(params.get("account_status"))accountQuery.set("status",params.get("account_status")!);
  if(params.get("account_offset"))accountQuery.set("offset",params.get("account_offset")!);
  const accountsHref=`/platform/${slug}/accounts/${accountQuery.size ? "?"+accountQuery : ""}`;
  const link=(updates: Record<string,string | null>, target: "history" | "run" = "history")=>{
    const query=new URLSearchParams(params.toString());
    for(const [key,value] of Object.entries(updates)){if(value)query.set(key,value);else query.delete(key);}
    if(target === "history"){query.delete("batch");query.delete("task");}
    return `/platform/${slug}/${target === "run" ? "runs" : "checks"}/${query.size ? "?"+query : ""}`;
  };
  const listHref=link({batch:null,task:null});
  const runLink=(updates: Record<string,string | null>)=>link(updates,"run");
  useEffect(()=>{
    // Old detail bookmarks retain their original record and return context.
    if(view === "history" && (batch || taskID))router.replace(runLink({}));
    if(view === "run" && !batch && !taskID)router.replace(listHref);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  },[view,batch,taskID,slug,router]);
  useEffect(()=>{setPending(!!pendingCredentialCheck(slug));},[slug]);
  useEffect(()=>{setResultStatus("");setResultOffset(0);setActionError("");setResolveOpen(false);setReason("");},[batch,taskID,mode]);
  useEffect(()=>{
    let active=true;
    const controller=new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    const load=async()=>{
      if(active)setLoading(true);
      try {
        if(batch){
          const result=await api.credentialCheck(batch,controller.signal);
          if(JSON.parse(result.task.args_json || "{}").platform!==slug)throw new Error("该批次不属于当前平台");
          if(active)setDetail(result);
        } else if(taskID){
          const result=await api.accountRefreshDetail(taskID,controller.signal);
          const task=result.task;
          const args=JSON.parse(task.args_json || "[]");
          if(task.script!=="asset_ops" || args[0]!=="refresh" || args[4]!==slug)throw new Error("该记录不是本平台的凭据刷新任务");
          if(active)setRefreshDetail(result);
        } else {
          const result=await api.credentialChecks(slug,mode,status,offset,controller.signal);
          if(active){
            setHistory({key,value:result});
            if(offset>0 && offset>=result.page.total)router.replace(link({offset:String(Math.max(0,Math.ceil(result.page.total/25)-1)*25)}),{scroll:false});
          }
        }
        if(active)setError("");
      }catch(err){if(active)setError(`记录读取失败，保留上次结果。${errorText(err)}`);}
      finally{if(active){setLoading(false);timer=setTimeout(load,document.hidden?30000:batch || taskID ? 2500 : 10000);}}
    };
    void load();return()=>{active=false;controller.abort();clearTimeout(timer);};
    // URL state is the history/detail owner; polling never changes the view.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  },[slug,mode,batch,taskID,status,offset,key,reload,router]);

  const launch=async(refs: string[], recover=false)=>{
    if(operating.current)return;
    operating.current=true;setBusy("check");setActionError("");
    try{
      const previous=recover ? pendingCredentialCheck(slug) : null;
      const result=await startCredentialCheck(slug,previous?.refs || refs,recover ? previous?.parent_id : batch || undefined);
      setPending(false);router.push(runLink({mode:null,task:null,batch:result.task_id}));
    }catch(err){setActionError(errorText(err));setPending(!!pendingCredentialCheck(slug));}
    finally{operating.current=false;setBusy("");}
  };
  const updateCredential=(ref:string,identity?:string,method?:CredentialUpdateMethod)=>setUpdateRequest({targets:[{ref,platform:slug,identity}],method,retryOf:taskID || undefined});
  const stop=async()=>{
    if(operating.current || !batch)return;
    operating.current=true;setBusy("stop");setActionError("");
    try{await api.stopTask(batch);setReload(value=>value+1);}
    catch(err){setActionError(`停止请求未确认：${errorText(err)}。请更新记录核对状态。`);}
    finally{operating.current=false;setBusy("");}
  };
  const recoverRefresh=async(manual=false)=>{
    if(operating.current || !refresh)return;
    operating.current=true;setBusy(manual ? "resolve" : "reconcile");setActionError("");
    try{
      const result=manual ? await api.resolveRefresh(refresh.id,reason) : await api.reconcileRefresh(refresh.id);
      setRefreshDetail(result);setResolveOpen(false);setReason("");setReload(value=>value+1);
    }catch(err){setActionError(errorText(err));setReload(value=>value+1);}
    finally{operating.current=false;setBusy("");}
  };
  const running=current && activeStatus(current.task.status);
  const items=current?.items || [];
  const unfinished=items.filter(item=>["pending","running","unknown"].includes(item.status));
  const visible=items.filter(item=>!resultStatus || item.status===resultStatus);
  const checked=items.filter(item=>!["pending","running"].includes(item.status)).length;
  const changeMode=(next: string)=>router.push(link({mode:next === "check" ? null : "refresh",batch:null,task:null,status:null,offset:null}));
  return <div className="sg-workspace sg-enter">
    <header className="sg-page-head"><div><p className="sg-eyebrow">{PLATFORM_TASKS[slug]?.name || slug} · {batch ? "检查明细" : taskID ? "更新记录" : "账号处理"}</p><h1>{view === "run" ? "本次运行" : "处理记录"}</h1><p className="sg-description">{view === "run" ? "当前任务的进度、结果与恢复操作。离开页面后任务继续执行。" : "可用性检查与凭据更新分开记录，更新记录包含 RT 刷新及登录重新授权。"}</p></div>
      <div className="flex gap-2"><button className="btn-ghost" disabled={loading} onClick={()=>setReload(value=>value+1)}>{loading ? "更新中…" : "更新记录"}</button>{batch || taskID ? <Link className="btn-ghost" href={listHref}>返回记录</Link> : <Link className="btn-primary" href={accountsHref}>选择账号</Link>}</div>
    </header>
    <AccountNavigation slug={slug} active={view === "run" ? "run" : "checks"} accountsHref={accountsHref} checksHref={listHref} runHref={view === "run" ? runLink({}) : undefined}/>
    <ErrorNote message={error || actionError}/>
    <CredentialUpdateDialog request={updateRequest} onClose={()=>setUpdateRequest(null)} onAccepted={()=>setReload(value=>value+1)}/>
    {pending && <div className="sg-inline-notice" role="status"><span>原批次提交结果尚未确认，先找回原记录。</span><button className="btn-ghost" disabled={!!busy} onClick={()=>launch([],true)}>核对原批次</button></div>}
    {!batch && !taskID && <section className="sg-task-surface">
      <header className="sg-account-toolbar"><div className="sg-segmented" aria-label="处理类型"><button aria-pressed={mode === "check"} onClick={()=>changeMode("check")}>{slug === "chatgpt" ? "可用性检查" : "报告核对"}</button><button aria-pressed={mode === "refresh"} onClick={()=>changeMode("refresh")}>凭据更新</button></div>
        <div className="sg-account-tools"><span className="sg-description">{page ? `${page.total} ${mode === "check" ? "个批次" : "条记录"}` : "读取中…"}</span><select className="field" aria-label="记录状态" value={status} onChange={event=>router.replace(link({status:event.target.value,offset:null}),{scroll:false})}>{filters.map(([key,label])=><option key={key} value={key}>{mode === "refresh" ? key === "succeeded" ? "更新成功" : key === "running" ? "更新中" : label : label}</option>)}</select></div>
      </header>
      {!page ? <div className="sg-table-skeleton">正在读取记录…</div> : mode === "refresh" ? <div className="overflow-auto"><TaskTable tasks={page.tasks} onSelect={id=>router.push(runLink({task:id}))} emptyText="尚无凭据更新记录，可从账号页选择更新方式"/></div> : <div className="overflow-auto"><table className="tbl sg-task-table whitespace-nowrap" aria-label="检查批次"><thead><tr><th>批次</th><th>状态</th><th>进度</th><th>结果</th><th>发起时间</th><th>操作</th></tr></thead><tbody>
        {page.tasks.map(task=>{
          const count=history!.value.counts[task.id];
          return <tr key={task.id}><td><Link className="font-medium hover:underline" href={runLink({batch:task.id})}>{task.title}</Link></td><td><BatchStatus status={task.status}/></td><td className="tabular-nums">{count ? `${count.checked} / ${count.total}` : "—"}</td><td><div className="flex gap-2">{count?.ok ? <Badge tone="ok">有效 {count.ok}</Badge> : null}{count?.needs_refresh ? <Badge tone="warn">待刷新 {count.needs_refresh}</Badge> : null}{count?.unknown ? <Badge tone="warn">待核对 {count.unknown}</Badge> : null}{count?.abnormal ? <Badge tone="bad">异常 {count.abnormal}</Badge> : null}{!count?.checked && <span className="text-muted-foreground">尚无结果</span>}</div></td><td className="text-[13px] text-muted-foreground">{datetime(task.created_at)}</td><td><Link className="btn-ghost" href={runLink({batch:task.id})}>查看运行</Link></td></tr>;
        })}
      </tbody></table>{page.total===0 && <p className="px-6 py-14 text-center text-sm text-muted-foreground">还没有检查批次，选择账号后即可开始。</p>}</div>}
      <footer className="sg-task-footer"><span>{page ? `${page.total ? offset+1 : 0}–${Math.min(offset+25,page.total)} / ${page.total}` : "—"}</span><div className="flex gap-2"><button className="btn-ghost" disabled={!page || offset===0} onClick={()=>router.replace(link({offset:String(Math.max(0,offset-25))}),{scroll:false})}>上一页</button><button className="btn-ghost" disabled={!page || offset+25>=page.total} onClick={()=>router.replace(link({offset:String(offset+25)}),{scroll:false})}>下一页</button></div></footer>
    </section>}

    {batch && (!current ? <div className="sg-table-skeleton">正在读取批次…</div> : <section className="sg-task-surface">
      <header className="sg-account-toolbar"><div><div className="flex flex-wrap items-center gap-3"><h2 className="text-base font-semibold">{current.task.title}</h2><BatchStatus status={current.task.status}/></div><p className="sg-description mt-1">{datetime(current.task.created_at)} · {current.task.actor || "操作员"}</p></div>
        <div className="flex gap-2">{running ? <button className="btn-danger" disabled={!!busy || current.task.status === "stopping"} onClick={stop}>{current.task.status === "stopping" ? "当前项收尾中…" : "停止后续检查"}</button> : unfinished.length>0 && <button className="btn-primary" disabled={!!busy} onClick={()=>launch(unfinished.map(item=>item.ref))}>重测未确认项（{unfinished.length}）</button>}</div>
      </header>
      <div className="sg-check-progress"><span className="tabular-nums">已返回 <strong>{checked}</strong> / {items.length}</span><progress aria-label="检查进度" max={items.length || 1} value={checked}/><span className="text-success">有效 {items.filter(item=>item.status === "ok").length}</span><span className="text-warning">待刷新 {items.filter(item=>item.status === "needs_refresh").length}</span><span>未执行 {items.filter(item=>["pending","running"].includes(item.status)).length}</span></div>
      {current.task.error && <p className="sg-check-context">{current.task.error}</p>}
      <div className="sg-account-toolbar"><h3 className="text-sm font-medium">逐条结果</h3><div className="sg-account-tools"><select className="field" aria-label="检查结果" value={resultStatus} onChange={event=>{setResultStatus(event.target.value);setResultOffset(0);}}><option value="">全部结果</option>{Object.entries(CHECK_LABELS).map(([key,label])=><option key={key} value={key}>{label}</option>)}</select></div></div>
      <div className="overflow-auto"><table className="tbl sg-task-table sg-check-results" aria-label="逐条检查结果"><thead><tr><th>账号</th><th>结果</th><th>反馈</th><th>操作</th></tr></thead><tbody>
        {visible.slice(resultOffset,resultOffset+15).map(item=><tr key={item.ref}><td><span className="block max-w-72 truncate" title={item.identity || item.ref}>{item.identity || item.ref}</span><span className="text-xs text-muted-foreground">{item.mode === "report" ? "历史报告 · " : ""}{datetime(item.checked_at)}</span></td><td><Badge tone={checkTone(item.status)}>{item.mode === "report" && item.status === "ok" ? "报告正常" : CHECK_LABELS[item.status] || item.status}</Badge></td><td className="sg-check-message">{item.message || (item.status === "pending" ? "尚未发起" : "正在检查，结果返回后更新")}{item.child_task_id && <Link className="ml-2 underline" href={`/tasks/?task=${encodeURIComponent(item.child_task_id)}`}>报告任务</Link>}</td><td><div className="flex gap-2">{!running && <button className="btn-ghost" disabled={!!busy} onClick={()=>launch([item.ref])}>{item.mode === "at" ? "重新检查" : "重新核对"}</button>}{item.mode === "at" && !running && <button className="btn-ghost" disabled={!!busy || !item.can_refresh} title={item.can_refresh ? "选择 RT 刷新或登录重新授权" : "已领取或已移除账号，请在主系统原任务处理"} onClick={()=>updateCredential(item.ref,item.identity)}>更新凭据</button>}</div></td></tr>)}
      </tbody></table>{visible.length===0 && <p className="p-8 text-center text-sm text-muted-foreground">没有匹配结果</p>}</div>
      <footer className="sg-task-footer"><span>{visible.length ? resultOffset+1 : 0}–{Math.min(resultOffset+15,visible.length)} / {visible.length}</span><div className="flex gap-2"><button className="btn-ghost" disabled={resultOffset===0} onClick={()=>setResultOffset(value=>Math.max(0,value-15))}>上一页</button><button className="btn-ghost" disabled={resultOffset+15>=visible.length} onClick={()=>setResultOffset(value=>value+15)}>下一页</button></div></footer>
    </section>)}
    {taskID && (!refresh ? <div className="sg-table-skeleton">正在读取原更新任务…</div> : <section className="sg-task-surface">
      <header className="sg-account-toolbar"><div className="min-w-0"><h2 className="break-all text-base font-semibold">{refresh.title}</h2><p className="sg-description mt-1">{datetime(refresh.created_at)}</p></div><StatusBadge status={refresh.status}/></header>
      <div className="sg-account-toolbar"><span className="sg-description">执行方式：{refreshDetail?.method === "authorize" ? "登录重新授权" : "RT 刷新"}</span><span className="sg-description">{activeStatus(refresh.status) ? refreshDetail?.progress?.next_action || "等待执行反馈" : refresh.status === "succeeded" ? "最新凭据与任务结果已保存" : "本次未完成，可查看原因后重试"}{activeStatus(refresh.status) && !!refreshDetail?.progress?.attempt && ` · 收尾重试 ${refreshDetail.progress.attempt} 次`}</span></div>
      {refresh.error && <p className="sg-check-context">{refresh.error}</p>}
      {refreshDetail?.evidence.resolution && <p className="sg-check-context">{refreshDetail.evidence.resolution}</p>}
      {refreshDetail && <div className="sg-account-toolbar"><span className="sg-description">生产账号库：{LIBRARY_LABELS[refreshDetail.library_status] || "待准备"}</span>{refreshDetail.verification_task_id ? <Link className="btn-ghost" href={`/platform/${slug}/runs/?batch=${encodeURIComponent(refreshDetail.verification_task_id)}`}>查看自动 AT 测试</Link> : refresh.status === "succeeded" && refreshDetail.library_status !== "used" && <button className="btn-ghost" disabled={!!busy} onClick={()=>launch([refreshDetail.evidence.ref])}>测试 AT 并更新可用状态</button>}</div>}
      {refreshDetail?.can_resolve && <div className="px-5 pb-4"><p className="sg-description mb-3">先核对已经保存的凭据。确认本次未完成时，记录证据后结案，再显式发起新的刷新。</p><div className="flex flex-wrap gap-2"><button className="btn-primary" disabled={!!busy || !refreshDetail.can_reconcile} onClick={()=>recoverRefresh()}>{busy === "reconcile" ? "核对中…" : "核对已保存凭据"}</button><button className="btn-ghost" disabled={!!busy} onClick={()=>setResolveOpen(true)}>人工结案</button></div>{!refreshDetail.can_reconcile && <p className="sg-description mt-2">原记录缺少可自动核对的版本证据，请结合执行日志人工处理。</p>}</div>}
      {refreshDetail?.can_retry && <div className="px-5 pb-4 flex gap-2"><button className="btn-primary" disabled={!!busy} onClick={()=>updateCredential(refreshDetail.evidence.ref,undefined,refreshDetail.method)}>重新更新凭据</button></div>}
      <div className="px-5 pb-5"><LogView key={refresh.id+":"+refresh.status} taskId={refresh.id}/></div>
      <footer className="sg-task-footer"><span>保存中断会自动恢复原结果；未确认成功则反馈失败，重试不会重复使用旧 RT。</span><Link className="btn-ghost" href={`/tasks/?task=${encodeURIComponent(refresh.id)}`}>完整执行记录</Link></footer>
    </section>)}
    <Drawer open={resolveOpen} title="确认本次更新未完成" subtitle="仅在已有证据确认未完成时结案；保留原日志与审计记录。" onClose={()=>{if(!busy)setResolveOpen(false);}} width={520}>
      <form className="space-y-4" onSubmit={event=>{event.preventDefault();void recoverRefresh(true);}}>
        <label className="block text-sm font-medium" htmlFor="refresh-resolution">核对证据与原因</label>
        <textarea id="refresh-resolution" className="field min-h-32 w-full" value={reason} onChange={event=>setReason(event.target.value)} required minLength={4} maxLength={300} placeholder="填写已核对的结果与未完成原因，不要填写 Token、密码或验证码。" disabled={!!busy}/>
        <ErrorNote message={actionError}/><div className="flex justify-end gap-2"><button className="btn-ghost" type="button" disabled={!!busy} onClick={()=>setResolveOpen(false)}>继续核对</button><button className="btn-danger" type="submit" disabled={!!busy || reason.trim().length<4}>{busy === "resolve" ? "保存中…" : "确认未完成并结案"}</button></div>
      </form>
    </Drawer>
    <p className="sg-description">{slug === "chatgpt" ? "AT 测试不轮换凭据；401 仅表示当前 AT 被拒绝。更新凭据时明确选择 RT 刷新或登录重新授权，已领取账号由主系统处理。" : "报告核对保留原平台能力与检查时间，不将历史报告当作实时测试。刷新与授权仍是独立操作。"}</p>
  </div>;
}
