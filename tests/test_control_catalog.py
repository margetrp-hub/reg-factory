import json
import unittest
from pathlib import Path

from schema.scripts import ENV_SCHEMA, EXTERNAL_LINKS, PLATFORMS, SCRIPTS, TASK_GROUPS

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
        self.assertEqual(sum(len(group["items"]) for group in ENV_SCHEMA), 182)
        catalog = json.loads((ROOT / "server/web/lib/generated-catalog.json").read_text(encoding="utf-8"))
        self.assertEqual(catalog, {"groups": TASK_GROUPS, "platforms": PLATFORMS, "tools": EXTERNAL_LINKS})
        groups = {group["group"]: group["area"] for group in ENV_SCHEMA}
        self.assertEqual(groups["Outlook 自注册"], "mail")
        self.assertEqual(groups["ChatGPT 邮箱"], "mail")
        self.assertEqual(groups["Codex add-phone 接码调参"], "sms")


if __name__ == "__main__":
    unittest.main()
