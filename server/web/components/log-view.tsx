"use client";

import { useEffect, useRef, useState } from "react";
import { api, type LogEvent } from "@/lib/api";

const STREAM_COLOR: Record<string, string> = {
  stdout: "text-foreground/85",
  stderr: "text-warning",
  system: "text-brand",
};

export function LogView({ taskId }: { taskId: string }) {
  const [lines, setLines] = useState<LogEvent[]>([]);
  const [done, setDone] = useState(false);
  const [error, setError] = useState("");
  const boxRef = useRef<HTMLDivElement>(null);
  const stickRef = useRef(true);

  useEffect(() => {
    if (!taskId) return;
    let source: EventSource | null = null;
    let alive = true;
    setLines([]);
    setDone(false);
    setError("");

    api
      .logs(taskId, 400)
      .then((data) => {
        if (!alive) return;
        setLines(data.events);
        source = new EventSource(`/api/tasks/${taskId}/stream`);
        source.addEventListener("log", (event) => {
          const payload = JSON.parse((event as MessageEvent).data) as LogEvent;
          setLines((prev) => (prev.length > 4000 ? [...prev.slice(-3000), payload] : [...prev, payload]));
        });
        source.addEventListener("done", () => {
          setDone(true);
          source?.close();
        });
        source.onerror = () => {
          setError("日志流已断开（任务可能已结束或服务重启）");
          source?.close();
        };
      })
      .catch((err) => setError((err as Error).message));

    return () => {
      alive = false;
      source?.close();
    };
  }, [taskId]);

  useEffect(() => {
    const box = boxRef.current;
    if (!box || !stickRef.current) return;
    box.scrollTop = box.scrollHeight;
  }, [lines]);

  const onScroll = () => {
    const box = boxRef.current;
    if (!box) return;
    stickRef.current = box.scrollHeight - box.scrollTop - box.clientHeight < 48;
  };

  return (
    <div className="flex h-full min-h-[320px] flex-col">
      <div className="mb-2 flex items-center justify-between text-[11px] text-muted-foreground">
        <span>
          {taskId ? `日志 ${taskId}` : "未选择任务"} · {lines.length} 行
        </span>
        <span className="flex items-center gap-2">
          {!done && taskId && (
            <span className="flex items-center gap-1 text-success">
              <span className="inline-block h-1.5 w-1.5 animate-pulse rounded-full bg-success" /> 实时
            </span>
          )}
          {done && <span className="text-muted-foreground">已结束</span>}
          <button
            className="btn-ghost px-2 py-0.5 text-[11px]"
            onClick={() => {
              const text = lines.map((line) => `[${line.stream}] ${line.line}`).join("\n");
              navigator.clipboard?.writeText(text);
            }}
          >
            复制
          </button>
        </span>
      </div>
      {error && <div className="mb-2 rounded border border-destructive/30 bg-destructive/10 px-2 py-1 text-[11px] text-destructive">{error}</div>}
      <div
        ref={boxRef}
        onScroll={onScroll}
        className="min-h-0 flex-1 overflow-auto rounded-lg border border-border bg-muted/40 p-3"
      >
        {lines.length === 0 && <div className="text-[12px] text-muted-foreground">等待输出…</div>}
        {lines.map((line, index) => (
          <div key={`${index}-${line.ts}`} className={`mono-log ${STREAM_COLOR[line.stream] || "text-muted-foreground"}`}>
            {line.line}
          </div>
        ))}
      </div>
    </div>
  );
}
