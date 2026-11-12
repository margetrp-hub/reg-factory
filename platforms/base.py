"""platforms/base.py — 平台统一接口。

每个平台用 PlatformSpec 描述自己的能力，实际执行逻辑仍在原有脚本里，
platforms/ 只做注册、路由和薄适配。
"""
from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Callable


@dataclass(frozen=True)
class PlatformSpec:
    """一个平台的静态描述。"""

    name: str
    """平台标识，与 accounts.platform 字段一致：chatgpt / claude / grok ..."""

    label: str
    """显示名。"""

    modes: tuple[str, ...] = ("browser",)
    """支持的执行方式：browser / http / batch"""

    steps: tuple[str, ...] = ("register",)
    """可组合的阶段：register / verify / plus / unlock"""

    script_names: tuple[str, ...] = ()
    """nodeserver 通过 tasks.script 派单时用的名字。

    这是契约，不能改。改名等于让编排侧找不到任务。
    """

    requires: tuple[str, ...] = ()
    """运行前置条件：email / proxy / captcha / card"""

    notes: str = ""
    """补充说明，写进 capabilities 自述。"""

    def as_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "label": self.label,
            "modes": list(self.modes),
            "steps": list(self.steps),
            "script_names": list(self.script_names),
            "requires": list(self.requires),
            "notes": self.notes,
        }


@dataclass
class StepResult:
    """一步执行的结果。

    统一形状，便于上层聚合与回执。
    """

    ok: bool
    step: str = ""
    """register / verify / plus / unlock"""

    platform: str = ""
    email: str = ""
    detail: dict[str, Any] = field(default_factory=dict)
    error: str = ""

    def as_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {"ok": self.ok, "step": self.step,
                               "platform": self.platform}
        if self.email:
            out["email"] = self.email
        if self.detail:
            out["detail"] = dict(self.detail)
        if self.error:
            out["error"] = self.error
        return out


# 步骤注册表：由各平台包在 import 时填充
_STEP_HANDLERS: dict[str, dict[str, Callable[..., Any]]] = {}


def register_step(platform: str, step: str, handler: Callable[..., Any]) -> None:
    """把一个步骤的实现挂到平台上。"""
    _STEP_HANDLERS.setdefault(platform, {})[step] = handler


def get_step(platform: str, step: str) -> Callable[..., Any] | None:
    return _STEP_HANDLERS.get(platform, {}).get(step)


def list_steps(platform: str) -> dict[str, Callable[..., Any]]:
    return dict(_STEP_HANDLERS.get(platform, {}))


def missing_steps(spec: PlatformSpec) -> list[str]:
    """列出 spec 声明了但还没实现挂上的步骤。"""
    live = list_steps(spec.name)
    return [s for s in spec.steps if s not in live]


__all__ = [
    "PlatformSpec",
    "StepResult",
    "register_step",
    "get_step",
    "list_steps",
    "missing_steps",
]
