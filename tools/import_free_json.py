# -*- coding: utf-8 -*-
"""把「邮箱+密码+2FA」的 ChatGPT 账号转成标准 Free 凭据 JSON。

流程：住宅代理 → 登录（密码 + TOTP）→ 读 /api/auth/session → 产出标准 JSON。
产物写到 /data/assets/<platform>/free/<ref>.json，供 Team / SUB2API 直接使用。

用法：
    python tools/import_free_json.py --platform chatgpt --ref import:a@b.com \
        --email a@b.com --password xxx --totp-secret YYY [--proxy ...]
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
SHOT_DIR = DATA_DIR / "runtime" / "free_json"


def emit(**payload) -> None:
    print(json.dumps(payload, ensure_ascii=False))


def totp_code(secret: str, digits: int = 6, period: int = 30) -> str:
    import base64
    import hashlib
    import hmac
    import struct

    cleaned = "".join(ch for ch in secret.upper() if ch.isalnum())
    key = base64.b32decode(cleaned + "=" * (-len(cleaned) % 8), casefold=True)
    counter = int(time.time()) // period
    digest = hmac.new(key, struct.pack(">Q", counter), hashlib.sha1).digest()
    offset = digest[-1] & 0x0F
    code = struct.unpack(">I", digest[offset:offset + 4])[0] & 0x7FFFFFFF
    return str(code % (10 ** digits)).zfill(digits)


def default_proxy() -> str:
    single = os.environ.get("REG_FACTORY_PROXY_URL", "").strip()
    if single:
        return single
    pool = os.environ.get("REG_FACTORY_PROXY_POOL", "").strip()
    for item in pool.split(","):
        if item.strip():
            return item.strip()
    return ""


async def shot(page, tag: str) -> str:
    SHOT_DIR.mkdir(parents=True, exist_ok=True)
    path = SHOT_DIR / f"{tag}-{int(time.time())}.png"
    try:
        await page.screenshot(path=str(path))
        return str(path)
    except Exception:
        return ""


async def fill_any(page, selectors, value, timeout=15000) -> bool:
    for selector in selectors:
        try:
            element = await page.wait_for_selector(selector, timeout=timeout, state="visible")
            if element:
                await element.fill(value)
                return True
        except Exception:
            continue
    return False


async def click_any(page, selectors, timeout=8000) -> bool:
    for selector in selectors:
        try:
            element = await page.wait_for_selector(selector, timeout=timeout, state="visible")
            if element:
                await element.click()
                return True
        except Exception:
            continue
    return False


async def wait_cloudflare(page, timeout=75) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            body = await page.inner_text("body")
        except Exception:
            body = ""
        if "Verify you are human" not in body and "security verification" not in body.lower():
            return True
        try:
            frame = page.frame_locator('iframe[src*="challenges.cloudflare.com"]')
            await frame.locator('input[type="checkbox"]').click(timeout=2000)
        except Exception:
            pass
        await page.wait_for_timeout(3000)
    return False


async def convert(args) -> dict:
    from patchright.async_api import async_playwright

    async with async_playwright() as pw:
        browser = await pw.chromium.launch(
            headless=os.environ.get("REG_FACTORY_HEADLESS", "0") != "1",
            args=["--no-sandbox", "--disable-dev-shm-usage", "--disable-blink-features=AutomationControlled"],
        )
        context_args = {"locale": "en-US"}
        proxy = args.proxy or default_proxy()
        if proxy:
            context_args["proxy"] = {"server": proxy}
        context = await browser.new_context(**context_args)
        page = await context.new_page()

        try:
            await page.goto("https://chatgpt.com/auth/login", wait_until="domcontentloaded", timeout=90000)
            if not await wait_cloudflare(page, 75):
                return {"ok": False, "reason": "turnstile_blocked", "shot": await shot(page, "cf")}

            if not await fill_any(page, ['input[name="email"]', 'input[type="email"]'], args.email, timeout=30000):
                return {"ok": False, "reason": "email_field_missing", "shot": await shot(page, "email")}
            await click_any(page, ['button[type="submit"]', 'button:has-text("Continue")'])
            await page.wait_for_timeout(3000)

            if not await fill_any(page, ['input[type="password"]', 'input[name="password"]'], args.password, timeout=30000):
                return {"ok": False, "reason": "password_field_missing", "shot": await shot(page, "password")}
            await click_any(page, ['button[type="submit"]', 'button:has-text("Continue")', 'button:has-text("Log in")'])
            await page.wait_for_timeout(6000)

            # 2FA
            if args.totp_secret:
                body = ""
                try:
                    body = await page.inner_text("body")
                except Exception:
                    pass
                if "verification code" in body.lower() or "authenticator" in body.lower():
                    code = totp_code(args.totp_secret)
                    await fill_any(page, ['input[autocomplete="one-time-code"]', 'input[name="code"]', 'input[inputmode="numeric"]'], code)
                    await click_any(page, ['button[type="submit"]', 'button:has-text("Continue")', 'button:has-text("Verify")'])
                    await page.wait_for_timeout(6000)

            # 取 session
            session = await page.evaluate(
                """async () => {
                    const r = await fetch('/api/auth/session', {credentials: 'include'});
                    if (!r.ok) return {error: r.status};
                    return await r.json();
                }"""
            )
            if not isinstance(session, dict) or not session.get("accessToken"):
                return {"ok": False, "reason": "session_missing", "detail": str(session)[:200],
                        "url": page.url, "shot": await shot(page, "session")}

            return {
                "ok": True,
                "email": (session.get("user") or {}).get("email") or args.email,
                "access_token": session.get("accessToken"),
                "expires": session.get("expires"),
                "user_id": (session.get("user") or {}).get("id"),
                "plan_type": ((session.get("account") or {}).get("planType")),
                "account_id": (session.get("account") or {}).get("id"),
                "proxy_used": bool(proxy),
            }
        finally:
            await browser.close()


def write_asset(platform: str, ref: str, result: dict) -> str:
    root = DATA_DIR / "assets" / platform / "free"
    root.mkdir(parents=True, exist_ok=True)
    name = ref.split(":")[-1].replace("/", "_")
    path = root / f"{name}.json"
    payload = {
        "ref": ref,
        "platform": platform,
        "kind": "free",
        "email": result.get("email", ""),
        "source": "converted:free_json",
        "created_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "payload": {
            "access_token": result.get("access_token"),
            "expires": result.get("expires"),
            "user_id": result.get("user_id"),
            "account_id": result.get("account_id"),
            "plan_type": result.get("plan_type"),
        },
    }
    path.write_text(json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8")
    return str(path)


def main() -> int:
    parser = argparse.ArgumentParser(description="转换为 Free 凭据 JSON")
    parser.add_argument("--platform", default="chatgpt", choices=["chatgpt"])
    parser.add_argument("--ref", required=True)
    parser.add_argument("--email", required=True)
    parser.add_argument("--password", default="")
    parser.add_argument("--totp-secret", default="")
    parser.add_argument("--proxy", default="")
    args = parser.parse_args()

    if not args.password:
        emit(ok=False, ref=args.ref, email=args.email, reason="need_password",
             detail="没有密码，无法登录换 token")
        return 2

    result = asyncio.run(convert(args))
    if result.get("ok"):
        path = write_asset(args.platform, args.ref, result)
        emit(ok=True, ref=args.ref, email=result.get("email"), plan=result.get("plan_type"),
             saved_to=path, proxy=result.get("proxy_used"))
        return 0

    emit(ok=False, ref=args.ref, email=args.email, **{k: v for k, v in result.items() if k != "ok"})
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
