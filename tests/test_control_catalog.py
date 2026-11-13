import json
import unittest
from pathlib import Path

from schema.scripts import CONFIG_MODES, ENV_SCHEMA, EXTERNAL_LINKS, PLATFORMS, SCRIPTS, TASK_GROUPS

ROOT = Path(__file__).resolve().parents[1]


class ControlCatalogTests(unittest.TestCase):
    def test_generated_tasks_preserve_every_parameter_and_mode(self):
        generated = json.loads((ROOT / "deploy/scripts.schema.json").read_text(encoding="utf-8"))
        self.assertEqual({item["id"] for item in generated}, {item["id"] for item in SCRIPTS})
        self.assertEqual(len(generated), 16)
        self.assertEqual(sum(len(item["args"]) for item in generated), 224)
        groups = {group["slug"] for group in TASK_GROUPS}
        for original, exported in zip(SCRIPTS, generated):
            self.assertEqual(original["id"], exported["id"])
            self.assertIn(exported["group"], groups)
            self.assertTrue((ROOT / exported["file"]).is_file())
            for source, target in zip(original["args"], exported["args"]):
                for key in ("flag", "type", "default", "help"):
                    self.assertEqual(source.get(key), target.get(key), (original["id"], key))
                self.assertEqual(source.get("choices") or [], target["choices"])
        for platform in PLATFORMS:
            self.assertTrue(any(platform["slug"] in item["platforms"] for item in generated), platform)

    def test_config_and_navigation_generated_from_complete_catalog(self):
        self.assertEqual(json.loads((ROOT / "deploy/config.schema.json").read_text(encoding="utf-8")), ENV_SCHEMA)
        fields = {item["key"]: (group, item) for group in ENV_SCHEMA for item in group["items"]}
        self.assertEqual(sum(len(group["items"]) for group in ENV_SCHEMA), len(fields))
        self.assertEqual(len(fields), 199)
        catalog = json.loads((ROOT / "server/web/lib/generated-catalog.json").read_text(encoding="utf-8"))
        self.assertEqual(catalog, {"groups": TASK_GROUPS, "platforms": PLATFORMS, "tools": EXTERNAL_LINKS,
                                   "config_modes": CONFIG_MODES})
        for key, area, section in [
            ("OUTLOOK_GRAPH_RECOVERY_TIMEOUT", "mail", "Outlook 与辅助邮箱"),
            ("CHATGPT_EMAIL_PROVIDER", "mail", "来源选择"),
            ("ICLOUD_MAIL_API_KEY", "mail", "iCloud"),
            ("CODEX_ADDPHONE_ATTEMPTS", "sms", "通用与重试"),
            ("SMS_TOKEN", "sms", "firefox.fun"),
            ("ROXY_CREATE_USE_PROXY_POOL", "browser", "RoxyBrowser"),
            ("CUSTOM_BROWSER_API_KEY", "browser", "自定义 API"),
            ("SUB2API_GROK_PROXY_ID", "integration", "SUB2API"),
            ("REG_FACTORY_PRODUCTION_PROXY", "proxy", "生产与凭据修复"),
            ("CHATGPT_PROXY_MODE", "proxy", "模式与平台覆盖"),
            ("REG_FACTORY_LIVENESS_ENABLED", "runtime", "周期巡检"),
        ]:
            group, _ = fields[key]
            self.assertEqual(group["area"], area, key)
            self.assertEqual(group["group"].split(" · ")[-1], section, key)
        group, concurrency = fields["REG_FACTORY_MAX_CONCURRENCY"]
        self.assertEqual(group["area"], "runtime")
        self.assertEqual(concurrency["default"], 3)
        self.assertEqual(concurrency["apply"], "live")
        self.assertEqual(fields["REG_FACTORY_LIVENESS_ENABLED"][1]["default"], "0")
        self.assertEqual(fields["REG_FACTORY_LIVENESS_ENABLED"][1]["apply"], "live")
        for key in ("CLAUDE_VISION_API_BASE", "CLAUDE_VISION_API_KEY"):
            self.assertFalse(fields[key][1].get("required", False))
            self.assertEqual(fields[key][1]["when"], {"CLAUDE_VISION_SOURCE": ["auto", "custom"]})

    def test_modes_use_existing_runtime_selectors_without_dropping_providers(self):
        fields = {item["key"]: item for group in ENV_SCHEMA for item in group["items"]}
        for area, spec in CONFIG_MODES.items():
            for key in spec["selectors"]:
                self.assertIn(key, fields)
                self.assertTrue(set(fields[key]["choices"]) <= set(spec["choices"]), key)
            declared = set(spec["common"]) | {group for choice in spec["choices"].values() for group in choice["groups"]}
            self.assertTrue({group["group"] for group in ENV_SCHEMA if group["area"] == area} <= declared, area)
        self.assertEqual(fields["REG_FACTORY_LIVENESS_ENABLED"]["default"], "0")


if __name__ == "__main__":
    unittest.main()
