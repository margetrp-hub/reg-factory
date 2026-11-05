"use client";

import { useEffect, useRef, useState } from "react";
import { type LogEvent } from "@/lib/api";

const STREAM_COLOR: Record<string, string> = {
  stdout: "text-[#d5dbe6]",
  stderr: "text-[#ff9d6b]",
  system: "text-[#6fb3ff]",
};

/** 终端风格实时日志：等宽、行号、自动跟随、SSE 推送。 */
export function LogView({ taskId = "", height = 420 }: { taskId?: string; height?: number }) {
  const [lines, setLines] = useState<LogEvent[]>([]);
  const [done, setDone] = useState(false);
  const [live, setLive] = useState(false);
  const [error, setError] = useState("");
  const [follow, setFollow] = useState(true);
  const [reload, setReload] = useState(0);
  const [result, setResult] = useState("");
  const boxRef = useRef<HTMLDivElement>(null);
  const stickRef = useRef(true);

  useEffect(() => {
    let lastID = 0;
    setLines([]);
    setDone(false);
    setLive(false);
    setError("");
    setResult("");
    const endpoint = taskId ? `/api/tasks/${encodeURIComponent(taskId)}/stream` : "/api/logs/stream";
    const source = new EventSource(endpoint);
    source.onopen = () => { setLive(true); setError(""); };
    source.addEventListener("log", (event) => {
      const payload = JSON.parse((event as MessageEvent).data) as LogEvent;
      if (payload.id <= lastID) return;
      lastID = payload.id;
      setLines((prev) => [...prev.slice(-4999), payload]);
    });
    source.addEventListener("done", (event) => {
      const data = JSON.parse((event as MessageEvent).data) as { status: string; exit_code: number; error?: string };
      const labels: Record<string, string> = { succeeded: "成功", failed: "失败", stopped: "已停止", interrupted: "已中断" };
      setResult(`${labels[data.status] || data.status} · 退出码 ${data.exit_code}${data.error ? ` · ${data.error}` : ""}`);
      setDone(true);
      setLive(false);
      setError("");
      source.close();
    });
    source.addEventListener("stream_error", () => setError("日志暂时不可读，正在重连…"));
    source.onerror = () => {
      setLive(false);
      setError(source.readyState === EventSource.CLOSED
        ? "无法连接日志，请检查登录状态后重连"
        : "连接中断，正在自动补读日志…");
      // EventSource sends Last-Event-ID on reconnect; keep its retry active.
    };

    return () => {
      source.close();
    };
  }, [taskId, reload]);

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
      ? { text: result || "已结束", tone: "text-white/60" }
      : live
        ? { text: "实时", tone: "text-[#4ade80]" }
        : { text: "连接中…", tone: "text-white/40" };

  return (
    <div className="overflow-hidden rounded-lg border border-white/10 bg-[#0b0e14]">
      <div className="flex flex-wrap items-center gap-2 border-b border-white/10 bg-white/[0.03] px-3 py-1.5 [&_button]:shrink-0 [&_button]:whitespace-nowrap [&_a]:shrink-0 [&_a]:whitespace-nowrap">
        <span className="flex gap-1.5">
          <span className="h-2 w-2 rounded-full bg-[#ff5f57]" />
          <span className="h-2 w-2 rounded-full bg-[#febc2e]" />
          <span className="h-2 w-2 rounded-full bg-[#28c840]" />
        </span>
        <span className="min-w-0 flex-1 truncate font-mono text-[11px] text-white/35">{taskId || "系统与全部任务"}</span>
        <span title={status.text} className={`order-last w-full break-words font-mono text-[11px] ${status.tone}`}>{status.text}</span>
        <span className="shrink-0 whitespace-nowrap font-mono text-[11px] text-white/30">{lines.length} 行</span>
        <a className="font-mono text-[10px] text-white/60" href={`${taskId ? `/api/tasks/${encodeURIComponent(taskId)}/logs` : "/api/logs"}?download=1`}>下载</a>
        <button
          onClick={() => setFollow((prev) => { stickRef.current = !prev; return !prev; })}
          className={`rounded px-1.5 py-0.5 font-mono text-[10px] ${
            follow ? "bg-white/10 text-white/70" : "text-white/35 hover:text-white/60"
          }`}
        >
          {follow ? "跟随中" : "已暂停"}
        </button>
        <button
          onClick={() => setReload((value) => value + 1)}
          className="rounded px-1.5 py-0.5 font-mono text-[10px] text-white/35 hover:text-white/60"
        >
          重连
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
          <div className="py-8 text-center text-[11px] text-white/25">{error || (done ? "此任务没有输出" : "等待输出…")}</div>
        ) : (
          lines.map((event, index) => (
            <div key={event.id} className="flex gap-3">
              <span className="w-[44px] shrink-0 select-none text-right text-white/20">{index + 1}</span>
              <span title={event.ts} className="shrink-0 select-none text-white/25">{new Date(event.ts).toLocaleTimeString("zh-CN", {hour12:false})}</span>
              {!taskId && <span className="w-[100px] shrink-0 truncate text-white/40" title={event.task_id}>{event.task_id || "服务"}</span>}
              <span className={`min-w-0 whitespace-pre-wrap break-all ${STREAM_COLOR[event.stream] || "text-white/70"}`}>
                {event.line}
              </span>
            </div>
          ))
        )}
      </div>
    </div>
  );
}
