"""platforms/dispatcher.py — 统一调度入口。

对外只暴露 run(platform, step, **kwargs)，内部按 registry 找实现。
保持 nodeserver 的 script 名不变的前提下，把调用收敛到一个入口。
"""
from __future__ import annotations

from typing import Any, Callable

from platforms.base import StepResult, get_step, missing_steps
from platforms.registry import all_specs, get_spec

_NOOP = lambda _m: None  # noqa: E731


def run(
    platform: str,
    step: str,
    *,
    logger: Callable[[str], None] = _NOOP,
    **kwargs: Any,
) -> StepResult:
    """执行某平台的一个步骤。"""
    spec = get_spec(platform)
    if spec is None:
        return StepResult(ok=False, platform=platform, step=step,
                          error=f"未知平台: {platform}")

    if step not in spec.steps:
        return StepResult(
            ok=False, platform=platform, step=step,
            error=f"{platform} 未声明 {step} 步骤（已声明: {list(spec.steps)}）")

    handler = get_step(platform, step)
    if handler is None:
        return StepResult(
            ok=False, platform=platform, step=step,
            error=f"{platform}.{step} 尚未实现挂载")

    try:
        raw = handler(logger=logger, **kwargs)
    except Exception as exc:
        return StepResult(ok=False, platform=platform, step=step,
                          error=f"{type(exc).__name__}: {exc}")

    if isinstance(raw, StepResult):
        return raw
    if isinstance(raw, dict):
        return StepResult(
            ok=bool(raw.get("ok")),
            platform=platform, step=step,
            email=str(raw.get("email") or ""),
            detail=raw,
            error=str(raw.get("error") or ""),
        )
    return StepResult(ok=True, platform=platform, step=step,
                      detail={"result": raw})


def health() -> dict[str, Any]:
    """各平台可用性一览，给自检用。"""
    out: dict[str, Any] = {}
    for name, spec in sorted(all_specs().items()):
        miss = missing_steps(spec)
        out[name] = {
            "ok": not miss,
            "declared": list(spec.steps),
            "missing": miss,
        }
    return out


__all__ = ["run", "health"]
