"""platforms/_runner.py — 子进程执行原脚本的统一入口。

所有薄包装共用这套逻辑：拼命令、跑、收日志、返回 StepResult。
不 import 原脚本，避免模块级副作用（很多脚本在 import 时就读配置、起浏览器）。
"""
from __future__ import annotations

import os
import shutil
import subprocess
import sys
import time
from pathlib import Path
from typing import Any, Callable

from platforms.base import StepResult

APP_ROOT = Path("/app")
_NOOP = lambda _m: None  # noqa: E731


def script_path(name: str) -> Path:
    return APP_ROOT / name


def run_script(
    script: str,
    args: list[str],
    *,
    platform: str = "",
    step: str = "",
    email: str = "",
    logger: Callable[[str], None] = _NOOP,
    timeout: int = 3600,
    env_extra: dict[str, str] | None = None,
) -> StepResult:
    """跑一个 /app 下的 Python 脚本，返回 StepResult。"""
    path = script_path(script)
    if not path.is_file():
        return StepResult(ok=False, platform=platform, step=step, email=email,
                          error=f"脚本不存在: {path}")

    cmd = [sys.executable, "-u", str(path)] + [str(a) for a in args]
    env = os.environ.copy()
    env.setdefault("PYTHONUNBUFFERED", "1")
    if env_extra:
        env.update({k: str(v) for k, v in env_extra.items()})

    logger(f"[runner] {platform}.{step} -> {' '.join(cmd)}")
    started = time.time()
    try:
        proc = subprocess.run(
            cmd, cwd=str(APP_ROOT), env=env,
            capture_output=True, text=True, timeout=timeout,
        )
    except subprocess.TimeoutExpired:
        return StepResult(ok=False, platform=platform, step=step, email=email,
                          error=f"超时 {timeout}s")
    except Exception as exc:
        return StepResult(ok=False, platform=platform, step=step, email=email,
                          error=f"{type(exc).__name__}: {exc}")

    elapsed = round(time.time() - started, 1)
    stdout = proc.stdout or ""
    stderr = proc.stderr or ""

    # 把最后若干行日志透给调用方
    tail = [ln for ln in stdout.splitlines() if ln.strip()][-8:]
    for ln in tail:
        logger(f"[{script}] {ln[:160]}")

    ok = proc.returncode == 0
    detail: dict[str, Any] = {
        "script": script,
        "args": args,
        "returncode": proc.returncode,
        "elapsed": elapsed,
        "stdout_tail": tail,
    }
    if not ok:
        detail["stderr_tail"] = [ln for ln in stderr.splitlines() if ln.strip()][-8:]

    return StepResult(ok=ok, platform=platform, step=step, email=email,
                      detail=detail,
                      error="" if ok else f"exit {proc.returncode}: {stderr[-200:]}")


def need_browser(cls: str = "") -> dict[str, str]:
    """有头浏览器需要 DISPLAY，容器内 entrypoint 已起 Xvfb。"""
    env = {}
    disp = os.environ.get("DISPLAY")
    if disp:
        env["DISPLAY"] = disp
    if cls:
        env["FINGERPRINT_BROWSER"] = cls
    return env


__all__ = ["run_script", "script_path", "need_browser", "APP_ROOT"]
