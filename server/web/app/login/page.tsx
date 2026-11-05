"use client";

import { useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import { api, ApiError } from "@/lib/api";

export default function LoginPage() {
  const router = useRouter();
  const [username, setUsername] = useState("admin");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [next, setNext] = useState("/");

  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    const target = params.get("next");
    // next 不能指回登录页，否则登录成功后原地打转
    const safe = target && target.startsWith("/") && !target.startsWith("/login") ? target : "/";
    setNext(safe);
    api
      .session()
      .then(() => router.replace(safe))
      .catch(() => null);
  }, [router]);

  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.login(username.trim(), password);
      router.replace(next);
    } catch (err) {
      const apiError = err as ApiError;
      setError(apiError.status === 429 ? "尝试过于频繁，请稍后再试" : apiError.detail || "登录失败");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="grid min-h-screen place-items-center bg-muted/20 px-4">
      <form onSubmit={submit} className="card w-full max-w-[420px] px-7 py-8 shadow-sm">
        <div className="mb-1 flex items-center gap-2">
          <div className="grid h-9 w-9 place-items-center rounded-lg bg-brand/20 text-brand">⌬</div>
          <div>
            <h1 className="text-[18px] font-semibold leading-tight">reg-factory</h1>
            <p className="text-[11px] uppercase tracking-[0.14em] text-muted-foreground">Go 控制面 · 登录</p>
          </div>
        </div>
        <div className="mb-5 mt-4 rounded-lg border border-border/70 bg-muted/40 px-3 py-2.5 text-[12px] leading-relaxed text-muted-foreground">
          统一管理生产任务、账号资产和运行审计。登录后按工作域继续操作。
        </div>

        {error && <div className="mb-4 rounded-lg border border-destructive/30 bg-destructive/10 px-3 py-2 text-[12px] text-destructive">{error}</div>}

        <label className="label" htmlFor="username">账号</label>
        <input id="username" className="field mb-4" value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" />

        <label className="label" htmlFor="password">密码</label>
        <input
          id="password"
          className="field"
          type="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          autoComplete="current-password"
          autoFocus
        />

        <button type="submit" className="btn-primary mt-6 w-full py-2" disabled={busy || !password}>
          {busy ? "验证中…" : "进入控制台"}
        </button>
        <p className="mt-4 text-center text-[11px] text-muted-foreground/70">使用部署方提供的独立面板口令</p>
      </form>
    </div>
  );
}
