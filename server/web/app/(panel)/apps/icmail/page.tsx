"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { Card, Empty, ErrorNote, Stat } from "@/components/ui";

const BASE = "/api/apps/icmail";

type Stats = { indexed: number; lastUid: number; sources: number; byTag: Record<string, number> };
type CodeResult = { email: string; code: string; receivedAt: number; ageSeconds: number; scanned: number };

declare global {
  interface Window {
    turnstile?: {
      render: (el: HTMLElement, opts: Record<string, unknown>) => string;
      reset: (id: string) => void;
    };
  }
}

function relative(seconds: number): string {
  if (!seconds || seconds < 0) return "刚刚";
  if (seconds < 60) return `${Math.round(seconds)} 秒前`;
  if (seconds < 3600) return `${Math.round(seconds / 60)} 分钟前`;
  if (seconds < 86400) return `${Math.round(seconds / 3600)} 小时前`;
  return `${Math.round(seconds / 86400)} 天前`;
}

export default function IcmailPage() {
  const [stats, setStats] = useState<Stats | null>(null);
  const [error, setError] = useState("");
  const [email, setEmail] = useState("");
  const [code, setCode] = useState<CodeResult | null>(null);
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);

  const boxRef = useRef<HTMLDivElement | null>(null);
  const widgetRef = useRef<string | null>(null);
  const tokenRef = useRef("");

  const loadStats = useCallback(async () => {
    try {
      const res = await fetch(`${BASE}/health`, { credentials: "same-origin" });
      const body = await res.json();
      if (!res.ok || body.ok === false) throw new Error(body?.error?.message || "读取失败");
      const s = body.data.stats;
      setStats({ indexed: s.indexed, lastUid: s.lastUid, sources: s.sources, byTag: s.byTag || {} });
      setError("");
    } catch (err) {
      setError((err as Error).message);
    }
  }, []);

  useEffect(() => {
    loadStats();
    const timer = setInterval(loadStats, 30000);
    return () => clearInterval(timer);
  }, [loadStats]);

  // Turnstile is loaded lazily; the page still works (with a clear error) if it
  // cannot be fetched.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const res = await fetch(`${BASE}/config`, { credentials: "same-origin" });
        const body = await res.json();
        const siteKey = body?.data?.siteKey as string | undefined;
        if (!siteKey || cancelled) return;
        if (!window.turnstile) {
          await new Promise<void>((resolve, reject) => {
            const script = document.createElement("script");
            script.src = "https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit";
            script.async = true;
            script.onload = () => resolve();
            script.onerror = () => reject(new Error("turnstile script failed"));
            document.head.appendChild(script);
          }).catch(() => undefined);
        }
        if (!window.turnstile || !boxRef.current || cancelled) return;
        widgetRef.current = window.turnstile.render(boxRef.current, {
          sitekey: siteKey,
          action: "otp_lookup",
          theme: "light",
          callback: (value: string) => {
            tokenRef.current = value || "";
          },
          "expired-callback": () => {
            tokenRef.current = "";
          }
        });
      } catch {
        /* leave the notice to the submit handler */
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const lookup = async () => {
    setNotice("");
    setBusy(true);
    try {
      const res = await fetch(`${BASE}/code`, {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ email, turnstileToken: tokenRef.current })
      });
      const body = await res.json();
      if (!res.ok || body.ok === false) {
        const map: Record<string, string> = {
          TURNSTILE_FAILED: "人机验证未通过，请重试验证。",
          TURNSTILE_NOT_CONFIGURED: "人机验证未配置。",
          TURNSTILE_UNAVAILABLE: "人机验证服务不可用。",
          BAD_REQUEST: "邮箱格式不正确。"
        };
        const key = body?.error?.code as string;
        throw new Error(map[key] || body?.error?.message || "查询失败");
      }
      const data = body.data as CodeResult;
      setCode(data.code ? data : null);
      if (!data.code) setNotice(`已检查最近 ${data.scanned} 封邮件，暂未找到验证码。`);
      if (widgetRef.current && window.turnstile) window.turnstile.reset(widgetRef.current);
      tokenRef.current = "";
    } catch (err) {
      setNotice((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const tags = stats?.byTag || {};
  const tagRows = Object.entries(tags).sort((a, b) => b[1] - a[1]);

  return (
    <div className="mx-auto w-full max-w-6xl space-y-4">
      <div className="flex items-end justify-between gap-3">
        <div>
          <h1 className="text-lg font-semibold">IC 邮箱</h1>
          <p className="text-sm text-muted-foreground">收件索引与验证码查询；只回验证码，不暴露邮件内容。</p>
        </div>
        <button className="btn" onClick={loadStats}>
          刷新
        </button>
      </div>

      <ErrorNote message={error} />

      <div className="grid gap-3 sm:grid-cols-3">
        <Stat label="已索引邮件" value={stats ? String(stats.indexed) : "-"} hint="recent index" />
        <Stat label="最新 UID" value={stats ? String(stats.lastUid) : "-"} hint="imap cursor" />
        <Stat label="收件源" value={stats ? String(stats.sources) : "-"} hint="imap accounts" />
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        <Card title="验证码查询" actions={<span className="text-xs text-muted-foreground">public lookup</span>}>
          <div className="space-y-3">
            <label className="field">
              <span>邮箱地址</span>
              <input
                className="input"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                placeholder="name@icloud.com"
                autoComplete="off"
              />
            </label>
            <div ref={boxRef} className="min-h-[66px]" />
            <button className="btn btn-primary w-full" disabled={busy || !email} onClick={lookup}>
              {busy ? "查询中…" : "获取验证码"}
            </button>
            {code ? (
              <div className="rounded-md border border-border bg-muted p-4 text-center">
                <div className="text-xs uppercase tracking-widest text-muted-foreground">验证码</div>
                <div className="my-2 font-mono text-3xl font-semibold tracking-[0.2em]">{code.code}</div>
                <div className="text-xs text-muted-foreground">收件时间 {relative(code.ageSeconds)} · 检查了 {code.scanned} 封</div>
              </div>
            ) : null}
            {notice ? <p className="text-sm text-muted-foreground">{notice}</p> : null}
          </div>
        </Card>

        <Card title="分类分布" actions={<span className="text-xs text-muted-foreground">classifier</span>}>
          {tagRows.length === 0 ? (
            <Empty>还没有索引到邮件</Empty>
          ) : (
            <ul className="space-y-2">
              {tagRows.map(([tag, count]) => (
                <li key={tag} className="flex items-center justify-between text-sm">
                  <span className="font-mono text-xs uppercase tracking-wide">{tag}</span>
                  <span className="tabular-nums">{count}</span>
                </li>
              ))}
            </ul>
          )}
        </Card>
      </div>
    </div>
  );
}
