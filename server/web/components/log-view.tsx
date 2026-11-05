"use client";

import { useEffect, useRef, useState } from "react";
import { api, type LogEvent } from "@/lib/api";

const STREAM_COLOR: Record<string, string> = {
  stdout: "text-[#d5dbe6]",
  stderr: "text-[#ff9d6b]",
  system: "text-[#6fb3ff]",
};

/** 终端风格实时日志：等宽、行号、自动跟随、SSE 推送。 */
export function LogView({ taskId, height = 420 }: { taskId: string; height?: number }) {
  const [lines, setLines] = useState<LogEvent[]>([]);
  const [done, setDone] = useState(false);
  const [live, setLive] = useState(false);
  const [error, setError] = useState("");
  const [follow, setFollow] = useState(true);
  const boxRef = useRef<HTMLDivElement>(null);
  const stickRef = useRef(true);

  useEffect(() => {
    if (!taskId) return;
    let source: EventSource | null = null;
    let alive = true;
    setLines([]);
    setDone(false);
    setLive(false);
    setError("");

    api
      .logs(taskId, 500)
      .then((data) => {
        if (!alive) return;
        setLines(data.events);
        source = new EventSource(`/api/tasks/${taskId}/stream`);
        source.addEventListener("open", () => setLive(true));
        source.addEventListener("log", (event) => {
          const payload = JSON.parse((event as MessageEvent).data) as LogEvent;
          setLines((prev) => (prev.length > 5000 ? [...prev.slice(-4000), payload] : [...prev, payload]));
        });
        source.addEventListener("done", () => {
          setDone(true);
          setLive(false);
          source?.close();
        });
        source.onerror = () => {
          setLive(false);
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
    const atBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 24;
    stickRef.current = atBottom;
    setFollow(atBottom);
  };

  const status = error
    ? { text: error, tone: "text-[#ff6b6b]" }
    : done
      ? { text: "已结束", tone: "text-white/40" }
      : live
        ? { text: "实时", tone: "text-[#4ade80]" }
        : { text: "连接中…", tone: "text-white/40" };

  return (
    <div className="overflow-hidden rounded-lg border border-white/10 bg-[#0b0e14]">
      <div className="flex items-center gap-3 border-b border-white/10 bg-white/[0.03] px-3 py-1.5">
        <span className="flex gap-1.5">
          <span className="h-2 w-2 rounded-full bg-[#ff5f57]" />
          <span className="h-2 w-2 rounded-full bg-[#febc2e]" />
          <span className="h-2 w-2 rounded-full bg-[#28c840]" />
        </span>
        <span className="min-w-0 flex-1 truncate font-mono text-[11px] text-white/35">{taskId}</span>
        <span className={`font-mono text-[11px] ${status.tone}`}>{status.text}</span>
        <span className="font-mono text-[11px] text-white/30">{lines.length} 行</span>
        <button
          onClick={() => setFollow((prev) => { stickRef.current = !prev; return !prev; })}
          className={`rounded px-1.5 py-0.5 font-mono text-[10px] ${
            follow ? "bg-white/10 text-white/70" : "text-white/35 hover:text-white/60"
          }`}
        >
          {follow ? "跟随中" : "已暂停"}
        </button>
        <button
          onClick={() => { setLines([]); stickRef.current = true; setFollow(true); }}
          className="rounded px-1.5 py-0.5 font-mono text-[10px] text-white/35 hover:text-white/60"
        >
          清屏
        </button>
      </div>

      <div
        ref={boxRef}
        onScroll={onScroll}
        style={{ height }}
        className="overflow-auto px-3 py-2 font-mono text-[11.5px] leading-[1.6]"
      >
        {lines.length === 0 ? (
          <div className="py-8 text-center text-[11px] text-white/25">{error || "等待输出…"}</div>
        ) : (
          lines.map((event, index) => (
            <div key={`${index}-${event.ts ?? ""}`} className="flex gap-3">
              <span className="w-[44px] shrink-0 select-none text-right text-white/20">{index + 1}</span>
              <span className={`whitespace-pre-wrap break-words ${STREAM_COLOR[event.stream] || "text-white/70"}`}>
                {event.line}
              </span>
            </div>
          ))
        )}
      </div>
    </div>
  );
}
