"use client";

import { useEffect, useState } from "react";
import { api } from "@/lib/api";
import { Card } from "@/components/ui";

type Result = { ref: string; email: string; kind: string; source: string; created_at?: string; credential_updated_at?: string; result_sha256: string; is_current?: boolean };

export function AssetResults() {
  const [assets, setAssets] = useState<Result[]>([]);
  const [ref, setRef] = useState("");
  const [query, setQuery] = useState("");
  const [results, setResults] = useState<Result[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [reload, setReload] = useState(0);
  useEffect(() => {
    let active = true;
    const timer = setTimeout(() => api.assetResults("", query).then(data => {
      if (active) { setAssets(data.items || []); setError(""); }
    }).catch(err => { if (active) setError(err.message); }), 250);
    return () => { active = false; clearTimeout(timer); };
  }, [reload, query]);
  useEffect(() => {
    let active = true;
    setResults([]); setError(""); setNotice("");
    if (!ref) { setBusy(false); return; }
    setBusy(true);
    api.assetResults(ref).then(data => { if (active) setResults((data.items || []).filter(item => ["oauth", "team", "free"].includes(item.kind))); })
      .catch(err => { if (active) setError(err.message); }).finally(() => { if (active) setBusy(false); });
    return () => { active = false; };
  }, [ref, reload]);
  async function download(result: Result) {
    setBusy(true); setError(""); setNotice("");
    try {
      const response = await fetch(`/api/assets/export?${new URLSearchParams({ref: result.ref, result_sha256: result.result_sha256, kind: result.kind})}`, {cache: "no-store"});
      const data = await response.json();
      if (!response.ok) throw new Error(data.detail || data.error || "导出失败");
      if (data.accounts?.length !== 1) throw new Error("结果数量不一致，未生成文件");
      const url = URL.createObjectURL(new Blob([JSON.stringify(data.accounts[0], null, 2)], {type: "application/json"}));
      const link = document.createElement("a"); link.href = url;
      link.download = `${(result.email || "team").replace(/[^a-zA-Z0-9@._-]/g, "_")}-${result.result_sha256.slice(0,12)}.json`;
      link.click(); URL.revokeObjectURL(url);
      setNotice("已导出账号当前凭据。");
    } catch (err) { setError((err as Error).message); } finally { setBusy(false); }
  }
  return <Card title="账号当前凭据" actions={<button className="btn" disabled={busy} onClick={() => setReload(n => n+1)}>重新读取</button>}>
    <div className="space-y-3">
      <p className="text-xs text-muted-foreground">每个账号只提供最近一次成功保存的凭据。失败原因和旧任务时间在运行记录中查看。</p>
      <input aria-label="搜索结果账号" placeholder="搜索邮箱或账号引用" className="w-full rounded border border-border bg-card p-2 text-sm" value={query} onChange={e => {setQuery(e.target.value); setRef("");}} />
      {assets.length === 100 && <p className="text-xs text-muted-foreground">显示前 100 个账号，请输入邮箱缩小范围。</p>}
      <select aria-label="选择结果账号" className="w-full rounded border border-border bg-card p-2 text-sm" value={ref} onChange={e => setRef(e.target.value)}>
        <option value="">选择账号</option>
        {[...new Map(assets.map(item => [item.ref, item])).values()].map(item => <option key={item.ref} value={item.ref}>{item.email || item.ref} · {item.ref}</option>)}
      </select>
      {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
      {notice && <p role="status" className="text-xs text-success">{notice}</p>}
      {busy && <p role="status" className="text-xs">读取中…</p>}
      {ref && !busy && !results.length && <p className="text-xs text-muted-foreground">没有可导出的已保存结果。</p>}
      <div className="max-h-80 overflow-auto">
        {results.map(result => <div key={result.result_sha256} className="flex flex-wrap items-center gap-3 border-b border-border py-3 text-xs">
          <div className="min-w-0 flex-1"><p>{result.email || result.ref}</p><p className="mt-1 break-all text-muted-foreground">凭据保存时间：{result.credential_updated_at ? new Date(result.credential_updated_at).toLocaleString() : "历史资料未记录保存时间"}</p></div>
          <button className="btn-primary" disabled={busy} onClick={() => download(result)}>下载当前凭据</button>
        </div>)}
      </div>
    </div>
  </Card>;
}
