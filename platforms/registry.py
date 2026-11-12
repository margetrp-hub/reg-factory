"""platforms/registry.py — 平台注册表。

启动时导入各平台包，收集它们的 SPEC。dispatcher 通过这里查能力。
"""
from __future__ import annotations

import importlib
import threading
from typing import Any

from platforms.base import PlatformSpec, missing_steps, list_steps

_LOCK = threading.RLock()
_SPECS: dict[str, PlatformSpec] = {}
_LOADED = False

# 平台名 -> 包路径。新增平台只改这张表。
PLATFORM_MODULES = {
    "chatgpt":  "platforms.chatgpt",
    "claude":   "platforms.claude",
    "outlook":  "platforms.outlook",
    "grok":     "platforms.grok",
    "github":   "platforms.github",
    "kiro":     "platforms.kiro",
    "replit":   "platforms.replit",
}


def _load_all() -> None:
    global _LOADED
    with _LOCK:
        if _LOADED:
            return
        for name, mod_path in PLATFORM_MODULES.items():
            try:
                mod = importlib.import_module(mod_path)
            except Exception as exc:  # 单个平台坏了不影响其他平台
                _SPECS[name] = PlatformSpec(
                    name=name, label=name,
                    notes=f"导入失败: {type(exc).__name__}: {exc}",
                )
                continue
            specs = getattr(mod, "ALL", None) or []
            for spec in specs:
                _SPECS[spec.name] = spec
        _LOADED = True


def all_specs() -> dict[str, PlatformSpec]:
    _load_all()
    return dict(_SPECS)


def get_spec(name: str) -> PlatformSpec | None:
    _load_all()
    return _SPECS.get(name)


def capabilities() -> dict[str, Any]:
    """给 nodeserver 的 /node/capabilities 用的自述。"""
    _load_all()
    out = {}
    for name, spec in sorted(_SPECS.items()):
        steps = list_steps(name)
        out[name] = {
            "label": spec.label,
            "modes": list(spec.modes),
            "declared_steps": list(spec.steps),
            "implemented_steps": sorted(steps),
            "missing_steps": missing_steps(spec),
            "script_names": list(spec.script_names),
            "requires": list(spec.requires),
            "notes": spec.notes,
        }
    return out


def script_names() -> dict[str, list[str]]:
    """平台 -> 派单名，供 nodeserver 校验 script 契约。"""
    _load_all()
    return {n: list(s.script_names) for n, s in _SPECS.items() if s.script_names}


__all__ = ["all_specs", "get_spec", "capabilities", "script_names",
           "PLATFORM_MODULES"]
