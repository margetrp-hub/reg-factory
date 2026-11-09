"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { api, type ConfigEntry } from "@/lib/api";
import { Badge, Card, ErrorNote } from "@/components/ui";
import { ConfigEditor } from "@/components/config-editor";

const KINDS = {
  mail: { title: "邮箱池管理", desc: "IC 地址池、收件来源、取码与同步记录；保留 Outlook 和通用邮箱。" },
  proxy: { title: "代理出口", desc: "住宅池、Clash 节点、生产修复出口与出口模式。" },
  sms: { title: "接码", desc: "短信与电话供应商、号码池、等待和重试参数。" },
  captcha: { title: "验证码", desc: "打码平台与视觉求解模型、投票和超时设置。" },
} as const;
type Kind = keyof typeof KINDS;
export const MATERIAL_KINDS: Kind[] = ["mail", "proxy", "sms", "captcha"];

export function MaterialsView({ kind }: { kind?: Kind }) {
  const [config, setConfig] = useState<ConfigEntry[]>([]);
  const [error, setError] = useState("");
  const [reload, setReload] = useState(0);
  useEffect(() => {
    if (kind) return;
    const controller = new AbortController();
    api.config(controller.signal).then(data => {
      if (!controller.signal.aborted) { setConfig(Object.values(data.groups).flat()); setError(""); }
    }).catch(err => { if (!controller.signal.aborted) setError(err.message); });
    return () => controller.abort();
  }, [kind, reload]);

  return <div className="sg-workspace">
    <header className="sg-page-head"><div><p className="sg-eyebrow">执行资源</p><h1>{kind ? KINDS[kind].title : "物料中心"}</h1><p className="sg-description">{kind ? KINDS[kind].desc : "按用途管理供给和连接设置，保留每种来源与执行模式。"}</p></div><div className="flex gap-2">{!kind && <button className="btn-ghost" onClick={() => setReload(value => value + 1)}>刷新</button>}{kind && <Link className="btn-ghost" href="/materials/">返回物料中心</Link>}</div></header>
    <ErrorNote message={error ? "部分数据读取失败，保留上次内容：" + error : ""} />
    {!kind ? <div className="grid gap-3 sm:grid-cols-2">{MATERIAL_KINDS.map(key => {
      const entries = config.filter(entry => entry.area === key);
      const configured = entries.filter(entry => entry.value.trim()).length;
      return <Link key={key} href={key === "mail" ? "/apps/icmail/" : "/materials/" + key + "/"} className="sg-mode-card"><div className="flex items-center justify-between gap-3"><h2>{KINDS[key].title}</h2><Badge tone="neutral">{configured} 项已设置</Badge></div><p>{KINDS[key].desc}</p><span className="sg-description">{key === "mail" ? "管理地址、来源与收信 →" : entries.length + " 项配置 · 管理与检测 →"}</span></Link>;
    })}</div> : <ConfigEditor key={kind} scope="system" area={kind} />}
    {!kind && <Card title="配置与运行数据"><p className="sg-description">物料页与系统设置使用同一份配置。邮箱源、邮件索引和账号池保留各自记录；配置保存不会改写已有账号或正在运行的任务。</p></Card>}
  </div>;
}
