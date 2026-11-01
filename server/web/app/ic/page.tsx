"use client";

// 对外唯一页面：输入邮箱，拿验证码。没有导航、没有邮箱列表、没有邮件内容。

import { useEffect, useRef, useState } from "react";

const BASE = "/api/apps/icmail/public";

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

export default function PublicCodePage() {
  const [email, setEmail] = useState("");
  const [result, setResult] = useState<CodeResult | null>(null);
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [ready, setReady] = useState(false);
  const boxRef = useRef<HTMLDivElement | null>(null);
  const widgetRef = useRef<string | null>(null);
  const tokenRef = useRef("");

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const res = await fetch(`${BASE}/config`);
        const body = await res.json();
        const siteKey = body?.data?.siteKey as string | undefined;
        if (!siteKey || cancelled) {
          setNotice("人机验证暂未配置，请联系管理员。");
          return;
        }
        if (!window.turnstile) {
          await new Promise<void>((resolve) => {
            const script = document.createElement("script");
            script.src = "https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit";
            script.async = true;
            script.onload = () => resolve();
            script.onerror = () => resolve();
            document.head.appendChild(script);
          });
        }
        if (!window.turnstile || !boxRef.current || cancelled) {
          setNotice("人机验证组件加载失败，请刷新页面。");
          return;
        }
        widgetRef.current = window.turnstile.render(boxRef.current, {
          sitekey: siteKey,
          action: "otp_lookup",
          theme: "light",
          callback: (v: string) => {
            tokenRef.current = v || "";
            setReady(true);
          },
          "expired-callback": () => {
            tokenRef.current = "";
            setReady(false);
          }
        });
      } catch {
        setNotice("初始化失败，请刷新页面。");
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
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ email, turnstileToken: tokenRef.current })
      });
      const body = await res.json();
      if (!res.ok || body.ok === false) {
        const map: Record<string, string> = {
          TURNSTILE_FAILED: "人机验证未通过，请重新验证。",
          TURNSTILE_NOT_CONFIGURED: "人机验证未配置。",
          TURNSTILE_UNAVAILABLE: "人机验证服务不可用，请稍后重试。",
          BAD_REQUEST: "邮箱格式不正确。"
        };
        const key = body?.error?.code as string;
        throw new Error(map[key] || body?.error?.message || "查询失败");
      }
      const data = body.data as CodeResult;
      setResult(data.code ? data : null);
      if (!data.code) setNotice(`已检查最近 ${data.scanned} 封邮件，暂未找到验证码，请稍后再试。`);
      if (widgetRef.current && window.turnstile) window.turnstile.reset(widgetRef.current);
      tokenRef.current = "";
      setReady(false);
    } catch (err) {
      setNotice((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="mx-auto flex min-h-screen w-full max-w-xl flex-col justify-center px-5 py-10">
      <div className="mb-6">
        <div className="font-mono text-xs uppercase tracking-[0.2em] text-muted-foreground">Code Lookup</div>
        <h1 className="mt-2 text-2xl font-semibold">获取验证码</h1>
        <p className="mt-1 text-sm text-muted-foreground">
          输入接收验证码的邮箱，完成人机验证后显示最新验证码。不展示邮件内容。
        </p>
      </div>

      <section className="card">
        <div className="card-body space-y-4">
          <label className="field">
            <span>邮箱地址</span>
            <input
              className="input"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              placeholder="name@icloud.com"
              autoComplete="off"
              inputMode="email"
            />
          </label>

          <div ref={boxRef} className="min-h-[66px]" />

          <button className="btn btn-primary w-full" disabled={busy || !email} onClick={lookup}>
            {busy ? "查询中…" : "获取验证码"}
          </button>

          {result ? (
            <div className="rounded-md border border-border bg-muted p-5 text-center">
              <div className="font-mono text-xs uppercase tracking-[0.2em] text-muted-foreground">验证码</div>
              <div className="my-2 select-all font-mono text-4xl font-semibold tracking-[0.22em]">{result.code}</div>
              <div className="text-xs text-muted-foreground">
                收件时间 {relative(result.ageSeconds)} · 检查了 {result.scanned} 封
              </div>
            </div>
          ) : null}

          {notice ? <p className="text-sm text-muted-foreground">{notice}</p> : null}
          {!ready && !notice ? (
            <p className="text-xs text-muted-foreground">完成上方人机验证后可查询。</p>
          ) : null}
        </div>
      </section>

      <p className="mt-6 text-center text-xs text-muted-foreground">本页只返回验证码与收件时间。</p>
    </div>
  );
}
