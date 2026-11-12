"""platforms/grok/register.py — Grok 注册（协议版）。浏览器版 register_grok.py，待条件切换。

薄包装：subprocess 调用 register_grok_http.py，不改原文件。
参数透传方式见 --email / --count。
"""
from __future__ import annotations

from typing import Any, Callable

from platforms._runner import need_browser, run_script

SCRIPT = "register_grok_http.py"
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
    """调 register_grok_http.py 注册账号。

    kwargs 里的其他键会以 --key value 形式透传，便于原脚本扩展参数。
    """
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

    r = run_script(SCRIPT, args, platform="grok", step="register",
                   email=email, logger=logger, timeout=timeout,
                   env_extra=need_browser())
    out = r.as_dict()
    out["script"] = SCRIPT
    return out
