# -*- coding: utf-8 -*-
"""改密码执行器：把导入的已接码账号改成我们控制的密码。

用法：
    python tools/change_password.py --platform chatgpt --email a@b.com \
        --old-password OLD --new-password NEW

设计：
- 只做「登录 + 改密码」两件事，不碰注册/接码；
- 失败时打印当前 URL 与截图路径，便于定位选择器漂移；
- 只有邮箱、没有旧密码的账号直接报 need_old_password（不盲目试）。
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

DATA_DIR = Path(os.environ.get("REG_FACTORY_DATA_DIR") or ROOT)
SHOT_DIR = DATA_DIR / "runtime" / "change_password"

CHATGPT_LOGIN = "https://auth.openai.com/log-in"
CHATGPT_SETTINGS = "https://chatgpt.com/#settings/Security"
CLAUDE_LOGIN = "https://claude.ai/login"
CLAUDE_SETTINGS = "https://claude.ai/settings/account"


def _default_proxy() -> str:
    """按优先级取代理：PROXY_URL > PROXY_POOL 第一条。"""
    single = os.environ.get("REG_FACTORY_PROXY_URL", "").strip()
    if single:
        return single
    pool = os.environ.get("REG_FACTORY_PROXY_POOL", "").strip()
    if pool:
        for item in pool.split(","):
            if item.strip():
                return item.strip()
    return ""


def totp_code(secret: str, digits: int = 6, period: int = 30) -> str:
    """标准 TOTP（base32 secret，无需 pyotp）。"""
    import base64
    import hashlib
    import hmac
    import struct

    cleaned = "".join(ch for ch in secret.upper() if ch.isalnum())
    padding = "=" * (-len(cleaned) % 8)
    key = base64.b32decode(cleaned + padding, casefold=True)
    counter = int(time.time()) // period
    digest = hmac.new(key, struct.pack(">Q", counter), hashlib.sha1).digest()
    offset = digest[-1] & 0x0F
    code = struct.unpack(">I", digest[offset:offset + 4])[0] & 0x7FFFFFFF
    return str(code % (10 ** digits)).zfill(digits)


def emit(**payload) -> None:
    print(json.dumps(payload, ensure_ascii=False))


async def _shot(page, tag: str) -> str:
    SHOT_DIR.mkdir(parents=True, exist_ok=True)
    path = SHOT_DIR / f"{tag}-{int(time.time())}.png"
    try:
        await page.screenshot(path=str(path), full_page=False)
    except Exception:
        return ""
    return str(path)


async def _click_any(page, selectors: list[str], timeout: int = 8000) -> bool:
    for selector in selectors:
        try:
            element = await page.wait_for_selector(selector, timeout=timeout, state="visible")
            if element:
                await element.click()
                return True
        except Exception:
            continue
    return False


async def _wait_turnstile(page, timeout: int = 75) -> bool:
    """Cloudflare 人机验证：机房 IP 直连常被拦，真实浏览器有时能自动过。"""
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            body = await page.inner_text("body")
        except Exception:
            body = ""
        if "Verify you are human" not in body and "Performing security verification" not in body:
            return True
        # 尝试点一下复选框（Turnstile 的非交互模式通常自动过）
        try:
            frame = page.frame_locator('iframe[src*="challenges.cloudflare.com"]')
            await frame.locator('input[type="checkbox"]').click(timeout=2000)
        except Exception:
            pass
        await page.wait_for_timeout(3000)
    return False


async def _fill_totp(page, secret: str) -> bool:
    """登录后的 2FA 页面：填 6 位 TOTP。"""
    if not secret:
        return False
    selectors = [
        'input[autocomplete="one-time-code"]',
        'input[name="code"]',
        'input[name="totp"]',
        'input[inputmode="numeric"]',
    ]
    code = totp_code(secret)
    if await _fill_any(page, selectors, code, timeout=12000):
        await _click_any(page, ['button[type="submit"]', 'button:has-text("Continue")', 'button:has-text("Verify")'])
        await page.wait_for_timeout(5000)
        return True
    return False


async def _fill_any(page, selectors: list[str], value: str, timeout: int = 8000) -> bool:
    for selector in selectors:
        try:
            element = await page.wait_for_selector(selector, timeout=timeout, state="visible")
            if element:
                await element.fill(value)
                return True
        except Exception:
            continue
    return False


async def change_chatgpt(page, email: str, old_pw: str, new_pw: str, totp: str = "") -> dict:
    await page.goto(CHATGPT_LOGIN, wait_until="domcontentloaded", timeout=60000)
    if not await _wait_turnstile(page, 60):
        return {"ok": False, "reason": "turnstile_blocked（机房 IP 被拦，需要住宅代理）", "shot": await _shot(page, "chatgpt-turnstile")}
    if not await _fill_any(page, ['input[name="email"]', 'input[type="email"]'], email, timeout=30000):
        return {"ok": False, "reason": "email_field_missing", "shot": await _shot(page, "chatgpt-email")}
    await _click_any(page, ['button[type="submit"]', 'button:has-text("Continue")'])

    if not await _fill_any(page, ['input[type="password"]', 'input[name="password"]'], old_pw, timeout=30000):
        return {"ok": False, "reason": "password_field_missing（可能需要接码或邮箱验证）", "shot": await _shot(page, "chatgpt-password")}
    await _click_any(page, ['button[type="submit"]', 'button:has-text("Continue")'])
    await page.wait_for_timeout(6000)

    # 需要 2FA 时用 TOTP 通过
    if "auth.openai.com" in page.url and totp:
        await _fill_totp(page, totp)

    if "auth.openai.com" in page.url and "log-in" in page.url:
        return {"ok": False, "reason": "login_failed（旧密码错误或需要二次验证）", "shot": await _shot(page, "chatgpt-login-failed")}

    await page.goto(CHATGPT_SETTINGS, wait_until="domcontentloaded", timeout=60000)
    await page.wait_for_timeout(4000)
    if not await _click_any(page, ['button:has-text("Change password")', 'a:has-text("Change password")'], timeout=15000):
        return {"ok": False, "reason": "change_password_entry_missing", "shot": await _shot(page, "chatgpt-settings")}

    filled = await _fill_any(page, ['input[name="currentPassword"]', 'input[autocomplete="current-password"]'], old_pw)
    if not filled:
        return {"ok": False, "reason": "current_password_field_missing", "shot": await _shot(page, "chatgpt-change")}
    await _fill_any(page, ['input[name="newPassword"]', 'input[autocomplete="new-password"]'], new_pw)
    await _click_any(page, ['button[type="submit"]', 'button:has-text("Save")', 'button:has-text("Change")'])
    await page.wait_for_timeout(5000)
    return {"ok": True, "reason": "submitted", "shot": await _shot(page, "chatgpt-done")}


async def change_claude(page, email: str, old_pw: str, new_pw: str, totp: str = "") -> dict:
    await page.goto(CLAUDE_LOGIN, wait_until="domcontentloaded", timeout=60000)
    if not await _fill_any(page, ['input[type="email"]', 'input[name="email"]'], email):
        return {"ok": False, "reason": "email_field_missing", "shot": await _shot(page, "claude-email")}
    await _click_any(page, ['button[type="submit"]', 'button:has-text("Continue with email")'])

    if not await _fill_any(page, ['input[type="password"]', 'input[name="password"]'], old_pw, timeout=20000):
        return {"ok": False, "reason": "password_field_missing（可能走 magic link）", "shot": await _shot(page, "claude-password")}
    await _click_any(page, ['button[type="submit"]', 'button:has-text("Continue")'])
    await page.wait_for_timeout(6000)

    await page.goto(CLAUDE_SETTINGS, wait_until="domcontentloaded", timeout=60000)
    await page.wait_for_timeout(3000)
    if not await _click_any(page, ['button:has-text("Change password")', 'button:has-text("Password")'], timeout=12000):
        return {"ok": False, "reason": "change_password_entry_missing", "shot": await _shot(page, "claude-settings")}
    await _fill_any(page, ['input[name="currentPassword"]', 'input[autocomplete="current-password"]'], old_pw)
    await _fill_any(page, ['input[name="newPassword"]', 'input[autocomplete="new-password"]'], new_pw)
    await _click_any(page, ['button[type="submit"]', 'button:has-text("Save")'])
    await page.wait_for_timeout(4000)
    return {"ok": True, "reason": "submitted", "shot": await _shot(page, "claude-done")}


async def run(args) -> int:
    if not args.old_password:
        emit(ok=False, platform=args.platform, email=args.email,
             reason="need_old_password", detail="只有邮箱、没有旧密码，无法改密码")
        return 2

    from playwright.async_api import async_playwright

    async with async_playwright() as pw:
        browser = await pw.chromium.launch(
            headless=os.environ.get("REG_FACTORY_HEADLESS", "0") != "1",
            args=["--no-sandbox", "--disable-dev-shm-usage", "--disable-blink-features=AutomationControlled"],
        )
        context_args = {"locale": "en-US"}
        if args.proxy:
            context_args["proxy"] = {"server": args.proxy}
        context = await browser.new_context(**context_args)
        page = await context.new_page()
        try:
            if args.platform == "chatgpt":
                result = await change_chatgpt(page, args.email, args.old_password, args.new_password, args.totp_secret)
            elif args.platform == "claude":
                result = await change_claude(page, args.email, args.old_password, args.new_password, args.totp_secret)
            else:
                result = {"ok": False, "reason": f"platform_not_supported:{args.platform}"}
        finally:
            await browser.close()

    emit(platform=args.platform, email=args.email, **result)
    return 0 if result.get("ok") else 1


def main() -> int:
    parser = argparse.ArgumentParser(description="改密码执行器")
    parser.add_argument("--platform", required=True, choices=["chatgpt", "claude"])
    parser.add_argument("--email", required=True)
    parser.add_argument("--old-password", default="")
    parser.add_argument("--new-password", required=True)
    parser.add_argument("--totp-secret", default="", help="2FA 的 base32 secret（可选）")
    parser.add_argument("--ref", default="", help="账号引用（仅用于结果回写，脚本不使用）")
    parser.add_argument("--proxy", default=_default_proxy(), help="住宅代理 http://user:pass@host:port")
    return asyncio.run(run(parser.parse_args()))


if __name__ == "__main__":
    raise SystemExit(main())
