#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""tools/devicecode_outlook.py — Outlook 设备码授权 CLI

核心逻辑在 common/outlook_devicecode.py，这里只是命令入口。

子命令
-----
  prepare   并发请求 device_code，输出 user_code 清单
  browser   用真实浏览器自动走完授权（走 common.browser.open_and_connect）
  collect   纯轮询（适合已经手动点过确认的情况）
  status    查看状态库里各状态分布

典型用法
-------
  # 全自动（不需要人操作）
  python3 tools/devicecode_outlook.py browser \
      --input /tmp/outlook.txt --limit 1 --headless

  # 分两步（先准备，人工点确认，再收集）
  python3 tools/devicecode_outlook.py prepare --input /tmp/outlook.txt
  python3 tools/devicecode_outlook.py collect --export /tmp/ok.txt

环境
----
  BROWSER_PROVIDER=bundled
  REG_FACTORY_BROWSER_PATH=/ms-playwright/chromium-1254/chrome-linux-arm64/chrome
"""
from __future__ import annotations

import argparse
import asyncio
import os
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from common.outlook_devicecode import (  # noqa: E402
    AuthResult,
    DeviceCode,
    authorize_in_browser,
    collect_batch,
    load_pending,
    open_state,
    poll_token,
    prepare_batch,
    request_device_code,
    save_state,
)

DEFAULT_STATE = "/data/devicecode.db"


def load_input(path: str, limit: int = 0) -> list[tuple[str, str]]:
    """读 email----password 文件。"""
    out: list[tuple[str, str]] = []
    seen: set[str] = set()
    for raw in Path(path).expanduser().read_text(encoding="utf-8-sig").splitlines():
        line = raw.strip()
        if not line or line.startswith("#") or "----" not in line:
            continue
        email, pwd = line.split("----", 1)
        email = email.strip().lower()
        if not email or email in seen:
            continue
        seen.add(email)
        out.append((email, pwd.strip()))
    return out[:limit] if limit else out


def log(msg: str) -> None:
    print(msg, flush=True)


# ---------------------------------------------------------------- prepare

def cmd_prepare(args) -> int:
    accounts = load_input(args.input, args.limit)
    print(f"=== 准备 {len(accounts)} 个 device_code ===")
    print(f"  provider: {os.environ.get('BROWSER_PROVIDER', '(未设)')}")
    print(f"  state:    {args.state}")
    print()

    results = prepare_batch(accounts, state_path=args.state,
                            concurrency=args.concurrency, logger=log)

    ok = sum(1 for dc in results if dc.user_code)
    print()
    print(f"  成功 {ok}  失败 {len(results) - ok}")

    rows = [dc for dc in results if dc.user_code]
    if rows:
        print()
        print("=" * 84)
        print(f"打开 {rows[0].verify_uri} ，依次输入下面的 code")
        print("=" * 84)
        print()
        for i, dc in enumerate(rows, 1):
            left = dc.remaining()
            mm, ss = divmod(left, 60)
            print(f"  {i:>3}. {dc.email:<42} {dc.user_code}   剩余 {mm}m{ss:02d}s")
        print()
        print(f"  然后运行：python3 {Path(__file__).name} collect --state {args.state}")

    return 0 if ok == len(results) else 1


# ---------------------------------------------------------------- browser

async def _authorize_one(dc: DeviceCode, args) -> AuthResult:
    """对一个账号走完整的浏览器授权。"""
    proxy = ""
    if args.proxy:
        proxy = args.proxy
    elif args.dedicated_proxy:
        proxy = _pick_proxy()

    return await authorize_in_browser(
        dc,
        headless=args.headless,
        proxy=proxy,
        logger=log,
        page_timeout=args.page_timeout,
    )


def _pick_proxy() -> str:
    """从 REG_FACTORY_PROXY_POOL 拿一个住宅代理。

    cli proxy 的格式：
      http://user-region-XX-sid-YYY-t-10:pass@host:PORT
    把 region/sid 换成实际值。
    """
    pool = os.environ.get("REG_FACTORY_PROXY_POOL", "").strip()
    if not pool:
        return ""
    import random
    import string
    sid = "".join(random.choices(string.ascii_lowercase + string.digits, k=8))
    return pool.replace("RANDOM", sid).replace("{sid}", sid)


def cmd_browser(args) -> int:
    accounts = load_input(args.input, args.limit)
    print(f"=== 浏览器自动授权 {len(accounts)} 个 ===")
    print(f"  provider: {os.environ.get('BROWSER_PROVIDER', '(未设)')}")
    print(f"  browser:  {os.environ.get('REG_FACTORY_BROWSER_PATH', '(默认)')}")
    print(f"  headless: {args.headless}")
    print(f"  代理:     {'dedicated' if args.dedicated_proxy else (args.proxy or '无')}")
    print()

    conn = open_state(args.state)
    ok = fail = 0

    for i, (email, pwd) in enumerate(accounts, 1):
        print(f"--- [{i}/{len(accounts)}] {email} ---")

        # 1. 先请求 device_code
        dc = request_device_code(email, pwd)
        if not dc.user_code:
            print(f"  ✗ 请求 device_code 失败: {dc.error[:80]}")
            save_state(conn, dc)
            fail += 1
            continue
        print(f"  code: {dc.user_code}  有效期 {dc.remaining()}s")
        save_state(conn, dc)

        # 2. 浏览器走完授权
        r = asyncio.run(_authorize_one(dc, args))

        if r.ok and r.refresh_token:
            print(f"  ✓ 授权成功，拿到 refresh_token ({len(r.refresh_token)} 字符)")
            dc.state = "ok"
            dc.access_token = r.access_token
            dc.refresh_token = r.refresh_token
            save_state(conn, dc)
            ok += 1
        else:
            print(f"  ✗ 失败: {r.note[:100]}  [{r.error_code}]")
            fail += 1

        # 每个号之间停一下，别打太快
        if i < len(accounts):
            time.sleep(3)

    conn.close()

    print()
    print(f"  成功 {ok}  失败 {fail}")

    if args.export and ok:
        _export_ok(args.state, args.export)
    return 0 if ok else 1


def _export_ok(state_path: str, out_path: str) -> None:
    conn = open_state(state_path)
    rows = conn.execute("""
        SELECT email, refresh_token FROM device_codes
        WHERE state='ok' AND refresh_token<>''
    """).fetchall()
    if rows:
        with open(out_path, "w", encoding="utf-8") as f:
            f.write("email----refresh_token\n")
            for e, rt in rows:
                f.write(f"{e}----{rt}\n")
        print(f"  已导出 {len(rows)} 条到 {out_path}")
    conn.close()


# ---------------------------------------------------------------- collect

def cmd_collect(args) -> int:
    print(f"=== 轮询 device_code ===")
    stat = collect_batch(state_path=args.state,
                         concurrency=args.concurrency, logger=log)
    print()
    print(f"  成功 {stat['ok']}  失败 {stat['fail']}  待处理 {stat['pending']}")
    if args.export:
        _export_ok(args.state, args.export)
    return 0


# ---------------------------------------------------------------- status

def cmd_status(args) -> int:
    conn = open_state(args.state)
    print(f"=== 状态库 {args.state} ===")
    rows = conn.execute("""
        SELECT state, COUNT(*) FROM device_codes GROUP BY state ORDER BY 2 DESC
    """).fetchall()
    if not rows:
        print("  (空)")
    for st, n in rows:
        print(f"  {st:<12} {n}")

    ok_rows = conn.execute(
        "SELECT email, length(refresh_token) FROM device_codes WHERE state='ok'"
    ).fetchall()
    if ok_rows:
        print()
        print("  已授权：")
        for e, l in ok_rows:
            print(f"    {e:<44} rt={l}字符")
    conn.close()
    return 0


# ---------------------------------------------------------------- main

def main() -> int:
    ap = argparse.ArgumentParser(description="Outlook 设备码授权")
    sub = ap.add_subparsers(dest="cmd", required=True)

    p = sub.add_parser("prepare", help="并发请求 device_code")
    p.add_argument("--input", "-i", required=True)
    p.add_argument("--state", default=DEFAULT_STATE)
    p.add_argument("--limit", type=int, default=0)
    p.add_argument("--concurrency", "-c", type=int, default=10)
    p.set_defaults(func=cmd_prepare)

    b = sub.add_parser("browser", help="浏览器自动完成授权（全自动）")
    b.add_argument("--input", "-i", required=True)
    b.add_argument("--state", default=DEFAULT_STATE)
    b.add_argument("--limit", type=int, default=0)
    b.add_argument("--headless", action="store_true", default=True)
    b.add_argument("--no-headless", dest="headless", action="store_false")
    b.add_argument("--proxy", default="", help="显式代理地址")
    b.add_argument("--dedicated-proxy", action="store_true",
                   help="从 REG_FACTORY_PROXY_POOL 分配")
    b.add_argument("--page-timeout", type=int, default=60000)
    b.add_argument("--export", default="", help="成功后导出 email----refresh_token")
    b.set_defaults(func=cmd_browser)

    c = sub.add_parser("collect", help="轮询换 token")
    c.add_argument("--state", default=DEFAULT_STATE)
    c.add_argument("--concurrency", "-c", type=int, default=20)
    c.add_argument("--export", default="")
    c.set_defaults(func=cmd_collect)

    s = sub.add_parser("status", help="查看状态")
    s.add_argument("--state", default=DEFAULT_STATE)
    s.set_defaults(func=cmd_status)

    args = ap.parse_args()
    if args.cmd == "browser":
        return args.func(args)
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
