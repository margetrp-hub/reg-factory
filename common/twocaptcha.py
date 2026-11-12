# -*- coding: utf-8 -*-
"""common/twocaptcha.py — 2captcha 打码服务 adapter

为什么单独写
-----------
项目里已有 CapSolver / EZCaptcha 的 solver，但没有 2captcha。
2captcha 的 API 更简单（纯 HTTP in.php / res.php），而且是主流的
FunCaptcha（Arkose）和 PressAndHold（PerimeterX）供应商。

支持的 captcha 类型
------------------
  funcaptcha     Arkose Labs / FunCaptcha —— Microsoft 注册页 enforcementFrame
  hcaptcha       hCaptcha
  recaptcha_v2   Google reCAPTCHA v2
  pressandhold   PerimeterX 按住不放（Outlook 注册也会遇到）
  turnstile      Cloudflare Turnstile

API
---
  POST https://2captcha.com/in.php
    key, method, ...参数, json=1
    → {"status":1,"request":"<taskid>"}

  GET  https://2captcha.com/res.php?key=&action=get&id=&json=1
    → {"status":1,"request":"<token>"}   成功
    → {"status":0,"request":"CAPCHA_NOT_READY"}  还没好
    → {"status":0,"request":"ERROR_XXX"}         失败

用法
----
    from common.twocaptcha import solve_funcaptcha, solve_pressandhold

    token = solve_funcaptcha(
        sitekey="B7D8911C-5CC8-A9A3-35B0-554ACEE604DA",
        pageurl="https://signup.live.com/signup?lic=1",
        surl="https://arkoselabs.com",
    )
"""
from __future__ import annotations

import base64
import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request

IN_URL = "https://2captcha.com/in.php"
RES_URL = "https://2captcha.com/res.php"

# 常见的「还没好」响应
PENDING = {"CAPCHA_NOT_READY", "CAPTCHA_NOT_READY"}

# 常见的致命错误
FATAL_PREFIX = ("ERROR_",)


class TwoCaptchaError(RuntimeError):
    """打码失败。"""


def _key() -> str:
    k = (os.environ.get("TWOCAPTCHA_API_KEY") or "").strip()
    if not k:
        raise TwoCaptchaError("TWOCAPTCHA_API_KEY 未配置")
    if len(k) != 32:
        raise TwoCaptchaError(
            f"TWOCAPTCHA_API_KEY 长度应为 32，实际 {len(k)}")
    return k


def _post(url: str, data: dict, timeout: float = 40.0) -> dict:
    body = urllib.parse.urlencode(data).encode()
    req = urllib.request.Request(url, data=body, method="POST")
    req.add_header("Content-Type", "application/x-www-form-urlencoded")
    with urllib.request.urlopen(req, timeout=timeout) as r:
        raw = r.read().decode("utf-8", "replace")
    try:
        return json.loads(raw)
    except Exception:
        raise TwoCaptchaError(f"响应不是 JSON: {raw[:200]}")


def _get(url: str, timeout: float = 40.0) -> dict:
    with urllib.request.urlopen(url, timeout=timeout) as r:
        raw = r.read().decode("utf-8", "replace")
    try:
        return json.loads(raw)
    except Exception:
        raise TwoCaptchaError(f"响应不是 JSON: {raw[:200]}")


def balance() -> float:
    """查余额。"""
    q = urllib.parse.urlencode({
        "key": _key(), "action": "getbalance", "json": 1})
    d = _get(f"{RES_URL}?{q}")
    if d.get("status") != 1:
        raise TwoCaptchaError(f"查余额失败: {d}")
    return float(d.get("request", 0))


def _create(payload: dict) -> str:
    """提交任务，返回 task id。"""
    data = dict(payload)
    data["key"] = _key()
    data["json"] = 1
    d = _post(IN_URL, data)
    if d.get("status") != 1:
        raise TwoCaptchaError(f"提交任务失败: {d.get('request')}")
    return str(d["request"])


def _poll(task_id: str, *, timeout: float = 180.0,
          interval: float = 5.0, verbose: bool = True) -> str:
    """轮询直到拿到 token。"""
    deadline = time.time() + timeout
    key = _key()
    while time.time() < deadline:
        time.sleep(interval)
        q = urllib.parse.urlencode({
            "key": key, "action": "get", "id": task_id, "json": 1})
        d = _get(f"{RES_URL}?{q}")
        req = str(d.get("request", ""))

        if d.get("status") == 1:
            if verbose:
                print(f"  [2captcha] ✓ 解出 ({len(req)} 字符)")
            return req
        if req in PENDING:
            if verbose:
                left = int(deadline - time.time())
                print(f"  [2captcha] 等待中… 剩余 {left}s")
            continue
        if req.startswith(FATAL_PREFIX):
            raise TwoCaptchaError(f"解打码失败: {req}")
        if verbose:
            print(f"  [2captcha] 未知响应: {d}")

    raise TwoCaptchaError(f"超时 {timeout}s 未解出")


# ================================================================ 各类型

def solve_funcaptcha(sitekey: str, pageurl: str, *, surl: str = "",
                     data: dict | None = None, timeout: float = 240.0,
                     verbose: bool = True) -> str:
    """Arkose Labs / FunCaptcha。

    Microsoft 注册页的 enforcementFrame 就是这个。

    参数
      sitekey   Arkose public key（pages 里的 data-pkey）
      pageurl   触发页 URL
      surl      Arkose 的服务域名（页面 JS 里的 surl 变量），
                常见 https://arkoselabs.com / https://client-api.arkoselabs.com
      data      Arkose 的 blob（可选，老版本需要）
    """
    payload = {
        "method": "funcaptcha",
        "publickey": sitekey,
        "pageurl": pageurl,
    }
    if surl:
        payload["surl"] = surl
    if data:
        payload["data"] = json.dumps(data) if isinstance(data, dict) else str(data)

    if verbose:
        print(f"  [2captcha] 提交 funcaptcha  sitekey={sitekey[:20]}…")
    tid = _create(payload)
    if verbose:
        print(f"  [2captcha] task={tid}")
    return _poll(tid, timeout=timeout, verbose=verbose)


def solve_pressandhold(pageurl: str, *, body: str = "",
                       app_id: str = "PXzC5j78di",
                       timeout: float = 180.0, verbose: bool = True) -> str:
    """PerimeterX 按住不放。

    Outlook 注册页有时会出现这个（"press and hold"）。
    body 是页面里 `window._pxParam1` 或 PX 的 JSON 配置，2captcha 需要它。
    """
    payload = {
        "method": "px",
        "pageurl": pageurl,
        "appid": app_id,
    }
    if body:
        payload["body"] = body

    if verbose:
        print(f"  [2captcha] 提交 pressandhold  appid={app_id}")
    tid = _create(payload)
    if verbose:
        print(f"  [2captcha] task={tid}")
    return _poll(tid, timeout=timeout, verbose=verbose)


def solve_hcaptcha(sitekey: str, pageurl: str, *, timeout: float = 180.0,
                   verbose: bool = True) -> str:
    """hCaptcha。"""
    payload = {
        "method": "hcaptcha",
        "sitekey": sitekey,
        "pageurl": pageurl,
    }
    tid = _create(payload)
    if verbose:
        print(f"  [2captcha] hcaptcha task={tid}")
    return _poll(tid, timeout=timeout, verbose=verbose)


def solve_recaptcha_v2(sitekey: str, pageurl: str, *, timeout: float = 180.0,
                       verbose: bool = True) -> str:
    """reCAPTCHA v2。"""
    payload = {
        "method": "userrecaptcha",
        "googlekey": sitekey,
        "pageurl": pageurl,
    }
    tid = _create(payload)
    if verbose:
        print(f"  [2captcha] recaptcha_v2 task={tid}")
    return _poll(tid, timeout=timeout, verbose=verbose)


def solve_turnstile(sitekey: str, pageurl: str, *, action: str = "",
                    cdata: str = "", timeout: float = 180.0,
                    verbose: bool = True) -> str:
    """Cloudflare Turnstile。"""
    payload = {
        "method": "turnstile",
        "sitekey": sitekey,
        "pageurl": pageurl,
    }
    if action:
        payload["action"] = action
    if cdata:
        payload["data"] = cdata
    tid = _create(payload)
    if verbose:
        print(f"  [2captcha] turnstile task={tid}")
    return _poll(tid, timeout=timeout, verbose=verbose)


def solve_image(image_bytes: bytes, *, timeout: float = 120.0,
                verbose: bool = True) -> str:
    """普通图片验证码。"""
    b64 = base64.b64encode(image_bytes).decode()
    payload = {
        "method": "base64",
        "body": b64,
    }
    tid = _create(payload)
    if verbose:
        print(f"  [2captcha] image task={tid}")
    return _poll(tid, timeout=timeout, verbose=verbose)


__all__ = [
    "TwoCaptchaError", "balance",
    "solve_funcaptcha", "solve_pressandhold",
    "solve_hcaptcha", "solve_recaptcha_v2",
    "solve_turnstile", "solve_image",
]
