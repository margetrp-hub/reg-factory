# -*- coding: utf-8 -*-
"""deploy/memory_guard.py — cgroup v2 内存水位闸门。

背景（SG 实测 2026-10-31）：
  * 单个有头 Chromium 空载 ~640-840MB，打开 claude.ai 后 1.30-1.45GB；
  * 打开 chatgpt.com 后 ~0.99GB；
  * 容器上限 8GB，3 并发就吃掉 4GB+，再叠加注册流程的额外标签页很容易顶到
    memory.max 被内核 OOM kill —— 那会连带影响同机的 33 个现役容器。

做法：
  只拦「会拉起浏览器」的两个入口（POST /api/run、POST /api/authorize-outlook），
  读容器自己的 /sys/fs/cgroup/memory.{current,max}，超过阈值直接 429，
  让调度层等一轮而不是把整机拖死。停止类接口永远放行。

环境变量：
  REG_FACTORY_MEMORY_GUARD_PCT       默认 85（达到即拒绝新任务）
  REG_FACTORY_MEMORY_GUARD_DISABLE   1 关闭
"""

from __future__ import annotations

import json
import os

CGROUP_ROOT = os.environ.get("REG_FACTORY_CGROUP_ROOT", "/sys/fs/cgroup")
GUARDED = {
    ("POST", "/api/run"),
    ("POST", "/api/authorize-outlook"),
}
ALWAYS_ALLOWED_PREFIXES = ("/api/stop", "/api/stop-all", "/__rf/")
MEMORY_PATH = "/__rf/memory"


def _read_int(path: str) -> int:
    try:
        with open(path, "r", encoding="utf-8") as handle:
            raw = handle.read().strip()
        return 0 if raw == "max" else int(raw)
    except (OSError, ValueError):
        return 0


def snapshot() -> dict:
    current = _read_int(os.path.join(CGROUP_ROOT, "memory.current"))
    maximum = _read_int(os.path.join(CGROUP_ROOT, "memory.max"))
    high = _read_int(os.path.join(CGROUP_ROOT, "memory.high"))
    pct = round(current * 100.0 / maximum, 1) if maximum else 0.0
    return {
        "current_mb": current // (1024 * 1024),
        "max_mb": maximum // (1024 * 1024),
        "high_mb": high // (1024 * 1024) if high else 0,
        "used_pct": pct,
    }


class MemoryGuard:
    """在网关内侧再挡一层：内存不够就不接新任务。"""

    def __init__(self, app, *, threshold_pct: float = 85.0, enabled: bool = True):
        self.app = app
        self.threshold = max(50.0, min(99.0, float(threshold_pct)))
        self.enabled = enabled

    @staticmethod
    async def _send_json(send, status: int, payload: dict):
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        await send({
            "type": "http.response.start",
            "status": status,
            "headers": [
                (b"content-type", b"application/json; charset=utf-8"),
                (b"cache-control", b"no-store"),
            ],
        })
        await send({"type": "http.response.body", "body": body, "more_body": False})

    async def __call__(self, scope, receive, send):
        if scope.get("type") != "http" or not self.enabled:
            await self.app(scope, receive, send)
            return

        path = scope.get("path") or "/"
        method = (scope.get("method") or "GET").upper()

        if path == MEMORY_PATH:
            await self._send_json(send, 200, {"ok": True, **snapshot()})
            return

        if path.startswith(ALWAYS_ALLOWED_PREFIXES) or (method, path) not in GUARDED:
            await self.app(scope, receive, send)
            return

        info = snapshot()
        if info["max_mb"] and info["used_pct"] >= self.threshold:
            print(
                f"[guard] reject {method} {path}: memory {info['used_pct']}% "
                f"({info['current_mb']}/{info['max_mb']}MB) >= {self.threshold}%",
                flush=True,
            )
            await self._send_json(send, 429, {
                "error": "memory_guard",
                "detail": (
                    f"容器内存已用 {info['used_pct']}%（{info['current_mb']}MB/"
                    f"{info['max_mb']}MB），超过 {self.threshold}% 阈值，暂不接新任务。"
                    "等运行中的浏览器任务结束后重试，或在面板里降低并发。"
                ),
                **info,
            })
            return

        await self.app(scope, receive, send)


def guard_from_env(app):
    enabled = os.environ.get("REG_FACTORY_MEMORY_GUARD_DISABLE", "").strip().lower() not in {
        "1", "true", "yes", "on",
    }
    try:
        threshold = float(os.environ.get("REG_FACTORY_MEMORY_GUARD_PCT", "85") or 85)
    except (TypeError, ValueError):
        threshold = 85.0
    guard = MemoryGuard(app, threshold_pct=threshold, enabled=enabled)
    info = snapshot()
    if enabled and info["max_mb"]:
        print(
            f"[guard] 内存闸门开启：{info['used_pct']}% "
            f"({info['current_mb']}MB/{info['max_mb']}MB)，阈值 {guard.threshold}%",
            flush=True,
        )
    elif not enabled:
        print("[guard] 内存闸门已禁用（REG_FACTORY_MEMORY_GUARD_DISABLE=1）", flush=True)
    else:
        print("[guard] 未能读取 cgroup 内存信息，闸门降级为放行", flush=True)
    return guard
