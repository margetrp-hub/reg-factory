# -*- coding: utf-8 -*-
"""common/outlook_devicecode.py — Outlook 设备码授权的可复用实现

为什么要有这个模块
----------------
1. 密码流（authorize_outlook.py --method http）被 Microsoft 拦：
     post.srf                   表单提交后卡在 JS 挑战
     interrupt/credentialaction 凭据验证中断，要求人工确认

2. 设备码流只要「在浏览器里已登录该账号 → 点确认」，不触发密码风控。

3. 浏览器部分必须走 common.browser.open_and_connect，才能自动获得：
     · 13 条 stealth 注入（navigator.webdriver / cdp_* / Error.stack 等）
     · browser_fingerprint 的按 worker 派生硬件组合
     · 时区/语言/地理位置跟随出口 IP
     · Accept-Language 强制英文（防代理地区导致 UI 本地化）
     · 代理绑定 + 创建/打开重试

设计
----
  纯函数层     request_device_code / poll_token        ← 不依赖浏览器
  浏览器层     authorize_in_browser                    ← 用 open_and_connect
  批量层       prepare_batch / collect_batch           ← 并发 + 状态持久化

调用方
----
  tools/devicecode_outlook.py     CLI
  其他脚本                        from common.outlook_devicecode import ...

依赖
----
  BROWSER_PROVIDER=bundled
  REG_FACTORY_BROWSER_PATH=/ms-playwright/chromium-1254/chrome-linux-arm64/chrome
"""
from __future__ import annotations

import asyncio
import json
import os
import sqlite3
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable, Optional

# Microsoft 公开 client（consumers 租户，支持个人账号）
CLIENT_ID = "9e5f94bc-e8a4-4e73-b8be-63364c29d753"
TENANT = "consumers"
DEVICECODE_URL = f"https://login.microsoftonline.com/{TENANT}/oauth2/v2.0/devicecode"
TOKEN_URL = f"https://login.microsoftonline.com/{TENANT}/oauth2/v2.0/token"

# 只要收信权限；offline_access 才能拿 refresh_token
SCOPE = (
    "https://outlook.office.com/IMAP.AccessAsUser.All "
    "https://outlook.office.com/POP.AccessAsUser.All "
    "offline_access"
)

VERIFY_URI = "https://www.microsoft.com/link"

_NOOP = lambda *_a, **_k: None  # noqa: E731


# ================================================================ 数据结构

@dataclass
class DeviceCode:
    """一次设备码授权的完整状态。"""
    email: str
    password: str = ""
    user_code: str = ""
    device_code: str = ""
    verify_uri: str = VERIFY_URI
    interval: int = 5
    expires_at: float = 0.0
    state: str = "pending"       # pending / ok / expired / declined / error
    access_token: str = ""
    refresh_token: str = ""
    error: str = ""

    def remaining(self) -> int:
        return max(0, int(self.expires_at - time.time()))

    def expired(self) -> bool:
        return self.expires_at > 0 and time.time() >= self.expires_at

    def to_row(self) -> tuple:
        return (self.email, self.password, self.user_code, self.device_code,
                self.verify_uri, self.interval, self.expires_at, self.state,
                self.access_token, self.refresh_token, self.error, time.time())


@dataclass
class AuthResult:
    """一次授权的结果。"""
    email: str
    ok: bool
    note: str = ""
    access_token: str = ""
    refresh_token: str = ""
    error_code: str = ""
    extra: dict[str, Any] = field(default_factory=dict)


# ================================================================ 纯 HTTP 层

def _http_post(url: str, data: dict, timeout: float = 25.0) -> tuple[int, str]:
    body = urllib.parse.urlencode(data).encode()
    req = urllib.request.Request(url, data=body, method="POST")
    req.add_header("Content-Type", "application/x-www-form-urlencoded")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")
    except Exception as e:
        return 0, str(e)


def request_device_code(email: str = "", password: str = "",
                        client_id: str = CLIENT_ID,
                        scope: str = SCOPE) -> DeviceCode:
    """请求一个 device_code。不依赖浏览器，纯 HTTP。"""
    dc = DeviceCode(email=email, password=password)
    status, body = _http_post(DEVICECODE_URL, {
        "client_id": client_id,
        "scope": scope,
    })
    if status != 200:
        dc.state = "error"
        dc.error = f"HTTP {status}: {body[:150]}"
        return dc

    try:
        d = json.loads(body)
    except Exception:
        dc.state = "error"
        dc.error = "响应不是 JSON"
        return dc

    dc.user_code = d.get("user_code", "")
    dc.device_code = d.get("device_code", "")
    dc.verify_uri = d.get("verification_uri", VERIFY_URI)
    dc.interval = int(d.get("interval", 5))
    dc.expires_at = time.time() + int(d.get("expires_in", 900))
    return dc


def poll_token(dc: DeviceCode, client_id: str = CLIENT_ID,
               timeout: float = 1800.0,
               stop_flag: Optional[threading.Event] = None) -> AuthResult:
    """轮询换 token。阻塞直到拿到或过期。"""
    deadline = min(dc.expires_at or (time.time() + timeout),
                   time.time() + timeout)
    interval = max(1, dc.interval)

    while time.time() < deadline:
        if stop_flag and stop_flag.is_set():
            return AuthResult(dc.email, False, "已停止", error_code="stopped")
        time.sleep(interval)

        status, body = _http_post(TOKEN_URL, {
            "client_id": client_id,
            "grant_type": "urn:ietf:params:oauth:grant-type:device_code",
            "device_code": dc.device_code,
        })

        if status == 200:
            try:
                d = json.loads(body)
            except Exception:
                continue
            at = d.get("access_token", "")
            rt = d.get("refresh_token", "")
            if at:
                dc.state = "ok"
                dc.access_token = at
                dc.refresh_token = rt
                return AuthResult(dc.email, True, "授权成功",
                                  access_token=at, refresh_token=rt)
            continue

        try:
            code = json.loads(body).get("error", "")
        except Exception:
            code = ""

        if code == "authorization_pending":
            continue
        if code == "slow_down":
            interval += 5
            continue
        if code in ("expired_token", "code_expired"):
            dc.state = "expired"
            return AuthResult(dc.email, False, "设备码过期", error_code="expired_token")
        if code in ("authorization_declined", "access_denied"):
            dc.state = "declined"
            return AuthResult(dc.email, False, "用户拒绝", error_code=code)

        dc.state = "error"
        dc.error = f"{code}: {body[:120]}"
        return AuthResult(dc.email, False, dc.error, error_code=code or "unknown")

    dc.state = "expired"
    return AuthResult(dc.email, False, "轮询超时", error_code="timeout")


# ================================================================ 浏览器层

async def authorize_in_browser(
    dc: DeviceCode,
    *,
    headless: bool = False,
    proxy: str = "",
    logger: Callable[[str], None] = _NOOP,
    page_timeout: int = 60_000,
) -> AuthResult:
    """用真实浏览器完成设备码授权。

    走 common.browser.open_and_connect —— 自动获得：
      · 13 条 stealth 注入
      · browser_fingerprint 的按 worker 派生硬件组合
      · 时区/语言/地理位置跟随出口 IP
      · Accept-Language 强制英文
      · 代理绑定 + 创建/打开重试

    流程：
      1. 打开 microsoft.com/link
      2. 输入 user_code
      3. 如果未登录 → 填邮箱 + 密码
      4. 「保持登录」选否
      5. 「保护账户」等提示能跳过就跳过
      6. 权限授权页点「是」
      7. 拿回调
    """
    # 延迟导入 —— 只用浏览器时才需要 Playwright
    from patchright.async_api import async_playwright

    from common.browser import open_and_connect, teardown

    if not dc.user_code:
        return AuthResult(dc.email, False, "没有 user_code",
                          error_code="no_user_code")

    logger(f"  打开浏览器 → {dc.verify_uri}")
    name = f"outlook_dc_{dc.email.split('@')[0]}_{int(time.time()) % 100000}"

    async with async_playwright() as p:
        bb = pid = browser = ctx = page = None
        try:
            browser_options: dict[str, Any] = {}
            if proxy:
                browser_options["proxy"] = proxy

            bb, pid, browser, ctx, page = await open_and_connect(
                name=name, p=p, browser_options=browser_options or None)

            # ---- 1. 打开设备码验证页
            await page.goto(dc.verify_uri, timeout=page_timeout,
                            wait_until="load")
            logger(f"  当前页: {page.url[:100]}")

            # 这页要 10-30 秒才把 input[name=otc] 挂到 DOM 上。
            # domcontentloaded 之后立刻操作会拿到 input=0，白跑。
            try:
                await page.wait_for_selector("input[name=otc]", timeout=40000,
                                             state="attached")
                logger("  已等到 otc 输入框")
            except Exception:
                logger("  otc 输入框未出现，尝试通用选择器")

            # ---- 2. 输入 user_code
            await _fill_user_code(page, dc.user_code, logger)

            # ---- 3. 登录（如果需要）
            if await _needs_login(page):
                logger("  需要登录，填写凭据")
                await _do_login(page, dc.email, dc.password, logger)

            # ---- 4. 处理授权确认
            await _handle_consent(page, logger)

            # ---- 5. 等回调完成
            ok = await _wait_for_completion(page, logger)

            if not ok:
                return AuthResult(
                    dc.email, False,
                    f"浏览器流程未完成，停在 {page.url[:100]}",
                    error_code="browser_stuck")

        except Exception as e:
            return AuthResult(dc.email, False,
                              f"{type(e).__name__}: {str(e)[:150]}",
                              error_code="browser_error")
        finally:
            try:
                if bb is not None and pid:
                    await teardown(bb, pid, delete=True)
                elif browser is not None:
                    await browser.close()
            except Exception:
                pass

    # ---- 6. 浏览器侧完成后，用 poll_token 拿真 token
    logger("  浏览器流程完成，开始轮询 token")
    return await asyncio.to_thread(poll_token, dc, CLIENT_ID, 120.0)


async def _human_fill(page, selector: str, value: str,
                      logger=None, delay: int = 60) -> bool:
    """拟人化填表：click 聚焦 → 逐字符 type。

    React / Vue 受控表单不认 page.fill()，因为 fill 只改 value 属性，
    不派发 input 事件。必须逐字符输入才能让框架的 onChange 跑到。
    """
    try:
        el = await page.wait_for_selector(selector, timeout=15000)
        if not el:
            return False
        await el.click()
        # 清空已有内容
        try:
            await el.press("Control+a")
            await el.press("Backspace")
        except Exception:
            pass
        await el.type(value, delay=delay)
        # blur 一下，触发校验
        try:
            await page.keyboard.press("Tab")
        except Exception:
            pass
        return True
    except Exception as e:
        if logger:
            logger(f"    human_fill({selector}) 失败: {type(e).__name__}")
        return False


async def _fill_user_code(page, code: str, logger) -> None:
    """在 microsoft.com/link 页面填入 8 位 code。

    实测页面结构（login.live.com/oauth20_remoteconnect.srf）：
      input[name=otc]        8 位 code 输入框
      button                 文案 "Allow access"
      hidden: canary / PPFT / hpgrequestid / i19
    """
    selectors = [
        'input[name="otc"]',
        'input[id="otc"]',
        'input[aria-label*="code" i]',
        'input[type="text"]',
    ]
    filled = False
    for sel in selectors:
        if await _human_fill(page, sel, code, logger, delay=80):
            logger(f"  已填入 code: {code}")
            filled = True
            break
    if not filled:
        logger("  ✗ code 填不进去")

    # 提交按钮实测文案是 "Allow access"，不是 "Next"
    for text in ("Allow access", "允许访问", "Next", "Continue", "下一步", "继续"):
        try:
            btn = page.get_by_role("button", name=text)
            if await btn.count() > 0:
                await btn.first.click()
                await asyncio.sleep(2)
                logger(f"  点击 {text}")
                return
        except Exception:
            continue


async def _needs_login(page) -> bool:
    """判断当前页是否在登录表单。"""
    try:
        url = page.url.lower()
        if "login.live.com" in url or "login.microsoftonline.com" in url:
            return True
        # 页面上有密码框就是登录页
        if await page.locator('input[type="password"]').count() > 0:
            return True
        # 有邮箱输入框
        if await page.locator('input[type="email"]').count() > 0:
            return True
    except Exception:
        pass
    return False


async def _do_login(page, email: str, password: str, logger) -> None:
    """填邮箱 → 密码 → 处理各种拦截页。"""
    # 邮箱 —— 必须逐字符输入，React 受控表单不认 fill()
    for sel in ('input[type="email"]', 'input[name="loginfmt"]', 'input[type="text"]'):
        if await _human_fill(page, sel, email, logger, delay=50):
            logger(f"  填入邮箱 {email}")
            break

    await _click_button(page, ("Next", "下一步", "继续"))
    await asyncio.sleep(2)

    # 密码 —— 同样逐字符。实测 fill() 后点提交，页面会重置回
    # "Enter the password for your Microsoft account."
    for sel in ('input[type="password"]', 'input[name="passwd"]'):
        if await _human_fill(page, sel, password, logger, delay=70):
            logger("  填入密码")
            break

    await _click_button(page, ("Sign in", "登录", "下一步", "Next"))
    await asyncio.sleep(3)

    # 「保持登录？」→ 选否（减少设备关联）
    await _click_button(page, ("No", "否", "Not now", "暂不"))
    await asyncio.sleep(2)

    # 「保护账户」/「验证身份」→ 尝试跳过
    for text in ("Skip for now", "暂时跳过", "Cancel", "取消", "Later", "稍后"):
        await _click_button(page, (text,), optional=True)


async def _handle_consent(page, logger) -> None:
    """处理 OAuth 同意页 / 已登录确认页。"""
    for text in ("Continue", "继续", "Yes", "是", "Accept", "接受", "Allow", "允许"):
        await _click_button(page, (text,), optional=True)


async def _click_button(page, texts: tuple, optional: bool = False) -> bool:
    """按文本找按钮并点击。"""
    for text in texts:
        try:
            btn = page.get_by_role("button", name=text)
            if await btn.count() > 0 and await btn.first.is_visible():
                await btn.first.click()
                await asyncio.sleep(1.5)
                return True
        except Exception:
            continue
        try:
            link = page.get_by_role("link", name=text)
            if await link.count() > 0 and await link.first.is_visible():
                await link.first.click()
                await asyncio.sleep(1.5)
                return True
        except Exception:
            continue
    return False


async def _wait_for_completion(page, logger, timeout: float = 40.0) -> bool:
    """等浏览器流程走完。

    成功标志：
      · URL 变成 microsoft.com/link 的「设备已连接」页
      · 或页面出现「You're all set」/「一切就绪」
      · 或回到 verify_uri 无 input
    """
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            url = page.url.lower()
            body = (await page.content())[:3000].lower()

            # 设备码成功的典型文案
            for marker in ("all set", "you're all set", "已全部设置", "连接成功",
                           "device is connected", "设备已连接",
                           "you can close this window", "可以关闭此窗口",
                           "you may close this window", "close this window"):
                if marker in body:
                    logger("  检测到成功标记")
                    return True

            # 停在 verify_uri 且没有输入框 → 说明提交完了
            if "microsoft.com/link" in url and "otc" not in body:
                return True
        except Exception:
            pass
        await asyncio.sleep(2)

    return False


# ================================================================ 批量层

def open_state(path: str) -> sqlite3.Connection:
    """打开状态库。check_same_thread=False 允许多线程共享。"""
    Path(path).parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(path, check_same_thread=False)
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("""
        CREATE TABLE IF NOT EXISTS device_codes (
            email         TEXT PRIMARY KEY,
            password      TEXT NOT NULL DEFAULT '',
            user_code     TEXT NOT NULL DEFAULT '',
            device_code   TEXT NOT NULL DEFAULT '',
            verify_uri    TEXT NOT NULL DEFAULT '',
            interval_sec  INTEGER NOT NULL DEFAULT 5,
            expires_at    REAL NOT NULL DEFAULT 0,
            state         TEXT NOT NULL DEFAULT 'pending',
            access_token  TEXT NOT NULL DEFAULT '',
            refresh_token TEXT NOT NULL DEFAULT '',
            error         TEXT NOT NULL DEFAULT '',
            updated_at    REAL NOT NULL DEFAULT 0
        )
    """)
    conn.execute("CREATE INDEX IF NOT EXISTS idx_dc_state ON device_codes(state)")
    conn.commit()
    return conn


def save_state(conn: sqlite3.Connection, dc: DeviceCode,
               lock: Optional[threading.Lock] = None) -> None:
    sql = """
        INSERT INTO device_codes
          (email,password,user_code,device_code,verify_uri,interval_sec,
           expires_at,state,access_token,refresh_token,error,updated_at)
        VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
        ON CONFLICT(email) DO UPDATE SET
          password=excluded.password, user_code=excluded.user_code,
          device_code=excluded.device_code, verify_uri=excluded.verify_uri,
          interval_sec=excluded.interval_sec, expires_at=excluded.expires_at,
          state=excluded.state, access_token=excluded.access_token,
          refresh_token=excluded.refresh_token, error=excluded.error,
          updated_at=excluded.updated_at
    """
    if lock:
        with lock:
            conn.execute(sql, dc.to_row())
            conn.commit()
    else:
        conn.execute(sql, dc.to_row())
        conn.commit()


def load_pending(conn: sqlite3.Connection) -> list[DeviceCode]:
    rows = conn.execute("""
        SELECT email,password,user_code,device_code,verify_uri,interval_sec,
               expires_at,state,access_token,refresh_token,error
        FROM device_codes WHERE state='pending'
    """).fetchall()
    out = []
    for r in rows:
        out.append(DeviceCode(
            email=r[0], password=r[1], user_code=r[2], device_code=r[3],
            verify_uri=r[4], interval=r[5], expires_at=r[6], state=r[7],
            access_token=r[8], refresh_token=r[9], error=r[10]))
    return out


def prepare_batch(accounts: list[tuple[str, str]], *,
                  state_path: str = "/data/devicecode.db",
                  concurrency: int = 10,
                  logger: Callable[[str], None] = _NOOP) -> list[DeviceCode]:
    """并发请求一批 device_code。"""
    conn = open_state(state_path)
    lock = threading.Lock()
    sem = threading.Semaphore(concurrency)
    results: list[DeviceCode] = []

    def worker(email: str, pwd: str):
        with sem:
            dc = request_device_code(email, pwd)
            save_state(conn, dc, lock)
            results.append(dc)
            mark = "✓" if dc.user_code else "✗"
            logger(f"  {mark} {email:<42} {dc.user_code or dc.error[:50]}")

    threads = [threading.Thread(target=worker, args=(e, p), daemon=True)
               for e, p in accounts]
    for t in threads:
        t.start()
    for t in threads:
        t.join(timeout=90)

    conn.close()
    return results


def collect_batch(*, state_path: str = "/data/devicecode.db",
                  concurrency: int = 20,
                  logger: Callable[[str], None] = _NOOP,
                  stop_flag: Optional[threading.Event] = None) -> dict[str, int]:
    """并发轮询所有 pending 的 device_code。"""
    conn = open_state(state_path)
    lock = threading.Lock()
    pending = [dc for dc in load_pending(conn) if not dc.expired()]
    if not pending:
        conn.close()
        return {"ok": 0, "fail": 0, "pending": 0}

    logger(f"  轮询 {len(pending)} 个 device_code")
    stat = {"ok": 0, "fail": 0, "pending": len(pending)}
    sem = threading.Semaphore(concurrency)

    def worker(dc: DeviceCode):
        with sem:
            r = poll_token(dc, stop_flag=stop_flag)
            save_state(conn, dc, lock)
            with lock:
                if r.ok:
                    stat["ok"] += 1
                    logger(f"  ✓ {dc.email}")
                else:
                    stat["fail"] += 1
                    logger(f"  ✗ {dc.email}  ({r.note[:60]})")

    threads = [threading.Thread(target=worker, args=(dc,), daemon=True)
               for dc in pending]
    for t in threads:
        t.start()
    for t in threads:
        t.join()

    conn.close()
    return stat


__all__ = [
    "CLIENT_ID", "SCOPE", "VERIFY_URI",
    "DeviceCode", "AuthResult",
    "request_device_code", "poll_token", "authorize_in_browser",
    "open_state", "save_state", "load_pending",
    "prepare_batch", "collect_batch",
]
