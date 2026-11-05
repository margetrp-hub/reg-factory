"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { api } from "@/lib/api";
import { isNodeConfig, NODE_CONFIG_FIELDS } from "@/lib/node-config";
import { Card, Empty, ErrorNote } from "@/components/ui";

type Entry = { key: string; value: string; secret: boolean; line: number };

/** 跨平台共用的配置：代理 / 邮箱 / 接码 / 打码。集中到「共性配置」一组。 */
const COMMON_PREFIXES = [
  "PROXY_", "CLASH",
  "ICMAIL_", "ICLOUD_", "YYDS_", "GPTMAIL_", "CFMAIL_", "MOEMAIL_", "REMAIL_", "CUSTOM_MAIL_",
  "SMS_", "HERO_",
  "CAPSOLVER_", "EZCAPTCHA_", "YESCAPTCHA_", "VISION_", "VOTE_", "GEMMA_",
  "BROWSER_PROVIDER", "BITBROWSER_", "ADSPOWER_",
];
const isCommon = (key: string) => COMMON_PREFIXES.some((prefix) => key.toUpperCase().startsWith(prefix));

export function ConfigEditor({ scope }: { scope: "system" | "node" }) {
  const isNode = scope === "node";
  const [groups, setGroups] = useState<Record<string, Entry[]>>({});
  const [activeGroup, setActiveGroup] = useState("");
  const [path, setPath] = useState("");
  const [dirty, setDirty] = useState<Record<string, string>>({});
  const [query, setQuery] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [loading, setLoading] = useState(true);
  const [loaded, setLoaded] = useState(false);
  const [rawOpen, setRawOpen] = useState(false);
  const [rawReady, setRawReady] = useState(false);
  const [raw, setRaw] = useState("");
  const [reveal, setReveal] = useState(false);
  const [proxy, setProxy] = useState("");
  const [target, setTarget] = useState("https://claude.ai");
  const [probe, setProbe] = useState<string>("");

  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      const data = await api.config();
      setGroups(data.groups);
      setPath(data.path);
      setLoaded(true);
      // Reconcile an uncertain save against the file, preserving unsaved values.
      const values = new Map<string, string>();
      Object.values(data.groups).flat().sort((a, b) => a.line - b.line).forEach((entry) => {
        if (!values.has(entry.key)) values.set(entry.key, entry.value);
      });
      setDirty((previous) => Object.fromEntries(
        Object.entries(previous).filter(([key, value]) => values.get(key) !== value),
      ));
    } catch (err) {
      setError(`配置读取失败：${(err as Error).message}。可重试读取，未保存的修改会保留。`);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const scopedGroups = useMemo<Record<string, Entry[]>>(() => {
    const seen = new Set<string>();
    const entries = Object.entries(groups)
      .flatMap(([name, list]) => list.map((entry) => ({ name, entry })))
      .sort((a, b) => a.entry.line - b.entry.line)
      .filter(({ entry }) => {
        if (seen.has(entry.key)) return false;
        seen.add(entry.key);
        return isNodeConfig(entry.key) === isNode;
      });

    if (isNode) {
      const existing = new Map(entries.map(({ entry }) => [entry.key, entry]));
      const known = new Set(NODE_CONFIG_FIELDS.map((field) => field.key));
      return {
        __node: [
          ...NODE_CONFIG_FIELDS.map((field) => existing.get(field.key) || {
            key: field.key, value: "", secret: field.secret, line: 0,
          }),
          ...entries.filter(({ entry }) => !known.has(entry.key)).map(({ entry }) => entry),
        ],
      };
    }

    const out: Record<string, Entry[]> = {};
    const common = entries.filter(({ entry }) => isCommon(entry.key)).map(({ entry }) => entry);
    if (common.length) out.__common = common;
    for (const { name, entry } of entries) {
      if (isCommon(entry.key)) continue;
      (out[name] ||= []).push(entry);
    }
    return out;
  }, [groups, isNode]);
  const tabItems = Object.entries(scopedGroups).map(([id, list]) => ({
    id, label: id === "__common" ? "共性配置" : id, count: list.length,
  }));
  const activeId = scopedGroups[activeGroup]?.length ? activeGroup : tabItems[0]?.id;
  const entries = query.trim()
    ? Object.values(scopedGroups).flat().filter((entry) => entry.key.toLowerCase().includes(query.trim().toLowerCase()))
    : scopedGroups[activeId] || [];

  const save = async () => {
    if (Object.keys(dirty).length === 0) return;
    setBusy(true);
    setNotice("");
    setError("");
    try {
      const data = await api.saveConfig(dirty);
      setNotice(isNode
        ? `已写入 ${data.changed.length} 项到配置文件。节点需重启后读取；容器环境中的同名变量仍优先，当前运行值尚未确认。`
        : `已保存 ${data.changed.length} 项：${data.changed.slice(0, 6).join(", ")}${data.changed.length > 6 ? "…" : ""}`);
      await load();
    } catch (err) {
      setError(`保存未确认：${(err as Error).message}。修改已保留，可重新读取文件核对或重试保存。`);
    } finally {
      setBusy(false);
    }
  };

  const openRaw = async () => {
    setRawOpen(true);
    setRawReady(false);
    try {
      const data = await api.configRaw();
      setRaw(data.content);
      setRawReady(true);
    } catch (err) {
      setError((err as Error).message);
    }
  };

  const saveRaw = async () => {
    setBusy(true);
    try {
      await api.saveRaw(raw);
      setNotice("原始配置已保存");
      setRawOpen(false);
      setDirty({});
      await load();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const runProbe = async () => {
    setProbe("检测中…");
    try {
      const result = await api.proxyCheck(proxy, target);
      setProbe(
        result.error
          ? `失败：${result.error}（${result.ms}ms）`
          : `HTTP ${result.status} · ${result.ms}ms${result.title ? ` · ${result.title}` : ""}`,
      );
    } catch (err) {
      setProbe((err as Error).message);
    }
  };

  return (
    <div className="space-y-4">
      <div className="grid gap-4 lg:grid-cols-[1.5fr_1fr]">
        <Card
          title={isNode ? "节点配置文件" : "环境配置"}
          className="min-w-0 [&>.card-head]:flex-wrap [&>.card-head>div]:flex-wrap"
          actions={
            <>
              <input
                className="field w-[170px] py-1 text-[12px]"
                placeholder="搜索键名"
                value={query}
                onChange={(e) => setQuery(e.target.value)}
              />
              <label className="flex items-center gap-1.5 text-[11px] text-muted-foreground">
                <input type="checkbox" checked={reveal} onChange={(e) => setReveal(e.target.checked)} /> 明文
              </label>
              <button className="btn-ghost" onClick={load} disabled={busy || loading}>
                {loading ? "读取中…" : "重新读取"}
              </button>
              {!isNode && <button className="btn-ghost" onClick={openRaw}>原始编辑</button>}
              <button className="btn-primary" onClick={save} disabled={busy || loading || !loaded || Object.keys(dirty).length === 0}>
                保存 {Object.keys(dirty).length > 0 ? `(${Object.keys(dirty).length})` : ""}
              </button>
            </>
          }
        >
          <div className="mb-2 text-[11px] text-muted-foreground">
            文件 <span className="font-mono">{path || "读取中"}</span> · {isNode
              ? "节点在启动时读取配置；保存后需重启节点进程，容器环境变量优先。"
              : "任务配置由新任务读取；进程级配置需重启，容器环境变量优先。"}
          </div>
          <ErrorNote message={error} />
          {notice && <div className="mb-2 rounded border border-success/30 bg-success/10 px-2 py-1 text-[11px] text-success">{notice}</div>}
          {!loaded ? <Empty>{loading ? "正在读取配置文件…" : "配置尚未读取，请重试读取。"}</Empty> : <div className={isNode ? "" : "grid gap-3 lg:grid-cols-[196px_1fr]"}>
            {!isNode && <nav className="space-y-0.5 overflow-auto">
              {tabItems.map((item) => {
                const active = (query.trim() ? "__all" : activeId) === item.id;
                return (
                  <button
                    key={item.id}
                    onClick={() => setActiveGroup(item.id)}
                    className={`flex w-full items-center gap-2 rounded-md px-2.5 py-1.5 text-left text-[12.5px] transition-colors ${
                      active ? "bg-brand/15 font-medium text-brand" : "text-muted-foreground hover:bg-accent/60 hover:text-foreground"
                    }`}
                  >
                    <span className="min-w-0 flex-1 truncate">{item.label}</span>
                    <span className="font-mono text-[10.5px] opacity-70">{item.count}</span>
                  </button>
                );
              })}
            </nav>}

            <div className="max-h-[calc(100vh-260px)] overflow-auto">
              {query.trim() && (
                <div className="mb-1.5 px-1 text-[11px] text-muted-foreground">搜索结果（{isNode ? "节点配置" : "系统配置，跨分组"}）</div>
              )}
              <table className="tbl">
                <thead>
                  <tr>
                    <th>键</th>
                    <th>值</th>
                    <th className="w-[42px] text-right">改</th>
                  </tr>
                </thead>
                <tbody>
                  {entries.map((entry) => {
                    const value = dirty[entry.key] ?? entry.value;
                    const changed = entry.key in dirty;
                    return (
                      <tr key={entry.key}>
                        <td className="max-w-[240px] text-[11px] text-muted-foreground">
                          {isNode && <div className="mb-1 text-[12px] text-foreground">{NODE_CONFIG_FIELDS.find((field) => field.key === entry.key)?.label || "节点配置项"}</div>}
                          <div className="truncate font-mono" title={entry.key}>{entry.key}</div>
                          {isNode && <div className="mt-1 text-[10.5px]">{entry.line ? `文件第 ${entry.line} 行` : "文件未配置 · 当前环境值未知"}</div>}
                        </td>
                        <td>
                          <input
                            className={`field py-1 text-[12px] ${changed ? "border-warning/50" : ""}`}
                            type={entry.secret && !reveal ? "password" : "text"}
                            aria-label={entry.key}
                            autoComplete="off"
                            disabled={busy || loading}
                            placeholder={isNode && !entry.line ? "填写后新增到配置文件" : undefined}
                            value={value}
                            onChange={(e) => {
                              const next = e.target.value;
                              setNotice("");
                              setDirty((previous) => {
                                const updated = { ...previous };
                                if (next === entry.value) delete updated[entry.key];
                                else updated[entry.key] = next;
                                return updated;
                              });
                            }}
                          />
                        </td>
                        <td className="text-right text-[11px] text-warning">{changed ? "●" : ""}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
              {entries.length === 0 && <Empty>没有匹配的配置项</Empty>}
            </div>
          </div>}
        </Card>

        <div className="space-y-4">
          {!isNode && <Card title="代理体检">
            <p className="mb-3 text-[11px] leading-relaxed text-muted-foreground">
              SG 机房 IP 直连 claude.ai / chatgpt.com / grok.com 会拿 403（Cloudflare）。填一条住宅代理试真实出口。
            </p>
            <label className="label">代理（可空 = 直连）</label>
            <input className="field mb-2" placeholder="http://user:pass@host:port" value={proxy} onChange={(e) => setProxy(e.target.value)} />
            <label className="label">目标站点</label>
            <input className="field mb-3" value={target} onChange={(e) => setTarget(e.target.value)} />
            <button className="btn-primary" onClick={runProbe}>
              测试
            </button>
            {probe && <div className="mt-3 rounded-lg border border-border bg-muted/30 px-3 py-2 font-mono text-[11.5px] text-foreground/85">{probe}</div>}
          </Card>}

          <Card title={isNode ? "配置与当前运行值" : "说明"}>
            {isNode ? <div className="space-y-3 text-[11.5px] leading-relaxed text-muted-foreground">
              <p>这里编辑同一份环境配置文件，不代表当前节点进程的有效值。文件缺少的字段也会显示，填写并保存后追加到文件。</p>
              <p>配置接口不返回容器环境变量；空白字段不表示当前节点未配置。若容器环境中已设置同名变量，编辑文件不会覆盖它，应在部署环境修改对应变量后重启。</p>
              <p>节点参数在启动时读取，保存不会热更新或重启。重启后请回到节点总览核对节点身份、平台和密钥指纹。</p>
              <Link className="btn-ghost inline-flex" href="/nodes/">查看当前节点</Link>
            </div> : <>
            <ul className="space-y-1.5 text-[11.5px] leading-relaxed text-muted-foreground">
              <li>· 与节点和 Python worker 共用同一份配置文件。</li>
              <li>· 同一键只认第一次出现；保存时替换首个匹配、其余的保持原样。</li>
              <li>· 含 KEY / TOKEN / PASSWORD 的项默认打码，勾选「明文」可查看。</li>
              <li>· 原始编辑会做 KEY=VALUE 校验，拒绝任何非注释的其它行。</li>
              <li>· 节点字段统一在 <Link className="text-brand" href="/nodes/config/">节点配置</Link> 中管理；原始编辑仍包含完整文件。</li>
            </ul>
            </>}
          </Card>
        </div>
      </div>

      {rawOpen && (
        <Card
          title="原始 .env 编辑"
          actions={
            <>
              <button className="btn-ghost" onClick={() => setRawOpen(false)}>
                取消
              </button>
              <button className="btn-primary" onClick={saveRaw} disabled={busy || !rawReady}>
                保存
              </button>
            </>
          }
        >
          <textarea
            className="field h-[420px] font-mono text-[12px] leading-relaxed"
            value={raw}
            disabled={busy || !rawReady}
            onChange={(e) => setRaw(e.target.value)}
            spellCheck={false}
          />
        </Card>
      )}
    </div>
  );
}
