import unittest
import asyncio
import sys
from unittest.mock import patch, Mock

import requests
from common import temp_email
from schema.mail_settings import MAIL_PROVIDERS, CHATGPT_MAIL_PROVIDERS
from schema.scripts import ENV_SCHEMA


class MailProviderContractTests(unittest.TestCase):
    def test_chatgpt_cli_keeps_selected_builtin_source(self):
        import register_chatgpt
        with patch.object(sys, "argv", ["register_chatgpt.py", "--email-provider", "icmail"]), patch.object(register_chatgpt, "select_chatgpt_node", side_effect=RuntimeError("fixture stops before network")), patch.object(register_chatgpt, "EMAIL_PROVIDER", "pool"):
            self.assertEqual(asyncio.run(register_chatgpt.main()), 2)
            self.assertEqual(register_chatgpt.EMAIL_PROVIDER, "icmail")
        field = next(item for group in ENV_SCHEMA for item in group["items"] if item["key"] == "CHATGPT_EMAIL_PROVIDER")
        self.assertEqual(set(field["choices"]), set(CHATGPT_MAIL_PROVIDERS))

    def test_every_configured_provider_has_matching_creation_and_reading(self):
        field = next(item for group in ENV_SCHEMA for item in group["items"] if item["key"] == "TEMP_EMAIL_PROVIDER")
        self.assertEqual(set(field["choices"]), set(MAIL_PROVIDERS))
        self.assertEqual(set(MAIL_PROVIDERS), set(temp_email._CREATE))
        self.assertEqual(set(MAIL_PROVIDERS), set(temp_email._FETCH))
        for provider in MAIL_PROVIDERS:
            with self.subTest(provider=provider):
                self.assertEqual(temp_email.normalize_provider(provider), provider)
                with patch.dict(temp_email._FETCH, {provider: lambda *args: [{"subject": "code", "text": "123456"}]}):
                    self.assertEqual(temp_email.fetch_messages("mailbox", provider)[0]["extracted"]["codes"], ["123456"])

    def test_invalid_configuration_never_changes_provider(self):
        with patch.dict(temp_email._CREATE, {"gptmail": unittest.mock.Mock()}) as _:
            with self.assertRaisesRegex(ValueError, "未知邮箱来源"):
                temp_email.create_mailbox(provider="misspelled-provider")
            temp_email._CREATE["gptmail"].assert_not_called()
        with patch.object(temp_email, "TEMP_EMAIL_PROVIDER", "custom"):
            self.assertEqual(temp_email.normalize_provider(), "custom")

    def test_read_failure_is_not_an_empty_inbox_or_timeout(self):
        for failure in (requests.ConnectionError("fixture transport"), ValueError("fixture response")):
            with self.subTest(failure=type(failure)), patch.object(temp_email, "fetch_messages", side_effect=failure):
                with self.assertRaises(type(failure)):
                    temp_email._scan_once("box", "custom", "owner@example.test", "", "", "", (), (), None)

    def test_api_read_rejection_never_looks_like_no_new_mail(self):
        for provider in ("moemail", "gptmail", "cfmail", "custom"):
            with self.subTest(provider=provider), patch.object(temp_email, "CUSTOM_MAIL_FETCH_PATH", "/messages"):
                session = Mock()
                session.get.return_value.status_code = 403
                session.post.return_value.status_code = 403
                with self.assertRaisesRegex(RuntimeError, "403"):
                    temp_email._FETCH[provider]("box", "owner@example.test", "mail-token", "fixture-key", "https://fixture.test", session)
                self.assertEqual(session.get.call_count + session.post.call_count, 1)

    def test_custom_message_detail_failure_preserves_cause(self):
        session = Mock()
        session.get.side_effect = [Mock(status_code=200, content=b"x", json=lambda: [{"id":"one"}]), Mock(status_code=503)]
        with patch.multiple(temp_email, CUSTOM_MAIL_FETCH_PATH="/messages", CUSTOM_MAIL_FETCH_METHOD="GET", CUSTOM_MAIL_LIST_PATH="", CUSTOM_MAIL_DETAIL_PATH="/messages/{msg_id}", CUSTOM_MAIL_MSG_ID_PATH="id"):
            with self.assertRaisesRegex(RuntimeError, "正文读取失败.*503"):
                temp_email._custom_fetch("box", "owner@example.test", "token", "key", "https://fixture.test", session)

    def test_http_providers_read_real_adapter_formats_through_public_entry(self):
        from unittest.mock import MagicMock
        message = {"subject": "Verification code", "text": "123456"}
        responses = {"moemail": {"messages": [message]}, "gptmail": {"data": {"emails": [message]}},
            "cfmail": {"results": [message]}, "custom": [message]}
        for provider, data in responses.items():
            with self.subTest(provider=provider):
                session = MagicMock()
                session.__enter__.return_value = session
                session.get.return_value = Mock(status_code=200, content=b"x", json=lambda: data)
                with patch.object(temp_email, "_session", return_value=session), patch.multiple(temp_email,
                        CUSTOM_MAIL_FETCH_METHOD="GET", CUSTOM_MAIL_FETCH_PATH="/messages", CUSTOM_MAIL_LIST_PATH=""):
                    result = temp_email.fetch_messages("box", provider, email="owner@example.test", token="token",
                        api_key="key", base_url="https://fixture.test")
                self.assertEqual(result[0]["extracted"]["codes"], ["123456"])
                session.__exit__.assert_called_once()

    def test_failed_request_does_not_expose_credentials_in_url(self):
        from unittest.mock import MagicMock
        session = MagicMock()
        session.__enter__.return_value = session
        session.get.side_effect = requests.ConnectionError("https://fixture.test?apikey=fixture-secret")
        with patch.object(temp_email, "_session", return_value=session):
            with self.assertRaises(RuntimeError) as failure:
                temp_email.fetch_messages("box", "icloud", email="owner@example.test", api_key="fixture-secret",
                    base_url="https://fixture.test")
        self.assertIn("ConnectionError", str(failure.exception))
        self.assertNotIn("fixture-secret", str(failure.exception))

    def test_configured_failover_continues_after_one_provider_is_unavailable(self):
        good = {"email": "other@example.test", "provider": "gptmail"}
        with patch.dict(temp_email._CREATE, {"moemail": Mock(side_effect=ValueError("MoeMail API key 未配置")),
                "gptmail": Mock(return_value=good)}):
            self.assertEqual(temp_email.create_mailbox(provider="moemail,gptmail"), good)

    def test_missing_remail_material_is_not_an_empty_inbox(self):
        with self.assertRaisesRegex(ValueError, "serviceToken"):
            temp_email._remail_fetch("box", "owner@example.test", "", "key", "https://fixture.test", Mock())

    def test_pool_requires_platform_before_any_mailbox_is_allocated(self):
        with patch.dict(temp_email._CREATE, {"icmail": unittest.mock.Mock(), "gptmail": unittest.mock.Mock()}):
            with self.assertRaisesRegex(ValueError, "指定目标平台"):
                temp_email.create_mailbox(provider="icmail,gptmail")
            temp_email._CREATE["icmail"].assert_not_called()
            temp_email._CREATE["gptmail"].assert_not_called()
