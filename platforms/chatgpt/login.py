"""platforms/chatgpt/login.py — Step 2：登录刷新 AT。

为什么需要这一步
----------------
Team 工作区停用后，账号登录会转会个人空间，plan 从
self_serve_business_prolite 变成 free，account_id 完全换掉。
这时旧 AT 指向停用的工作区（402 deactivated_workspace），
必须重新登录才能拿到指向个人空间的新 AT。

实测案例
--------
92.autumn.daubs@icloud.com
  旧 AT: plan=self_serve_business_prolite  acct=bf0caee7-304...
  新 AT: plan=free                         acct=11718952-0f4...

产物
----
{ok, email, access_token, account_id, plan, has_orgs, expires_at}
"""
from __future__ import annotations

from typing import Any, Callable

from platforms._runner import run_script

SCRIPT = "login_export.py"
_NOOP = lambda _m: None  # noqa: E731


def login(
    *,
    email: str,
    save_to: str = "",
    logger: Callable[[str], None] = _NOOP,
    timeout: int = 600,
    **kwargs: Any,
) -> dict[str, Any]:
    """登录指定账号并导出新 AT。

    需要该账号在库里有 password + totp。整个流程走 Python 浏览器，
    因为登录页有 Cloudflare 挑战，纯协议过不去。
    """
    args = ["--email", str(email)]
    if save_to:
        args += ["--save", str(save_to)]

    r = run_script(SCRIPT, args, platform="chatgpt", step="login",
                   email=email, logger=logger, timeout=timeout)
    out = r.as_dict()
    out["script"] = SCRIPT

    # 成功时把新 AT 读出来放进 detail，方便调用方直接用
    if r.ok and save_to:
        try:
            with open(save_to, encoding="utf-8") as f:
                lines = [ln.strip() for ln in f if ln.strip()]
            if len(lines) >= 2:
                out["access_token"] = lines[0]
                out["account_id"] = lines[1]
        except Exception as exc:
            out["read_error"] = f"{type(exc).__name__}: {exc}"

    return out
