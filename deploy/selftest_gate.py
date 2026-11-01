# -*- coding: utf-8 -*-
"""deploy/selftest_gate.py — 不依赖 FastAPI 的闸门自测（标准库 asyncio）。

  python deploy/selftest_gate.py

覆盖：健康检查、未认证 401/302、错误密码限流、正确登录发 Cookie、
Cookie 透传下游、Bearer / Basic 脚本凭证、登出。
"""

from __future__ import annotations

import asyncio
import base64
import json
import os
import sys
import urllib.parse

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from deploy.gate import COOKIE_NAME, PanelGate  # noqa: E402

PASS = 0
FAIL = 0


def check(name: str, condition: bool, detail: str = "") -> None:
    global PASS, FAIL
    if condition:
        PASS += 1
        print(f"  \033[32m✓\033[0m {name}")
    else:
        FAIL += 1
        print(f"  \033[31m✗\033[0m {name} {detail}")


async def downstream(scope, receive, send):
    body = json.dumps({"downstream": True, "path": scope.get("path")}).encode()
    await send({"type": "http.response.start", "status": 200,
                "headers": [(b"content-type", b"application/json")]})
    await send({"type": "http.response.body", "body": body})


async def call(app, *, method="GET", path="/", query="", headers=None, body=b"", client=("1.2.3.4", 1234)):
    scope = {
        "type": "http",
        "http_version": "1.1",
        "method": method,
        "scheme": "http",
        "path": path,
        "raw_path": (path + (("?" + query) if query else "")).encode(),
        "query_string": query.encode(),
        "root_path": "",
        "headers": [(k.lower().encode(), v.encode()) for k, v in (headers or {}).items()],
        "client": client,
        "server": ("test", 80),
    }
    sent_body = b""
    state = {"status": 0, "headers": {}}

    async def receive():
        return {"type": "http.request", "body": body, "more_body": False}

    async def send(message):
        nonlocal sent_body
        if message["type"] == "http.response.start":
            state["status"] = message["status"]
            state["headers"] = {
                k.decode().lower(): v.decode() for k, v in message.get("headers", [])
            }
        elif message["type"] == "http.response.body":
            sent_body += message.get("body", b"")

    await app(scope, receive, send)
    return state["status"], state["headers"], sent_body


async def main() -> int:
    password = "unit-test-password-very-long"
    gate = PanelGate(downstream, user="admin", password=password, max_fails=3, window=300)

    status, _, body = await call(gate, path="/__rf/health")
    check("健康检查放行", status == 200 and b'"ok":true' in body, f"status={status}")

    status, headers, body = await call(gate, path="/api/status")
    check("未认证 API 返回 401 JSON", status == 401 and b"unauthorized" in body, f"status={status}")

    status, headers, _ = await call(gate, path="/", query="")
    location = headers.get("location", "")
    check("未认证页面 302 到登录页", status == 302 and "/__rf/login" in location, f"{status} {location}")
    check("next 参数带上原路径", "next=%2F" in location or "next=/" in location, location)

    status, _, body = await call(gate, path="/__rf/login")
    check("登录页返回 HTML", status == 200 and b"<form" in body, f"status={status}")

    form = urllib.parse.urlencode({"username": "admin", "password": "wrong", "next": "/"}).encode()
    status, _, _ = await call(gate, method="POST", path="/__rf/login", body=form,
                              headers={"content-type": "application/x-www-form-urlencoded"})
    check("错误密码返回 401", status == 401, f"status={status}")

    form = urllib.parse.urlencode({"username": "admin", "password": password, "next": "/#/tasks"}).encode()
    status, headers, _ = await call(gate, method="POST", path="/__rf/login", body=form,
                                    headers={"content-type": "application/x-www-form-urlencoded"})
    cookie = headers.get("set-cookie", "")
    check("正确密码 302 并下发 Cookie", status == 302 and cookie.startswith(f"{COOKIE_NAME}="), f"{status} {cookie[:40]}")
    check("Cookie 为 HttpOnly + SameSite", "HttpOnly" in cookie and "SameSite=Lax" in cookie, cookie[:80])

    token = cookie.split(";", 1)[0]
    status, _, body = await call(gate, path="/api/status", headers={"cookie": token})
    check("带 Cookie 透传下游", status == 200 and b"downstream" in body, f"status={status}")

    status, _, body = await call(gate, path="/api/status", headers={"x-api-key": password})
    check("X-API-Key 脚本凭证", status == 200 and b"downstream" in body, f"status={status}")

    basic = "Basic " + base64.b64encode(f"admin:{password}".encode()).decode()
    status, _, body = await call(gate, path="/api/status", headers={"authorization": basic})
    check("HTTP Basic 凭证", status == 200 and b"downstream" in body, f"status={status}")

    status, _, _ = await call(gate, path="/api/status", headers={"x-api-key": "nope"})
    check("错误 API Key 拒绝", status == 401, f"status={status}")

    # 限流：连续错误密码（上限 3）
    limited = False
    for _ in range(5):
        form = urllib.parse.urlencode({"username": "admin", "password": "bad", "next": "/"}).encode()
        status, _, _ = await call(gate, method="POST", path="/__rf/login", body=form, client=("9.9.9.9", 1))
        if status == 429:
            limited = True
            break
    check("连续失败触发 429 限流", limited)

    status, headers, _ = await call(gate, path="/__rf/logout")
    check("登出清 Cookie", status == 302 and "Max-Age=0" in headers.get("set-cookie", ""))

    # HTTPS + auto cookie secure
    https_gate = PanelGate(downstream, user="admin", password=password)
    good_form = urllib.parse.urlencode({"username": "admin", "password": password, "next": "/"}).encode()
    status, headers, _ = await call(https_gate, method="POST", path="/__rf/login", body=good_form,
                                    headers={"x-forwarded-proto": "https"})
    check("HTTPS 下 Cookie 带 Secure", status == 302 and "Secure" in headers.get("set-cookie", ""),
          headers.get("set-cookie", "")[:80])

    print(f"\n通过 {PASS} 项，失败 {FAIL} 项")
    return 0 if FAIL == 0 else 1


if __name__ == "__main__":
    raise SystemExit(asyncio.run(main()))
