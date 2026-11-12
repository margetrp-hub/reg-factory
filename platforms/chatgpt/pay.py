"""platforms/chatgpt/pay.py — Step 4：支付。

按渠道分派到不同实现。当前只有 card 有 Go 实现。

渠道可用性来自 checkout 响应的 payment_method_types
实测 SG 地区给的是 [link card]，没有 paypal。
"""
from __future__ import annotations

import os
import subprocess
from typing import Any, Callable

_NOOP = lambda _m: None  # noqa: E731

_BIN = os.environ.get("REG_FACTORY_PLUSBIND", "/usr/local/bin/plusbind")

SUPPORTED_CHANNELS = ("card", "link", "paypal", "gopay")


def card(
    *,
    access_token: str,
    account_id: str,
    checkout_id: str,
    processor: str = "openai_llc",
    publishable_key: str = "",
    number: str = "",
    exp_month: str = "",
    exp_year: str = "",
    cvc: str = "",
    billing: dict[str, str] | None = None,
    logger: Callable[[str], None] = _NOOP,
    timeout: int = 300,
    **kwargs: Any,
) -> dict[str, Any]:
    """走 Stripe card 渠道绑卡。

    卡号留空时从环境变量读（PLUS_CARD_NUMBER 等），
    这样调用方不必把卡号放进代码或日志。
    """
    number = number or os.environ.get("PLUS_CARD_NUMBER", "")
    exp_month = exp_month or os.environ.get("PLUS_CARD_EXP_MONTH", "")
    exp_year = exp_year or os.environ.get("PLUS_CARD_EXP_YEAR", "")
    cvc = cvc or os.environ.get("PLUS_CARD_CVC", "")

    if not all([number, exp_month, exp_year, cvc]):
        return {"ok": False, "error": "卡信息不全（检查 PLUS_CARD_* 环境变量）"}

    if not os.path.isfile(_BIN):
        return {"ok": False, "error": f"Go 二进制不存在: {_BIN}"}

    args = [_BIN, "-token", access_token, "-account", account_id,
            "-checkout", checkout_id, "-processor", processor,
            "-number", number, "-exp_month", exp_month,
            "-exp_year", exp_year, "-cvc", cvc]
    if publishable_key:
        args += ["-pk", publishable_key]
    if billing:
        for k, v in billing.items():
            if v:
                args += [f"-{k}", str(v)]

    logger(f"[plusbind] checkout={checkout_id[:20]}... 提交卡号")
    try:
        p = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        return {"ok": False, "error": f"超时 {timeout}s"}
    except Exception as exc:
        return {"ok": False, "error": f"{type(exc).__name__}: {exc}"}

    stdout = p.stdout or ""
    err = ""
    for ln in stdout.splitlines():
        if "✗" in ln:
            err = ln.strip()

    if p.returncode != 0:
        return {"ok": False, "error": err or f"exit {p.returncode}",
                "stdout": stdout[-600:]}

    return {"ok": True, "channel": "card", "impl": "go",
            "stdout_tail": [ln for ln in stdout.splitlines() if ln.strip()][-8:]}


def pay(
    channel: str = "card",
    *,
    logger: Callable[[str], None] = _NOOP,
    **kwargs: Any,
) -> dict[str, Any]:
    """按渠道分派。"""
    ch = (channel or "card").strip().lower()
    if ch == "card":
        return card(logger=logger, **kwargs)
    if ch in ("link", "paypal", "gopay"):
        return {"ok": False,
                "error": f"渠道 {ch} 尚未实现（需要不同的前置条件）"}
    return {"ok": False, "error": f"未知渠道: {ch}，支持 {SUPPORTED_CHANNELS}"}
