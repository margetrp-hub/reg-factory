"use client";

import { useEffect, useState } from "react";

export type MailSource = { id: number; name: string; host: string; port: number; username: string; folder: string; enabled: boolean; idle: boolean; intervalSeconds: number; heartbeatSeconds: number; lastUID: number; lastOKAt?: string; lastError?: string; passwordSet: boolean; runtime: string; transport: string; credentialMode: string; clientID: string; refreshTokenSet: boolean };
export type Mailbox = { email: string; sourceId: number; sourceName: string; sourceEnabled: boolean; state: string; manualState: string; origin: string; total: number; riskHits: number; lastAt: number; lastTag: string; manual: boolean; provider: string; role: string; transport: string; parentId: string; parentState: string };
export type MailMessage = { id: number; sourceId: number; uid: string; subject: string; from: string; to: string; primaryTag: string; tags: string[]; alias: string; sentAt: number; text?: string; snippet: string };
export type MailRun = { id: number; sourceId: number; sourceName: string; requestId: string; state: string; requestedAt: number; startedAt: number; finishedAt: number; note: string; indexed: number };
export type MailCode = { email: string; code: string; receivedAt: number; ageSeconds: number; scanned: number };
export type MailConfig = { retentionDays: number; bootstrapLimit: number; fetchChunk: number; adminKeySet: boolean; turnstile: { siteKey: string; secretSet: boolean; expectedHostname: string } };

export const MAIL_TAGS: Record<string, string> = { otp: "验证码", risk: "风险", banned: "封禁", payment: "付款", promo: "推广", other: "其他" };
export const MAIL_STATES: Record<string, string> = { active: "可用", paused: "已暂停", blocked: "已阻止" };
export const MAIL_STATUS_LABELS: Record<string, string> = { ...MAIL_STATES, auth_failed: "邮箱授权失效", exhausted: "邮箱额度用尽", retired: "已停用", parent_banned: "邮箱母号停用", in_use: "使用中" };
export const mailTone = (state: string) => ["active", "completed"].includes(state) ? "ok" : ["failed", "blocked", "banned", "risk"].includes(state) ? "bad" : ["paused", "running", "interrupted", "payment"].includes(state) ? "warn" : state === "pending" || state === "otp" ? "info" : "neutral";
export const mailTime = (value?: number | string) => value ? new Date(typeof value === "number" ? value * 1000 : value).toLocaleString() : "—";

export class MailRequestError extends Error {
  constructor(message: string, public status: number) { super(message); }
}

export async function mailRequest<T>(path: string, init: RequestInit = {}): Promise<T> {
  const signal = init.signal ? AbortSignal.any([init.signal, AbortSignal.timeout(15000)]) : AbortSignal.timeout(15000);
  const response = await fetch("/api/apps/mail" + path, { credentials: "same-origin", ...init, signal, headers: { "content-type": "application/json", ...init.headers } });
  const body = await response.json();
  if (!response.ok || body.ok !== true) throw new MailRequestError(body.error?.message || body.detail || `请求失败（${response.status}）`, response.status);
  return body.data as T;
}

// Serial polling keeps the last confirmed page on failure; navigation cancels it.
export function useMailData<T>(path: string, refresh = 0, interval = 20000) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  useEffect(() => {
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    const load = async () => {
      setLoading(true);
      try {
        const next = await mailRequest<T>(path, { signal: controller.signal });
        if (!controller.signal.aborted) { setData(next); setError(""); }
      } catch (err) {
        if (!controller.signal.aborted) setError(`读取失败，保留上次内容：${(err as Error).message}`);
      } finally {
        if (!controller.signal.aborted) { setLoading(false); if (interval) timer = setTimeout(load, interval); }
      }
    };
    void load();
    return () => { controller.abort(); clearTimeout(timer); };
  }, [path, refresh, interval]);
  return { data, error, loading };
}
