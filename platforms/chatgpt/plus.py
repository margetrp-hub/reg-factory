"""platforms/chatgpt/plus.py — ChatGPT Plus 直充的同进程封装。

把 vendor/chatgpt_plus 从「独立 HTTP 服务（127.0.0.1:5601）」改成
「可导入的库函数」，供 regd 的任务执行器直接调用。

设计原则
--------
1. vendor 里的代码一行不改，只做 sys.path 注入后 import
2. 配置路径用 REG_FACTORY_PLUS_CONFIG 覆盖，不依赖 vendor 目录下的 json
3. 暴露三个层次的入口：
     preflight_only()   只预检，不付款（拿 checkout_id 和金额）
     run_plus()         完整流程：预检 → 绑卡 → 支付
     check_config()     自检配置是否可用
4. 不引入任何新的全局状态；缓存由 standalone_flow 内部管理

用法
----
    from platforms.chatgpt.plus import run_plus, preflight_only

    # 只看金额，不付
    r = preflight_only(access_token=at, account_id=acct)
    if r["ok"]:
        print(r["amount"], r["currency"], r["checkout_id"])

    # 完整直充
    r = run_plus(
        access_token=at,
        account_id=acct,
        card={"number": "...", "exp_month": 11, "exp_year": 2029,
              "cvc": "252", "name": "Michael Carter"},
        billing={"line1": "1 Raffles Place", "postal_code": "048616",
                 "country": "SG", "city": "Singapore"},
        logger=print,
    )
"""
from __future__ import annotations

import os
import sys
import threading
from pathlib import Path
from typing import Any, Callable

# ---------------------------------------------------------------- vendor 注入
_APP_ROOT = Path(__file__).resolve().parents[2]
_VENDOR_DIR = _APP_ROOT / "vendor" / "chatgpt_plus"

if not _VENDOR_DIR.is_dir():
    raise ImportError(
        f"vendor/chatgpt_plus not found at {_VENDOR_DIR}. "
        "Plus 直充依赖该目录，请确认部署完整。"
    )

if str(_VENDOR_DIR) not in sys.path:
    sys.path.insert(0, str(_VENDOR_DIR))

# 配置路径：优先环境变量，否则用 vendor 目录下的 standalone_config.json
_DEFAULT_CONFIG = _VENDOR_DIR / "standalone_config.json"
_CONFIG_ENV = os.environ.get("REG_FACTORY_PLUS_CONFIG", "").strip()
if not _CONFIG_ENV and _DEFAULT_CONFIG.is_file():
    os.environ.setdefault("REG_FACTORY_PLUS_CONFIG", str(_DEFAULT_CONFIG))

_NOOP = lambda _m: None  # noqa: E731

_IMPORT_LOCK = threading.Lock()
_loaded = False
_run_flow = None
_preflight = None
_load_config = None


def _ensure_loaded():
    """延迟导入，避免在模块层面触发 vendor 的副作用。"""
    global _loaded, _run_flow, _preflight, _load_config
    if _loaded:
        return
    with _IMPORT_LOCK:
        if _loaded:
            return
        from standalone_flow import (  # type: ignore[import-not-found]
            _load_config as _lc,
            preflight as _pf,
            run_flow as _rf,
        )
        _run_flow = _rf
        _preflight = _pf
        _load_config = _lc
        _loaded = True


# ---------------------------------------------------------------- 配置自检
REQUIRED_CONFIG_KEYS = ("country", "currency", "bind_country", "bind_currency")


def check_config() -> dict[str, Any]:
    """检查 Plus 直充配置是否完整可用。

    返回
    ----
    {
      "ok": bool,
      "config_path": str,
      "missing": [key, ...],
      "warnings": [str, ...],
      "config": dict,
    }
    """
    _ensure_loaded()
    path = os.environ.get("REG_FACTORY_PLUS_CONFIG", "")
    warnings: list[str] = []

    if not path:
        return {"ok": False, "config_path": "", "missing": list(REQUIRED_CONFIG_KEYS),
                "warnings": ["REG_FACTORY_PLUS_CONFIG 未设置"], "config": {}}

    if not Path(path).is_file():
        return {"ok": False, "config_path": path, "missing": list(REQUIRED_CONFIG_KEYS),
                "warnings": [f"配置文件不存在: {path}"], "config": {}}

    try:
        cfg = dict(_load_config() or {})
    except Exception as exc:
        return {"ok": False, "config_path": path, "missing": list(REQUIRED_CONFIG_KEYS),
                "warnings": [f"配置解析失败: {exc}"], "config": {}}

    missing = [k for k in REQUIRED_CONFIG_KEYS if not str(cfg.get(k) or "").strip()]

    promo = str(cfg.get("promo_campaign") or "").strip()
    if not promo:
        warnings.append("promo_campaign 为空：将按原价建单，可能非 0 元")
    if not cfg.get("reject_nonzero", False):
        warnings.append("reject_nonzero 未开启：非 0 元单也会继续")
    if not cfg.get("billing"):
        warnings.append("billing 为空：需要调用方在 payload 里补账单地址")

    return {"ok": not missing, "config_path": path, "missing": missing,
            "warnings": warnings, "config": cfg}


# ---------------------------------------------------------------- 预检
def preflight_only(
    access_token: str,
    account_id: str,
    *,
    billing: dict[str, str] | None = None,
    promo_campaign: str | None = None,
    logger: Callable[[str], None] = _NOOP,
) -> dict[str, Any]:
    """只做预检：提链、读金额，不绑卡不付款。

    用于在真正扣款前确认「这一单是不是 0 元」。
    """
    _ensure_loaded()
    payload: dict[str, Any] = {
        "access_token": str(access_token or "").strip(),
        "account_id": str(account_id or "").strip(),
        "flow_mode": "link_only",
    }
    if billing:
        payload["billing"] = {k: str(v) for k, v in billing.items() if v}
    if promo_campaign:
        payload["promo_pool"] = [promo_campaign]

    try:
        return dict(_preflight(payload, logger) or {})
    except Exception as exc:
        return {"ok": False, "error": f"{type(exc).__name__}: {exc}"}


# ---------------------------------------------------------------- 完整直充
def run_plus(
    access_token: str,
    account_id: str,
    *,
    card: dict[str, Any],
    billing: dict[str, str] | None = None,
    promo_campaign: str | None = None,
    flow_mode: str = "full",
    logger: Callable[[str], None] = _NOOP,
) -> dict[str, Any]:
    """完整 Plus 直充：预检 → 绑卡 → 支付 → 回执。

    flow_mode
    ---------
    full        完整流程
    bind_only   只绑卡，不确认支付
    link_only   只提链

    card 必填字段由 vendor 侧校验，典型形状：
        {"number": "...", "exp_month": 11, "exp_year": 2029,
         "cvc": "252", "name": "..."}
    """
    _ensure_loaded()
    payload: dict[str, Any] = {
        "access_token": str(access_token or "").strip(),
        "account_id": str(account_id or "").strip(),
        "card": dict(card or {}),
        "flow_mode": flow_mode,
    }
    if billing:
        payload["billing"] = {k: str(v) for k, v in billing.items() if v}
    if promo_campaign:
        payload["promo_pool"] = [promo_campaign]

    try:
        return dict(_run_flow(payload, logger) or {})
    except Exception as exc:
        return {"ok": False, "error": f"{type(exc).__name__}: {exc}"}


# ---------------------------------------------------------------- 便捷判断
def checkout_id(access_token: str, account_id: str,
                *, logger: Callable[[str], None] = _NOOP) -> str:
    """只拿 checkout_id，给后续手动处理用。"""
    r = preflight_only(access_token, account_id, logger=logger)
    return str(r.get("checkout_id") or "") if r.get("ok") else ""


def is_zero_amount(amount: Any) -> bool:
    """判断预检返回的金额是否为 0。

    vendor 返回的 amount 是字符串，可能是 "0.00" / "0" / "PHP 0.00"。
    """
    text = str(amount or "").strip()
    if not text or text.lower() == "unknown":
        return False
    digits = "".join(ch for ch in text if ch.isdigit() or ch == ".")
    try:
        return float(digits or "1") == 0.0
    except ValueError:
        return False


__all__ = [
    "check_config",
    "preflight_only",
    "run_plus",
    "checkout_id",
    "is_zero_amount",
]
