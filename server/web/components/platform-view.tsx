"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { api, type AccountRow, type Script, type Task } from "@/lib/api";
import { Badge, Card, Empty, Stat, StatusBadge, timeAgo } from "@/components/ui";
import { TaskForm } from "@/components/task-form";

type PlatformSpec = {
  name: string;
  slug: string;
  platform: string;
  accent: string;
  summary: string;
  /** 主流程脚本（排在前面、默认选中） */
  primary: string[];
  /** 该平台特有的动作按钮 */
  actions: { id: string; label: string; hint: string }[];
  /** 该平台真正需要关注的指标 */
  metrics: { id: string; label: string; hint?: string; tone?: "neutral" | "warn" | "bad" }[];
};

const SPECS: Record<string, PlatformSpec> = {
  claude: {
    name: "Claude",
    slug: "claude",
    platform: "claude",
    accent: "bg-[#d97757]",
    summary: "浏览器流程重（单页约 1.3GB，hCaptcha 需视觉 API）；批量优先走 ClaudeX HTTP 协议，不启浏览器、并发可到 20+。",
    primary: ["register_claude"],
    actions: [
      { id: "liveness", label: "批量测活", hint: "按号池扫描缓存判定 session 状态" },
    ],
    metrics: [
      { id: "ok", label: "可用 session" },
      { id: "expired", label: "过期/受限", tone: "warn" },
      { id: "unknown", label: "未测活" },
    ],
  },
  chatgpt: {
    name: "ChatGPT",
    slug: "chatgpt",
    platform: "chatgpt",
    accent: "bg-[#10a37f]",
    summary: "注册后用 Codex OAuth 拿 refresh_token 导入 SUB2API；add-phone 需要接码，Plus 试用资格走号池扫描。",
    primary: ["register_chatgpt", "oauth_codex", "plus_codex_import"],
    actions: [
      { id: "liveness", label: "批量测活", hint: "校验 session 与 Plus 试用资格" },
    ],
    metrics: [
      { id: "ok", label: "可用账号" },
      { id: "expired", label: "过期/受限", tone: "warn" },
      { id: "unknown", label: "未测活" },
    ],
  },
  grok: {
    name: "Grok",
    slug: "grok",
    platform: "grok",
    accent: "bg-foreground",
    summary: "直连会被 Cloudflare Turnstile 拦，必须住宅代理；注册成功可直接导入 SUB2API Grok 渠道。",
    primary: ["register_grok"],
    actions: [
      { id: "liveness", label: "批量测活", hint: "校验 SSO 与 OAuth 授权状态" },
    ],
    metrics: [
      { id: "ok", label: "可用 SSO" },
      { id: "expired", label: "受限", tone: "warn" },
      { id: "unknown", label: "未测活" },
    ],
  },
  outlook: {
    name: "Outlook",
    slug: "outlook",
    platform: "outlook",
    accent: "bg-[#0078d4]",
    summary: "注册是常驻循环（成功率低于阈值会自动停）；Graph 授权是独立步骤，需要辅助邮箱接码，授权后才有 refresh_token。",
    primary: ["outlook_reg_loop", "unlock_outlook"],
    actions: [
      { id: "authorize", label: "批量 Graph 授权", hint: "粘贴 email----password，逐号授权并回写 RT" },
      { id: "liveness", label: "批量测活", hint: "用 refresh_token 校验 Graph 可用性" },
    ],
    metrics: [
      { id: "ok", label: "可用邮箱" },
      { id: "expired", label: "RT 失效", tone: "warn" },
      { id: "unknown", label: "未测活" },
    ],
  },
};

export function PlatformView({ slug }: { slug: string }) {
  const spec = SPECS[slug] || SPECS.claude;
  const [scripts, setScripts] = useState<Script[]>([]);
  const [activeId, setActiveId] = useState("");
  const [tasks, setTasks] = useState<Task[]>([]);
  const [accounts, setAccounts] = useState<AccountRow[]>([]);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState("");
  const [authText, setAuthText] = useState("");
  const [showAuth, setShowAuth] = useState(false);

  const load = useCallback(async () => {
    try {
      const [scriptData, taskData, accountData] = await Promise.all([
        api.scripts(),
        api.tasks(80),
        api.accounts({ platform: spec.platform, limit: 200 }),
      ]);
      const byId = new Map(scriptData.scripts.map((item) => [item.id, item]));
      const mine = spec.primary
        .map((id) => byId.get(id))
        .filter((item): item is Script => Boolean(item));
      setScripts(mine);
      setActiveId((prev) => (mine.some((item) => item.id === prev) ? prev : mine[0]?.id || ""));
      setTasks(taskData.tasks.filter((task) => {
        if (spec.primary.includes(task.script)) return true;
        return (byId.get(task.script)?.platform || "").toLowerCase() === spec.platform;
      }));
      setAccounts(accountData.accounts);
      setError("");
    } catch (err) {
      setError((err as Error).message);
    }
  }, [spec.platform, spec.primary]);

  useEffect(() => {
    load();
    const timer = setInterval(load, 8000);
    return () => clearInterval(timer);
  }, [load]);

  const active = scripts.find((item) => item.id === activeId);
  const counts = useMemo(() => {
    const ok = accounts.filter((row) => row.status === "ok").length;
    const expired = accounts.filter((row) => row.status === "expired").length;
    const bad = accounts.filter((row) => row.status === "banned" || row.status === "invalid").length;
    const unknown = accounts.filter((row) => row.status === "unknown").length;
    return {
      ok, expired, bad, unknown,
      succeeded: tasks.filter((t) => t.status === "succeeded").length,
      failed: tasks.filter((t) => t.status === "failed").length,
      running: tasks.filter((t) => t.status === "running" || t.status === "queued").length,
    };
  }, [accounts, tasks]);

  const runLiveness = async () => {
    const targets = accounts.filter((row) => row.status !== "ok").slice(0, 5);
    if (targets.length === 0) {
      setNotice("没有待测活的账号");
      return;
    }
    setBusy("liveness");
    try {
      for (const row of targets) {
        await api.accountLiveness(row.ref, row.platform || spec.platform, false);
      }
      setNotice(`已提交 ${targets.length} 个测活任务`);
      await load();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy("");
    }
  };

  const submitAuthorize = async () => {
    if (!authText.trim()) return;
    setBusy("authorize");
    setError("");
    try {
      const data = await api.outlookAuthorize(authText, 3);
      setNotice(`已提交 Graph 授权任务 ${data.task_id}`);
      setShowAuth(false);
      setAuthText("");
      await load();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy("");
    }
  };

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-3">
        <span className={`grid h-9 w-9 place-items-center rounded-md ${spec.accent} text-[13px] font-semibold text-primary-foreground`}>
          {spec.name.slice(0, 1)}
        </span>
        <div className="min-w-0 flex-1">
          <h1 className="text-[16px] font-semibold leading-tight">{spec.name} 工作台</h1>
          <p className="text-[11px] leading-relaxed text-muted-foreground">{spec.summary}</p>
        </div>
        <div className="flex items-center gap-2">
          {spec.actions.map((action) =>
            action.id === "authorize" ? (
              <button key={action.id} className="btn-ghost" onClick={() => setShowAuth((v) => !v)} title={action.hint}>
                {action.label}
              </button>
            ) : (
              <button key={action.id} className="btn-ghost" onClick={runLiveness} disabled={busy === "liveness"} title={action.hint}>
                {busy === "liveness" ? "提交中…" : action.label}
              </button>
            ),
          )}
        </div>
      </div>

      <div className="grid grid-cols-2 gap-3 lg:grid-cols-5">
        {spec.metrics.map((metric) => (
          <Stat
            key={metric.id}
            label={metric.label}
            value={counts[metric.id as keyof typeof counts] ?? 0}
            tone={metric.tone}
          />
        ))}
        <Stat label="任务" value={`${counts.succeeded}/${counts.failed}`} hint={`运行中 ${counts.running}`} />
      </div>

      {showAuth && (
        <Card
          title="批量 Graph 授权（Outlook）"
          actions={
            <>
              <button className="btn-ghost" onClick={() => setShowAuth(false)}>取消</button>
              <button className="btn-primary" onClick={submitAuthorize} disabled={busy === "authorize"}>
                提交授权
              </button>
            </>
          }
        >
          <p className="mb-2 text-[11px] text-muted-foreground">
            每行 <span className="font-mono">email----password</span>；需要配置可接码的辅助邮箱（YYDS / iCloud / 自有 Outlook）。
          </p>
          <textarea
            className="field h-[160px] font-mono text-[12px]"
            placeholder="user@outlook.com----password&#10;user2@outlook.com----password"
            value={authText}
            onChange={(e) => setAuthText(e.target.value)}
          />
        </Card>
      )}

      {error && <div className="rounded-lg border border-destructive/30 bg-destructive/10 px-3 py-2 text-[12px] text-destructive">{error}</div>}
      {notice && <div className="rounded-lg border border-success/30 bg-success/10 px-3 py-2 text-[12px] text-success">{notice}</div>}

      <div className="grid gap-4 xl:grid-cols-[1.05fr_1fr]">
        <Card
          title="启动任务"
          actions={
            scripts.length > 1 ? (
              <select className="field w-[210px] py-1 text-[12px]" value={activeId} onChange={(e) => setActiveId(e.target.value)}>
                {scripts.map((item) => (
                  <option key={item.id} value={item.id}>{item.title}</option>
                ))}
              </select>
            ) : null
          }
        >
          {active ? <TaskForm script={active} onStarted={() => load()} /> : <Empty>该平台暂无可用脚本</Empty>}
        </Card>

        <div className="space-y-4">
          <Card title="最近任务" bodyClass="p-0">
            <div className="max-h-[230px] overflow-auto">
              {tasks.length === 0 ? (
                <div className="p-4"><Empty>该平台还没有任务</Empty></div>
              ) : (
                tasks.map((task) => (
                  <Link
                    key={task.id}
                    href={`/tasks/?id=${task.id}`}
                    className="flex items-center gap-2 border-b border-border/60 px-4 py-2 text-[12.5px] last:border-0 hover:bg-accent/40"
                  >
                    <StatusBadge status={task.status} />
                    <span className="min-w-0 flex-1 truncate">{task.title}</span>
                    <span className="font-mono text-[11px] text-muted-foreground">{timeAgo(task.created_at)}</span>
                  </Link>
                ))
              )}
            </div>
          </Card>

          <Card
            title={`${spec.name} 账号`}
            actions={<Link href="/accounts/" className="btn-ghost px-2 py-1 text-[11px]">全部账号</Link>}
            bodyClass="p-0"
          >
            <div className="max-h-[260px] overflow-auto">
              {accounts.length === 0 ? (
                <div className="p-4"><Empty>该平台暂无账号，先跑一轮注册</Empty></div>
              ) : (
                accounts.slice(0, 40).map((row) => (
                  <div key={row.ref} className="flex items-center gap-2 border-b border-border/60 px-4 py-1.5 text-[12px] last:border-0">
                    <span className="min-w-0 flex-1 truncate font-mono text-[11px]" title={row.ref}>{row.identity || row.ref}</span>
                    <Badge tone={row.status === "ok" ? "ok" : row.status === "unknown" ? "neutral" : "bad"}>{row.status}</Badge>
                    <span className="w-[64px] text-right text-[11px] text-muted-foreground">
                      {row.health_checked_at ? timeAgo(row.health_checked_at) : "从未"}
                    </span>
                  </div>
                ))
              )}
            </div>
          </Card>
        </div>
      </div>
    </div>
  );
}

export const PLATFORM_SLUGS = Object.keys(SPECS);
