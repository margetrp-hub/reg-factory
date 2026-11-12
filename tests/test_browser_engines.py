import asyncio
import os
import unittest
from types import SimpleNamespace
from unittest.mock import AsyncMock, MagicMock, patch

from common import browser_engines as engines


class BrowserEngineTests(unittest.TestCase):
    def test_proxy_dict_preserves_credentials_and_rejects_missing_server(self):
        value = {"server": "http://proxy.example:3010", "username": "fixture", "password": "secret"}
        self.assertEqual(engines._proxy_dict(value), value)
        self.assertEqual(engines._proxy_dict("http://name%40example:p%40ss@proxy.example:3010")["password"], "p@ss")
        with self.assertRaises(ValueError):
            engines._proxy_dict({"username": "fixture"})

    def test_unknown_engine_does_not_silently_run_another(self):
        with patch.dict(os.environ, {"BROWSER_ENGINE": "missing"}):
            with self.assertRaises(ValueError):
                engines.current_engine()

    def test_camoufox_owns_context_and_closes_after_page_failure(self):
        async def journey(fail):
            context = SimpleNamespace(new_page=AsyncMock(side_effect=RuntimeError("page failed") if fail else None))
            browser = SimpleNamespace(new_context=AsyncMock(return_value=context))
            cm = SimpleNamespace(__aenter__=AsyncMock(return_value=browser), __aexit__=AsyncMock())
            constructor = MagicMock(return_value=cm)
            with patch("camoufox.async_api.AsyncCamoufox", constructor), patch.object(engines, "firefox_path", return_value="fixture-firefox"):
                if fail:
                    with self.assertRaisesRegex(RuntimeError, "page failed"):
                        await engines.launch_camoufox("fixture", headless=True)
                else:
                    handle, actual_browser, actual_context, _ = await engines.launch_camoufox("fixture", headless=True)
                    self.assertIs(actual_browser, browser)
                    self.assertIs(actual_context, context)
                    await handle.close_browser_async()
                    await handle.close_browser_async()
                cm.__aexit__.assert_awaited_once()
                self.assertNotIn("--no-sandbox", constructor.call_args.kwargs["args"])
        asyncio.run(journey(False))
        asyncio.run(journey(True))
