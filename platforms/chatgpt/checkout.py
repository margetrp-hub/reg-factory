"""platforms/chatgpt/checkout.py — Step 3：提链。

这是唯一一步已经完整 Go 化的。Python 侧只做二进制调用。

为什么 Step 3 用 Go
-------------------
· 纯 HTTP，不需要浏览器
· 需要精确的 TLS/HTTP2 指纹（httpcloak 覆盖得比手搓好）
· 需要结构化错误（从 stdout grep 不如直接拿 JSON）

产物
----
{ok, checkout_id, processor_entity, publishable_key, amount, currency, country}

Go 二进制位置
-------------
REG_FACTORY_PLUSCHECK  环境变量，默认 /usr/local/bin/pluscheck
"""
from __future__ import annotations

import json
import os
import subprocess
from typing import Any, Callable

_NOOP = lambda _m: None  # noqa: E731

_BIN = os.environ.get("REG_FACTORY_PLUSCHECK", "/usr/local/bin/pluscheck")


def checkout(
    *,
    access_token: str,
    account_id: str,
    region: str = "",
    country: str = "",
    currency: str = "",
    promo: str = "plus-1-month-free",
    alpn: str = "h1",
    logger: Callable[[str], None] = _NOOP,
    timeout: int = 180,
    **kwargs: Any,
) -> dict[str, Any]:
    """调 Go 二进制提链，返回结构化结果。"""
    if not os.path.isfile(_BIN):
        return {"ok": False, "error": f"Go 二进制不存在: {_BIN}"}

    args = [_BIN, "-token", access_token, "-account", account_id,
            "-alpn", alpn, "-promo", promo]
    if region:
        args += ["-region", region]
    if country:
        args += ["-country", country]
    if currency:
        args += ["-currency", currency]

    logger(f"[pluscheck] region={region or country+'/'+currency} alpn={alpn}")
    try:
        p = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        return {"ok": False, "error": f"超时 {timeout}s"}
    except Exception as exc:
        return {"ok": False, "error": f"{type(exc).__name__}: {exc}"}

    stdout = p.stdout or ""
    # Go 侧把结果写在 JSON 文件里，stdout 是给人看的
    result_path = os.path.join(os.environ.get("TMPDIR", "/tmp"),
                               "pluscheck-go.json")
    parsed: dict[str, Any] = {}
    try:
        with open(result_path, encoding="utf-8") as f:
            parsed = json.load(f)
    except Exception:
        pass

    if p.returncode != 0:
        # 从 stdout 里找错误行
        err_line = ""
        for ln in stdout.splitlines():
            if "✗" in ln or "error" in ln.lower():
                err_line = ln.strip()
        return {"ok": False, "error": err_line or f"exit {p.returncode}",
                "stdout": stdout[-500:]}

    parsed["ok"] = True
    parsed["impl"] = "go"
    logger(f"[pluscheck] checkout_id={parsed.get('checkout_id')} "
           f"amount={parsed.get('amount')} {parsed.get('currency')}")
    return parsed
