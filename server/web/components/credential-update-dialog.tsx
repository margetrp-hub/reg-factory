"use client";

import { useEffect, useRef, useState } from "react";
import Link from "next/link";
import { api, ApiError } from "@/lib/api";
import { accountAction, pendingCredentialUpdate, UPDATE_LABELS, LOGIN_MODE_LABELS, type CredentialUpdateMethod, type LoginMode } from "@/lib/account-actions";
import { Drawer } from "@/components/drawer";
import { Badge } from "@/components/ui";
import { accountReturn } from "@/lib/account-context";

export type UpdateTarget = {ref:string; platform:string; identity?:string; claimed?:boolean; library_status?:string; has_credentials?:boolean};
type Result = {task_id?:string; error?:string; method:CredentialUpdateMethod};
type Request = {targets:UpdateTarget[]; method?:CredentialUpdateMethod; retryOf?:string};
const storageKey = "sg-last-credential-update";
const signature = (targets:UpdateTarget[]) => JSON.stringify(targets.map(row=>[row.platform,row.ref]).sort());

/** One choice and one submission/recovery surface for all credential entry points. */
export function CredentialUpdateDialog({request,onClose,onAccepted,returnTo}: {request:Request | null; onClose:()=>void; onAccepted?:()=>void; returnTo?:string}) {
  const [targets,setTargets]=useState<UpdateTarget[]>([]);
  const [method,setMethod]=useState<CredentialUpdateMethod | null>(null);
  const [results,setResults]=useState<Record<string,Result>>({});
  const [busy,setBusy]=useState(false);
  const [progress,setProgress]=useState(0);
  const [context,setContext]=useState("");
  const [options,setOptions]=useState<Awaited<ReturnType<typeof api.accountLoginOptions>>["accounts"]>({});
  const [loginModes,setLoginModes]=useState<Record<string,LoginMode | "">>({});
  const [loadingOptions,setLoadingOptions]=useState(false);
  const [optionsError,setOptionsError]=useState("");
  const [reloadOptions,setReloadOptions]=useState(0);
  const submitting=useRef(false);
  useEffect(()=>{
    if (!request) return;
    const unique=Array.from(new Map(request.targets.map(row=>[row.ref,row])).values());
    const original=unique.map(row=>pendingCredentialUpdate(row.ref)).find(Boolean);
    const context=signature(unique)+(request.retryOf || "");
    setContext(context);setTargets(unique);setMethod(original ? original.method || "refresh" : request.method || (unique.length && unique.every(row=>row.platform==="chatgpt" && row.has_credentials===false) ? "authorize" : null));setResults({});setProgress(0);
    try {
      const saved=JSON.parse(sessionStorage.getItem(storageKey) || "null");
      if (saved?.signature===context && saved.results && ["refresh","authorize"].includes(saved.method)) {
        setMethod(saved.method);setResults(saved.results);
      }
    } catch { /* Original tasks remain in server history. */ }
  },[request]);
  useEffect(()=>{
    if (!request) return;
    const refs=Array.from(new Set(request.targets.filter(row=>row.platform==="chatgpt").map(row=>row.ref)));
    setOptions({});setLoginModes({});setOptionsError("");setLoadingOptions(false);
    if (!refs.length) return;
    const controller=new AbortController();
    setLoadingOptions(true);
    (async()=>{
      const accounts:Awaited<ReturnType<typeof api.accountLoginOptions>>["accounts"]={};
      for(let i=0;i<refs.length;i+=100){
        Object.assign(accounts,(await api.accountLoginOptions(refs.slice(i,i+100),controller.signal)).accounts);
      }
      return {accounts};
    })().then(({accounts})=>{
      if (controller.signal.aborted) return;
      setOptions(accounts);
      setLoginModes(Object.fromEntries(Object.entries(accounts).map(([ref,row])=>{
        const selected=pendingCredentialUpdate(ref)?.loginMode || row.selected_mode;
        return [ref,selected ? row.modes.includes(selected as LoginMode) ? selected : "" : row.modes[0] || ""];
      })));
    }).catch(error=>{if(!controller.signal.aborted)setOptionsError((error as Error).message);})
      .finally(()=>{if(!controller.signal.aborted)setLoadingOptions(false);});
    return ()=>controller.abort();
  },[request,reloadOptions]);
  const accepted=Object.values(results).filter(row=>row.task_id).length;
  const unresolved=targets.filter(row=>!results[row.ref]?.task_id);
  const hasPending=targets.some(row=>!!pendingCredentialUpdate(row.ref));
  const started=Object.keys(results).length>0 || hasPending;
  const close=()=>{if(!submitting.current)onClose();};
  const submit=async()=>{
    if (!method || submitting.current) return;
    submitting.current=true;setBusy(true);setProgress(0);
    const next={...results};
    let received=false;
    try {
      for (const [index,row] of unresolved.entries()) {
        const effective=pendingCredentialUpdate(row.ref)?.method || method;
        try {
          if (effective==="authorize" && row.platform!=="chatgpt") throw new Error("此平台请使用原平台授权入口；本次未提交");
          const loginMode=pendingCredentialUpdate(row.ref)?.loginMode || loginModes[row.ref];
          if (!pendingCredentialUpdate(row.ref) && row.platform==="chatgpt") {
            if (effective==="refresh" && !options[row.ref]?.has_refresh_token) throw new Error("此账号没有 RT，请选择登录重新授权");
            if (effective==="authorize" && !loginMode) throw new Error(options[row.ref]?.reason || "请按账号资料选择登录执行模式");
          }
          const receipt=await accountAction(row.ref,row.platform,effective,effective==="authorize" ? loginMode || undefined : undefined);
          if (!receipt.task_id) throw new Error("没有返回任务编号，请恢复原请求");
          next[row.ref]={task_id:receipt.task_id,method:receipt.method || effective};received=true;
        } catch(err) {
          next[row.ref]={method:effective,error:err instanceof ApiError ? err.detail || err.message : (err as Error).message};
        }
        setProgress(index+1);setResults({...next});
        try {sessionStorage.setItem(storageKey,JSON.stringify({signature:context,method,results:next}));} catch { /* Optional navigation metadata, never credentials. */ }
      }
    } finally {submitting.current=false;setBusy(false);if(received)onAccepted?.();}
  };
  const newUpdate=()=>{setResults({});setMethod(null);try{sessionStorage.removeItem(storageKey);}catch{/* optional */}};
  return <Drawer open={!!request} title="更新凭据" subtitle={`${targets.length} 个账号 · 选择本次执行方式，受理后在原运行记录查看结果。`} variant="dialog" width={620} onClose={close}
    footer={<><button className="btn-ghost" disabled={busy} onClick={close}>{accepted ? "返回账号" : "取消"}</button>{accepted>0 && !unresolved.length && <button className="btn-ghost" disabled={busy} onClick={newUpdate}>再次更新</button>}<button className="btn-primary" disabled={busy || !method || !unresolved.length || (!hasPending && (loadingOptions || !!optionsError))} onClick={()=>void submit()}>{busy ? `提交中 ${progress} / ${unresolved.length}` : started ? "恢复未受理项" : "开始更新"}</button></>}>
    <fieldset disabled={busy || hasPending} className="space-y-2">
      <legend className="mb-3 text-sm font-medium">更新方式</legend>
      {(["refresh","authorize"] as const).filter(value=>value!=="authorize" || targets.some(row=>row.platform==="chatgpt")).map(value=><label key={value} className={`flex cursor-pointer items-start gap-3 rounded-xl border p-4 transition-colors focus-within:ring-2 focus-within:ring-ring ${method===value ? "border-foreground/30 bg-accent" : "border-border hover:bg-accent/50"}`}>
        <input className="mt-1" type="radio" name="credential-update-method" value={value} checked={method===value} onChange={()=>setMethod(value)}/>
        <span><span className="block text-sm font-medium">{UPDATE_LABELS[value]}</span><span className="mt-1 block text-[13px] leading-relaxed text-muted-foreground">{value==="refresh" ? "已有 Refresh Token 时使用，更新凭据，不重新登录。RT 不可用时明确返回原因。" : "首次生成凭据或 RT 不可用时使用。按各账号资料选择执行模式；受理时记住选择，后续主控和本地修复共用。"}</span></span>
      </label>)}
    </fieldset>
    {loadingOptions && <p role="status" className="mt-3 text-sm text-muted-foreground">正在读取账号资料与已保存的模式…</p>}
    {optionsError && <div role="alert" className="mt-3 text-sm text-destructive">{optionsError}<button className="btn-ghost ml-2" onClick={()=>setReloadOptions(n=>n+1)}>重新读取</button></div>}
    <p className="my-4 text-[13px] leading-relaxed text-muted-foreground">ChatGPT 更新成功后自动检查 AT。已使用账号也可更新凭据，使用记录保持。检查可用性本身不刷新 RT，也不登录。</p>
    <div className="space-y-2" aria-label="更新提交结果">{targets.map(row=>{
      const result=results[row.ref];
      const pending=pendingCredentialUpdate(row.ref);
      return <div key={row.ref} className="rounded-lg border border-border px-3 py-3"><div className="flex flex-wrap items-center justify-between gap-2"><span className="min-w-0 break-all text-sm">{row.identity || row.ref}</span>{result?.task_id ? <Badge tone="info">已受理 · {UPDATE_LABELS[result.method]}</Badge> : <Badge tone={result?.error ? "warn" : "neutral"}>{pending ? "提交待确认" : result?.error ? "未受理" : "待提交"}</Badge>}</div>
        {result?.error && <p role="alert" className="mt-2 break-words text-xs text-destructive">{result.error}</p>}
        {method==="authorize" && row.platform==="chatgpt" && !result?.task_id && options[row.ref] && <fieldset disabled={busy || !!pending} className="mt-3 space-y-2">
          <legend className="mb-2 text-xs text-muted-foreground">本账号执行模式</legend>
          {options[row.ref].modes.map(mode=><label key={mode} className="flex items-center gap-2 text-sm"><input type="radio" name={`login-mode-${row.ref}`} value={mode} checked={loginModes[row.ref]===mode} onChange={()=>setLoginModes({...loginModes,[row.ref]:mode})}/>{LOGIN_MODE_LABELS[mode]}</label>)}
          {options[row.ref].reason && <p className="text-xs text-destructive">{options[row.ref].reason}</p>}
          {!options[row.ref].modes.includes("protocol") && <p className="text-xs text-muted-foreground">协议授权需要密码和 2FA 资料。</p>}
        </fieldset>}
        {method==="refresh" && row.platform==="chatgpt" && options[row.ref] && !options[row.ref].has_refresh_token && <p className="mt-2 text-xs text-destructive">没有 RT，请选择登录重新授权。</p>}
        {pending && <p className="mt-2 text-xs text-muted-foreground">先找回原任务{pending.method ? `（${UPDATE_LABELS[pending.method]}）` : ""}，不会重复提交已受理操作。</p>}
        {result?.task_id && <Link className="btn-ghost mt-2" onClick={close} href={`/platform/${row.platform}/runs/?mode=refresh&task=${encodeURIComponent(result.task_id)}&return_to=${encodeURIComponent(accountReturn(returnTo,`/platform/${row.platform}/accounts/`))}`}>查看运行</Link>}
      </div>;
    })}</div>
    <p role="status" className="mt-4 text-xs text-muted-foreground">{accepted ? `已受理 ${accepted} 项，不代表凭据已经更新成功。已受理任务不会随关闭弹窗而取消。` : "只提交你选择的方式；任务和失败原因均保留在处理记录。"}</p>
  </Drawer>;
}
