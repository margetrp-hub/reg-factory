"use client";

import type { ReactNode } from "react";

export function Card({
  title,
  actions,
  children,
  className = "",
  bodyClass = "",
}: {
  title?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
  className?: string;
  bodyClass?: string;
}) {
  return (
    <section className={`card ${className}`}>
      {(title || actions) && (
        <header className="card-head">
          <h2 className="card-title">{title}</h2>
          <div className="flex items-center gap-2">{actions}</div>
        </header>
      )}
      <div className={`card-body ${bodyClass}`}>{children}</div>
    </section>
  );
}

const tones: Record<string, string> = {
  neutral: "bg-accent text-foreground/85",
  info: "bg-brand/20 text-brand",
  ok: "bg-success/20 text-success",
  warn: "bg-warning/20 text-warning",
  bad: "bg-destructive/20 text-destructive",
};

export function Badge({ tone = "neutral", children }: { tone?: keyof typeof tones; children: ReactNode }) {
  return <span className={`badge ${tones[tone] || tones.neutral}`}>{children}</span>;
}

export function StatusBadge({ status }: { status: string }) {
  const map: Record<string, { tone: keyof typeof tones; text: string }> = {
    queued: { tone: "info", text: "排队" },
    running: { tone: "warn", text: "运行中" },
    stopping: { tone: "warn", text: "停止中" },
    succeeded: { tone: "ok", text: "成功" },
    completed: { tone: "ok", text: "已完成" },
    waiting_user: { tone: "warn", text: "待人工处理" },
    unknown: { tone: "warn", text: "等待核对" },
    cancelled: { tone: "neutral", text: "已取消" },
    failed: { tone: "bad", text: "失败" },
    stopped: { tone: "neutral", text: "已停止" },
    interrupted: { tone: "warn", text: "中断" },
  };
  const item = map[status] || { tone: "neutral" as const, text: status };
  return <Badge tone={item.tone}>{item.text}</Badge>;
}

export function Stat({
  label,
  value,
  hint,
  tone = "neutral",
}: {
  label: string;
  value: ReactNode;
  hint?: ReactNode;
  tone?: keyof typeof tones;
}) {
  return (
    <div className="rounded-[var(--radius)] border border-border bg-card px-3.5 py-3">
      <div className="text-[11px] uppercase tracking-[0.09em] text-muted-foreground">{label}</div>
      <div className={`mt-1 font-mono text-[20px] leading-tight ${tone === "bad" ? "text-destructive" : tone === "warn" ? "text-warning" : "text-foreground"}`}>
        {value}
      </div>
      {hint && <div className="mt-0.5 text-[11px] text-muted-foreground">{hint}</div>}
    </div>
  );
}

export function Field({
  label,
  hint,
  children,
}: {
  label: ReactNode;
  hint?: ReactNode;
  children: ReactNode;
}) {
  return (
    <label className="block">
      <span className="label">{label}</span>
      {children}
      {hint && <span className="mt-1 block text-[11px] leading-snug text-muted-foreground/80">{hint}</span>}
    </label>
  );
}

export function Toggle({
  checked,
  onChange,
  label,
  ariaLabel,
}: {
  checked: boolean;
  onChange: (next: boolean) => void;
  label?: string;
  ariaLabel?: string;
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={ariaLabel || label}
      onClick={() => onChange(!checked)}
      className={`inline-flex items-center gap-2 text-[13px] ${checked ? "text-primary-foreground" : "text-muted-foreground"}`}
    >
      <span
        className={`relative h-5 w-9 rounded-full border transition-colors ${
          checked ? "border-ring bg-brand/30" : "border-border bg-muted"
        }`}
      >
        <span
          className={`absolute top-0.5 h-3.5 w-3.5 rounded-full bg-background transition-all ${
            checked ? "left-[18px]" : "left-0.5"
          }`}
        />
      </span>
      {label}
    </button>
  );
}

export function Spinner({ className = "" }: { className?: string }) {
  return (
    <span
      className={`inline-block h-3.5 w-3.5 animate-spin rounded-full border-2 border-border border-t-brand ${className}`}
    />
  );
}

export function EntryArrow() {
  return <svg aria-hidden="true" className="sg-entry-arrow" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.65" strokeLinecap="round" strokeLinejoin="round"><path d="M7 17 17 7M7 7h10v10" /></svg>;
}

export function Empty({ children }: { children: ReactNode }) {
  return (
    <div className="rounded-lg border border-dashed border-border px-4 py-8 text-center text-[13px] text-muted-foreground">
      {children}
    </div>
  );
}

export function ErrorNote({ message }: { message?: string | null }) {
  if (!message) return null;
  return (
    <div role="alert" className="rounded-lg border border-destructive/30 bg-destructive/10 px-3 py-2 text-[12px] text-destructive">
      {message}
    </div>
  );
}

export function timeAgo(iso: string): string {
  if (!iso) return "-";
  const ts = new Date(iso).getTime();
  if (Number.isNaN(ts)) return iso;
  const diff = Math.max(0, Date.now() - ts) / 1000;
  if (diff < 60) return `${Math.floor(diff)} 秒前`;
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`;
  if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`;
  return `${Math.floor(diff / 86400)} 天前`;
}

export function duration(sec: number): string {
  if (!sec || sec < 0) return "-";
  if (sec < 60) return `${Math.floor(sec)}s`;
  if (sec < 3600) return `${Math.floor(sec / 60)}m${Math.floor(sec % 60)}s`;
  return `${Math.floor(sec / 3600)}h${Math.floor((sec % 3600) / 60)}m`;
}
