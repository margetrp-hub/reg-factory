# -*- coding: utf-8 -*-
"""deploy/gate.py — 面板公网访问闸门（纯 ASGI 中间件，零第三方依赖）。

webui/server.py 明确假设“只绑 127.0.0.1，绝不监听公网”，因为面板可以读写
.env（含全部服务密钥）并直接拉起注册任务。服务器部署时它在容器内监听
0.0.0.0，所以必须在它前面加一道鉴权，否则等于把密钥编辑器和任务执行器
直接暴露给互联网。

特性：
  * 三套凭证：Cookie 会话（浏览器）、HTTP Basic、Bearer / X-API-Key（脚本）
  * 登录页自带内联样式，不依赖放行任何静态路径
  * /api/* 未认证返回 401 JSON；页面请求 302 到登录页
  * 按 IP 的登录失败限流（内存计数，单进程 uvicorn 足够）
  * 健康检查 /__rf/health 免认证，供 Docker/负载均衡探活
  * 审计：失败尝试、登录成功、登出写入容器日志

环境变量：
  REG_FACTORY_PANEL_USER        默认 admin
  REG_FACTORY_PANEL_PASSWORD    必填（除非显式 REG_FACTORY_ALLOW_ANONYMOUS=1）
  REG_FACTORY_ALLOW_ANONYMOUS   1/true 时关闭鉴权（仅限内网自用）
  REG_FACTORY_COOKIE_SECURE     auto（默认，按协议判断）/ 1 / 0
  REG_FACTORY_SESSION_TTL       会话有效期秒数，默认 7 天
  REG_FACTORY_LOGIN_MAX_FAILS   单 IP 失败上限，默认 10
  REG_FACTORY_LOGIN_WINDOW      失败计数窗口秒数，默认 300
"""

from __future__ import annotations

import base64
import hashlib
import hmac
import html
import json
import os
import time
from urllib.parse import parse_qs, quote, urlparse

COOKIE_NAME = "rf_panel"
LOGIN_PATH = "/__rf/login"
LOGOUT_PATH = "/__rf/logout"
HEALTH_PATH = "/__rf/health"
MEMORY_PATH = "/__rf/memory"
PUBLIC_PATHS = {HEALTH_PATH, MEMORY_PATH}
PUBLIC_PREFIXES = ("/static/", "/assets/", "/favicon.ico")
API_PREFIX = "/api/"

_JSON_401 = json.dumps({"error": "unauthorized", "detail": "panel login required"}).encode("utf-8")
_JSON_429 = json.dumps({"error": "too_many_attempts", "detail": "login rate limited"}).encode("utf-8")


def _truthy(value: str | None, default: bool = False) -> bool:
    if value is None or str(value).strip() == "":
        return default
    return str(value).strip().lower() in {"1", "true", "yes", "on"}


class PanelGate:
    """鉴权 + 登录页 + 健康检查，包在 FastAPI app 外面。"""

    def __init__(
        self,
        app,
        *,
        user: str,
        password: str,
        enabled: bool = True,
        cookie_secure: str = "auto",
        session_ttl: int = 7 * 24 * 3600,
        max_fails: int = 10,
        window: int = 300,
        trust_proxy: bool = True,
    ):
        self.app = app
        self.user = (user or "admin").strip()
        self.password = password or ""
        self.enabled = bool(enabled and self.password)
        self.cookie_secure = (cookie_secure or "auto").strip().lower()
        self.session_ttl = max(60, int(session_ttl))
        self.max_fails = max(1, int(max_fails))
        self.window = max(30, int(window))
        self.trust_proxy = bool(trust_proxy)
        self._fails: dict[str, list[float]] = {}
        # 会话值 = HMAC(密码派生 key, 用户名)，改密码即全部失效，无需落盘。
        self._session = hmac.new(
            (self.password or "anonymous").encode("utf-8"),
            f"reg-factory-panel:{self.user}".encode("utf-8"),
            hashlib.sha256,
        ).hexdigest()
        self._expected = {
            "basic": "Basic " + base64.b64encode(f"{self.user}:{self.password}".encode("utf-8")).decode("ascii"),
            "bearer": self.password,
        }

    # ------------------------------------------------------------ 工具
    @staticmethod
    def _headers(scope) -> dict[str, str]:
        return {
            k.decode("latin-1").lower(): v.decode("latin-1")
            for k, v in (scope.get("headers") or [])
        }

    @staticmethod
    def _cookies(headers: dict[str, str]) -> dict[str, str]:
        jar: dict[str, str] = {}
        for part in (headers.get("cookie") or "").split(";"):
            if "=" in part:
                key, _, value = part.partition("=")
                jar[key.strip()] = value.strip()
        return jar

    def _client_ip(self, scope) -> str:
        headers = self._headers(scope)
        if self.trust_proxy:
            forwarded = headers.get("x-forwarded-for") or headers.get("x-real-ip")
            if forwarded:
                return forwarded.split(",")[0].strip()
        client = scope.get("client")
        return (client[0] if client else "") or "unknown"

    def _is_https(self, scope) -> bool:
        if scope.get("scheme") == "https":
            return True
        headers = self._headers(scope)
        return (headers.get("x-forwarded-proto") or "").split(",")[0].strip().lower() == "https"

    def _secure_cookie(self, scope) -> bool:
        if self.cookie_secure in {"1", "true", "yes", "on"}:
            return True
        if self.cookie_secure in {"0", "false", "no", "off"}:
            return False
        return self._is_https(scope)

    def _authorized(self, scope) -> bool:
        headers = self._headers(scope)
        authorization = headers.get("authorization") or ""
        if authorization.startswith("Basic ") and hmac.compare_digest(authorization, self._expected["basic"]):
            return True
        token = ""
        if authorization.lower().startswith("bearer "):
            token = authorization[7:].strip()
        token = token or (headers.get("x-api-key") or "").strip()
        if token and hmac.compare_digest(token, self._expected["bearer"]):
            return True
        cookie = self._cookies(headers).get(COOKIE_NAME) or ""
        return bool(cookie) and hmac.compare_digest(cookie, self._session)

    def _rate_limited(self, ip: str) -> bool:
        now = time.time()
        bucket = [t for t in self._fails.get(ip, []) if now - t < self.window]
        self._fails[ip] = bucket
        return len(bucket) >= self.max_fails

    def _record_fail(self, ip: str) -> None:
        self._fails.setdefault(ip, []).append(time.time())
        if len(self._fails) > 4096:  # 防御性清理，避免长期运行膨胀
            now = time.time()
            self._fails = {
                key: [t for t in values if now - t < self.window]
                for key, values in self._fails.items()
                if any(now - t < self.window for t in values)
            }

    # ------------------------------------------------------------ 响应
    @staticmethod
    async def _send(send, status: int, body: bytes, headers: list[tuple[bytes, bytes]]):
        await send({
            "type": "http.response.start",
            "status": status,
            "headers": [(b"content-type", b"text/plain; charset=utf-8"), *headers],
        })
        await send({"type": "http.response.body", "body": body, "more_body": False})

    async def _send_json(self, send, status: int, body: bytes):
        await self._send(send, status, body, [
            (b"content-type", b"application/json; charset=utf-8"),
            (b"cache-control", b"no-store"),
        ])

    async def _send_redirect(self, send, location: str, extra: list[tuple[bytes, bytes]] | None = None):
        await self._send(send, 302, b"", [
            (b"location", location.encode("latin-1")),
            (b"cache-control", b"no-store"),
            *(extra or []),
        ])

    async def _send_login_page(self, send, scope, *, error: str = "", status: int = 200):
        headers = self._headers(scope)
        query = parse_qs(urlparse(scope.get("raw_path", b"").decode("latin-1")).query or "")
        nxt = (query.get("next") or [scope.get("raw_path", b"/").decode("latin-1") or "/"])[0]
        nxt = nxt if nxt.startswith("/") and not nxt.startswith("//") else "/"
        banner = f'<p class="err">{html.escape(error)}</p>' if error else ""
        body = f"""<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>reg-factory 控制台登录</title>
<style>
:root{{color-scheme:dark}}
*{{box-sizing:border-box}}
body{{margin:0;min-height:100vh;display:grid;place-items:center;background:
radial-gradient(1200px 600px at 20% -10%,#1b2a4a 0%,transparent 60%),#0b0f17;
font:15px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI","Noto Sans SC",sans-serif;color:#e6edf3}}
.card{{width:min(380px,92vw);padding:32px 28px 24px;border-radius:16px;
background:#111827cc;border:1px solid #ffffff14;box-shadow:0 24px 64px #0008;backdrop-filter:blur(8px)}}
h1{{margin:0 0 4px;font-size:20px;letter-spacing:.02em}}
.sub{{margin:0 0 22px;color:#8b98a9;font-size:13px}}
label{{display:block;margin:14px 0 6px;font-size:12px;color:#9aa7b8;letter-spacing:.04em}}
input{{width:100%;padding:11px 12px;border-radius:10px;border:1px solid #ffffff1f;background:#0b1220;
color:#e6edf3;font-size:14px;outline:none}}
input:focus{{border-color:#4c8dff66;box-shadow:0 0 0 3px #4c8dff22}}
button{{width:100%;margin-top:22px;padding:11px;border:0;border-radius:10px;cursor:pointer;
background:linear-gradient(180deg,#4c8dff,#2f6fe0);color:#fff;font-size:15px;font-weight:600}}
button:hover{{filter:brightness(1.06)}}
.err{{margin:0 0 10px;padding:9px 12px;border-radius:9px;background:#ff4d4f1a;color:#ff9c9e;font-size:13px}}
.foot{{margin:18px 0 0;color:#5f6b7c;font-size:11px;text-align:center}}
</style></head><body>
<form class="card" method="post" action="{LOGIN_PATH}">
  <h1>reg-factory</h1>
  <p class="sub">服务器控制台 · 需要登录后访问</p>
  {banner}
  <input type="hidden" name="next" value="{html.escape(nxt)}">
  <label for="u">账号</label>
  <input id="u" name="username" autocomplete="username" value="{html.escape(self.user)}" required>
  <label for="p">密码</label>
  <input id="p" name="password" type="password" autocomplete="current-password" required autofocus>
  <button type="submit">进入控制台</button>
  <p class="foot">访问密钥保存在服务器 /data/.env 与 /data/runtime/asset-api-key.txt</p>
</form></body></html>""".encode("utf-8")
        await send({
            "type": "http.response.start",
            "status": status,
            "headers": [
                (b"content-type", b"text/html; charset=utf-8"),
                (b"cache-control", b"no-store"),
                (b"x-frame-options", b"DENY"),
                (b"x-content-type-options", b"nosniff"),
            ],
        })
        await send({"type": "http.response.body", "body": body, "more_body": False})

    # ------------------------------------------------------------ 路由
    async def __call__(self, scope, receive, send):
        if scope.get("type") != "http":
            await self.app(scope, receive, send)
            return

        path = scope.get("path") or "/"
        if path == HEALTH_PATH:
            await self._send_json(send, 200, b'{"ok":true,"gate":"panel"}')
            return
        if path == MEMORY_PATH:
            # 内存水位由 memory_guard 回答，闸门只负责免认证放行
            await self.app(scope, receive, send)
            return
        if not self.enabled:
            await self.app(scope, receive, send)
            return
        if path.startswith(PUBLIC_PREFIXES):
            await self.app(scope, receive, send)
            return

        ip = self._client_ip(scope)

        if path == LOGIN_PATH:
            await self._handle_login(scope, receive, send, ip)
            return
        if path == LOGOUT_PATH:
            cookie = f"{COOKIE_NAME}=; Path=/; Max-Age=0; HttpOnly; SameSite=Lax"
            await self._send_redirect(send, f"{LOGIN_PATH}", [(b"set-cookie", cookie.encode("latin-1"))])
            return

        if self._authorized(scope):
            await self.app(scope, receive, send)
            return

        if path.startswith(API_PREFIX):
            await self._send_json(send, 401, _JSON_401)
            return
        target = path
        if scope.get("query_string"):
            target += "?" + scope["query_string"].decode("latin-1")
        await self._send_redirect(send, f"{LOGIN_PATH}?next={quote(target, safe='/')}")

    async def _handle_login(self, scope, receive, send, ip: str):
        method = (scope.get("method") or "GET").upper()
        if method == "GET":
            await self._send_login_page(send, scope)
            return
        if method != "POST":
            await self._send_json(send, 405, b'{"error":"method_not_allowed"}')
            return
        if self._rate_limited(ip):
            print(f"[gate] login rate limited ip={ip}", flush=True)
            await self._send_login_page(send, scope, error="尝试过于频繁，请稍后再试。", status=429)
            return

        body = b""
        while True:
            message = await receive()
            if message.get("type") != "http.request":
                break
            body += message.get("body", b"")
            if not message.get("more_body", False) or len(body) > 16384:
                break
        form = parse_qs(body.decode("utf-8", "replace"))
        username = (form.get("username") or [""])[0]
        password = (form.get("password") or [""])[0]
        nxt = (form.get("next") or ["/"])[0]
        nxt = nxt if nxt.startswith("/") and not nxt.startswith("//") else "/"

        ok_user = hmac.compare_digest(username.encode(), self.user.encode())
        ok_pass = hmac.compare_digest(password.encode(), self.password.encode())
        if not (ok_user and ok_pass):
            self._record_fail(ip)
            print(f"[gate] login failed ip={ip}", flush=True)
            await self._send_login_page(send, scope, error="账号或密码不正确。", status=401)
            return

        secure = "; Secure" if self._secure_cookie(scope) else ""
        cookie = (
            f"{COOKIE_NAME}={self._session}; Path=/; Max-Age={self.session_ttl}; "
            f"HttpOnly; SameSite=Lax{secure}"
        )
        print(f"[gate] login ok ip={ip} next={nxt}", flush=True)
        await self._send_redirect(send, nxt, [(b"set-cookie", cookie.encode("latin-1"))])


def gate_from_env(app):
    """按环境变量构造闸门；密码缺失且未显式放行时直接拒绝启动（fail closed）。"""
    allow_anonymous = _truthy(os.environ.get("REG_FACTORY_ALLOW_ANONYMOUS"), False)
    user = os.environ.get("REG_FACTORY_PANEL_USER", "admin") or "admin"
    password = os.environ.get("REG_FACTORY_PANEL_PASSWORD", "") or ""
    if not allow_anonymous and not password:
        raise SystemExit(
            "[gate] 未设置 REG_FACTORY_PANEL_PASSWORD。服务器部署必须设置面板密码；"
            "仅内网自用可显式设置 REG_FACTORY_ALLOW_ANONYMOUS=1。"
        )
    if allow_anonymous:
        print("[gate] 警告：REG_FACTORY_ALLOW_ANONYMOUS=1，面板无鉴权，仅限内网。", flush=True)
    return PanelGate(
        app,
        user=user,
        password=password,
        enabled=not allow_anonymous,
        cookie_secure=os.environ.get("REG_FACTORY_COOKIE_SECURE", "auto"),
        session_ttl=int(os.environ.get("REG_FACTORY_SESSION_TTL", str(7 * 24 * 3600)) or 7 * 24 * 3600),
        max_fails=int(os.environ.get("REG_FACTORY_LOGIN_MAX_FAILS", "10") or 10),
        window=int(os.environ.get("REG_FACTORY_LOGIN_WINDOW", "300") or 300),
    )
