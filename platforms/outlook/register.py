"""platforms/outlook/register.py — Outlook 注册。

薄包装：subprocess 调用 register_outlook_standalone.py。
批量版 outlook_reg_loop.py 留待合并。
"""
from __future__ import annotations

from typing import Any, Callable

from platforms._runner import need_browser, run_script

SCRIPT = "register_outlook_standalone.py"
_NOOP = lambda _m: None  # noqa: E731


def register(
    *,
    email: str = "",
    count: int = 0,
    extra_args: list[str] | None = None,
    logger: Callable[[str], None] = _NOOP,
    timeout: int = 3600,
    **kwargs: Any,
) -> dict[str, Any]:
    args: list[str] = []
    if email:
        args += ["--email", str(email)]
    if count:
        args += ["--count", str(count)]
    for k, v in (kwargs or {}).items():
        if v in (None, "", False):
            continue
        flag = "--" + str(k).replace("_", "-")
        if v is True:
            args.append(flag)
        else:
            args += [flag, str(v)]
    if extra_args:
        args += [str(a) for a in extra_args]

    r = run_script(SCRIPT, args, platform="outlook", step="register",
                   email=email, logger=logger, timeout=timeout,
                   env_extra=need_browser())
    out = r.as_dict()
    out["script"] = SCRIPT
    return out
