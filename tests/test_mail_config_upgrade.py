import importlib.util
from pathlib import Path
import unittest


spec = importlib.util.spec_from_file_location("mail_upgrade", Path(__file__).resolve().parents[1] / "deploy/upgrade-mail-config.py")
upgrade = importlib.util.module_from_spec(spec)
spec.loader.exec_module(upgrade)


class MailConfigUpgradeTests(unittest.TestCase):
    def test_rename_retains_previous_precedence_and_secrets_without_duplicates(self):
        original = "# saved\nICMAIL_IMAP_USER=original\nMAIL_IMAP_USER=lower-priority\nICMAIL_ADMIN_KEY=secret\nHME_ADMIN_KEY=older-secret\nTURNSTILE_SITE_KEY=shared\nPRIVATE_CUSTOM=retained\n"
        converted, keys = upgrade.upgrade(original)
        self.assertIn("MAIL_IMAP_USER=original\n", converted)
        self.assertIn("MAIL_ADMIN_KEY=secret\n", converted)
        self.assertNotIn("older-secret", converted)
        self.assertNotIn("ICMAIL_", converted)
        self.assertIn("MAIL_TURNSTILE_SITE_KEY=shared\n", converted)
        self.assertIn("TURNSTILE_SITE_KEY=shared\n", converted)
        self.assertIn("PRIVATE_CUSTOM=retained", converted)
        self.assertNotIn("secret", repr(keys))
        self.assertEqual(upgrade.upgrade(converted), (converted, []))

    def test_explicit_current_key_wins_over_old_admin_key(self):
        converted, _ = upgrade.upgrade("MAIL_ADMIN_KEY=current\nICMAIL_ADMIN_KEY=previous\n")
        self.assertEqual(converted.count("MAIL_ADMIN_KEY="), 1)
        self.assertIn("MAIL_ADMIN_KEY=current", converted)
