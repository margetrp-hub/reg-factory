"use client";

import { useEffect } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { EntryArrow } from "@/components/ui";

const KINDS = {
  mail: { title: "邮箱池管理", desc: "IC 地址池、收件来源、取码与同步记录；保留 Outlook 和通用邮箱。" },
  proxy: { title: "代理出口", desc: "住宅池、Clash 节点、生产修复出口与出口模式。" },
  sms: { title: "接码", desc: "短信与电话供应商、号码池、等待和重试参数。" },
  captcha: { title: "验证码", desc: "打码平台与视觉求解模型、投票和超时设置。" },
} as const;
type Kind = keyof typeof KINDS;
export const MATERIAL_KINDS: Kind[] = ["mail", "proxy", "sms", "captcha"];

export function MaterialsView({ kind }: { kind?: Kind }) {
  const router = useRouter();
  const target = (key: Kind) => key === "mail" ? "/apps/mail/" : `/settings/?category=${key}`;
  useEffect(() => {
    if (kind) router.replace(target(kind));
  }, [kind, router]);

  return <div className="sg-workspace">
    <header className="sg-page-head"><div><p className="sg-eyebrow">执行资源</p><h1>供给与连接</h1><p className="sg-description">邮箱统一入池；连接、模式与公共参数在配置中心维护。</p></div><Link className="btn-ghost" href="/production/">返回生产中心</Link></header>
    {kind ? <Link className="btn-primary" href={target(kind)}>进入{KINDS[kind].title}</Link> : <div className="grid gap-3 sm:grid-cols-2">{MATERIAL_KINDS.map(key => <Link key={key} href={target(key)} className="sg-mode-card"><span className="sg-mode-title">{KINDS[key].title}<EntryArrow /></span><p>{KINDS[key].desc}</p><span className="sg-description">{key === "mail" ? "地址、来源与收信" : "在配置中心管理"}</span></Link>)}</div>}
  </div>;
}
