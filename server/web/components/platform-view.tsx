"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { api, type AccountRow, type AccountSummary, type TaskPage } from "@/lib/api";
import { Badge, Card, Empty, ErrorNote } from "@/components/ui";
import { TaskTable } from "@/components/task-table";
import { PLATFORM_TASKS, scriptsForPlatform } from "@/lib/task-catalog";

type FlowStep = {
  id: string;
  title: string;
  desc: string;
  /** 该步依赖的数据（用于自动判断就绪） */
  needs?: "mail" | "proxy" | "sms" | "accounts" | "tasks" | "none";
  href?: string;
  cta?: string;
  /** 该步要做的事（任务 id 提示） */
  tasks?: string[];
};

type PlatformSpec = {
  name: string;
  slug: string;
  accent: string;
  summary: string;
  flow: FlowStep[];
};

const SPECS: Record<string, PlatformSpec> = {
  claude: {
    name: "Claude",
    slug: "claude",
    accent: "bg-[#d97757]",
    summary: "邮箱取码 → 注册（浏览器或协议）→ sessionKey 入库 → 测活/刷新。协议路径不启浏览器。",
    flow: [
      { id: "supply", title: "① 供给就绪", desc: "IC 索引可收信 / Outlook RT 可用 / 代理已配", needs: "mail" },
      { id: "register", title: "② 下发注册", desc: "浏览器流程（hCaptcha 视觉）或协议注册（零浏览器）", needs: "none", href: "/platform/claude/tasks/", cta: "去下单", tasks: ["register_claude"] },
      { id: "code", title: "③ 取码 / 验证", desc: "IC 按别名查码；Outlook 走 Graph 取信", needs: "mail", href: "/apps/icmail/", cta: "IC 工作台" },
      { id: "asset", title: "④ 资产入库", desc: "sessionKey 落到 cookies/claude/，同步进 accounts 表", needs: "accounts", href: "/platform/claude/accounts/", cta: "看账号" },
      { id: "ops", title: "⑤ 测活 / 刷新", desc: "按扫描缓存判 ok/expired/banned；已领取账号禁止自动刷新", needs: "accounts", href: "/platform/claude/accounts/", cta: "批量测活" },
    ],
  },
  chatgpt: {
    name: "ChatGPT",
    slug: "chatgpt",
    accent: "bg-[#10a37f]",
    summary: "注册 → Codex OAuth 拿 refresh_token → 导入 SUB2API（add-phone 需接码）→ 测活。",
    flow: [
      { id: "supply", title: "① 供给就绪", desc: "邮箱池 + 接码供应商（add-phone 需要）", needs: "sms" },
      { id: "register", title: "② 下发注册", desc: "register_chatgpt：可开 2FA、可选直接导入", needs: "none", href: "/platform/chatgpt/tasks/", cta: "去下单", tasks: ["register_chatgpt"] },
      { id: "authorize", title: "③ Codex 授权", desc: "oauth_codex 换 refresh_token，或 plus_codex_import 批量导入 Plus", needs: "none", href: "/platform/chatgpt/tasks/", cta: "去授权", tasks: ["oauth_codex", "plus_codex_import"] },
      { id: "asset", title: "④ 资产入库", desc: "tokens/ 与 SUB2API/CPA 记录；本地面板可导出", needs: "accounts", href: "/platform/chatgpt/accounts/", cta: "看账号" },
      { id: "ops", title: "⑤ 测活", desc: "校验 session 与 Plus 试用资格", needs: "accounts", href: "/platform/chatgpt/accounts/", cta: "批量测活" },
    ],
  },
  grok: {
    name: "Grok",
    slug: "grok",
    accent: "bg-foreground",
    summary: "住宅代理 → 注册（Turnstile）→ SSO 入库 → 导入 SUB2API Grok 渠道。",
    flow: [
      { id: "supply", title: "① 供给就绪", desc: "必须住宅代理，否则 Turnstile 直接拦", needs: "proxy" },
      { id: "register", title: "② 下发注册", desc: "register_grok：--sub2api 可直接导入", needs: "none", href: "/platform/grok/tasks/", cta: "去下单", tasks: ["register_grok"] },
      { id: "asset", title: "③ 资产入库", desc: "SSO 落到 cookies/grok/，OAuth 落 tokens/", needs: "accounts", href: "/platform/grok/accounts/", cta: "看账号" },
      { id: "ops", title: "④ 测活", desc: "校验 SSO 与授权状态", needs: "accounts", href: "/platform/grok/accounts/", cta: "批量测活" },
    ],
  },
  outlook: {
    name: "Outlook",
    slug: "outlook",
    accent: "bg-[#0078d4]",
    summary: "常驻注册 → Graph 授权（辅助邮箱接码）→ RT 入库 → 解锁/恢复。",
    flow: [
      { id: "supply", title: "① 供给就绪", desc: "辅助邮箱（iCloud/YYDS/自有）用于接码", needs: "mail" },
      { id: "register", title: "② 常驻注册", desc: "outlook_reg_loop：成功率低于阈值自动停", needs: "none", href: "/platform/outlook/tasks/", cta: "去下单", tasks: ["outlook_reg_loop"] },
      { id: "authorize", title: "③ Graph 授权", desc: "unlock_outlook：逐号授权并回写 refresh_token", needs: "none", href: "/platform/outlook/tasks/", cta: "批量授权", tasks: ["unlock_outlook"] },
      { id: "asset", title: "④ 资产入库", desc: "emails.txt 的 RT 字段 + accounts 表", needs: "accounts", href: "/platform/outlook/accounts/", cta: "看账号" },
      { id: "ops", title: "⑤ 测活 / 续期", desc: "Graph token 续期；RT 失效标记 relogin_required", needs: "accounts", href: "/platform/outlook/accounts/", cta: "批量测活" },
    ],
  },
};

export function PlatformView({ slug }: { slug: string }) {
  const spec = useMemo<PlatformSpec>(()=>SPECS[slug] || {
    name:PLATFORM_TASKS[slug]?.name || slug,slug,accent:"bg-foreground",
    summary:"保留本平台全部执行模式，以及组合流程、账号记录与导出能力。",
    flow:[
      {id:"configure",title:"准备执行配置",desc:"选择浏览器、网络出口和邮箱来源。",href:"/settings/",cta:"配置",needs:"none"},
      {id:"execute",title:"选择任务模式",desc:"本平台及组合流程的全部参数都可编辑。",href:`/platform/${slug}/tasks/`,cta:"发起任务",needs:"none"},
      {id:"result",title:"核对账号与结果",desc:"保留原执行回执、凭据版本与后续恢复入口。",href:`/platform/${slug}/accounts/`,cta:"账号记录",needs:"accounts"},
    ],
  },[slug]);
  const [accounts, setAccounts] = useState<AccountRow[]>([]);
  const [taskPage, setTaskPage] = useState<TaskPage | null>(null);
  const [summary, setSummary] = useState<AccountSummary | null>(null);
  const [error, setError] = useState("");
  const [reload, setReload] = useState(0);
  const [envReady, setEnvReady] = useState<{ mail: boolean; proxy: boolean; sms: boolean }>({
    mail: false, proxy: false, sms: false,
  });

  const load = useCallback(async (signal: AbortSignal) => {
    try {
      const catalog = await api.scripts(signal);
      const ids = scriptsForPlatform(catalog.scripts,spec.slug).map(script=>script.id);
      const [accountData, taskData, config, accountTotals] = await Promise.all([
        api.accounts({ platform: spec.slug, limit: 15 },signal),
        ids.length ? api.tasks(12,{scripts:ids},signal) : Promise.resolve({tasks:[],stats:{},count:0,total:0,limit:12,offset:0} as TaskPage),
        api.config(signal),
        api.accountSummary(spec.slug,signal),
      ]);
      if (signal.aborted) return;
      setAccounts(accountData.accounts);
      setTaskPage(taskData);
      setSummary(accountTotals.platforms[0] || {platform:spec.slug,total:0,available:0,abnormal:0,unknown:0});
      setError("");
      const entries = Object.values(config.groups).flat();
      const has = (prefixes: string[]) =>
        entries.some((entry) => prefixes.some((p) => entry.key.toUpperCase().startsWith(p)) && entry.value.trim() !== "");
      setEnvReady({
        mail: has(["ICMAIL", "ICLOUD", "YYDS", "GPTMAIL", "CFMAIL", "MOEMAIL", "CUSTOM_MAIL"]),
        proxy: has(["PROXY", "CLASH"]),
        sms: has(["SMS", "HERO"]),
      });
    } catch (err) {
      if (!signal.aborted) setError(`平台读取失败，已保留上次结果。${(err as Error).message}`);
    }
  }, [spec.slug]);

  useEffect(() => {
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => { await load(controller.signal); if (!controller.signal.aborted) timer=setTimeout(poll,document.hidden?30000:15000); };
    void poll();
    return () => { controller.abort(); clearTimeout(timer); };
  }, [load,reload]);

  const stats = useMemo(() => {
    return {
      ok:summary?.available, bad:summary?.abnormal,
      running:taskPage ? (taskPage.stats.running || 0)+(taskPage.stats.queued || 0) : undefined,
      last:taskPage?.tasks[0],
    };
  }, [summary,taskPage]);

  const readiness = (step: FlowStep): { tone: "ok" | "warn" | "neutral"; text: string } => {
    if (error || !summary) return {tone:"neutral",text:error ? "状态待刷新" : "读取中"};
    switch (step.needs) {
      case "mail":
        return envReady.mail
          ? { tone: "ok", text: "邮箱已配" }
          : { tone: "warn", text: "邮箱未配" };
      case "proxy":
        return envReady.proxy ? { tone: "ok", text: "代理已配" } : { tone: "warn", text: "缺代理" };
      case "sms":
        return envReady.sms ? { tone: "ok", text: "接码已配" } : { tone: "warn", text: "无接码" };
      case "accounts":
        return summary.total ? { tone: "ok", text: `${summary.total} 个账号` } : { tone: "neutral", text: "暂无账号" };
      case "tasks":
        return stats.last ? { tone: "ok", text: stats.last.status } : { tone: "neutral", text: "未运行" };
      default:
        return { tone: "neutral", text: "" };
    }
  };

  return (
    <div className="sg-workspace">
      <div className="flex flex-wrap items-center gap-3">
        <span className={`grid h-9 w-9 place-items-center rounded-md ${spec.accent} text-[13px] font-semibold text-primary-foreground`}>
          {spec.name.slice(0, 1)}
        </span>
        <div className="min-w-0 flex-1">
          <h1 className="text-[16px] font-semibold leading-tight">{spec.name} 流程</h1>
          <p className="text-[11px] leading-relaxed text-muted-foreground">{spec.summary}</p>
        </div>
        <div className="flex gap-2">
          <button className="btn-ghost" onClick={()=>setReload(value=>value+1)}>刷新</button>
          <Link href={`/platform/${spec.slug}/tasks/`} className="btn-ghost text-[11px]">任务</Link>
          <Link href={`/platform/${spec.slug}/accounts/`} className="btn-ghost text-[11px]">账号</Link>
          <Link href={`/platform/${spec.slug}/checks/`} className="btn-ghost text-[11px]">凭据检查</Link>
        </div>
      </div>

      <ErrorNote message={error}/>
      <div className="flex flex-wrap gap-x-8 gap-y-2 border-y border-border px-4 py-3 text-[13px] text-muted-foreground"><span>账号总数 <strong className="ml-2 text-foreground tabular-nums">{summary?.total ?? "—"}</strong></span><span>正常 <strong className="ml-2 text-success tabular-nums">{stats.ok ?? "—"}</strong></span><span>异常 <strong className="ml-2 text-destructive tabular-nums">{stats.bad ?? "—"}</strong></span><span>运行 / 排队 <strong className="ml-2 text-foreground tabular-nums">{stats.running ?? "—"}</strong></span></div>

      <Card title="流程步骤" bodyClass="p-0">
        <div className="divide-y divide-border">
          {spec.flow.map((step, index) => {
            const state = readiness(step);
            return (
              <div key={step.id} className="flex flex-wrap items-center gap-3 px-4 py-3">
                <span className="grid h-7 w-7 shrink-0 place-items-center rounded-full border border-border text-[11px] font-medium text-muted-foreground">
                  {index + 1}
                </span>
                <div className="min-w-0 flex-1">
                  <div className="flex items-center gap-2">
                    <span className="text-[13px] font-medium">{step.title}</span>
                    {state.text && <Badge tone={state.tone}>{state.text}</Badge>}
                  </div>
                  <div className="mt-0.5 text-[11px] leading-snug text-muted-foreground">{step.desc}</div>
                </div>
                {step.href && (
                  <Link href={step.href} className="btn-ghost shrink-0 px-2 py-1 text-[11px]">
                    {step.cta || "进入"} →
                  </Link>
                )}
              </div>
            );
          })}
        </div>
      </Card>

      <div className="grid gap-4 xl:grid-cols-2">
        <Card
          title="最近任务"
          bodyClass="p-0"
          actions={<Link href={`/platform/${spec.slug}/tasks/`} className="btn-ghost px-2 py-1 text-[11px]">全部</Link>}
        >
          <div className="max-h-[280px] overflow-auto">
            <TaskTable tasks={taskPage?.tasks || []} limit={12} showScript emptyText="还没有任务" />
          </div>
        </Card>

        <Card
          title={`${spec.name} 账号`}
          bodyClass="p-0"
          actions={<Link href={`/platform/${spec.slug}/accounts/`} className="btn-ghost px-2 py-1 text-[11px]">全部</Link>}
        >
          <div className="max-h-[280px] overflow-auto">
            {accounts.length === 0 ? (
              <div className="p-4"><Empty>暂无账号，从第 ② 步开始跑</Empty></div>
            ) : (
              <table className="tbl">
                <thead>
                  <tr className="border-b border-border">
                    <th className="px-3 py-1.5 text-left font-medium">账号</th>
                    <th className="px-2 py-1.5 text-left font-medium">状态</th>
                    <th className="px-3 py-1.5 text-right font-medium">测活</th>
                  </tr>
                </thead>
                <tbody>
                  {accounts.slice(0, 15).map((row) => (
                    <tr key={row.ref} className="border-b border-border/60 last:border-0">
                      <td className="max-w-[220px] truncate px-3 py-1.5 font-mono text-[11px]" title={row.ref}>
                        {row.identity || row.ref}
                      </td>
                      <td className="px-2 py-1.5">
                        <Badge tone={row.status === "ok" ? "ok" : row.status === "unknown" ? "neutral" : "bad"}>{row.status}</Badge>
                      </td>
                      <td className="px-3 py-1.5 text-right text-[11px] text-muted-foreground">
                        {row.health_checked_at ? new Date(row.health_checked_at).toLocaleDateString() : "从未"}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        </Card>
      </div>
    </div>
  );
}
