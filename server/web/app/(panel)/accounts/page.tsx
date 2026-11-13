"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import Link from "next/link";
import { api, type AccountRow, type AccountSummary } from "@/lib/api";
import { Badge, Card, Empty, timeAgo } from "@/components/ui";
import { Drawer } from "@/components/drawer";
import { pendingCredentialCheck, startCredentialCheck } from "@/lib/account-actions";
import { CredentialUpdateDialog, type UpdateTarget } from "@/components/credential-update-dialog";
import { AccountSources } from "@/components/account-sources";
import { AccountSummaryStrip } from "@/components/account-summary-strip";
import { LIBRARY_LABELS } from "@/components/account-navigation";
import { PLATFORMS } from "@/lib/task-catalog";

const STATUS_LABEL: Record<string, { text: string; tone: "ok" | "warn" | "bad" | "neutral" | "info" }> = {
  ok: { text: "正常", tone: "ok" },
  expired: { text: "过期", tone: "warn" },
  needs_refresh: { text: "AT 待刷新", tone: "warn" },
  banned: { text: "封禁", tone: "bad" },
  workspace_deactivated: { text: "空间停用", tone: "bad" },
  invalid: { text: "失效", tone: "bad" },
  unknown: { text: "未测", tone: "neutral" },
};

export default function AccountsPage() {
  const [rows, setRows] = useState<AccountRow[]>([]);
  const [summaries,setSummaries]=useState<AccountSummary[] | null>(null);
  const [total,setTotal]=useState(0);
  const [offset,setOffset]=useState(0);
  const [loading,setLoading]=useState(false);
  const requestSequence=useRef(0);
  const [kind, setKind] = useState("");
  const [platform, setPlatform] = useState("");
  const [status, setStatus] = useState("");
  const [busy, setBusy] = useState("");
  const [notice, setNotice] = useState("");
  const [error, setError] = useState("");
  const [updateRequest,setUpdateRequest]=useState<{targets:UpdateTarget[]} | null>(null);
  const [checkLink,setCheckLink]=useState("");
  const [detail, setDetail] = useState<AccountRow | null>(null);
  const [detailTarget, setDetailTarget] = useState<AccountRow | null>(null);
  const [detailError, setDetailError] = useState("");
  const detailRequest = useRef<AbortController | null>(null);
  const [showImport, setShowImport] = useState(false);
  const [importText, setImportText] = useState("");
  const [importPlatform, setImportPlatform] = useState("chatgpt");
  const [autoPwd, setAutoPwd] = useState(false);
  const [newPwd, setNewPwd] = useState("");
  const [picked, setPicked] = useState<Set<string>>(new Set());
  const [showPwd, setShowPwd] = useState(false);
  const [pwdValue, setPwdValue] = useState("");

  const load = useCallback(async (signal?: AbortSignal) => {
    const sequence=++requestSequence.current;
    setLoading(true);
    try {
      const [data, full] = await Promise.all([
        api.accounts({ kind, platform, status, limit: 50, offset },signal),
        api.accountSummary("",signal),
      ]);
      if(signal?.aborted || sequence!==requestSequence.current)return;
      setRows(data.accounts);
      setTotal(data.total);
      if(offset > 0 && offset >= data.total)setOffset(Math.max(0,Math.ceil(data.total/50)-1)*50);
      setSummaries(full.platforms);
      setError("");
    } catch (err) {
      if(!signal?.aborted && sequence===requestSequence.current)setError(`读取失败，保留上次结果。${(err as Error).message}`);
    } finally { if(!signal?.aborted && sequence===requestSequence.current)setLoading(false); }
  }, [kind, platform, status,offset]);

  useEffect(() => {
    const controller=new AbortController();
    let timer:ReturnType<typeof setTimeout>;
    const poll=async()=>{await load(controller.signal);if(!controller.signal.aborted)timer=setTimeout(poll,document.hidden?30000:15000);};
    void poll();
    return () => {controller.abort();clearTimeout(timer);};
  }, [load]);

  useEffect(()=>{const value=new URLSearchParams(window.location.search).get("platform");if(value)setPlatform(value);},[]);
  useEffect(()=>{setPicked(new Set());},[kind,platform,status,offset]);
  useEffect(()=>()=>detailRequest.current?.abort(),[]);

  const openDetail = async (row: AccountRow) => {
    detailRequest.current?.abort();
    const controller=new AbortController();
    detailRequest.current=controller;
    setDetailTarget(row);setDetail(null);setDetailError("");
    try {
      const result=await api.accountDetail(row.ref,controller.signal);
      if(!controller.signal.aborted)setDetail(result.account);
    } catch(err) {
      if(!controller.signal.aborted)setDetailError(`详情读取失败，可重试。${(err as Error).message}`);
    }
  };
  const closeDetail = () => {detailRequest.current?.abort();setDetailTarget(null);setDetail(null);};

  const sync = async () => {
    setBusy("sync");
    try {
      const data = await api.syncAccounts();
      setNotice(`已同步 ${data.synced} 条`);
      await load();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy("");
    }
  };

  const check = async (refs: string[], plat: string) => {
    if (busy) return;
    setBusy("check");
    setNotice("");
    setError("");setCheckLink("");
    try {
      const pending=pendingCredentialCheck(plat);
      const result = await startCredentialCheck(plat,pending?.refs || refs,pending?.parent_id);
      setNotice(pending ? "已找回原检查批次，请查看运行结果。" : "检查已受理，请查看运行结果。");
      setCheckLink(`/platform/${plat}/runs/?batch=${encodeURIComponent(result.task_id)}`);
    } catch (err) {
      setError(`${(err as Error).message}。再次检查会先恢复原批次。`);
    } finally {
      setBusy("");
    }
  };
  const openUpdate=(targets:UpdateTarget[])=>{closeDetail();setUpdateRequest({targets});};
  const selectedRows=rows.filter(row=>picked.has(row.ref));
  const selectedPlatform=selectedRows.length && selectedRows.every(row=>row.platform===selectedRows[0].platform) ? selectedRows[0].platform : "";

  const summary=useMemo<AccountSummary | null>(()=>summaries ? summaries.filter(item=>!platform || item.platform===platform).reduce((sum,item)=>({platform,total:sum.total+item.total,available:sum.available+item.available,abnormal:sum.abnormal+item.abnormal,unknown:sum.unknown+item.unknown,used:sum.used+item.used,leased:sum.leased+item.leased,source_count:sum.source_count+item.source_count}),{platform,total:0,available:0,abnormal:0,unknown:0,used:0,leased:0,source_count:0}) : null,[summaries,platform]);

  return (
    <div className="space-y-4">
      <CredentialUpdateDialog request={updateRequest} onClose={()=>setUpdateRequest(null)} onAccepted={()=>void load()}/>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="text-xs text-muted-foreground">账号执行和下载使用最近一次成功保存的凭据，旧任务保留执行记录。</p>
        <Link href="/assets/" className="btn-primary">当前凭据与导出</Link>
      </div>
      <nav aria-label="平台筛选" className="flex flex-wrap gap-2">
        {[{slug:"",name:"全部平台"},...PLATFORMS].map(item=><button key={item.slug} aria-pressed={platform===item.slug} className={platform===item.slug ? "btn-primary" : "btn-ghost"} onClick={()=>{setPlatform(item.slug);setOffset(0);}}>{item.name}<span className="ml-2 tabular-nums opacity-75">{summaries ? (item.slug ? summaries.find(row=>row.platform===item.slug)?.total || 0 : summaries.reduce((sum,row)=>sum+row.total,0)) : "—"}</span></button>)}
      </nav>
      <AccountSummaryStrip summary={summary}/>

      <Card
        className="sg-account-management"
        title={`账号记录 · ${total} 个匹配账号`}
        actions={
          <>
            <select aria-label="来源类型" className="field w-[110px] py-1 text-[12px]" value={kind} onChange={(e) => {setKind(e.target.value);setOffset(0);}}>
              <option value="">全部类型</option>
              <option value="oauth">OAuth</option>
              <option value="imported">导入资料</option>
              <option value="free">Free</option>
              <option value="team">Team</option>
              <option value="mail">邮箱账号</option>
              <option value="emails">邮箱</option>
              <option value="cookies">Cookie</option>
              <option value="tokens">Token</option>
              <option value="outlook">Outlook</option>
            </select>
            <select aria-label="凭据状态" className="field w-[110px] py-1 text-[12px]" value={status} onChange={(e) => {setStatus(e.target.value);setOffset(0);}}>
              <option value="">全部状态</option>
              <option value="ok">正常</option>
              <option value="expired">过期</option>
              <option value="needs_refresh">AT 待刷新</option>
              <option value="banned">封禁</option>
              <option value="workspace_deactivated">空间停用</option>
              <option value="unknown">未测</option>
            </select>
            <button className="btn-ghost" onClick={() => setShowImport(true)}>导入账号</button>
            <Link className="btn-ghost" href={`/platform/${platform || "chatgpt"}/checks/`}>处理记录</Link>
            <button className="btn-ghost" disabled={!selectedPlatform || !!busy} title={!selectedPlatform ? "选择同一平台的账号后批量检查" : "只检查当前凭据，不更新凭据"} onClick={()=>check(selectedRows.map(row=>row.ref),selectedPlatform)}>检查可用性</button>
            <button
              className="btn-primary"
              disabled={picked.size === 0 || !!busy}
              onClick={()=>openUpdate(selectedRows)}
              title="选择 RT 刷新或登录重新授权"
            >
              更新凭据 {picked.size > 0 ? `(${picked.size})` : ""}
            </button>
            <button
              className="btn-ghost"
              disabled={picked.size === 0}
              onClick={() => setShowPwd(true)}
              title="给选中的账号批量改密码"
            >
              改密码 {picked.size > 0 ? `(${picked.size})` : ""}
            </button>
            <button className="btn-ghost" onClick={sync} disabled={busy === "sync"}>
              同步资产
            </button>
            <button className="btn-ghost" onClick={()=>void load()} disabled={loading}>{loading ? "读取中…" : "更新列表"}</button>
          </>
        }
        bodyClass="p-0"
      >
        {error && <div className="px-4 py-2 text-[12px] text-destructive" role="alert">{error}</div>}
        {notice && <div className="px-4 py-2 text-[12px]" role="status">{notice}{checkLink && <Link className="ml-2 underline" href={checkLink}>查看运行</Link>}</div>}
        {!summaries ? <p className="p-6 text-sm text-muted-foreground" role="status">{error ? "数据尚未加载，请重试更新列表。" : "正在读取账号…"}</p> : rows.length === 0 ? (
          <div className="p-4">
            <Empty>
              当前筛选下没有账号。可以调整筛选，或导入账号资料。
            </Empty>
          </div>
        ) : (
          <div className="max-h-[560px] overflow-auto">
            <table className="tbl whitespace-nowrap" aria-label="全部账号列表" aria-busy={loading}>
              <thead>
                <tr className="border-b border-border">
                  <th className="px-3 py-2"><span className="sr-only">选择</span></th>
                  <th className="px-4 py-2 text-left font-medium">账号</th>
                  <th className="px-2 py-2 text-left font-medium">平台</th>
                  <th className="px-2 py-2 text-left font-medium">状态</th>
                  <th className="px-2 py-2 text-left font-medium">测活</th>
                  <th className="px-2 py-2 text-left font-medium">密码</th>
                  <th className="px-2 py-2 text-left font-medium">业务状态</th>
                  <th className="px-2 py-2 text-left font-medium">来源</th>
                  <th className="px-4 py-2 text-right font-medium">操作</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((row) => {
                  const meta = STATUS_LABEL[row.status] || STATUS_LABEL.unknown;
                  const used=row.claimed || row.library_status==="used";
                  return (
                    <tr
                      key={row.ref}
                      onClick={() => void openDetail(row)}
                      className="cursor-pointer border-b border-border/60 last:border-0 hover:bg-accent/40"
                    >
                      <td className="px-3 py-1.5" onClick={(e) => e.stopPropagation()}>
                        <input
                          type="checkbox"
                          aria-label={`选择 ${row.identity || row.ref}`}
                          checked={picked.has(row.ref)}
                          onChange={(e) => {
                            const next = new Set(picked);
                            if (e.target.checked) next.add(row.ref); else next.delete(row.ref);
                            setPicked(next);
                          }}
                        />
                      </td>
                      <td className="max-w-[280px] truncate px-4 py-1.5 font-mono text-[11px] text-foreground" title={row.ref}>
                        {row.identity || row.ref}
                      </td>
                      <td className="px-2 py-1.5">
                        <Badge tone="info">{row.platform || "-"}</Badge>
                      </td>
                      <td className="px-2 py-1.5">
                        <Badge tone={meta.tone}>{meta.text}</Badge>
                      </td>
                      <td className="px-2 py-1.5 text-muted-foreground">
                        {row.health_checked_at ? timeAgo(row.health_checked_at) : "从未"}
                      </td>
                      <td className="px-2 py-1.5">
                        <Badge tone={row.password_state === "changed" ? "ok" : row.password_state === "failed" ? "bad" : row.password_state === "pending" ? "warn" : "neutral"}>
                          {row.password_state === "changed" ? "已改" : row.password_state === "failed" ? "失败" : row.password_state === "pending" ? "排队" : "未改"}
                        </Badge>
                      </td>
                      <td className="px-2 py-1.5">
                        <Badge tone={row.library_status==="available" ? "ok" : row.library_status==="leased" ? "warn" : "neutral"}>{LIBRARY_LABELS[row.library_status || (used ? "used" : "pending")] || "待核验"}</Badge>
                      </td>
                      <td className="px-2 py-1.5" onClick={event=>event.stopPropagation()}><button className="btn-ghost tabular-nums" onClick={()=>void openDetail(row)} aria-label={`查看 ${row.identity || row.ref} 的来源`}>{row.source_count ?? 1} 份来源</button></td>
                      <td className="px-4 py-1.5 text-right" onClick={event=>event.stopPropagation()}>
                        <button
                          className="btn-ghost mr-1 px-2 py-0.5 text-[11px]"
                          disabled={!!busy}
                          onClick={() => check([row.ref], row.platform)}
                        >
                          {row.platform==="chatgpt" ? "检查可用性" : "核对报告"}
                        </button>
                        <button
                          className="btn-ghost px-2 py-0.5 text-[11px]"
                          disabled={!!busy || used}
                          title={used ? "已使用账号在主系统原任务中刷新" : undefined}
                          onClick={() => openUpdate([row])}
                        >
                          更新凭据
                        </button>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
        <footer className="sg-task-footer"><span>{summaries ? `${total ? offset+1 : 0}–${Math.min(offset+50,total)} / ${total}` : "—"}</span><div className="flex gap-2"><button className="btn-ghost" disabled={offset===0 || loading || !!busy} onClick={()=>setOffset(Math.max(0,offset-50))}>上一页</button><button className="btn-ghost" disabled={offset+50>=total || loading || !!busy} onClick={()=>setOffset(offset+50)}>下一页</button></div></footer>
      </Card>

      <Drawer
        open={showPwd}
        title={`批量改密码（${picked.size} 个）`}
        subtitle="使用导入时保存的旧密码与 TOTP；失败会在账号列表标记"
        onClose={() => setShowPwd(false)}
        footer={
          <>
            <button className="btn-ghost px-2 py-1 text-[11px]" onClick={() => setShowPwd(false)}>取消</button>
            <button
              className="btn-primary px-3 py-1 text-[11px]"
              disabled={!!busy || !pwdValue.trim()}
              onClick={async () => {
                setBusy("pwd");
                try {
                  const data = await api.changePasswords({ refs: Array.from(picked), new_password: pwdValue });
                  setNotice(`已排队 ${data.queued.length} 个，跳过 ${data.skipped.length} 个`);
                  setShowPwd(false);
                  setPwdValue("");
                  setPicked(new Set());
                  await load();
                } catch (err) {
                  setError((err as Error).message);
                } finally {
                  setBusy("");
                }
              }}
            >
              开始改密码
            </button>
          </>
        }
      >
        <div className="space-y-2">
          <label className="label">统一新密码</label>
          <input className="field py-1.5 text-[12.5px]" value={pwdValue} onChange={(e) => setPwdValue(e.target.value)} />
          <p className="text-[11px] leading-relaxed text-muted-foreground">
            只对「导入过凭据」的账号生效；已领取账号会跳过；每个账号排一个任务，结果写回账号的密码状态列。
          </p>
        </div>
      </Drawer>

      <Drawer
        open={showImport}
        title="导入已接码账号"
        subtitle="每行 email----password 或 email；导入后可选自动改密码"
        onClose={() => setShowImport(false)}
        width={560}
        footer={
          <>
            <button className="btn-ghost px-2 py-1 text-[11px]" onClick={() => setShowImport(false)}>取消</button>
            <button
              className="btn-primary px-3 py-1 text-[11px]"
              disabled={!!busy || !importText.trim()}
              onClick={async () => {
                setBusy("import");
                try {
                  const data = await api.importAccounts({
                    accounts: importText,
                    platform: importPlatform,
                    auto_change_password: autoPwd,
                    new_password: autoPwd ? newPwd : "",
                  });
                  setNotice(`导入 ${data.imported} 个账号${data.tasks.length ? `，已排 ${data.tasks.length} 个改密码任务` : ""}。首次生成凭据：选择账号 → 更新凭据 → 登录重新授权。`);
                  setShowImport(false);
                  setImportText("");
                  await load();
                } catch (err) {
                  setError((err as Error).message);
                } finally {
                  setBusy("");
                }
              }}
            >
              导入
            </button>
          </>
        }
      >
        <div className="space-y-2">
          <div>
            <label className="label">平台</label>
            <select className="field py-1.5 text-[12.5px]" value={importPlatform} onChange={(e) => setImportPlatform(e.target.value)}>
              <option value="chatgpt">ChatGPT</option>
              <option value="claude">Claude</option>
              <option value="grok">Grok</option>
              <option value="outlook">Outlook</option>
            </select>
          </div>
          <div>
            <label className="label">账号列表</label>
            <textarea
              className="field h-[200px] font-mono text-[12px]"
              placeholder={"user1@icloud.com----password\nuser2@icloud.com----password\nuser3@icloud.com"}
              value={importText}
              onChange={(e) => setImportText(e.target.value)}
            />
            <p className="mt-1 text-[11px] text-muted-foreground">
              支持 <span className="font-mono">email----password</span>、<span className="font-mono">email password</span> 或只有邮箱。
            </p>
          </div>
          <label className="flex items-center gap-2 text-[12.5px]">
            <input type="checkbox" checked={autoPwd} onChange={(e) => setAutoPwd(e.target.checked)} />
            导入后自动改密码（账号归我们控制）
          </label>
          {autoPwd && (
            <div>
              <label className="label">新密码</label>
              <input className="field py-1.5 text-[12.5px]" type="text" placeholder="统一新密码"
                     value={newPwd} onChange={(e) => setNewPwd(e.target.value)} />
              <p className="mt-1 text-[11px] text-muted-foreground">
                每个账号会排一个 change_password 任务；需要旧密码才能改（只有邮箱的账号会失败并要求补密码）。
              </p>
            </div>
          )}
        </div>
      </Drawer>

      <Drawer
        open={!!detailTarget}
        title={detailTarget?.identity || detailTarget?.ref || "账号"}
        subtitle={detail ? `${detail.platform || "-"} · ${detail.kind || "-"}` : ""}
        onClose={closeDetail}
        width={600}
        footer={
          detail ? (
            <>
              <button
                className="btn-ghost px-2 py-1 text-[11px]"
                disabled={!!busy}
                onClick={() => {closeDetail();void check([detail.ref], detail.platform);}}
              >
                {detail.platform==="chatgpt" ? "检查可用性" : "核对报告"}
              </button>
              <button
                className="btn-primary px-3 py-1 text-[11px]"
                disabled={!!busy || detail.claimed || detail.library_status==="used"}
                title={detail.claimed || detail.library_status==="used" ? "已使用账号在主系统原任务中刷新" : undefined}
                onClick={() => openUpdate([detail])}
              >
                更新凭据
              </button>
            </>
          ) : null
        }
      >
        {detailError ? <div role="alert"><p className="text-sm text-destructive">{detailError}</p><button className="btn-ghost mt-3" onClick={()=>detailTarget && void openDetail(detailTarget)}>重试读取详情</button></div> : detailTarget && !detail && <p className="text-sm text-muted-foreground" role="status">正在读取账号详情…</p>}
        {detail && (
          <><table className="tbl">
            <tbody>
              {[
                { k: "ref", v: detail.ref },
                { k: "平台", v: detail.platform || "-" },
                { k: "类型", v: detail.kind || "-" },
                { k: "状态", v: detail.status || "unknown" },
                { k: "业务状态", v: LIBRARY_LABELS[detail.library_status || (detail.claimed ? "used" : "pending")] || "待核验" },
                { k: "领取", v: detail.claimed ? `已领（${detail.claimed_at || "-"}）` : "未领取" },
                { k: "版本", v: String(detail.version ?? "—") },
                { k: "刷新状态", v: detail.refresh_state || "-" },
                { k: "刷新时间", v: detail.refresh_at || "-" },
                { k: "测活时间", v: detail.health_checked_at || "从未" },
                { k: "测活信息", v: detail.health_message || "-" },
                { k: "源文件", v: detail.secret_ref || "-" },
                { k: "备注", v: detail.note || "-" },
                  { k: "凭据保存", v: detail.credential_updated_at || "历史资料未记录" },
                  { k: "状态更新", v: detail.updated_at || "-" },
              ].map((row) => (
                <tr key={row.k}>
                  <td className="w-[86px] text-[11px] text-muted-foreground">{row.k}</td>
                  <td className="break-all font-mono text-[11.5px]">{row.v}</td>
                </tr>
              ))}
            </tbody>
          </table><AccountSources account={detail} onSelect={source=>void openDetail(source)}/></>
        )}
      </Drawer>

      <Card title="治理规则">
        <ul className="space-y-1.5 text-[11.5px] leading-relaxed text-muted-foreground">
          <li>· OAuth 测试只使用 AT，不交换 RT；401 表示需要刷新后复测，不代表账号或订阅失效。</li>
          <li>· 更新凭据时选择 RT 刷新或登录重新授权；首次生成选择重新授权。已领取账号由主系统原任务更新。</li>
          <li>· 凭据任务超时或断连按失败结束并释放占用；可重新发起修复，不重复提交可能已消耗的 RT。</li>
          <li>· 周期巡检默认关闭，在「配置 → 运行与资源 → 周期巡检」管理；只检查可用性。</li>
        </ul>
      </Card>
    </div>
  );
}
