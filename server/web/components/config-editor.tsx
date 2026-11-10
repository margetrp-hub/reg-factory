"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { api, type ConfigEntry } from "@/lib/api";
import { isNodeConfig, NODE_CONFIG_FIELDS } from "@/lib/node-config";
import { CONFIG_CATEGORIES, configCategory, configSectionLabel } from "@/lib/config-categories";
import { Card, Empty, ErrorNote } from "@/components/ui";
import { Drawer } from "@/components/drawer";

type Entry = ConfigEntry;

export function ConfigEditor({ scope, area }: { scope: "system" | "node"; area?: string }) {
  const isNode = scope === "node";
  const [groups, setGroups] = useState<Record<string, Entry[]>>({});
  const [activeGroup, setActiveGroup] = useState("");
  const [activeCategory, setActiveCategory] = useState("");
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
  const [rawOriginal, setRawOriginal] = useState("");
  const [reveal, setReveal] = useState(false);
  const [proxy, setProxy] = useState("");
  const [target, setTarget] = useState("https://claude.ai");
  const [probe, setProbe] = useState<string>("");
  const [probeOpen, setProbeOpen] = useState(false);
  const [probing, setProbing] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      const data = await api.config();
      setGroups(data.groups);
      setPath(data.path);
      setLoaded(true);
      setError(data.schema_error || "");
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
    const params = new URLSearchParams(window.location.search);
    setQuery(params.get("q") || "");
    setActiveCategory(params.get("category") || "");
    setActiveGroup(params.get("section") || "");
    load();
  }, [load]);

  useEffect(() => {
    if (!Object.keys(dirty).length && !rawOpen && !busy) return;
    const warn = (event: BeforeUnloadEvent) => { event.preventDefault(); event.returnValue = ""; };
    const leaving = (event: MouseEvent) => {
      const link = (event.target as Element).closest?.("a[href]");
      if (link && !window.confirm("还有未保存的配置，确定离开并放弃本页修改？")) { event.preventDefault(); event.stopPropagation(); }
    };
    window.addEventListener("beforeunload", warn);
    document.addEventListener("click", leaving, true);
    return () => { window.removeEventListener("beforeunload", warn); document.removeEventListener("click", leaving, true); };
  }, [dirty, rawOpen, busy]);

  const scopedGroups = useMemo<Record<string, Entry[]>>(() => {
    const seen = new Set<string>();
    const entries = Object.entries(groups)
      .flatMap(([name, list]) => list.map((entry) => ({ name, entry })))
      .sort((a, b) => a.entry.line - b.entry.line)
      .filter(({ entry }) => {
        if (seen.has(entry.key)) return false;
        seen.add(entry.key);
        return isNodeConfig(entry.key) === isNode && (!area || entry.area === area);
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
    for (const { name, entry } of entries) {
      (out[name] ||= []).push(entry);
    }
    return out;
  }, [groups, isNode, area]);
  const rows = Object.entries(scopedGroups).flatMap(([group, list]) => list.map(entry => ({ entry, group, category: configCategory(entry.area) })));
  const categories = CONFIG_CATEGORIES.filter(category => rows.some(row => row.category === category.id));
  const categoryId = area || (categories.some(category => category.id === activeCategory) ? activeCategory : categories[0]?.id);
  const category = CONFIG_CATEGORIES.find(item => item.id === categoryId);
  const sections = Object.entries(scopedGroups).map(([id, list]) => ({
    id, label: configSectionLabel(id), entries: list.filter(entry => isNode || configCategory(entry.area) === categoryId),
  })).filter(section => section.entries.length).sort((a, b) => {
    const priority = (label: string) => /通用|来源选择|模式与平台/.test(label) ? 0 : 1;
    return priority(a.label) - priority(b.label) || a.label.localeCompare(b.label, "zh-CN");
  });
  const activeId = sections.some(section => section.id === activeGroup) ? activeGroup : sections[0]?.id;
  const search = query.trim().toLowerCase();
  const entries = rows.filter(row => search
    ? `${row.entry.key} ${row.entry.label || ""} ${row.entry.help || ""} ${row.group} ${CONFIG_CATEGORIES.find(item => item.id === row.category)?.label || ""}`.toLowerCase().includes(search)
    : (isNode || row.category === categoryId) && row.group === activeId);
  const changedCategories = categories.filter(category => rows.some(row => row.category === category.id && row.entry.key in dirty));

  useEffect(() => {
    if (!loaded || isNode) return;
    const url = new URL(window.location.href);
    if (categoryId) url.searchParams.set("category", categoryId);
    if (activeId) url.searchParams.set("section", activeId);
    if (query) url.searchParams.set("q", query); else url.searchParams.delete("q");
    window.history.replaceState(window.history.state, "", url);
  }, [loaded, isNode, categoryId, activeId, query]);

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
    if (Object.keys(dirty).length) {
      setError("请先保存当前修改，再打开原始编辑，避免两种编辑方式互相覆盖。");
      return;
    }
    setRawOpen(true);
    setRawReady(false);
    setError("");
    try {
      const data = await api.configRaw();
      setRaw(data.content);
      setRawOriginal(data.content);
      setRawReady(true);
    } catch (err) {
      setError((err as Error).message);
    }
  };

  const closeRaw = () => {
    if (busy || (raw !== rawOriginal && !window.confirm("放弃原始配置的未保存修改？"))) return;
    setRawOpen(false);
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
    if (probing) return;
    setProbing(true);
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
    } finally {
      setProbing(false);
    }
  };

  return (
    <div className="space-y-4">
      <div className="grid gap-4">
        <Card
          title={isNode ? "节点配置" : area ? category?.label || "配置" : "系统配置"}
          className="sg-config-workspace min-w-0 [&>.card-head]:flex-wrap [&>.card-head>div]:flex-wrap"
          actions={
            <>
              <label className="flex items-center gap-1.5 text-[11px] text-muted-foreground">
                <input type="checkbox" checked={reveal} onChange={(e) => setReveal(e.target.checked)} /> 显示密钥
              </label>
              <button className="btn-ghost" onClick={load} disabled={busy || loading || rawOpen}>
                {loading ? "读取中…" : "重新读取"}
              </button>
              {!isNode && !area && <button className="btn-ghost" onClick={openRaw} disabled={busy || loading || rawOpen}>原始编辑</button>}
              {!isNode && categoryId === "proxy" && <button className="btn-ghost" onClick={() => setProbeOpen(true)}>检测出口</button>}
              <button className="btn-primary" onClick={save} disabled={busy || loading || rawOpen || !loaded || Object.keys(dirty).length === 0}>
                保存全部修改 {Object.keys(dirty).length > 0 ? `(${Object.keys(dirty).length})` : ""}
              </button>
            </>
          }
        >
          <div className="mb-4 text-[13px] leading-relaxed text-muted-foreground">
            {isNode
              ? "节点在启动时读取配置；保存后需重启节点进程，容器环境变量优先。"
              : "任务配置由新任务读取；进程级配置需重启，容器环境变量优先。"}
          </div>
          <ErrorNote message={error} />
          {notice && <div className="mb-2 rounded border border-success/30 bg-success/10 px-2 py-1 text-[11px] text-success">{notice}</div>}
          {!loaded ? <Empty>{loading ? "正在读取配置文件…" : "配置尚未读取，请重试读取。"}</Empty> : <div>
            <div className={`sg-config-selectors ${isNode || area ? "sg-config-selectors-scoped" : ""}`}>
              {!isNode && !area && <label><span className="label">配置类别</span><select className="field" aria-label="配置类别" value={categoryId || ""} onChange={event => { setActiveCategory(event.target.value); setActiveGroup(""); setQuery(""); }}>
                {categories.map(item => <option key={item.id} value={item.id}>{item.label}{changedCategories.includes(item) ? " · 未保存" : ""}</option>)}
              </select></label>}
              {!isNode && <label><span className="label">配置对象</span><select className="field" aria-label="配置对象" value={activeId || ""} onChange={event => { setActiveGroup(event.target.value); setQuery(""); }}>
                {sections.map(item => <option key={item.id} value={item.id}>{item.label}{item.entries.some(entry => entry.key in dirty) ? " · 未保存" : ""}</option>)}
              </select></label>}
              <label><span className="label">{isNode ? "搜索节点配置" : area ? "搜索本类配置" : "搜索全部配置"}</span><input className="field" aria-label="搜索配置" placeholder="名称、用途或配置键" value={query} onChange={event => setQuery(event.target.value)} /></label>
            </div>
            <div className="sg-config-context">
              <p>{search ? `找到 ${entries.length} 项${isNode ? "节点配置" : area ? "本类配置" : "配置，含所有类别"}` : category?.description || "节点在启动时读取身份和密钥。"}</p>
              {search ? <button className="btn-ghost" onClick={() => setQuery("")}>返回当前配置</button> : !isNode && <span>切换类别和对象仅改变查看范围</span>}
            </div>
            {Object.keys(dirty).length > 0 && <p role="status" className="sg-config-draft">{Object.keys(dirty).length} 项修改尚未保存{changedCategories.length ? ` · ${changedCategories.map(item => item.label).join("、")}` : ""}。切换查看会保留修改，保存将一次提交全部修改。</p>}
            <div key={`${categoryId}:${activeId}:${Boolean(search)}`} className="sg-config-fields sg-enter">
              <table className="tbl sg-config-table">
                <thead>
                  <tr>
                    <th>配置与用途</th>
                    <th>设置</th>
                    <th className="w-[42px] text-right"><span className="sr-only">修改状态</span></th>
                  </tr>
                </thead>
                <tbody>
                  {entries.map(({ entry, group, category: entryCategory }) => {
                    const value = dirty[entry.key] ?? entry.value;
                    const changed = entry.key in dirty;
                    const update = (next: string) => {
                      setNotice("");
                      setDirty(previous => { const updated={...previous}; if(next===entry.value)delete updated[entry.key]; else updated[entry.key]=next; return updated; });
                    };
                    const choices = entry.type === "bool" ? ["true", "false"] : entry.choices;
                    const placeholder = entry.default === undefined ? "未设置" : `未设置 · 默认 ${String(entry.default)}`;
                    return (
                      <tr key={entry.key}>
                        <td className="max-w-[240px] text-[11px] text-muted-foreground">
                          {search && !isNode && <p className="sg-config-breadcrumb">{CONFIG_CATEGORIES.find(item => item.id === entryCategory)?.label} / {configSectionLabel(group)}</p>}
                          <div className="mb-1 text-[13px] text-foreground">{entry.label || (isNode ? NODE_CONFIG_FIELDS.find((field) => field.key === entry.key)?.label : "") || entry.key}</div>
                          <div className="truncate font-mono" title={entry.key}>{entry.key}</div>
                          {entry.help && <p className="mt-2 max-w-lg text-[12px] leading-relaxed">{entry.help}</p>}
                          <p className="mt-1 text-[11px]">{entry.line ? "文件已设置" : "文件未设置"}{entry.apply === "restart" ? " · 重启后读取" : ""}{entry.readonly ? " · 原配置留存" : ""}</p>
                          {isNode && <div className="mt-1 text-[10.5px]">{entry.line ? `文件第 ${entry.line} 行` : "文件未配置 · 当前环境值未知"}</div>}
                        </td>
                        <td>
                          {choices?.length && !entry.secret ? <select className={`field ${changed ? "border-warning/50" : ""}`} aria-label={entry.label || entry.key} disabled={busy || loading || rawOpen || entry.readonly} value={value} onChange={event=>update(event.target.value)}>
                            <option value="">{placeholder}</option>
                            {value && !choices.includes(value) && <option value={value}>{value}（已保存）</option>}
                            {choices.map(choice=><option key={choice} value={choice}>{entry.type==="bool" ? choice==="true" ? "开启" : "关闭" : choice}</option>)}
                          </select> : <input
                            className={`field py-1 text-[12px] ${changed ? "border-warning/50" : ""}`}
                            type={entry.secret && !reveal ? "password" : "text"}
                            aria-label={entry.label || entry.key}
                            autoComplete="off"
                            disabled={busy || loading || rawOpen || entry.readonly}
                            placeholder={placeholder}
                            value={value}
                            onChange={(e) => update(e.target.value)}
                          />}
                        </td>
                        <td className="text-right text-[11px] text-warning" aria-label={changed ? "已修改" : "未修改"}>{changed ? "●" : ""}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
              {entries.length === 0 && <Empty>没有匹配的配置项</Empty>}
            </div>
          </div>}
        </Card>

          <Drawer variant="dialog" open={probeOpen} title="检测出口" subtitle="以本次目标的实际响应判断连接状态。检测不会更改已保存的出口配置。" onClose={() => setProbeOpen(false)} width={520}>
            <p className="mb-3 text-[13px] leading-relaxed text-muted-foreground">
              不同域名和执行阶段的可用出口可能不同。留空检测直连，也可以填写指定代理进行对比。
            </p>
            <label className="label">代理（可空 = 直连）</label>
            <input aria-label="检测代理" type="password" autoComplete="off" className="field mb-2" placeholder="http://user:pass@host:port" value={proxy} onChange={(e) => setProxy(e.target.value)} />
            <label className="label">目标站点</label>
            <input aria-label="检测目标站点" className="field mb-3" value={target} onChange={(e) => setTarget(e.target.value)} />
            <button className="btn-primary" onClick={runProbe} disabled={probing}>
              {probing ? "检测中…" : "开始检测"}
            </button>
            {probe && <div className="mt-3 rounded-lg border border-border bg-muted/30 px-3 py-2 font-mono text-[11.5px] text-foreground/85">{probe}</div>}
          </Drawer>

        <details className="rounded-lg border border-border p-4 text-[13px]"><summary className="cursor-pointer font-medium">配置生效与保存说明</summary><div className="mt-3">
          <p className="mb-3 break-all text-[12px] text-muted-foreground">配置文件：{path || "尚未读取"}</p>
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
              <li>· 密钥字段默认隐藏，勾选「显示密钥」可查看。</li>
              <li>· 原始编辑会做 KEY=VALUE 校验，拒绝任何非注释的其它行。</li>
              <li>· 节点字段统一在 <Link className="text-brand" href="/nodes/config/">节点配置</Link> 中管理；原始编辑仍包含完整文件。</li>
            </ul>
            </>}
          </Card>
        </div></details>
      </div>

      {rawOpen && (
        <Card
          title="原始 .env 编辑"
          actions={
            <>
              <button className="btn-ghost" onClick={closeRaw} disabled={busy}>
                取消
              </button>
              {!rawReady && <button className="btn-ghost" onClick={openRaw}>重试读取</button>}
              <button className="btn-primary" onClick={saveRaw} disabled={busy || !rawReady || raw === rawOriginal}>
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
