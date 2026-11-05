"use client";

import { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { api, type AccountRow, type Script, type Task } from "@/lib/api";
import { Badge, Card, Empty, Stat } from "@/components/ui";
import { TaskForm } from "@/components/task-form";
import { TaskTable } from "@/components/task-table";
import { LogView } from "@/components/log-view";
import { Tabs } from "@/components/tabs";
import { groupOf } from "@/lib/task-catalog";

const PLATFORM_META: Record<string, { name: string; primary: string[] }> = {
  claude: { name: "Claude", primary: ["register_claude"] },
  chatgpt: { name: "ChatGPT", primary: ["register_chatgpt", "oauth_codex", "plus_codex_import"] },
  grok: { name: "Grok", primary: ["register_grok"] },
  outlook: { name: "Outlook", primary: ["outlook_reg_loop", "unlock_outlook"] },
};

/** 平台子页：tasks = 该平台任务下发；accounts = 该平台账号资产。 */
export function PlatformSubview({ slug, tab }: { slug: string; tab: "tasks" | "accounts" }) {
  const meta = PLATFORM_META[slug] || { name: slug, primary: [] };
  const [scripts, setScripts] = useState<Script[]>([]);
  const [selected, setSelected] = useState("");
  const [tasks, setTasks] = useState<Task[]>([]);
  const [accounts, setAccounts] = useState<AccountRow[]>([]);
  const [activeRun, setActiveRun] = useState("");
  const [busy, setBusy] = useState("");
  const [inner, setInner] = useState<"queue" | "log">("queue");

  const load = useCallback(async () => {
    const [scriptData, taskData, accountData] = await Promise.all([
      api.scripts(),
      api.tasks(120),
      api.accounts({ platform: slug, limit: 300 }),
    ]);
    const ids = new Set(meta.primary);
    const mine = scriptData.scripts.filter((item) => ids.has(item.id));
    setScripts(mine);
    setSelected((prev) => (mine.some((item) => item.id === prev) ? prev : mine[0]?.id || ""));
    setTasks(taskData.tasks.filter((task) => ids.has(task.script)));
    setAccounts(accountData.accounts);
  }, [slug, meta.primary]);

  useEffect(() => {
    load();
    const timer = setInterval(load, 6000);
    return () => clearInterval(timer);
  }, [load]);

  const active = scripts.find((item) => item.id === selected);
  const selectedTask = tasks.find((task) => task.id === activeRun);

  const runLiveness = async () => {
    const targets = accounts.filter((row) => row.status !== "ok").slice(0, 5);
    if (targets.length === 0) return;
    setBusy("liveness");
    for (const row of targets) {
      await api.accountLiveness(row.ref, row.platform || slug, false).catch(() => null);
    }
    setBusy("");
    load();
  };

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-3">
        <Link href={`/platform/${slug}/`} className="btn-ghost px-2 py-1 text-[11px]">← {meta.name} 概览</Link>
        <div className="min-w-0 flex-1">
          <h1 className="text-[16px] font-semibold leading-tight">
            {meta.name} · {tab === "tasks" ? "任务" : "账号"}
          </h1>
          <p className="text-[11px] text-muted-foreground">
            {tab === "tasks" ? "该平台的任务模板与下发；参数与 CLI 一致。" : "该平台产出的账号资产，可逐条测活/刷新。"}
          </p>
        </div>
        <div className="flex gap-2">
          <Link
            href={`/platform/${slug}/tasks/`}
            className={tab === "tasks" ? "btn-primary text-[11px]" : "btn-ghost text-[11px]"}
          >
            任务
          </Link>
          <Link
            href={`/platform/${slug}/accounts/`}
            className={tab === "accounts" ? "btn-primary text-[11px]" : "btn-ghost text-[11px]"}
          >
            账号
          </Link>
        </div>
      </div>

      {tab === "tasks" ? (
        <div className="grid gap-4 xl:grid-cols-[1fr_1.05fr]">
          <div className="space-y-3">
            <div className="grid gap-2 sm:grid-cols-2">
              {scripts.map((script) => (
                <button
                  key={script.id}
                  onClick={() => setSelected(script.id)}
                  className={`rounded-[var(--radius)] border p-3 text-left transition-colors ${
                    selected === script.id
                      ? "border-brand bg-brand/10"
                      : "border-border bg-card hover:border-brand/40 hover:bg-accent/40"
                  }`}
                >
                  <div className="text-[13px] font-medium">{script.title}</div>
                  <div className="mt-1 line-clamp-2 text-[11px] leading-snug text-muted-foreground">
                    {script.desc || script.file}
                  </div>
                </button>
              ))}
              {scripts.length === 0 && <Empty>该平台暂无任务模板</Empty>}
            </div>
            <Card
              title={active ? `下单 · ${active.title}` : "下单"}
              actions={active ? <span className="font-mono text-[11px] text-muted-foreground">{active.file}</span> : null}
            >
              {active ? (
                <TaskForm script={active} onStarted={(id) => { setActiveRun(id); load(); }} />
              ) : (
                <Empty>上方选一个任务</Empty>
              )}
            </Card>
          </div>
          <div className="space-y-3">
            <Tabs
              items={[
                { id: "queue", label: "队列", count: tasks.length },
                { id: "log", label: "实时日志" },
              ]}
              value={inner}
              onChange={(id) => setInner(id as "queue" | "log")}
              right={selectedTask ? <span className="font-mono text-[11px] text-muted-foreground">exit {selectedTask.exit_code}</span> : null}
            />
            {inner === "log" ? (
              <Card title={selectedTask ? selectedTask.title : "实时日志"} bodyClass="p-2">
                {activeRun ? <LogView taskId={activeRun} height={520} /> : <Empty>队列里点一个任务看实时日志</Empty>}
              </Card>
            ) : (
            <Card title="本平台任务" bodyClass="p-0">
              <div className="max-h-[300px] overflow-auto">
                <TaskTable
                  tasks={tasks}
                  activeId={activeRun}
                  onSelect={setActiveRun}
                  onStop={async (id) => { await api.stopTask(id).catch(() => null); load(); }}
                  emptyText="该平台还没有任务"
                />
              </div>
            </Card>
            )}
          </div>
        </div>
      ) : (
        <div className="space-y-4">
          <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
            <Stat label="账号总数" value={accounts.length} />
            <Stat label="可用" value={accounts.filter((row) => row.status === "ok").length} />
            <Stat label="封禁/失效" value={accounts.filter((row) => row.status === "banned" || row.status === "invalid").length} tone="bad" />
            <Stat label="已领取" value={accounts.filter((row) => row.claimed).length} />
          </div>
          <Card
            title={`${meta.name} 账号`}
            bodyClass="p-0"
            actions={
              <button className="btn-ghost px-2 py-1 text-[11px]" onClick={runLiveness} disabled={busy === "liveness"}>
                {busy === "liveness" ? "提交中…" : "批量测活"}
              </button>
            }
          >
            <div className="max-h-[560px] overflow-auto">
              {accounts.length === 0 ? (
                <div className="p-4"><Empty>该平台暂无账号，先去任务页跑一轮注册</Empty></div>
              ) : (
                <table className="tbl">
                  <thead>
                    <tr className="border-b border-border">
                      <th className="px-3 py-1.5 text-left font-medium">账号</th>
                      <th className="px-2 py-1.5 text-left font-medium">类型</th>
                      <th className="px-2 py-1.5 text-left font-medium">状态</th>
                      <th className="px-2 py-1.5 text-left font-medium">领取</th>
                      <th className="px-2 py-1.5 text-right font-medium">测活</th>
                      <th className="px-3 py-1.5 text-right font-medium">操作</th>
                    </tr>
                  </thead>
                  <tbody>
                    {accounts.map((row) => (
                      <tr key={row.ref} className="border-b border-border/60 last:border-0 hover:bg-accent/40">
                        <td className="max-w-[260px] truncate px-3 py-1.5 font-mono text-[11px]" title={row.ref}>
                          {row.identity || row.ref}
                        </td>
                        <td className="px-2 py-1.5 text-[11px] text-muted-foreground">{row.kind}</td>
                        <td className="px-2 py-1.5">
                          <Badge tone={row.status === "ok" ? "ok" : row.status === "unknown" ? "neutral" : "bad"}>
                            {row.status}
                          </Badge>
                        </td>
                        <td className="px-2 py-1.5 text-[11px] text-muted-foreground">{row.claimed ? "已领" : "可供应"}</td>
                        <td className="px-2 py-1.5 text-right text-[11px] text-muted-foreground">
                          {row.health_checked_at ? new Date(row.health_checked_at).toLocaleString() : "从未"}
                        </td>
                        <td className="px-3 py-1.5 text-right">
                          <button
                            className="btn-ghost px-2 py-0.5 text-[11px]"
                            onClick={async () => {
                              await api.accountLiveness(row.ref, row.platform || slug, false).catch(() => null);
                              load();
                            }}
                          >
                            测活
                          </button>
                          <button
                            className="btn-ghost ml-1 px-2 py-0.5 text-[11px]"
                            disabled={row.claimed}
                            onClick={async () => {
                              await api.accountRefresh(row.ref, row.platform || slug, false).catch(() => null);
                              load();
                            }}
                          >
                            刷新
                          </button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </div>
          </Card>
          <p className="text-[11px] text-muted-foreground">
            任务分类：{groupOf(meta.primary[0] || "").title}；批量操作走 <Link href="/accounts/" className="text-brand">资产中心</Link>。
          </p>
        </div>
      )}
    </div>
  );
}
