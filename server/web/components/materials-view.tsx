"use client";

import { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { api, type AccountRow } from "@/lib/api";
import { Badge, Card, Empty, Stat } from "@/components/ui";
import { Drawer } from "@/components/drawer";

type Entry = { key: string; value: string; secret: boolean; line: number };
type SourceRow = {
  id: number; name: string; host: string; port: number; username: string; folder: string;
  enabled: boolean; lastUID: number; passwordSet: boolean;
};

const KINDS = {
  mail: {
    title: "邮箱供给",
    desc: "IC 多源索引、Outlook 池、临时邮箱供应商。",
    prefixes: ["ICMAIL", "ICLOUD", "YYDS", "GPTMAIL", "CFMAIL", "MOEMAIL", "REMAIL", "CUSTOM_MAIL"],
  },
  proxy: {
    title: "代理出口",
    desc: "住宅池、Clash 节点与出口模式。",
    prefixes: ["PROXY", "CLASH"],
  },
  sms: {
    title: "接码",
    desc: "短信 / 电话验证供应商与号码池。",
    prefixes: ["SMS", "HERO"],
  },
  captcha: {
    title: "验证码",
    desc: "打码平台与视觉求解（hCaptcha / Turnstile / Arkose）。",
    prefixes: ["CAPSOLVER", "EZCAPTCHA", "YESCAPTCHA", "VISION", "VOTE", "GEMMA"],
  },
} as const;

type Kind = keyof typeof KINDS;
export const MATERIAL_KINDS: Kind[] = ["mail", "proxy", "sms", "captcha"];

export function MaterialsView({ kind }: { kind?: Kind }) {
  const [config, setConfig] = useState<Record<string, Entry[]>>({});
  const [accounts, setAccounts] = useState<AccountRow[]>([]);
  const [icStats, setIcStats] = useState<{ indexed: number; sources: number; byTag: Record<string, number> } | null>(null);
  const [icSources, setIcSources] = useState<SourceRow[]>([]);
  const [error, setError] = useState("");
  const [editKey, setEditKey] = useState<Entry | null>(null);
  const [editValue, setEditValue] = useState("");
  const [busy, setBusy] = useState("");

  const load = useCallback(async () => {
    try {
      const [cfg, acc, health, sources] = await Promise.all([
        api.config(),
        api.accounts({ limit: 500 }),
        fetch("/api/apps/icmail/health", { credentials: "same-origin" }).then((r) => r.json()).catch(() => null),
        fetch("/api/apps/icmail/sources", { credentials: "same-origin" }).then((r) => r.json()).catch(() => null),
      ]);
      setConfig(cfg.groups);
      setAccounts(acc.accounts);
      if (health?.ok) setIcStats(health.data.stats);
      if (sources?.ok) setIcSources(sources.data.sources || []);
      setError("");
    } catch (err) {
      setError((err as Error).message);
    }
  }, []);

  useEffect(() => {
    load();
    const timer = setInterval(load, 20000);
    return () => clearInterval(timer);
  }, [load]);

  const entriesOf = (prefixes: readonly string[]) =>
    Object.values(config)
      .flat()
      .filter((entry) => prefixes.some((prefix) => entry.key.toUpperCase().startsWith(prefix)));

  const outlookCount = accounts.filter((row) => row.platform === "outlook").length;
  const mailboxCount = accounts.filter((row) => row.kind === "emails").length;

  if (!kind) {
    return (
      <div className="space-y-4">
        <Header title="物料中心" desc="消耗类资源按类分页管理；注册产出在资产中心，这里只放供给与余额。" />
        {error && <ErrorLine text={error} />}
        <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
          <Stat label="邮箱池" value={mailboxCount} hint={`Outlook ${outlookCount}`} />
          <Stat label="IC 索引" value={icStats?.indexed ?? 0} hint={`源 ${icStats?.sources ?? icSources.length}`} />
          <Stat label="接码已配" value={entriesOf(KINDS.sms.prefixes).filter((e) => e.value).length} />
          <Stat label="打码已配" value={entriesOf(KINDS.captcha.prefixes).filter((e) => e.value).length} />
        </div>
        <div className="grid gap-3 sm:grid-cols-2">
          {MATERIAL_KINDS.map((key) => {
            const meta = KINDS[key];
            const entries = entriesOf(meta.prefixes);
            const filled = entries.filter((entry) => entry.value.trim() !== "").length;
            return (
              <Link
                key={key}
                href={`/materials/${key}/`}
                className="group rounded-[var(--radius)] border border-border bg-card p-4 transition-colors hover:border-brand/50 hover:bg-accent/30"
              >
                <div className="flex items-center justify-between">
                  <span className="text-[14px] font-semibold">{meta.title}</span>
                  <Badge tone={filled > 0 ? "ok" : "warn"}>{filled > 0 ? `${filled} 项已配` : "未配置"}</Badge>
                </div>
                <p className="mt-1 text-[11px] leading-snug text-muted-foreground">{meta.desc}</p>
                <div className="mt-2 flex items-center justify-between text-[11px] text-muted-foreground">
                  <span>{entries.length} 个相关键</span>
                  <span className="text-brand opacity-0 transition-opacity group-hover:opacity-100">进入 →</span>
                </div>
              </Link>
            );
          })}
        </div>
        <Card title="口径">
          <ul className="space-y-1.5 text-[11.5px] leading-relaxed text-muted-foreground">
            <li>· 物料 = 会被注册消耗掉的东西：邮箱、代理 IP、接码号、打码余额。</li>
            <li>· 资产 = 注册产出：sessionKey、Cookie、OAuth token、Outlook 账号，统一在资产中心。</li>
            <li>· 物料配置写在 <span className="font-mono">/data/.env</span>，面板「配置」页可改，立即对新任务生效。</li>
            <li>· IC 的源与别名是活数据，直接在 IC 工作台增删改，不写 .env。</li>
          </ul>
        </Card>
      </div>
    );
  }

  const meta = KINDS[kind];
  const entries = entriesOf(meta.prefixes);

  return (
    <div className="space-y-4">
      <Header title={`物料 · ${meta.title}`} desc={meta.desc} back={{ href: "/materials/", label: "物料中心" }} />
      {error && <ErrorLine text={error} />}

      {kind === "mail" && (
        <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
          <Stat label="IC 索引" value={icStats?.indexed ?? 0} />
          <Stat label="IC 源" value={icSources.length} />
          <Stat label="邮箱池" value={mailboxCount} />
          <Stat label="Outlook" value={outlookCount} />
        </div>
      )}

      {kind === "mail" && (
        <Card
          title="IC 邮箱源"
          bodyClass="p-0"
          actions={<Link href="/apps/icmail/sources/" className="btn-ghost px-2 py-1 text-[11px]">管理源</Link>}
        >
          <div className="max-h-[240px] overflow-auto">
            {icSources.length === 0 ? (
              <div className="p-4"><Empty>没有 IMAP 源，去 IC 工作台新增</Empty></div>
            ) : (
              <table className="tbl">
                <thead>
                  <tr className="border-b border-border">
                    <th className="px-3 py-1.5 text-left font-medium">状态</th>
                    <th className="px-2 py-1.5 text-left font-medium">名称</th>
                    <th className="px-2 py-1.5 text-left font-medium">服务器</th>
                    <th className="px-2 py-1.5 text-left font-medium">账号</th>
                    <th className="px-2 py-1.5 text-left font-medium">文件夹</th>
                    <th className="px-3 py-1.5 text-right font-medium">lastUID</th>
                  </tr>
                </thead>
                <tbody>
                  {icSources.map((row) => (
                    <tr key={row.id} className="border-b border-border/60 last:border-0">
                      <td className="px-3 py-1.5"><Badge tone={row.enabled ? "ok" : "neutral"}>{row.enabled ? "启用" : "停用"}</Badge></td>
                      <td className="px-2 py-1.5">{row.name}</td>
                      <td className="px-2 py-1.5 font-mono text-[11px] text-muted-foreground">{row.host}:{row.port}</td>
                      <td className="max-w-[180px] truncate px-2 py-1.5 font-mono text-[11px]">{row.username || "-"}</td>
                      <td className="px-2 py-1.5 text-[11px] text-muted-foreground">{row.folder}</td>
                      <td className="px-3 py-1.5 text-right font-mono text-[11px] text-muted-foreground">{row.lastUID}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        </Card>
      )}

      {kind === "mail" && (
        <Card title="邮箱池（资产侧）" bodyClass="p-0" actions={<Link href="/accounts/" className="btn-ghost px-2 py-1 text-[11px]">资产中心</Link>}>
          <div className="max-h-[300px] overflow-auto">
            {accounts.filter((row) => row.kind === "emails" || row.platform === "outlook").length === 0 ? (
              <div className="p-4"><Empty>池子里还没有邮箱</Empty></div>
            ) : (
              <table className="tbl">
                <thead>
                  <tr className="border-b border-border">
                    <th className="px-3 py-1.5 text-left font-medium">邮箱</th>
                    <th className="px-2 py-1.5 text-left font-medium">平台</th>
                    <th className="px-2 py-1.5 text-left font-medium">状态</th>
                    <th className="px-3 py-1.5 text-right font-medium">测活</th>
                  </tr>
                </thead>
                <tbody>
                  {accounts
                    .filter((row) => row.kind === "emails" || row.platform === "outlook")
                    .slice(0, 60)
                    .map((row) => (
                      <tr key={row.ref} className="border-b border-border/60 last:border-0">
                        <td className="max-w-[240px] truncate px-3 py-1.5 font-mono text-[11px]">{row.identity || row.ref}</td>
                        <td className="px-2 py-1.5 text-[11px] text-muted-foreground">{row.platform}</td>
                        <td className="px-2 py-1.5">
                          <Badge tone={row.status === "ok" ? "ok" : row.status === "unknown" ? "neutral" : "bad"}>{row.status}</Badge>
                        </td>
                        <td className="px-3 py-1.5 text-right text-[11px] text-muted-foreground">
                          {row.health_checked_at ? new Date(row.health_checked_at).toLocaleString() : "从未"}
                        </td>
                      </tr>
                    ))}
                </tbody>
              </table>
            )}
          </div>
        </Card>
      )}

      <Card title="配置键" bodyClass="p-0">
        <div className="max-h-[420px] overflow-auto">
          {entries.length === 0 ? (
            <div className="p-4"><Empty>没有匹配的配置键</Empty></div>
          ) : (
            <table className="tbl">
              <thead>
                <tr className="border-b border-border">
                  <th className="px-3 py-1.5 text-left font-medium">键</th>
                  <th className="px-2 py-1.5 text-left font-medium">当前值</th>
                  <th className="px-3 py-1.5 text-right font-medium">状态</th>
                </tr>
              </thead>
              <tbody>
                {entries.map((entry) => (
                  <tr
                    key={entry.key}
                    onClick={() => { setEditKey(entry); setEditValue(entry.secret ? "" : entry.value); }}
                    className="cursor-pointer border-b border-border/60 last:border-0 hover:bg-accent/40"
                  >
                    <td className="px-3 py-1.5 font-mono text-[11px]">{entry.key}</td>
                    <td className="max-w-[280px] truncate px-2 py-1.5 text-[11px] text-muted-foreground">
                      {entry.secret ? "••••••" : entry.value || "(空)"}
                    </td>
                    <td className="px-3 py-1.5 text-right">
                      <Badge tone={entry.value.trim() ? "ok" : "neutral"}>{entry.value.trim() ? "已配" : "空"}</Badge>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </Card>

      <Drawer
        open={!!editKey}
        title={editKey?.key || ""}
        subtitle={editKey?.secret ? "敏感项：留空表示不修改" : "直接改这一项，保存后立即生效"}
        onClose={() => setEditKey(null)}
        footer={
          <>
            <button className="btn-ghost px-2 py-1 text-[11px]" onClick={() => setEditKey(null)}>取消</button>
            <button
              className="btn-primary px-3 py-1 text-[11px]"
              disabled={!!busy || (editKey?.secret && !editValue)}
              onClick={async () => {
                if (!editKey) return;
                setBusy("save");
                try {
                  await api.saveConfig({ [editKey.key]: editValue });
                  setEditKey(null);
                  await load();
                } catch (err) {
                  setError((err as Error).message);
                } finally {
                  setBusy("");
                }
              }}
            >
              保存
            </button>
          </>
        }
      >
        {editKey && (
          <div className="space-y-2">
            <label className="label">当前值</label>
            <div className="rounded-md border border-border/60 bg-muted/30 px-2 py-1.5 font-mono text-[11.5px] text-muted-foreground">
              {editKey.secret ? (editKey.value ? "已设置（隐藏）" : "(空)") : editKey.value || "(空)"}
            </div>
            <label className="label">新值</label>
            <input
              className="field py-1.5 text-[12.5px]"
              type={editKey.secret ? "password" : "text"}
              placeholder={editKey.secret ? "留空 = 不修改" : ""}
              value={editValue}
              onChange={(e) => setEditValue(e.target.value)}
            />
            <p className="text-[11px] leading-relaxed text-muted-foreground">
              写入 <span className="font-mono">/data/.env</span>；新任务立即生效，面板进程内的值需重启容器。
            </p>
          </div>
        )}
      </Drawer>

      <Card title="怎么改">
        <p className="text-[11.5px] leading-relaxed text-muted-foreground">
          在 <Link href="/settings/" className="text-brand">配置</Link> 页直接改这些键（写入
          <span className="font-mono"> /data/.env </span>），保存后新任务立即生效，不需要重启。IC 的源/别名不走 .env，
          去 <Link href="/apps/icmail/" className="text-brand">IC 工作台</Link> 增删改。
        </p>
      </Card>
    </div>
  );
}

function Header({
  title,
  desc,
  back,
}: {
  title: string;
  desc: string;
  back?: { href: string; label: string };
}) {
  return (
    <div className="flex flex-wrap items-center gap-3">
      {back && <Link href={back.href} className="btn-ghost px-2 py-1 text-[11px]">← {back.label}</Link>}
      <div className="min-w-0 flex-1">
        <h1 className="text-[16px] font-semibold leading-tight">{title}</h1>
        <p className="text-[11px] text-muted-foreground">{desc}</p>
      </div>
      {!back && <Link href="/settings/" className="btn-ghost">去配置页</Link>}
    </div>
  );
}

function ErrorLine({ text }: { text: string }) {
  return (
    <div className="rounded-lg border border-destructive/30 bg-destructive/10 px-3 py-2 text-[12px] text-destructive">
      {text}
    </div>
  );
}
