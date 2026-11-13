import asyncio
import json
import os
import tempfile
import threading
import time
import unittest
from concurrent.futures import ThreadPoolExecutor
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from unittest.mock import patch
from urllib.parse import parse_qs, urlsplit

from common import emails, mail_pool


class MailPoolWorkflowTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.messages = []
        self.enabled = True
        self.requests = []
        test = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_args): pass
            def do_GET(self):
                parsed = urlsplit(self.path)
                query = parse_qs(parsed.query)
                test.requests.append((parsed.path, query))
                if self.headers.get("X-API-Key") != "test-secret":
                    self.send_response(401); self.end_headers(); return
                status = 200
                if parsed.path.endswith("/workflow/candidates"):
                    data = {"items": [{"email": "person@example.test", "sourceId": 1,
                        "sourceEnabled": True, "origin": "imported"}], "total": 1}
                elif not test.enabled:
                    status, data = 409, {}
                else:
                    cursor = int(query.get("cursor", [0])[0])
                    after = float(query.get("after", [0])[0])
                    data = {"email": "person@example.test", "source_id": 1,
                        "cursor": max([m["id"] for m in test.messages] or [0]),
                        "messages": [m for m in test.messages if m["id"] > cursor and m["sentAt"] >= after]}
                self.send_response(status); self.send_header("Content-Type", "application/json"); self.end_headers()
                self.wfile.write(json.dumps({"ok": status == 200, "data": data}).encode())

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=self.server.serve_forever, daemon=True); thread.start()
        self.addCleanup(self.server.server_close); self.addCleanup(self.server.shutdown)
        env = patch.dict(os.environ, {"REG_FACTORY_DATA_DIR": self.directory.name,
            "REG_FACTORY_MAIL_ADDR": f"127.0.0.1:{self.server.server_port}",
            "REG_FACTORY_MAIL_KEY": "test-secret", "HTTP_PROXY": "http://unreachable.invalid:1"})
        env.start(); self.addCleanup(env.stop)
        pool = patch.object(emails, "EMAILS_FILE", str(Path(self.directory.name) / "emails.txt"))
        pool.start(); self.addCleanup(pool.stop)

    def mail(self, code, age=0):
        self.messages.append({"id": len(self.messages)+1, "sentAt": int(time.time())-age,
            "subject": "OpenAI verification", "text": f"Your code is {code}", "from": "noreply@openai.com"})

    def test_register_then_authorize_fresh_code_and_pause_recovery(self):
        import register_chatgpt
        from common.account_records import canonical_account_line, parse_account_line
        from tools.import_plus_codex import MailCodeProvider
        from tools.chatgpt_reauthorize import login_record

        self.mail("111111", age=3600)
        with patch.object(register_chatgpt, "EMAIL_PROVIDER", "icmail"), patch.object(register_chatgpt, "FIXED_EMAIL", None):
            registration = register_chatgpt.allocate_chatgpt_registration_mailbox()
        mailbox = registration["mailbox"]
        self.mail("222222")
        self.assertEqual(asyncio.run(mail_pool.poll_code(mailbox, max_wait=.2)), "222222")
        with self.assertRaises(mail_pool.MailboxError): mail_pool.allocate()
        record = login_record({"email": mailbox["email"], "email_provider": "icmail"})
        record = parse_account_line(canonical_account_line(record))
        self.assertEqual(record["provider"], "icmail")
        provider = MailCodeProvider(record, None, None)
        self.assertEqual(asyncio.run(provider.prepare()), "icmail-pool")
        self.mail("333333")
        self.assertEqual(asyncio.run(provider(record["email"], time.time())), "333333")
        with self.assertRaises(mail_pool.MailboxError): asyncio.run(provider("other@example.test", time.time()))
        self.enabled = False
        with self.assertRaises(mail_pool.MailboxError): asyncio.run(provider.prepare())
        self.enabled = True
        asyncio.run(provider.prepare())
        self.assertIsNone(asyncio.run(mail_pool.poll_code(provider.icmail, max_wait=.03, poll_interval=.01)))
        self.mail("444444")
        self.assertEqual(asyncio.run(provider(record["email"], time.time())), "444444")
        self.assertTrue(all(path.startswith("/api/apps/mail/") for path, _ in self.requests))

    def test_concurrent_registration_reserves_only_once(self):
        def claim():
            try: return mail_pool.allocate()["email"]
            except mail_pool.MailboxError: return None
        with ThreadPoolExecutor(max_workers=2) as executor:
            results = list(executor.map(lambda _: claim(), range(2)))
        self.assertEqual(results.count("person@example.test"), 1)

    def test_unavailable_candidate_does_not_block_the_next_mailbox(self):
        candidates = {"items": [{"email": email, "sourceId": 1, "sourceEnabled": True,
            "origin": "imported"} for email in ("paused@example.test", "person@example.test")], "total": 2}
        original_get = mail_pool._get

        def read(path, **params):
            if path == "/workflow/candidates":
                return candidates
            if params.get("email") == "paused@example.test":
                raise mail_pool.MailboxUnavailable("paused after listing")
            return original_get(path, **params)

        with patch.object(mail_pool, "_get", side_effect=read):
            self.assertEqual(mail_pool.allocate("kiro")["email"], "person@example.test")
        self.assertEqual(self.requests[-1][1]["platform"], ["kiro"])

    def test_source_failure_is_reported_without_allocating_another_mailbox(self):
        with patch.object(mail_pool, "_get", side_effect=mail_pool.MailboxError("source unavailable")):
            with self.assertRaisesRegex(mail_pool.MailboxError, "source unavailable"):
                mail_pool.allocate("grok")
        self.assertFalse(emails._load_used("grok"))

    def test_other_registration_modes_share_pool_and_fresh_message_adapter(self):
        from common.temp_email import create_mailbox, fetch_messages
        self.mail("111111", age=30)
        mailbox = create_mailbox(provider="icmail", platform="claude")
        self.mail("222222")
        messages = fetch_messages(mailbox["id"], "icmail", email=mailbox["email"], token=mailbox["token"])
        self.assertEqual(len(messages), 1)
        self.assertEqual(messages[0]["extracted"]["codes"], ["222222"])
        with self.assertRaises(mail_pool.MailboxError): mail_pool.allocate("chatgpt")

    def test_exported_credentials_preserve_mailbox_source_for_next_authorization(self):
        from common.session_export import build_cpa_codex_json
        from tools.chatgpt_reauthorize import login_record
        payload = build_cpa_codex_json({"email": "person@example.test", "accessToken": "fixture-at",
            "email_provider": "icmail"})["auth_json"]
        self.assertEqual(login_record(payload)["provider"], "icmail")

    def test_registration_provider_metadata_does_not_override_legacy_mailbox_rules(self):
        from common.account_records import parse_account_line
        record = parse_account_line(json.dumps({"email": "person@icloud.com", "email_provider": "remail"}))
        self.assertEqual(record["provider"], "icloud")

    def test_rejects_remote_target_before_sending_panel_key(self):
        with patch.dict(os.environ, {"REG_FACTORY_MAIL_ADDR": "example.invalid:8800"}):
            with self.assertRaises(mail_pool.MailboxError): mail_pool.bind("person@example.test")
        self.assertEqual(self.requests, [])

    def test_mail_defaults_and_cleared_secret_match_catalog(self):
        from schema.mail_settings import MAIL_DEFAULTS, mail_setting
        from schema.scripts import ENV_SCHEMA
        fields = {field["key"]: field for group in ENV_SCHEMA for field in group["items"]}
        with patch.dict(os.environ, {key: " " for key in MAIL_DEFAULTS}):
            for key, default in MAIL_DEFAULTS.items():
                self.assertEqual(mail_setting(key), default)
                self.assertEqual(fields[key]["default"], default)
                self.assertTrue(fields[key]["empty_default"])
        self.assertNotIn("ICLOUD_MAIL_API_KEY", MAIL_DEFAULTS)


if __name__ == "__main__":
    unittest.main()
