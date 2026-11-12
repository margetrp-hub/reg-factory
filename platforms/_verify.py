"""platforms/_verify.py — 跨平台账号验活。

各平台共用一套判定逻辑，只在「打哪个端点」上做区分。
不启动浏览器，纯 HTTP，代价最小。
"""
from __future__ import annotations

from typing import Any, Callable

_NOOP = lambda _m: None  # noqa: E731

# 平台 -> (验活 URL 模板, 需要的额外 header)
ENDPOINTS = {
    "chatgpt": "https://chatgpt.com/backend-api/subscriptions?account_id={acct}",
    "claude":  "https://claude.ai/api/organizations",
    "grok":    "https://grok.com/rest/app-chat/conversations",
    "github":  "https://api.github.com/user",
    "kiro":    "https://api.kiro.dev/v1/me",
}

# 状态常量，与本轮数据库里使用的值一致
S_ALIVE = "alive"
S_NEEDS_REFRESH = "needs_refresh"
S_WS_DEAD = "workspace_deactivated"
S_BANNED = "banned"
S_UNKNOWN = "unknown"


def _classify(status: int, body: str) -> tuple[str, str]:
    """把一次探测结果归类成状态。"""
    low = (body or "").lower()

    if "deleted or deactivated" in low:
        return S_BANNED, "account deleted or deactivated"
    if "deactivated_workspace" in low:
        return S_WS_DEAD, "402 deactivated_workspace"
    if "token_revoked" in low:
        return S_NEEDS_REFRESH, "401 token_revoked"
    if "unusual activity" in low:
        return S_BANNED, "unusual activity"
    if status == 200:
        return S_ALIVE, "200"
    if status == 401:
        return S_NEEDS_REFRESH, "401"
    if status == 402:
        return S_WS_DEAD, "402"
    if status == 403:
        return S_UNKNOWN, "403 forbidden"
    return S_UNKNOWN, f"{status}"


def _at_status_of(state: str) -> str:
    """把账号层面的状态映射成 AT 层面的状态。

    两者不是一一对应：
      alive                    →  valid（AT 可用）
      needs_refresh            →  needs_refresh（AT 失效但可刷）
      workspace_deactivated    →  workspace_deactivated（AT 指向停用工作区）
      banned                   →  revoked（账号没了，AT 必然废）
      unknown                  →  unknown
    """
    return {
        S_ALIVE: "valid",
        S_NEEDS_REFRESH: "needs_refresh",
        S_WS_DEAD: "workspace_deactivated",
        S_BANNED: "revoked",
        S_UNKNOWN: "unknown",
    }.get(state, "unknown")


def verify(
    access_token: str,
    account_id: str = "",
    *,
    platform: str = "chatgpt",
    logger: Callable[[str], None] = _NOOP,
    timeout: int = 45,
    **kwargs: Any,
) -> dict[str, Any]:
    """验活。返回 {ok, status, detail, http_status}。"""
    token = str(access_token or "").strip()
    if not token:
        return {"ok": False, "platform": platform, "status": S_UNKNOWN,
                "at_status": "unknown", "error": "access_token 为空"}

    tpl = ENDPOINTS.get(platform)
    if not tpl:
        return {"ok": False, "platform": platform, "status": S_UNKNOWN,
                "at_status": "unknown",
                "error": f"平台 {platform} 暂无验活端点定义"}

    url = tpl.format(acct=account_id) if "{acct}" in tpl else tpl

    headers = {
        "Accept": "application/json",
        "Authorization": f"Bearer {token}",
        "User-Agent": ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
                       "AppleWebKit/537.36 (KHTML, like Gecko) "
                       "Chrome/136.0.0.0 Safari/537.36"),
    }
    if platform == "chatgpt" and account_id:
        headers["chatgpt-account-id"] = account_id
        headers["Referer"] = "https://chatgpt.com/"
        headers["Origin"] = "https://chatgpt.com"
    if platform == "github":
        headers["Accept"] = "application/vnd.github+json"

    try:
        from curl_cffi import requests as creq
        r = creq.get(url, headers=headers, impersonate="chrome136",
                     timeout=timeout)
    except ImportError:
        import urllib.request
        req = urllib.request.Request(url, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                body = resp.read().decode("utf-8", "ignore")
                status = resp.status
        except Exception as exc:
            return {"ok": False, "platform": platform, "status": S_UNKNOWN,
                    "error": f"{type(exc).__name__}: {exc}"}
        state, reason = _classify(status, body)
        return {"ok": state == S_ALIVE, "platform": platform, "status": state,
                "at_status": _at_status_of(state),
                "http_status": status, "reason": reason}
    except Exception as exc:
        return {"ok": False, "platform": platform, "status": S_UNKNOWN,
                "error": f"{type(exc).__name__}: {exc}"}

    body = r.text or ""
    state, reason = _classify(r.status_code, body)
    logger(f"[verify] {platform} {r.status_code} -> {state} ({reason})")

    return {
        "ok": state == S_ALIVE,
        "platform": platform,
        "status": state,
        "at_status": _at_status_of(state),
        "http_status": r.status_code,
        "reason": reason,
        "body_head": body[:200],
    }


__all__ = ["verify", "ENDPOINTS", "S_ALIVE", "S_NEEDS_REFRESH",
           "S_WS_DEAD", "S_BANNED", "S_UNKNOWN"]
