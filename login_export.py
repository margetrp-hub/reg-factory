#!/usr/bin/env python3
"""登录指定账号，导出新的 access_token 和 account_id。

用途：workspace_deactivated 的号，工作区停用后登录会落到个人空间，
      这时拿到的 AT 指向个人账号，可以用于 Plus 直充。

用法：
  python3 login_export.py --email xxx@icloud.com
  python3 login_export.py --email xxx@icloud.com --save /tmp/new_at.txt
"""
import argparse
import asyncio
import base64
import hashlib
import hmac
import json
import os
import random
import sqlite3
import string
import struct
import sys
import time

DB = "/data/regd.db"
UA = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36")
CP_USER = "zvub1264278"
CP_PASS = "o8i19taa"


def totp_code(secret):
    key = base64.b32decode(secret + "=" * (-len(secret) % 8))
    now = time.time()
    step = int(now) // 30
    remain = 30 - (int(now) % 30)
    if remain < 8:
        step += 1
        remain += 30
    h = hmac.new(key, struct.pack(">Q", step), hashlib.sha1).digest()
    o = h[19] & 15
    return str((struct.unpack(">I", h[o:o + 4])[0] & 0x7FFFFFFF) % 1000000).zfill(6), remain


def decode_at(at):
    """解出 plan / account_id / email / exp。"""
    out = {"plan": "", "account_id": "", "email": "", "iat": None, "exp": None,
           "has_orgs": False}
    try:
        seg = at.split(".")[1]
        cl = json.loads(base64.urlsafe_b64decode(seg + "=" * (-len(seg) % 4)))
        auth = cl.get("https://api.openai.com/auth") or {}
        prof = cl.get("https://api.openai.com/profile") or {}
        out["plan"] = auth.get("chatgpt_plan_type", "") or ""
        out["account_id"] = auth.get("chatgpt_account_id", "") or ""
        out["email"] = prof.get("email", "") or ""
        out["iat"] = cl.get("iat")
        out["exp"] = cl.get("exp")
        out["has_orgs"] = bool(auth.get("organizations"))
    except Exception:
        pass
    return out


async def run_login(email, pw, totp_sec):
    from patchright.async_api import async_playwright

    sid = "".join(random.choices(string.ascii_lowercase + string.digits, k=8))
    user = f"{CP_USER}-region-US-city-Portland-sid-{sid}-t-10"
    pcfg = {"server": "http://us.cliproxy.io:3010",
            "username": user, "password": CP_PASS}

    async with async_playwright() as p:
        b = await p.chromium.launch(
            headless=False, proxy=pcfg,
            args=["--no-sandbox", "--disable-dev-shm-usage",
                  "--disable-blink-features=AutomationControlled",
                  "--window-size=1400,1200"])
        ctx = await b.new_context(locale="en-US",
                                  timezone_id="America/Los_Angeles",
                                  viewport={"width": 1400, "height": 1200},
                                  user_agent=UA)
        await ctx.add_init_script(
            "Object.defineProperty(navigator,'webdriver',{get:()=>undefined});"
            "window.chrome={runtime:{}};")
        pg = await ctx.new_page()

        async def txt():
            try:
                t = await pg.evaluate("() => document.body.innerText")
                return t if isinstance(t, str) else ""
            except Exception:
                return ""

        async def wait_sel(sels, budget=200):
            w = 0
            while w < budget:
                t = await txt()
                low = t.lower()
                if "just a moment" in low or "security verification" in low:
                    await pg.wait_for_timeout(3500)
                    w += 3.5
                    continue
                for s in sels:
                    try:
                        if await pg.locator(s).count():
                            return True, t
                    except Exception:
                        pass
                await pg.wait_for_timeout(3000)
                w += 3
            return False, await txt()

        try:
            for attempt in range(3):
                try:
                    await pg.goto("https://chatgpt.com/auth/login",
                                  wait_until="commit", timeout=150000)
                    break
                except Exception:
                    await pg.wait_for_timeout(4000)

            ok, t = await wait_sel(['input[type="email"]'], 260)
            if not ok:
                return None, f"登录页未就绪: {t.strip()[:60]}"
            print("      ✓ 登录页")

            el = pg.locator('input[type="email"]').first
            await el.click()
            await pg.wait_for_timeout(500)
            await el.type(str(email), delay=90)
            await pg.wait_for_timeout(1300)
            await el.press("Enter")
            print("      ✓ 邮箱已提交")

            ok, t = await wait_sel(
                ['input[type="password"]:not([aria-hidden="true"])'], 200)
            if not ok:
                return None, f"密码页未出现: {t.strip()[:60]}"
            print("      ✓ 密码页")

            pel = pg.locator(
                'input[type="password"]:not([aria-hidden="true"])').first
            await pel.click()
            await pg.wait_for_timeout(350)
            await pel.type(str(pw), delay=85)
            await pg.wait_for_timeout(1300)
            await pel.press("Enter")
            print("      ✓ 密码已提交")

            ok, t = await wait_sel(
                ['input[autocomplete="one-time-code"]',
                 'input[inputmode="numeric"]',
                 'input[name="code"]'], 180)
            low = t.lower()

            if "deleted or deactivated" in low:
                return None, "账号已删除"
            if "check your inbox" in low:
                return None, "需要邮箱验证码"

            if ok and ("authenticator" in low or "6-digit" in low
                       or "two-factor" in low or "code" in low):
                code, rem = totp_code(str(totp_sec))
                print(f"      TOTP {code} 剩 {rem}s")
                f = None
                for s in ('input[autocomplete="one-time-code"]',
                          'input[inputmode="numeric"]',
                          'input[name="code"]'):
                    l2 = pg.locator(s)
                    if await l2.count():
                        f = l2.first
                        break
                if not f:
                    return None, "TOTP 页无输入框"
                await f.click()
                await pg.wait_for_timeout(250)
                await f.fill("")
                await f.type(str(code), delay=65)
                await pg.wait_for_timeout(600)
                btn = pg.get_by_role("button", name="Continue", exact=True)
                if await btn.count():
                    await btn.first.click(timeout=15000)
                await pg.wait_for_timeout(16000)
                t = await txt()
                low = t.lower()
                print(f"      TOTP 后: {[l.strip() for l in t.splitlines() if l.strip()][:4]}")
                if "deleted or deactivated" in low:
                    return None, "TOTP 后: 账号已删除"

            # workspace 选择页
            if "workspace" in pg.url or "select" in low:
                loc = pg.get_by_text("Personal", exact=False)
                if await loc.count():
                    try:
                        await loc.first.click(timeout=12000)
                        print("      ✓ 选 Personal")
                        await pg.wait_for_timeout(12000)
                    except Exception:
                        pass

            # 回首页确认登录
            await pg.goto("https://chatgpt.com/", wait_until="commit",
                          timeout=150000)
            await pg.wait_for_timeout(12000)

            # 从 /api/auth/session 拿 AT
            got = None
            try:
                sess = await pg.evaluate(
                    """async () => {
                        const r = await fetch('/api/auth/session', {credentials:'include'});
                        if (!r.ok) return null;
                        return await r.json();
                    }""")
                if isinstance(sess, dict):
                    got = sess.get("accessToken")
            except Exception as e:
                print(f"      session 接口失败: {e}")

            cookies = await ctx.cookies()
            has_st = any("session-token" in c["name"] for c in cookies)

            if got:
                print("      ✓ 从 /api/auth/session 拿到 AT")
                return got, "ok"
            if has_st:
                return None, "登录成功且有 session-token，但未取到 AT"
            return None, f"登录未完成: {(await txt()).strip()[:60]}"

        except Exception as e:
            import traceback
            traceback.print_exc()
            return None, f"{type(e).__name__}: {str(e)[:80]}"
        finally:
            try:
                await b.close()
            except Exception:
                pass


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--email", required=True)
    ap.add_argument("--save", default="")
    args = ap.parse_args()

    c = sqlite3.connect(DB)
    c.row_factory = sqlite3.Row
    row = c.execute("SELECT * FROM accounts WHERE identity=?",
                    (args.email,)).fetchone()
    if not row:
        print(f"✗ {args.email} 不在库")
        return 1
    d = dict(row)

    cred = c.execute(
        "SELECT password, totp FROM account_credentials WHERE ref=?",
        (d["ref"],)).fetchone()
    if not cred or not cred["password"] or not cred["totp"]:
        print("✗ 无 password/totp")
        return 1

    print(f"=== {args.email} ===")
    print(f"  当前 status={d['status']} at_status={d['at_status']}")

    # 旧 AT 的 claim 作为对照
    sr = d.get("secret_ref") or ""
    p = os.path.join("/data", sr) if sr else ""
    if p and os.path.exists(p):
        try:
            pl = (json.load(open(p)) or {}).get("payload") or {}
            old = decode_at(pl.get("access_token") or "")
            print(f"  旧 AT: plan={old['plan']} acct={old['account_id'][:12]} "
                  f"orgs={old['has_orgs']}")
        except Exception:
            pass

    at, msg = asyncio.run(run_login(args.email, str(cred["password"]),
                                    str(cred["totp"])))
    print()
    if not at:
        print(f"  ✗ 登录失败: {msg}")
        return 1

    info = decode_at(at)
    print(f"  ✓ 拿到新 AT（{len(at)} 字符）")
    print(f"    plan       {info['plan']}")
    print(f"    account_id {info['account_id']}")
    print(f"    email      {info['email']}")
    print(f"    has_orgs   {info['has_orgs']}")
    if info["exp"]:
        print(f"    exp        {time.strftime('%Y-%m-%d %H:%M', time.localtime(info['exp']))}")

    save = args.save or f"/tmp/at_{args.email.replace('@','_')}.txt"
    with open(save, "w", encoding="utf-8") as f:
        f.write(f"{at}\n{info['account_id']}\n")
    print(f"\n  已保存 {save}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
