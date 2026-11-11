"use client";

import { useEffect, useRef, useState } from "react";
import Link from "next/link";
import { ApiError } from "@/lib/api";
import { accountAction, pendingCredentialUpdate, UPDATE_LABELS, type CredentialUpdateMethod } from "@/lib/account-actions";
import { Drawer } from "@/components/drawer";
import { Badge } from "@/components/ui";

export type UpdateTarget = {ref:string; platform:string; identity?:string; claimed?:boolean; library_status?:string};
type Result = {task_id?:string; error?:string; method:CredentialUpdateMethod};
type Request = {targets:UpdateTarget[]; method?:CredentialUpdateMethod; retryOf?:string};
const storageKey = "sg-last-credential-update";
const signature = (targets:UpdateTarget[]) => JSON.stringify(targets.map(row=>[row.platform,row.ref]).sort());

/** One choice and one submission/recovery surface for all credential entry points. */
export function CredentialUpdateDialog({request,onClose,onAccepted}: {request:Request | null; onClose:()=>void; onAccepted?:()=>void}) {
  const [targets,setTargets]=useState<UpdateTarget[]>([]);
  const [method,setMethod]=useState<CredentialUpdateMethod | null>(null);
  const [results,setResults]=useState<Record<string,Result>>({});
  const [busy,setBusy]=useState(false);
  const [progress,setProgress]=useState(0);
  const [context,setContext]=useState("");
  const submitting=useRef(false);
  useEffect(()=>{
    if (!request) return;
    const unique=Array.from(new Map(request.targets.map(row=>[row.ref,row])).values());
    const original=unique.map(row=>pendingCredentialUpdate(row.ref)).find(Boolean);
    const context=signature(unique)+(request.retryOf || "");
    setContext(context);setTargets(unique);setMethod(original ? original.method || "refresh" : request.method || null);setResults({});setProgress(0);
    try {
      const saved=JSON.parse(sessionStorage.getItem(storageKey) || "null");
      if (saved?.signature===context && saved.results && ["refresh","authorize"].includes(saved.method)) {
        setMethod(saved.method);setResults(saved.results);
      }
    } catch { /* Original tasks remain in server history. */ }
  },[request]);
  const eligible=targets.filter(row=>!row.claimed && row.library_status!=="used");
  const accepted=Object.values(results).filter(row=>row.task_id).length;
  const unresolved=eligible.filter(row=>!results[row.ref]?.task_id);
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
          const receipt=await accountAction(row.ref,row.platform,effective);
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
    footer={<><button className="btn-ghost" disabled={busy} onClick={close}>{accepted ? "返回账号" : "取消"}</button>{accepted>0 && !unresolved.length && <button className="btn-ghost" disabled={busy} onClick={newUpdate}>再次更新</button>}<button className="btn-primary" disabled={busy || !method || !unresolved.length} onClick={()=>void submit()}>{busy ? `提交中 ${progress} / ${unresolved.length}` : started ? "恢复未受理项" : "开始更新"}</button></>}>
    <fieldset disabled={busy || hasPending} className="space-y-2">
      <legend className="mb-3 text-sm font-medium">更新方式</legend>
      {(["refresh","authorize"] as const).filter(value=>value!=="authorize" || eligible.some(row=>row.platform==="chatgpt")).map(value=><label key={value} className={`flex cursor-pointer items-start gap-3 rounded-xl border p-4 transition-colors focus-within:ring-2 focus-within:ring-ring ${method===value ? "border-foreground/30 bg-accent" : "border-border hover:bg-accent/50"}`}>
        <input className="mt-1" type="radio" name="credential-update-method" value={value} checked={method===value} onChange={()=>setMethod(value)}/>
        <span><span className="block text-sm font-medium">{UPDATE_LABELS[value]}</span><span className="mt-1 block text-[13px] leading-relaxed text-muted-foreground">{value==="refresh" ? "已有 Refresh Token 时使用，更新凭据，不重新登录。RT 不可用时明确返回原因。" : "首次生成凭据或 RT 不可用时使用。有密码＋2FA走协议授权，其余资料沿用浏览器与邮箱授权。"}</span></span>
      </label>)}
    </fieldset>
    <p className="my-4 text-[13px] leading-relaxed text-muted-foreground">ChatGPT 更新成功后自动检查 AT，确认可用后进入生产账号库。检查可用性本身不刷新 RT，也不登录。</p>
    <div className="space-y-2" aria-label="更新提交结果">{targets.map(row=>{
      const result=results[row.ref];const owned=row.claimed || row.library_status==="used";
      const pending=pendingCredentialUpdate(row.ref);
      return <div key={row.ref} className="rounded-lg border border-border px-3 py-3"><div className="flex flex-wrap items-center justify-between gap-2"><span className="min-w-0 break-all text-sm">{row.identity || row.ref}</span>{result?.task_id ? <Badge tone="info">已受理 · {UPDATE_LABELS[result.method]}</Badge> : owned ? <Badge tone="neutral">主系统处理</Badge> : <Badge tone={result?.error ? "warn" : "neutral"}>{pending ? "提交待确认" : result?.error ? "未受理" : "待提交"}</Badge>}</div>
        {owned && <p className="mt-2 text-xs text-muted-foreground">已使用账号由主系统原任务更新，本次跳过。</p>}
        {result?.error && <p role="alert" className="mt-2 break-words text-xs text-destructive">{result.error}</p>}
        {pending && <p className="mt-2 text-xs text-muted-foreground">先找回原任务{pending.method ? `（${UPDATE_LABELS[pending.method]}）` : ""}，不会重复提交已受理操作。</p>}
        {result?.task_id && <Link className="btn-ghost mt-2" href={`/platform/${row.platform}/runs/?mode=refresh&task=${encodeURIComponent(result.task_id)}`}>查看运行</Link>}
      </div>;
    })}</div>
    <p role="status" className="mt-4 text-xs text-muted-foreground">{accepted ? `已受理 ${accepted} 项，不代表凭据已经更新成功。已受理任务不会随关闭弹窗而取消。` : "只提交你选择的方式；任务和失败原因均保留在处理记录。"}</p>
  </Drawer>;
}
