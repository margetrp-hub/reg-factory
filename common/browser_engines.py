# -*- coding: utf-8 -*-
"""common/browser_engines.py — 三引擎统一抽象

为什么单独一层
-------------
现在有三套浏览器能力，API 兼容性差别很大：

  patchright   Playwright 的 C++ 补丁分支
               API: goto / fill / locator / title()
               返回: Playwright Browser / Page
               兼容: 100%（现有 21 个脚本都用这套）

  camoufox     Firefox + C++ 补丁，Playwright 兼容层
               API: 同上（AsyncCamoufox 提供 new_page）
               返回: Playwright Browser / Page（伪装成 Firefox）
               兼容: 100%

  ruyipage     Firefox + fingerprintBrowser 内核
               API: get(url, wait=) / eles("//xpath") / el.input()
               返回: FirefoxPage（不是 Playwright 对象）
               兼容: 0%（必须写适配层）

所以分两条路：

  路线 A（Playwright 兼容）  patchright / camoufox
    → 走 open_and_connect 现有的五元组约定
    → (bb, pid, browser, ctx, page)

  路线 B（原生 API）          ruyipage
    → 单独入口 open_ruyipage
    → 返回 (session, page)，page 是 FirefoxPage
    → 调用方要用 ruyipage 的 API

用法
----
  # 路线 A：切引擎
  export BROWSER_ENGINE=camoufox     # 或 patchright（默认）

  bb, pid, browser, ctx, page = await open_and_connect(name="x", p=p)

  # 路线 B：ruyipage
  from common.browser_engines import open_ruyipage
  session, page = open_ruyipage(name="x", proxy="...")
  page.get(url, wait="complete")
  page.eles("//input")
  session.quit()
"""
from __future__ import annotations

import os
import sys
import asyncio
import shlex
from pathlib import Path
from contextlib import asynccontextmanager
from typing import Any, Optional

# Runtime discovery uses the installed package and the container's shared cache.
# Never bind an engine to a root-only cache, one browser revision or one CPU.
CHROMIUM_PATH = os.environ.get("REG_FACTORY_BROWSER_PATH", "")
FIREFOX_PATH = os.environ.get("CAMOUFOX_BROWSER_PATH", "")

ENGINES = ("patchright", "camoufox", "ruyipage")


def current_engine() -> str:
    """当前引擎（BROWSER_ENGINE 环境变量）。"""
    v = (os.environ.get("BROWSER_ENGINE") or "").strip().lower()
    if v and v not in ENGINES:
        raise ValueError("未知浏览器引擎: " + v)
    if v not in ENGINES:
        # 兼容旧配置：BROWSER_PROVIDER=camoufox 也认
        p = (os.environ.get("BROWSER_PROVIDER") or "").strip().lower()
        if p in ENGINES:
            return p
        return "patchright"
    return v


def _headless_default() -> bool:
    """REG_FACTORY_BROWSER_HEADLESS：0 = 有头（Xvfb），1 = 无头。"""
    v = (os.environ.get("REG_FACTORY_BROWSER_HEADLESS") or "").strip().lower()
    if not v:
        return False
    return v in {"1", "true", "yes", "on"}


def _extra_args() -> list[str]:
    return shlex.split(os.environ.get("CAMOUFOX_BROWSER_ARGS", ""))


def firefox_path(override: str = "") -> str:
    configured = override or os.environ.get("CAMOUFOX_BROWSER_PATH", "")
    if configured:
        if not Path(configured).is_file():
            raise RuntimeError("配置的 Firefox 内核文件不存在")
        return configured
    from camoufox.pkgman import launch_path
    return launch_path()


# ================================================================ 路线 A: camoufox

async def launch_camoufox(
    name: str,
    *,
    proxy: str = "",
    headless: Optional[bool] = None,
    browser_path: str = "",
    user_data_dir: str = "",
    **kwargs: Any,
):
    """camoufox 的 Playwright 兼容启动。

    返回 (handle, browser, context, page)
      handle 是个轻量对象，带 .close() 供 teardown 调用
    """
    from camoufox.async_api import AsyncCamoufox

    headless = _headless_default() if headless is None else headless

    cm_kwargs: dict[str, Any] = {
        "headless": headless,
        "humanize": True,
        # camoufox 在 Linux 上会警告 Canvas 噪声（privacy.resistFingerprinting
        # 的行为），声明知道这件事就不再刷警告
        "i_know_what_im_doing": True,
        # 伪装成 Windows，避免 plat 暴露 Linux
        "os": ["windows"],
        "args": _extra_args(),
    }
    if headless is False:
        # 有头需要窗口尺寸
        cm_kwargs["window"] = (1440, 900)
    if proxy:
        # camoufox 的 proxy 参数接受 "user:pass@host:port" 或 dict
        cm_kwargs["proxy"] = _proxy_dict(proxy)
    cm_kwargs["executable_path"] = firefox_path(browser_path)
    if user_data_dir:
        cm_kwargs["user_data_dir"] = user_data_dir
        cm_kwargs["persistent_context"] = True
    if kwargs:
        cm_kwargs.update(kwargs)

    cm = AsyncCamoufox(**cm_kwargs)
    browser = await cm.__aenter__()

    class _CamoufoxHandle:
        """包装 AsyncCamoufox，给上层一个统一的 close 接口。"""

        engine = "camoufox"
        name = ""

        def __init__(self, cm_obj, br):
            self._cm = cm_obj
            self._browser = br
            self.name = name
            self._closed = False

        async def close_browser_async(self, _pid=None):
            if not self._closed:
                self._closed = True
                await self._cm.__aexit__(None, None, None)

        def close_browser(self, _pid=None):
            try:
                loop = asyncio.get_running_loop()
            except RuntimeError:
                return asyncio.run(self.close_browser_async(_pid))
            return loop.create_task(self.close_browser_async(_pid))

        def delete_browser(self, _pid=None):
            pass

    handle = _CamoufoxHandle(cm, browser)
    try:
        context = browser if user_data_dir else await browser.new_context()
        page = await context.new_page()
    except BaseException:
        await handle.close_browser_async()
        raise
    return handle, browser, context, page


def _proxy_dict(raw: str | dict) -> dict:
    """把 user:pass@host:port / http://user:pass@host:port 转成 camoufox 的 dict。"""
    if isinstance(raw, dict):
        if raw.get("server"):
            return {key: raw[key] for key in ("server", "username", "password", "bypass") if key in raw}
        raw = raw.get("proxy_str") or raw.get("proxy") or ""
    s = raw.strip()
    if not s:
        raise ValueError("代理配置缺少 server 或代理 URL")
    if "://" not in s:
        s = "http://" + s
    from urllib.parse import urlparse, unquote
    u = urlparse(s)
    if not u.hostname or u.scheme not in {"http", "https", "socks5"}:
        raise ValueError("代理 URL 无效")
    host = f"[{u.hostname}]" if ":" in u.hostname else u.hostname
    d = {"server": f"{u.scheme}://{host}" + (f":{u.port}" if u.port else "")}
    if u.username:
        d["username"] = unquote(u.username)
    if u.password:
        d["password"] = unquote(u.password)
    return d


# ================================================================ 路线 B: ruyipage

class RuyipageSession:
    """ruyipage 会话包装。

    ruyipage.launch() 直接返回 FirefoxPage，没有 browser 概念。
    这里包一层，提供 quit / cookies / 截图 等常用操作。
    """

    engine = "ruyipage"

    def __init__(self, page, name: str = ""):
        self.page = page
        self.name = name

    # ---- 常用快捷方法（对齐 Playwright 命名，减少调用方改动）----

    def goto(self, url: str, wait: str = "complete", timeout: int = 45):
        """导航。wait 只接受 none / interactive / complete。"""
        return self.page.get(url, wait=wait, timeout=timeout)

    @property
    def url(self) -> str:
        return self.page.url

    @property
    def title(self) -> str:
        return self.page.title

    def find(self, selector: str):
        """找元素。CSS 不认，必须 XPath 或 tag:。"""
        return self.page.eles(selector)

    def js(self, script: str):
        """执行 JS 并尝试拿 Python 值。"""
        r = self.page.eval_handle(script)
        # ScriptResult → ScriptRemoteValue
        inner = getattr(r, "result", r)
        for attr in ("value", "raw"):
            if hasattr(inner, attr):
                v = getattr(inner, attr)
                return v() if callable(v) else v
        return inner

    def screenshot(self, path: str):
        return self.page.screenshot(path)

    def quit(self):
        try:
            self.page.quit()
        except Exception:
            pass


def open_ruyipage(
    name: str = "ruyipage",
    *,
    proxy: str = "",
    headless: Optional[bool] = None,
    browser_path: str = "",
    fpfile: str = "",
    user_dir: str = "",
    **kwargs: Any,
) -> RuyipageSession:
    """启动 ruyipage。同步接口（ruyipage 的同步 API 更稳定）。"""
    from ruyipage import launch

    headless = _headless_default() if headless is None else headless
    bp = firefox_path(browser_path or os.environ.get("RUYIPAGE_BROWSER_PATH", ""))

    launch_kwargs: dict[str, Any] = {
        "headless": headless,
        "browser_path": bp,
        "close_on_exit": True,
        "window_size": (1440, 900),
    }
    if proxy:
        launch_kwargs["proxy"] = proxy
    if fpfile:
        launch_kwargs["fpfile"] = fpfile
    if user_dir:
        launch_kwargs["user_dir"] = user_dir
    if kwargs:
        launch_kwargs.update(kwargs)

    page = launch(**launch_kwargs)
    return RuyipageSession(page, name)


# ================================================================ 统一入口（测试用）

@asynccontextmanager
async def any_browser(name: str, *, engine: str = "", proxy: str = "",
                      headless: Optional[bool] = None):
    """按 engine 自动选择路线的上下文管理器（只给新代码用）。

    Playwright 兼容引擎 → yield (page, ctx)
    ruyipage           → yield (RuyipageSession, None)
    """
    eng = (engine or current_engine()).lower()
    if eng not in ENGINES:
        raise ValueError("未知浏览器引擎: " + eng)

    if eng == "ruyipage":
        sess = open_ruyipage(name=name, proxy=proxy, headless=headless)
        try:
            yield sess, None
        finally:
            sess.quit()
        return

    if eng == "camoufox":
        handle, browser, ctx, page = await launch_camoufox(
            name, proxy=proxy, headless=headless)
        try:
            yield page, ctx
        finally:
            await handle.close_browser_async()
        return

    # patchright：走现有的 open_and_connect
    from patchright.async_api import async_playwright
    from common.browser import open_and_connect, teardown

    async with async_playwright() as p:
        bb = pid = None
        try:
            opts = {"proxy": proxy} if proxy else None
            bb, pid, _browser, ctx, page = await open_and_connect(
                name=name, p=p, browser_options=opts)
            yield page, ctx
        finally:
            if bb is not None and pid:
                await teardown(bb, pid, delete=True)


__all__ = [
    "ENGINES", "current_engine", "launch_camoufox",
    "RuyipageSession", "open_ruyipage", "any_browser",
    "CHROMIUM_PATH", "FIREFOX_PATH",
]
