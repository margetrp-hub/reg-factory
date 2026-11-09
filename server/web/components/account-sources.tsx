"use client";

import { useEffect, useState } from "react";
import { api, type AccountRow } from "@/lib/api";
import { Badge, ErrorNote } from "@/components/ui";
import { LIBRARY_LABELS } from "@/components/account-navigation";

export function AccountSources({account,onSelect}: {account: AccountRow; onSelect: (source: AccountRow)=>void}) {
  const [sources,setSources]=useState<AccountRow[] | null>(null);
  const [error,setError]=useState("");
  const [loading,setLoading]=useState(false);
  const [reload,setReload]=useState(0);
  useEffect(()=>{
    const controller=new AbortController();
    setLoading(true);
    setSources(null);setError("");
    api.accountSources(account.ref,controller.signal).then(result=>{
      if (!controller.signal.aborted) { setSources(result.accounts);setError(""); }
    }).catch(err=>{ if(!controller.signal.aborted)setError(`来源读取失败，可重试。${(err as Error).message}`); })
      .finally(()=>{if(!controller.signal.aborted)setLoading(false);});
    return ()=>controller.abort();
  },[account.ref,reload]);
  return <section aria-label="原始来源记录" className="mt-6 border-t border-border pt-5" aria-busy={loading}>
    <header className="flex items-center justify-between gap-3"><h3 className="text-sm font-medium">来源记录 <span className="ml-1 font-normal tabular-nums text-muted-foreground">{sources?.length ?? "—"}</span></h3><button type="button" className="btn-ghost" disabled={loading} onClick={()=>setReload(value=>value+1)}>{loading ? "读取中…" : "重新读取来源"}</button></header>
    <p className="mt-2 text-xs leading-relaxed text-muted-foreground">旧引用用于追溯任务，执行和下载统一使用当前凭据。账号复用或修复成功后更新，失败保留上一次成功资料。</p>
    <p className="mt-2 text-xs text-muted-foreground">凭据保存时间：{account.credential_updated_at ? new Date(account.credential_updated_at).toLocaleString() : "历史资料未记录保存时间"}</p>
    <ErrorNote message={error}/>
    {!sources && loading && <p className="py-5 text-sm text-muted-foreground" role="status">正在读取来源记录…</p>}
    {sources && <div className="mt-3 divide-y divide-border">{sources.map(source=><div key={source.ref} className="py-3">
      <div className="flex items-center justify-between gap-3"><div className="flex min-w-0 flex-wrap items-center gap-2"><span className="text-sm font-medium">{source.kind || "账号资料"}</span><Badge tone={source.library_status==="available" ? "ok" : source.library_status==="leased" ? "warn" : "neutral"}>{LIBRARY_LABELS[source.library_status || "pending"] || "待核验"}</Badge><span className="text-xs tabular-nums text-muted-foreground">版本 {source.version ?? "—"}</span></div><button type="button" className="btn-ghost shrink-0" aria-pressed={source.ref===account.ref} onClick={()=>onSelect(source)}>查看来源</button></div>
      <p className="mt-2 break-all font-mono text-xs text-muted-foreground">{source.ref}</p>
      {source.refresh_state && <p className="mt-1 text-xs text-muted-foreground">原刷新记录：{source.refresh_state} · {source.refresh_at || "尚无时间"}</p>}
    </div>)}</div>}
    {sources?.length===0 && <p className="py-4 text-sm text-muted-foreground">没有可查看的来源记录</p>}
  </section>;
}
