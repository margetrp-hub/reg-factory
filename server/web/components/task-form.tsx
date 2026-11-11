"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { api, ApiError, type Script, type ScriptArg } from "@/lib/api";
import { ErrorNote, Field, Toggle } from "@/components/ui";

function initialValue(arg: ScriptArg): unknown {
  if (arg.type === "bool") return Boolean(arg.default);
  if (arg.type === "multi") return Array.isArray(arg.default) ? arg.default : [];
  if (arg.default === null || arg.default === undefined) return "";
  return arg.default;
}

type FormProps = { script: Script; onStarted: (id: string) => void; onProtectionChange?: (protectedDraft: boolean) => void };

export function TaskForm(props: FormProps) {
  return <ModeForm key={props.script.id} {...props} />;
}

function ModeForm({ script, onStarted, onProtectionChange }: FormProps) {
  const args = script.args || [];
  const [values, setValues] = useState<Record<string, unknown>>(() => {
    const out: Record<string, unknown> = {};
    for (const arg of args) out[arg.flag] = initialValue(arg);
    return out;
  });
  const [advanced, setAdvanced] = useState(false);
  const [query, setQuery] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [dirty, setDirty] = useState(false);
  const [uncertain, setUncertain] = useState(false);
  const pending = useRef<{ id: string; payload: Record<string, unknown> | null } | null>(null);
  const submitting = useRef(false);
  const storageKey = `sg-task-request:${script.id}`;

  useEffect(() => {
    try {
      const id = sessionStorage.getItem(storageKey);
      if (id && /^[0-9a-f-]{36}$/i.test(id)) {
        pending.current = { id, payload: null };
        setUncertain(true);
        setError("上次提交尚未确认。请核对原任务；若尚未受理，重新填写原参数后仍使用原请求编号提交。凭据和参数不会存入浏览器存储。");
      }
    } catch { /* Storage may be disabled; the current page still retains the request. */ }
  }, [storageKey]);

  useEffect(() => {
    if (!dirty && !busy && !uncertain) return;
    const warn = (event: BeforeUnloadEvent) => { event.preventDefault(); event.returnValue = ""; };
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [dirty, busy, uncertain]);

  useEffect(() => {
    onProtectionChange?.(dirty || busy || uncertain);
  }, [dirty, busy, uncertain, onProtectionChange]);

  const { common, rest, modes } = useMemo(() => {
    const modes = args.filter(arg => arg.type === "choice");
    const remaining = args.filter(arg => !modes.includes(arg));
    const primary = remaining.slice(0, 4);
    return { common: primary, rest: remaining.slice(primary.length), modes };
  }, [args]);
  const found = query.trim() ? args.filter(arg=>`${arg.flag} ${arg.help || ""}`.toLowerCase().includes(query.trim().toLowerCase())) : null;

  const update = (flag: string, value: unknown) => {
    setDirty(true);
    setValues((prev) => ({ ...prev, [flag]: value }));
  };

  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    if (submitting.current) return;
    submitting.current = true;
    setBusy(true);
    setError("");
    setNotice("");
    let creating = false;
    try {
      if (pending.current) {
        try {
          const original = await api.task(`request-${pending.current.id}`);
          pending.current = null;
          try { sessionStorage.removeItem(storageKey); } catch { /* optional storage */ }
          setUncertain(false);
          setDirty(false);
          onStarted(original.id);
          return;
        } catch (err) {
          if (!(err instanceof ApiError) || err.status !== 404) throw err;
        }
      }
      const payload: Record<string, unknown> = {};
      for (const arg of args) {
        const value = values[arg.flag];
        if (arg.type === "bool") {
          payload[arg.flag] = Boolean(value);
          continue;
        }
        if (arg.type === "multi") {
          const list = Array.isArray(value) ? value.filter(Boolean) : String(value || "").split(",").filter(Boolean);
          if (list.length) payload[arg.flag] = list;
          continue;
        }
        if (value === "" || value === null || value === undefined) continue;
        payload[arg.flag] = value;
      }
      if (!pending.current) {
        const bytes = crypto.getRandomValues(new Uint8Array(16));
        bytes[6] = (bytes[6] & 15) | 64;
        bytes[8] = (bytes[8] & 63) | 128;
        const hex = Array.from(bytes, byte => byte.toString(16).padStart(2, "0")).join("");
        pending.current = { id: `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`, payload };
      }
      pending.current.payload ||= payload;
      try { sessionStorage.setItem(storageKey, pending.current.id); } catch { /* optional storage */ }
      creating = true;
      const created = await api.createTask(script.id, pending.current.payload, pending.current.id);
      pending.current = null;
      try { sessionStorage.removeItem(storageKey); } catch { /* optional storage */ }
      setDirty(false);
      setUncertain(false);
      onProtectionChange?.(false);
      setNotice(`已入队 ${created.id}`);
      onStarted(created.id);
    } catch (err) {
      const apiError = err as ApiError;
      const rejected = creating && apiError instanceof ApiError && apiError.status >= 400 && apiError.status < 500 && ![401, 408, 409].includes(apiError.status);
      if (rejected) {
        pending.current = null;
        try { sessionStorage.removeItem(storageKey); } catch { /* optional storage */ }
      }
      setUncertain(!rejected);
      setError(rejected ? (apiError.detail || apiError.message) : "提交结果暂未确认。参数已保留，点击“核对原请求”会返回同一个任务，不会重复启动。");
    } finally {
      submitting.current = false;
      setBusy(false);
    }
  };

  return (
    <form onSubmit={submit} className="space-y-3">
      {script.desc && <p className="text-[12px] leading-relaxed text-muted-foreground">{script.desc}</p>}
      {script.warning && (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-3 py-2 text-[12px] text-warning">
          {script.warning}
        </div>
      )}

      {args.length > 4 && <div className="flex items-center gap-3"><input className="field" aria-label="查找执行参数" placeholder={`查找全部 ${args.length} 项参数或用途`} value={query} onChange={event=>setQuery(event.target.value)} /><span className="shrink-0 text-[12px] text-muted-foreground">{args.length} 项参数</span></div>}
      {!found && modes.length > 0 && <fieldset disabled={busy || (uncertain && pending.current?.payload !== null)} className="grid gap-3 rounded-xl border border-border bg-muted/25 p-4 sm:grid-cols-2">
        <legend className="px-1 text-[13px] font-medium">模式与来源</legend>{modes.map(arg=><ArgControl key={arg.flag} arg={arg} value={values[arg.flag]} onChange={value=>update(arg.flag,value)} />)}
      </fieldset>}
      <fieldset disabled={busy || (uncertain && pending.current?.payload !== null)} className="grid gap-3 sm:grid-cols-2 disabled:opacity-70">
        {(found || common).map((arg) => (
          <ArgControl key={arg.flag} arg={arg} value={values[arg.flag]} onChange={(v) => update(arg.flag, v)} />
        ))}
      </fieldset>

      {found?.length === 0 && <p className="text-sm text-muted-foreground">没有匹配参数，清空搜索可查看全部设置。</p>}
      {!found && rest.length > 0 && (
        <div>
          <button type="button" aria-expanded={advanced} className="btn-ghost text-[12px]" onClick={() => setAdvanced((v) => !v)}>
            {advanced ? "收起更多设置" : `更多设置（${rest.length}）`}
          </button>
          {advanced && (
            <fieldset disabled={busy || (uncertain && pending.current?.payload !== null)} className="mt-3 grid gap-3 sm:grid-cols-2 disabled:opacity-70">
              {rest.map((arg) => (
                <ArgControl key={arg.flag} arg={arg} value={values[arg.flag]} onChange={(v) => update(arg.flag, v)} />
              ))}
            </fieldset>
          )}
        </div>
      )}

      <ErrorNote message={error} />
      {notice && <div className="text-[12px] text-success">{notice}</div>}

      <div className="flex items-center gap-2 pt-1">
        <button type="submit" className="btn-primary" disabled={busy}>
          {busy ? "提交中…" : uncertain ? "核对原请求" : "启动任务"}
        </button>
        <span className="text-[12px] text-muted-foreground">受理后在原任务查看进度与结果</span>
      </div>
    </form>
  );
}

function ArgControl({ arg, value, onChange }: { arg: ScriptArg; value: unknown; onChange: (next: unknown) => void }) {
  const label = arg.flag.replace(/^--/, "");
  if (arg.type === "bool") {
    return (
      <div className="flex items-center justify-between gap-3 rounded-lg border border-border bg-muted/30 px-3 py-2">
        <div><span className="text-[12px] text-foreground/85">{label}</span>{arg.help && <p className="mt-1 text-[12px] leading-relaxed text-muted-foreground">{arg.help}</p>}</div>
        <Toggle checked={Boolean(value)} onChange={onChange} ariaLabel={label} />
      </div>
    );
  }
  if (arg.type === "choice" && arg.choices?.length) {
    return (
      <Field label={label} hint={arg.help}>
        <select className="field" aria-label={label} value={String(value ?? "")} onChange={(e) => onChange(e.target.value)}>
          {arg.choices.map((choice) => (
            <option key={choice} value={choice}>
              {arg.labels?.[choice] || choice || "(空)"}
            </option>
          ))}
        </select>
      </Field>
    );
  }
  if (arg.type === "multi") {
    const text = Array.isArray(value) ? value.join(",") : String(value ?? "");
    return (
      <Field label={label} hint={arg.help || "多个值用英文逗号分隔"}>
        <input
          className="field"
          aria-label={label}
          value={text}
          onChange={(e) => onChange(e.target.value.split(",").map((v) => v.trim()).filter(Boolean))}
        />
      </Field>
    );
  }
  if (arg.type === "int") {
    return (
      <Field label={label} hint={arg.help}>
        <input
          className="field"
          aria-label={label}
          type="number"
          value={String(value ?? "")}
          onChange={(e) => onChange(e.target.value === "" ? "" : Number(e.target.value))}
        />
      </Field>
    );
  }
  return (
    <Field label={label} hint={arg.help}>
      <input className="field" aria-label={label} type={/password|token|secret|api-key/i.test(arg.flag) ? "password" : "text"} autoComplete="off" value={String(value ?? "")} onChange={(e) => onChange(e.target.value)} />
    </Field>
  );
}
