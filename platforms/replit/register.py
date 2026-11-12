"""platforms/replit/register.py — Replit 注册。

逻辑目前内嵌在 register.py 的 register_replit()，尚未独立成脚本。
这里先声明接口，实现留空，dispatcher 会如实报告未实现。
"""
from __future__ import annotations

from typing import Any, Callable

_NOOP = lambda _m: None  # noqa: E731


def register(*, email: str = "", logger: Callable[[str], None] = _NOOP,
             **kwargs: Any) -> dict[str, Any]:
    return {
        "ok": False,
        "script": "",
        "error": "Replit 尚未抽出独立脚本，逻辑在 register.py 的 register_replit()",
    }
