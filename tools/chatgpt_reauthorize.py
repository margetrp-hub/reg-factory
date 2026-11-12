"""Supplier-local OAuth login for Work authorization commands."""
from __future__ import annotations

import asyncio
import base64
import hashlib
import os
import secrets
import time
from urllib.parse import urlencode


def login_record(document: dict) -> dict:
    from common.account_records import parse_account_line

    value = document.get("payload", document)
    saved = value.get("reauthorization") or {}
    notes = value.get("notes") or {}
    if not isinstance(notes, dict):
        notes = {}
    mailbox = notes.get("mailbox") or {}
    # Never pass the OpenAI refresh token to the mailbox provider.
    record = {key: value.get(key) for key in (
        "email", "account_password", "chatgpt_password", "login_password",
        "mail_api_url", "mail_api_key", "provider", "mailbox_provider", "email_provider", "two_factor", "otp_secret",
    ) if value.get(key)}
    record.update(mailbox)
    record.update(saved)
    record["email"] = value.get("email") or document.get("email") or record.get("email")
    if not record.get("two_factor"):
        record["two_factor"] = value.get("totp") or ""
    if not record.get("account_password"):
        record["account_password"] = (notes.get("gpt") or {}).get("password") or value.get("chatgpt_password") or value.get("password") or ""
    import json
    return parse_account_line(json.dumps(record), plus_credentials=True)


def _claims(token: str) -> dict:
    import json
    try:
        return json.loads(base64.urlsafe_b64decode(token.split(".")[1] + "==="))
    except (ValueError, IndexError):
        return {}


def validate_identity(document: dict, tokens: dict) -> None:
    value = document.get("payload", document)
    expected_email = str(value.get("email") or document.get("email") or "").lower()
    expected_workspace = str(value.get("workspace_account_id") or value.get("chatgpt_account_id") or value.get("account_id") or "")
    claims = [_claims(str(tokens.get(k) or "")) for k in ("access_token", "id_token")]
    emails = {str(c.get("email") or (c.get("https://api.openai.com/profile") or {}).get("email") or "").lower() for c in claims} - {""}
    workspaces = {str((c.get("https://api.openai.com/auth") or {}).get("chatgpt_account_id") or "") for c in claims} - {""}
    if not expected_email or expected_email not in emails:
        raise ValueError("OAuth account identity mismatch")
    if expected_workspace and expected_workspace not in workspaces:
        raise ValueError("OAuth workspace identity mismatch")


async def authorize_document(document: dict, *, timeout: int = 600, on_tokens=None, on_progress=None, mailbox=None) -> dict:
    record = login_record(document)
    if record.get("account_password") and record.get("two_factor"):
        # The supplier already has a complete protocol login implementation.
        # Password/TOTP accounts do not depend on a desktop browser process.
        return await asyncio.to_thread(_authorize_protocol, document, record, timeout, on_tokens, on_progress)
    from patchright.async_api import async_playwright
    from common.browser import open_and_connect, teardown
    from common.oauth_codex import drive_authorize, REDIRECT_URI
    from tools.import_plus_codex import MailCodeProvider
    from common.proxy_switch import browser_proxy_fields

    value = document.get("payload", document)
    verifier, state = secrets.token_urlsafe(48), secrets.token_urlsafe(32)
    challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip("=")
    client_id = "app_EMoamEEZ73f0CkXaXp7hrann"
    url = "https://auth.openai.com/oauth/authorize?" + urlencode({
        "response_type": "code", "client_id": client_id, "redirect_uri": REDIRECT_URI,
        "scope": "openid profile email offline_access", "code_challenge": challenge,
        "code_challenge_method": "S256", "state": state,
    })
    handle = profile_id = None
    print("[reauthorize] SG开始全新OAuth登录，不复用旧凭据", flush=True)
    async with async_playwright() as playwright:
        try:
            handle, profile_id, _, context, page = await open_and_connect(
                name="work_reauthorize_" + secrets.token_hex(6), p=playwright,
                browser_options=browser_proxy_fields({
                    "PROXY_MODE": "residential" if os.environ.get("REG_FACTORY_PRODUCTION_PROXY", "").strip() else "none",
                    "REG_FACTORY_PROXY": os.environ.get("REG_FACTORY_PRODUCTION_PROXY", "").strip(),
                }),
            )
            if mailbox and mailbox.get('provider') in {'imap', 'qq', 'icloud', 'outlook'}:
                from aishop_node.mailbox import IMAPMailbox
                import threading
                provider = IMAPMailbox(mailbox, stopped=threading.Event())
                class CentralMail:
                    async def prepare(self):
                        await asyncio.to_thread(provider.prime)
                    async def __call__(self, account_email, received_after):
                        return await asyncio.to_thread(provider.wait_for_otp, account_email, timeout=min(timeout, 150), issued_after=received_after)
                mail = CentralMail()
            else:
                mail = MailCodeProvider(record, context, page, max_wait=min(timeout, 150))
            await mail.prepare()
            code, callback_state, _ = await asyncio.wait_for(drive_authorize(
                page, url, timeout=timeout, account_email=record["email"],
                allow_phone=False, email_code_provider=mail,
                totp_secret=record.get("two_factor") or "",
                account_password=record.get("account_password") or "",
                prefer_team=bool(value.get("workspace_account_id")),
                workspace_id=str(value.get("workspace_account_id") or value.get("chatgpt_account_id") or value.get("account_id") or ""),
            ), timeout=timeout + 30)
            if not code or callback_state != state:
                raise RuntimeError("OAuth callback incomplete")
            print("[reauthorize] SG登录授权完成，正在交换新凭据", flush=True)
            tokens = await asyncio.to_thread(_exchange, code, verifier, client_id, REDIRECT_URI)
            if on_tokens:
                on_tokens(tokens)
            validate_identity(document, tokens)
            expires_in = int(tokens.get("expires_in") or 0)
            tokens["expires_at"] = int(time.time()) + expires_in if expires_in else 0
            return tokens
        finally:
            if handle and profile_id:
                await teardown(handle, profile_id)


def _authorize_protocol(document: dict, record: dict, timeout: int, on_tokens=None, on_progress=None) -> dict:
    from pathlib import Path
    from vendor.reauth_web.config import Settings
    from vendor.reauth_web.engine import ReauthError, reauth_account
    from vendor.reauth_web.parsing import AccountLine

    value = document.get("payload", document)
    settings = Settings(data_dir=Path(os.environ.get("REG_FACTORY_DATA_DIR") or "."),
        proxy=os.environ.get("REG_FACTORY_PRODUCTION_PROXY", "").strip(), account_timeout_seconds=timeout,
        extra={"workspace_id": str(value.get("workspace_account_id") or value.get("chatgpt_account_id") or value.get("account_id") or "")})
    account = AccountLine(email=record["email"], password=record["account_password"],
        totp_secret=record["two_factor"], raw="")
    print("[reauthorize] SG启动原账号密码/TOTP协议授权", flush=True)
    def progress(step, _message):
        if on_progress:
            on_progress(step)
        # The shared engine's mfa event announces intent before the first HTTP
        # request. It is not evidence that MFA was reached or accepted.
        if step != "mfa":
            print(f"[reauthorize] SG授权步骤 {'authorize' if step == 'login' else step}", flush=True)

    def retry(evidence):
        import json
        print("[reauthorize] " + json.dumps(evidence, sort_keys=True), flush=True)

    try:
        result = reauth_account(account, settings, on_step=progress, on_retry=retry)
    except ReauthError as exc:
        if exc.evidence:
            import json
            print("[reauthorize] " + json.dumps(exc.evidence, sort_keys=True), flush=True)
        raise
    tokens = dict(result["payloads"]["cpa"])
    if on_tokens:
        on_tokens(tokens)
    validate_identity(document, tokens)
    expires_at = int(_claims(tokens["access_token"]).get("exp") or 0)
    tokens["expires_at"] = expires_at
    tokens["expires_in"] = max(0, expires_at - int(time.time())) if expires_at else 0
    return tokens


def _exchange(code: str, verifier: str, client_id: str, redirect_uri: str) -> dict:
    import requests
    with requests.Session() as session:
        session.trust_env = False
        proxy = os.environ.get("REG_FACTORY_PRODUCTION_PROXY", "").strip()
        if proxy:
            session.proxies.update({"http": proxy, "https": proxy})
        response = session.post("https://auth.openai.com/oauth/token", data={
            "grant_type": "authorization_code", "code": code, "code_verifier": verifier,
            "client_id": client_id, "redirect_uri": redirect_uri,
        }, timeout=30)
        response.raise_for_status()
        tokens = response.json()
        if not tokens.get("access_token") or not tokens.get("refresh_token"):
            raise ValueError("OAuth exchange missing credentials")
        return tokens
