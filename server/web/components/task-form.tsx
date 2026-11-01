"use client";

import { useMemo, useState } from "react";
import { api, ApiError, type Script, type ScriptArg } from "@/lib/api";
import { ErrorNote, Field, Toggle } from "@/components/ui";

function initialValue(arg: ScriptArg): unknown {
  if (arg.type === "bool") return Boolean(arg.default);
  if (arg.type === "multi") return Array.isArray(arg.default) ? arg.default : [];
  if (arg.default === null || arg.default === undefined) return "";
  return arg.default;
}

export function TaskForm({ script, onStarted }: { script: Script; onStarted: (id: string) => void }) {
  const args = script.args || [];
  const [values, setValues] = useState<Record<string, unknown>>(() => {
    const out: Record<string, unknown> = {};
    for (const arg of args) out[arg.flag] = initialValue(arg);
    return out;
  });
  const [advanced, setAdvanced] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  // 低频参数收进“更多设置”：bool 与带默认值的前 4 个算常用
  const { common, rest } = useMemo(() => {
    const primary = args.slice(0, Math.min(args.length, 4));
    return { common: primary, rest: args.slice(primary.length) };
  }, [args]);

  const update = (flag: string, value: unknown) => setValues((prev) => ({ ...prev, [flag]: value }));

  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError("");
    setNotice("");
    try {
      const payload: Record<string, unknown> = {};
      for (const arg of args) {
        const value = values[arg.flag];
        if (arg.type === "bool") {
          if (value) payload[arg.flag] = true;
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
      const created = await api.createTask(script.id, payload);
      setNotice(`已入队 ${created.id}`);
      onStarted(created.id);
    } catch (err) {
      const apiError = err as ApiError;
      setError(apiError.status === 429 ? "内存闸门已触发，等运行中的任务结束再试" : apiError.detail || apiError.message);
    } finally {
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

      <div className="grid gap-3 sm:grid-cols-2">
        {common.map((arg) => (
          <ArgControl key={arg.flag} arg={arg} value={values[arg.flag]} onChange={(v) => update(arg.flag, v)} />
        ))}
      </div>

      {rest.length > 0 && (
        <div>
          <button type="button" className="btn-ghost text-[11px]" onClick={() => setAdvanced((v) => !v)}>
            {advanced ? "收起更多设置" : `更多设置（${rest.length}）`}
          </button>
          {advanced && (
            <div className="mt-3 grid gap-3 sm:grid-cols-2">
              {rest.map((arg) => (
                <ArgControl key={arg.flag} arg={arg} value={values[arg.flag]} onChange={(v) => update(arg.flag, v)} />
              ))}
            </div>
          )}
        </div>
      )}

      <ErrorNote message={error} />
      {notice && <div className="text-[12px] text-success">{notice}</div>}

      <div className="flex items-center gap-2 pt-1">
        <button type="submit" className="btn-primary" disabled={busy}>
          {busy ? "提交中…" : "启动任务"}
        </button>
        <span className="text-[11px] text-muted-foreground">
          脚本 <span className="font-mono">{script.file}</span>
        </span>
      </div>
    </form>
  );
}

function ArgControl({ arg, value, onChange }: { arg: ScriptArg; value: unknown; onChange: (next: unknown) => void }) {
  const label = arg.flag.replace(/^--/, "");
  if (arg.type === "bool") {
    return (
      <div className="flex items-center justify-between rounded-lg border border-border bg-muted px-3 py-2">
        <span className="text-[12px] text-foreground/85">{label}</span>
        <Toggle checked={Boolean(value)} onChange={onChange} />
      </div>
    );
  }
  if (arg.type === "choice" && arg.choices?.length) {
    return (
      <Field label={label} hint={arg.help}>
        <select className="field" value={String(value ?? "")} onChange={(e) => onChange(e.target.value)}>
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
          type="number"
          value={String(value ?? "")}
          onChange={(e) => onChange(e.target.value === "" ? "" : Number(e.target.value))}
        />
      </Field>
    );
  }
  return (
    <Field label={label} hint={arg.help}>
      <input className="field" value={String(value ?? "")} onChange={(e) => onChange(e.target.value)} />
    </Field>
  );
}
