"use client";

import { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { api, type AccountRow } from "@/lib/api";
import { Badge, Card, Empty, Stat, timeAgo } from "@/components/ui";
import { Drawer } from "@/components/drawer";

const STATUS_LABEL: Record<string, { text: string; tone: "ok" | "warn" | "bad" | "neutral" | "info" }> = {
  ok: { text: "正常", tone: "ok" },
  expired: { text: "过期", tone: "warn" },
  banned: { text: "封禁", tone: "bad" },
  invalid: { text: "失效", tone: "bad" },
  unknown: { text: "未测", tone: "neutral" },
};

export default function AccountsPage() {
  const [rows, setRows] = useState<AccountRow[]>([]);
  const [all, setAll] = useState<AccountRow[]>([]);
  const [kind, setKind] = useState("");
  const [platform, setPlatform] = useState("");
  const [status, setStatus] = useState("");
  const [busy, setBusy] = useState("");
  const [notice, setNotice] = useState("");
  const [error, setError] = useState("");
  const [treeDrawer, setTreeDrawer] = useState<string>("");
  const [tree, setTree] = useState<{
    root: string;
    total: number;
    groups: { platform: string; kind: string; count: number; items?: { ref: string; email: string; source: string }[] }[];
  } | null>(null);
  const [detail, setDetail] = useState<AccountRow | null>(null);
  const [showImport, setShowImport] = useState(false);
  const [importText, setImportText] = useState("");
  const [importPlatform, setImportPlatform] = useState("chatgpt");
  const [autoPwd, setAutoPwd] = useState(false);
  const [newPwd, setNewPwd] = useState("");
  const [picked, setPicked] = useState<Set<string>>(new Set());
  const [showPwd, setShowPwd] = useState(false);
  const [pwdValue, setPwdValue] = useState("");

  const load = useCallback(async () => {
    try {
      const [data, full] = await Promise.all([
        api.accounts({ kind, platform, status, limit: 300 }),
        api.accounts({ limit: 1000 }),
      ]);
      setRows(data.accounts);
      setAll(full.accounts);
      setError("");
    } catch (err) {
      setError((err as Error).message);
    }
  }, [kind, platform, status]);

  useEffect(() => {
    load();
    const timer = setInterval(load, 8000);
    return () => clearInterval(timer);
  }, [load]);

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

  const act = async (ref: string, plat: string, action: "liveness" | "refresh") => {
    setBusy(ref + action);
    setNotice("");
    try {
      if (action === "liveness") {
        await api.accountLiveness(ref, plat, false);
        setNotice(`已提交测活：${ref}`);
      } else {
        await api.accountRefresh(ref, plat, false);
        setNotice(`已提交刷新：${ref}`);
      }
      await load();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy("");
    }
  };

  const counts = rows.reduce<Record<string, number>>((acc, row) => {
    acc[row.status] = (acc[row.status] || 0) + 1;
    return acc;
  }, {});

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="text-xs text-muted-foreground">账号状态与已保存的结果版本分别查看；导出时可以明确选择完整凭据。</p>
        <Link href="/assets/" className="btn-primary">结果版本与导出</Link>
      </div>
      {(() => {
        const platformCards = [
          { slug: "claude", name: "Claude", tone: "bg-[#d97757]" },
          { slug: "chatgpt", name: "ChatGPT", tone: "bg-[#10a37f]" },
          { slug: "grok", name: "Grok", tone: "bg-foreground" },
          { slug: "outlook", name: "Outlook", tone: "bg-[#0078d4]" },
        ];
        return (
          <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
            {platformCards.map((card) => {
              const items = all.filter((row) => (row.platform || "") === card.slug);
              const ok = items.filter((row) => row.status === "ok").length;
              const active = platform === card.slug;
              return (
                <button
                  key={card.slug}
                  onClick={() => setPlatform(active ? "" : card.slug)}
                  className={`rounded-[var(--radius)] border p-3 text-left transition-colors ${
                    active ? "border-brand bg-brand/10" : "border-border bg-card hover:border-brand/40 hover:bg-accent/40"
                  }`}
                >
                  <div className="flex items-center gap-2">
                    <span className={`grid h-6 w-6 place-items-center rounded ${card.tone} text-[11px] font-semibold text-primary-foreground`}>
                      {card.name.slice(0, 1)}
                    </span>
                    <span className="flex-1 text-[13px] font-medium">{card.name}</span>
                    <span className="font-mono text-[12px]">{items.length}</span>
                  </div>
                  <div className="mt-1.5 flex items-center justify-between text-[11px] text-muted-foreground">
                    <span>可用 {ok}</span>
                    <span>{items.length === 0 ? "暂无资产" : `${items.length - ok} 待处理`}</span>
                  </div>
                </button>
              );
            })}
          </div>
        );
      })()}

      <Card
        title="统一资产库"
        actions={<span className="font-mono text-[11px] text-muted-foreground">{tree?.root ?? "/data/assets"}</span>}
        bodyClass="p-0"
      >
        <div className="grid gap-2 p-3 sm:grid-cols-2 xl:grid-cols-4">
          {["claude", "chatgpt", "grok", "outlook"].map((slug) => {
            const groups = (tree?.groups || []).filter((g) => g.platform === slug);
            const total = groups.reduce((sum, g) => sum + g.count, 0);
            return (
              <button
                key={slug}
                onClick={() => {
                  setPlatform(slug);
                  setTreeDrawer(slug);
                }}
                className={`rounded-[var(--radius)] border p-3 text-left transition-colors ${platform === slug ? "border-brand bg-brand/10" : "border-border bg-card hover:border-brand/40 hover:bg-accent/40"}`}
              >
                <div className="flex items-center justify-between">
                  <span className="text-[13px] font-medium">{slug}</span>
                  <span className="font-mono text-[12px]">{total}</span>
                </div>
                <div className="mt-1.5 flex flex-wrap gap-1">
                  {groups.length === 0 ? (
                    <span className="text-[11px] text-muted-foreground">暂无资产</span>
                  ) : (
                    groups.map((group) => (
                      <Badge key={group.kind} tone="info">{group.kind} {group.count}</Badge>
                    ))
                  )}
                </div>
              </button>
            );
          })}
        </div>
      </Card>

      <div className="grid grid-cols-2 gap-3 lg:grid-cols-5">
        <Stat label="账号总数" value={rows.length} hint="accounts 表" />
        <Stat label="正常" value={counts.ok ?? 0} tone="neutral" />
        <Stat label="过期/受限" value={counts.expired ?? 0} tone="warn" />
        <Stat label="封禁/失效" value={(counts.banned ?? 0) + (counts.invalid ?? 0)} tone="bad" />
        <Stat label="未测活" value={counts.unknown ?? 0} />
      </div>

      <Card
        title="账号主数据"
        actions={
          <>
            <select className="field w-[110px] py-1 text-[12px]" value={kind} onChange={(e) => setKind(e.target.value)}>
              <option value="">全部类型</option>
              <option value="emails">邮箱</option>
              <option value="cookies">Cookie</option>
              <option value="tokens">Token</option>
              <option value="outlook">Outlook</option>
            </select>
            <select className="field w-[110px] py-1 text-[12px]" value={platform} onChange={(e) => setPlatform(e.target.value)}>
              <option value="">全部平台</option>
              <option value="claude">claude</option>
              <option value="chatgpt">chatgpt</option>
              <option value="grok">grok</option>
              <option value="outlook">outlook</option>
            </select>
            <select className="field w-[110px] py-1 text-[12px]" value={status} onChange={(e) => setStatus(e.target.value)}>
              <option value="">全部状态</option>
              <option value="ok">正常</option>
              <option value="expired">过期</option>
              <option value="banned">封禁</option>
              <option value="unknown">未测</option>
            </select>
            <button className="btn-ghost" onClick={() => setShowImport(true)}>导入账号</button>
            <button
              className="btn-primary"
              disabled={picked.size === 0 || !!busy}
              onClick={async () => {
                setBusy("convert");
                try {
                  const data = await api.convertAccounts({ refs: Array.from(picked) });
                  setNotice(
                    `已排队 ${data.queued.length} 个转换任务${data.skipped.length ? `，跳过 ${data.skipped.length} 个（${data.skipped[0].reason}）` : ""}`,
                  );
                  setPicked(new Set());
                  await load();
                } catch (err) {
                  setError((err as Error).message);
                } finally {
                  setBusy("");
                }
              }}
              title="登录这些账号，产出标准 Free 凭据 JSON"
            >
              {busy === "convert" ? "转换中…" : `转 Free JSON ${picked.size > 0 ? `(${picked.size})` : ""}`}
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
          </>
        }
        bodyClass="p-0"
      >
        {error && <div className="px-4 py-2 text-[12px] text-destructive">{error}</div>}
        {notice && <div className="px-4 py-2 text-[12px] text-success">{notice}</div>}
        {rows.length === 0 ? (
          <div className="p-4">
            <Empty>
              {platform
                ? `${platform} 还没有资产。去「生产 → ${platform}」跑一轮流程，产出会自动进这里。`
                : "暂无账号主数据。点「同步资产」把邮箱池 / Cookie / Token 写进 accounts 表。"}
            </Empty>
          </div>
        ) : (
          <div className="max-h-[560px] overflow-auto">
            <table className="tbl">
              <thead>
                <tr className="border-b border-border">
                  <th className="px-4 py-2 text-left font-medium">账号</th>
                  <th className="px-2 py-2 text-left font-medium">平台</th>
                  <th className="px-2 py-2 text-left font-medium">状态</th>
                  <th className="px-2 py-2 text-left font-medium">测活</th>
                  <th className="px-2 py-2 text-left font-medium">领取</th>
                  <th className="px-4 py-2 text-right font-medium">操作</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((row) => {
                  const meta = STATUS_LABEL[row.status] || STATUS_LABEL.unknown;
                  return (
                    <tr
                      key={row.ref}
                      onClick={async () => {
                        try {
                          const data = await api.accountDetail(row.ref);
                          setDetail(data.account);
                        } catch {
                          setDetail(row);
                        }
                      }}
                      className="cursor-pointer border-b border-border/60 last:border-0 hover:bg-accent/40"
                    >
                      <td className="px-3 py-1.5" onClick={(e) => e.stopPropagation()}>
                        <input
                          type="checkbox"
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
                        {row.claimed ? <Badge tone="neutral">已领</Badge> : <Badge tone="ok">可供应</Badge>}
                      </td>
                      <td className="px-4 py-1.5 text-right">
                        <button
                          className="btn-ghost mr-1 px-2 py-0.5 text-[11px]"
                          disabled={busy === row.ref + "liveness"}
                          onClick={() => act(row.ref, row.platform, "liveness")}
                        >
                          测活
                        </button>
                        <button
                          className="btn-ghost px-2 py-0.5 text-[11px]"
                          disabled={busy === row.ref + "refresh"}
                          onClick={() => act(row.ref, row.platform, "refresh")}
                        >
                          刷新
                        </button>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      <Drawer
        open={!!treeDrawer}
        title={`${treeDrawer} 全部资产`}
        subtitle={tree?.root ? `${tree.root}/${treeDrawer}` : ""}
        onClose={() => setTreeDrawer("")}
        width={520}
      >
        <div className="space-y-3">
          {(tree?.groups || []).filter((g) => g.platform === treeDrawer).map((group) => (
            <div key={group.kind}>
              <div className="mb-1 flex items-center gap-2">
                <span className="text-[12px] font-medium">{group.kind}</span>
                <Badge tone="info">{group.count}</Badge>
              </div>
              <table className="tbl">
                <thead>
                  <tr>
                    <th>ref</th>
                    <th>邮箱</th>
                    <th>来源</th>
                  </tr>
                </thead>
                <tbody>
                  {(group.items || []).map((item) => (
                    <tr key={item.ref} className="cursor-pointer hover:bg-accent/40"
                        onClick={async () => {
                          try {
                            const data = await api.accountDetail(item.ref);
                            setDetail(data.account);
                            setTreeDrawer("");
                          } catch {
                            /* 没有索引条目时忽略 */
                          }
                        }}>
                      <td className="max-w-[200px] truncate font-mono text-[11px]">{item.ref}</td>
                      <td className="max-w-[180px] truncate text-[11px] text-muted-foreground">{item.email || "-"}</td>
                      <td className="text-[11px] text-muted-foreground">{item.source || "-"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ))}
          {(tree?.groups || []).filter((g) => g.platform === treeDrawer).length === 0 && (
            <Empty>该平台还没有资产。去「生产 → {treeDrawer}」跑一轮流程。</Empty>
          )}
        </div>
      </Drawer>

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
                  setNotice(`导入 ${data.imported} 个账号${data.tasks.length ? `，已排 ${data.tasks.length} 个改密码任务` : ""}`);
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
        open={!!detail}
        title={detail?.identity || detail?.ref || "账号"}
        subtitle={detail ? `${detail.platform || "-"} · ${detail.kind || "-"}` : ""}
        onClose={() => setDetail(null)}
        footer={
          detail ? (
            <>
              <button
                className="btn-ghost px-2 py-1 text-[11px]"
                disabled={busy === detail.ref + "liveness"}
                onClick={async () => {
                  setBusy(detail.ref + "liveness");
                  await api.accountLiveness(detail.ref, detail.platform, false).catch(() => null);
                  setBusy("");
                  await load();
                }}
              >
                测活
              </button>
              <button
                className="btn-primary px-3 py-1 text-[11px]"
                disabled={busy === detail.ref + "refresh"}
                onClick={async () => {
                  setBusy(detail.ref + "refresh");
                  await api.accountRefresh(detail.ref, detail.platform, false).catch(() => null);
                  setBusy("");
                  await load();
                }}
              >
                刷新凭据
              </button>
            </>
          ) : null
        }
      >
        {detail && (
          <table className="tbl">
            <tbody>
              {[
                { k: "ref", v: detail.ref },
                { k: "平台", v: detail.platform || "-" },
                { k: "类型", v: detail.kind || "-" },
                { k: "状态", v: detail.status || "unknown" },
                { k: "领取", v: detail.claimed ? `已领（${detail.claimed_at || "-"}）` : "可供应" },
                { k: "刷新状态", v: detail.refresh_state || "-" },
                { k: "刷新时间", v: detail.refresh_at || "-" },
                { k: "测活时间", v: detail.health_checked_at || "从未" },
                { k: "测活信息", v: detail.health_message || "-" },
                { k: "源文件", v: detail.secret_ref || "-" },
                { k: "备注", v: detail.note || "-" },
                { k: "更新时间", v: detail.updated_at || "-" },
              ].map((row) => (
                <tr key={row.k}>
                  <td className="w-[86px] text-[11px] text-muted-foreground">{row.k}</td>
                  <td className="break-all font-mono text-[11.5px]">{row.v}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Drawer>

      <Card title="治理规则">
        <ul className="space-y-1.5 text-[11.5px] leading-relaxed text-muted-foreground">
          <li>· 测活/刷新会以任务形式进队列，受并发与内存闸门约束，不会绕过调度直接扫号。</li>
            <li>· 刷新会直接写回当前凭据 JSON；已交付账号也可由管理员按需修复 401。</li>
          <li>· 封禁类只标记不可修；超时/断连标 unknown，不自动重试。</li>
          <li>· 定时巡检开关与频率在「配置」页：REG_FACTORY_LIVENESS_*。</li>
        </ul>
      </Card>
    </div>
  );
}
