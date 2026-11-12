"""platforms/claude/register.py — Claude 注册。

薄包装：subprocess 调用 register.py，用 --protocol 切换执行方式。

register.py 是 Claude 的统一入口：
  --protocol browser  浏览器流程（默认）
  --protocol http     协议流程

不 import 原脚本（7291 行，import 时有副作用），保持命令行调用。
"""
from __future__ import annotations

from typing import Any, Callable

from platforms._runner import need_browser, run_script

SCRIPT = "register.py"
_NOOP = lambda _m: None  # noqa: E731


def register(
    *,
    email: str = "",
    password: str = "",
    protocol: str = "browser",
    node: str = "auto",
    timeout: int = 3600,
    extra_args: list[str] | None = None,
    logger: Callable[[str], None] = _NOOP,
    **kwargs: Any,
) -> dict[str, Any]:
    """调 register.py 注册 Claude 账号。

    protocol  browser / http
    node      Clash 节点名，claude.com 有区域封锁，默认 auto
    """
    args: list[str] = [
        "--count", "1",
        "--concurrency", "1",
        "--timeout", str(timeout),
        "--protocol", str(protocol or "browser"),
        "--node", str(node or "auto"),
    ]
    if email:
        args += ["--email", str(email)]
    if password:
        args += ["--password", str(password)]

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

    r = run_script(SCRIPT, args, platform="claude", step="register",
                   email=email, logger=logger, timeout=timeout,
                   env_extra=need_browser())
    out = r.as_dict()
    out["script"] = SCRIPT
    out["protocol"] = protocol
    out["node"] = node
    return out
