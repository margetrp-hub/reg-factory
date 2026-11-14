"""Supplier-local OAuth login for Work authorization commands."""
from __future__ import annotations

import asyncio
import base64
import hashlib
import os
import secrets
import time
from urllib.parse import urlencode, urlsplit
from common.proxy_switch import production_proxy


def _workspace_id(document):
    value = document.get('payload', document)
    for item in (value, value.get('credentials') or {}):
        for key in ('workspace_account_id', 'chatgpt_account_id', 'account_id'):
            if item.get(key):
                return str(item[key])
    return ''


async def _drive_authorization(page, url, **kwargs):
    """Stop on an explicit provider refusal; never classify page prose as a ban."""
    from common.oauth_codex import drive_authorize
    from common.oauth_errors import account_failure
    from vendor.reauth_web.engine import ReauthError, failure_evidence
    refused = asyncio.get_running_loop().create_future()
    async def observe(response):
        if urlsplit(response.url).hostname not in {'auth.openai.com', 'chatgpt.com'} or not 400 <= response.status < 500:
            return
        if response.status == 429:
            explicit = {'error_code': 'rate_limit_exceeded', 'message': '授权服务请求过于频繁'}
        else:
            try:
                explicit = account_failure(await response.json())
            except ValueError:
                return
        if explicit and not refused.done():
            refused.set_result((explicit, response.status))
    page.on('response', observe)
    drive = asyncio.create_task(drive_authorize(page, url, **kwargs))
    try:
        await asyncio.wait({drive, refused}, return_when=asyncio.FIRST_COMPLETED)
        if refused.done():
            explicit, status = refused.result()
            raise ReauthError(explicit['message'], evidence=failure_evidence({'error': explicit['error_code'], 'status': status}))
        return await drive
    finally:
        page.remove_listener('response', observe)
        refused.cancel()
        if not drive.done():
            drive.cancel()
        await asyncio.gather(drive, return_exceptions=True)


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
    expected_workspace = _workspace_id(document)
    claims = [_claims(str(tokens.get(k) or "")) for k in ("access_token", "id_token")]
    emails = {str(c.get("email") or (c.get("https://api.openai.com/profile") or {}).get("email") or "").lower() for c in claims} - {""}
    workspaces = {str((c.get("https://api.openai.com/auth") or {}).get("chatgpt_account_id") or "") for c in claims} - {""}
    if not expected_email or emails != {expected_email}:
        raise ValueError("OAuth account identity mismatch")
    if expected_workspace and workspaces != {expected_workspace}:
        raise ValueError("OAuth workspace identity mismatch")


async def authorize_document(document: dict, *, timeout: int = 600, login_mode: str = "browser", on_tokens=None, on_progress=None, mailbox=None) -> dict:
    from vendor.reauth_web.engine import ReauthError
    try:
        # Central material already contains a separately validated mailbox.
        # Do not require a node-local mailbox/password to use that association.
        record = ({"email": document["email"], "account_password": document.get("password", ""),
                   "two_factor": document.get("totp", "")} if mailbox else login_record(document))
    except ValueError as exc:
        raise ReauthError("登录资料格式无效", evidence={"error_code": "credential_login_material_invalid", "token_exchange_started": False}) from exc
    # The saved/selected mode chooses transport. Merely having password + TOTP
    # must never force protocol login over the operator's browser selection.
    if login_mode == "protocol":
        if not record.get("account_password") or not record.get("two_factor"):
            raise ReauthError("协议授权需要账号密码和2FA资料", evidence={"error_code": "credential_login_material_invalid", "token_exchange_started": False})
        return await asyncio.to_thread(_authorize_protocol, document, record, timeout, on_tokens, on_progress)
    if login_mode != "browser":
        raise ReauthError("登录执行模式无效", evidence={"error_code": "credential_login_mode_invalid", "token_exchange_started": False})
    from patchright.async_api import async_playwright, Error as BrowserError
    from common.browser import open_and_connect, teardown
    from common.oauth_codex import REDIRECT_URI
    from tools.import_plus_codex import MailCodeProvider
    from common.proxy_switch import browser_proxy_fields

    proxy = production_proxy()

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
                    "PROXY_MODE": "residential" if proxy else "none",
                    "REG_FACTORY_PROXY": proxy,
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
            # Snapshot before requesting OTP; doing this after mail arrives
            # would incorrectly discard the current code as old mail. A mailbox
            # outage only fails login if that login actually needs email OTP.
            from requests import RequestException
            mail_error = None
            try:
                await asyncio.wait_for(mail.prepare(), timeout=min(timeout, 15))
            except (RuntimeError, ValueError, RequestException, TimeoutError, BrowserError) as exc:
                mail_error = exc
            metadata = {}
            async def email_code(account_email, received_after):
                if mail_error is not None:
                    metadata['error_code'] = 'email_otp_provider_required'
                    raise ReauthError("邮箱收信服务不可用", evidence={"error_code": 'email_otp_provider_required', "token_exchange_started": False}) from mail_error
                metadata['error_code'] = 'email_otp_not_found'
                result = await mail(account_email, received_after)
                if result:
                    metadata['error_code'] = 'email_otp_verification_failed'
                return result

            if on_progress:
                on_progress("authorize")
            code, callback_state, _ = await asyncio.wait_for(_drive_authorization(
                page, url, timeout=timeout, account_email=record["email"],
                allow_phone=False, email_code_provider=email_code,
                totp_secret=record.get("two_factor") or "",
                account_password=record.get("account_password") or "",
                prefer_team=bool(_workspace_id(document)),
                workspace_id=_workspace_id(document),
                result_metadata=metadata,
            ), timeout=timeout + 30)
            if not code:
                error = metadata.get('error_code') or 'oauth_callback_not_found'
                raise ReauthError("登录授权未完成", evidence={"error_code": error, "token_exchange_started": False})
            if callback_state != state:
                raise ReauthError("授权回调不完整", evidence={"error_code": "oauth_callback_invalid", "token_exchange_started": False})
            print("[reauthorize] SG登录授权完成，正在交换新凭据", flush=True)
            tokens = await asyncio.to_thread(_exchange, code, verifier, client_id, REDIRECT_URI)
            if on_tokens:
                on_tokens(tokens)
            validate_identity(document, tokens)
            expires_in = int(tokens.get("expires_in") or 0)
            tokens["expires_at"] = int(time.time()) + expires_in if expires_in else 0
            return tokens
        except BrowserError as exc:
            raise ReauthError("授权浏览器运行失败", evidence={"error_code": "authorization_browser_error"}) from exc
        finally:
            if handle and profile_id:
                await teardown(handle, profile_id)


def _authorize_protocol(document: dict, record: dict, timeout: int, on_tokens=None, on_progress=None) -> dict:
    from pathlib import Path
    from vendor.reauth_web.config import Settings
    from vendor.reauth_web.engine import reauth_account
    from vendor.reauth_web.parsing import AccountLine

    settings = Settings(data_dir=Path(os.environ.get("REG_FACTORY_DATA_DIR") or "."),
        proxy=production_proxy(), account_timeout_seconds=timeout,
        extra={"workspace_id": _workspace_id(document)})
    account = AccountLine(email=record["email"], password=record["account_password"],
        totp_secret=record["two_factor"], raw="")
    print("[reauthorize] 按账号选择执行协议授权", flush=True)
    def progress(step, _message):
        if on_progress:
            on_progress(step)
        if step != "mfa":
            print(f"[reauthorize] SG授权步骤 {'authorize' if step == 'login' else step}", flush=True)
    def retry(evidence):
        import json
        print("[reauthorize] " + json.dumps(evidence, sort_keys=True), flush=True)
    result = reauth_account(account, settings, on_step=progress, on_retry=retry)
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
        proxy = production_proxy()
        if proxy:
            session.proxies.update({"http": proxy, "https": proxy})
        response = session.post("https://auth.openai.com/oauth/token", data={
            "grant_type": "authorization_code", "code": code, "code_verifier": verifier,
            "client_id": client_id, "redirect_uri": redirect_uri,
        }, timeout=30)
        if isinstance(response.status_code, int) and 400 <= response.status_code < 500:
            from common.oauth_errors import REFRESH_RELOGIN_CODES, account_failure, error_code
            from vendor.reauth_web.engine import ReauthError, failure_evidence
            try:
                payload = response.json()
                explicit = account_failure(payload)
                code = error_code(payload)
                if not explicit and code in REFRESH_RELOGIN_CODES:
                    explicit = {'error_code': code, 'message': '授权凭据已失效，需要重新登录'}
            except ValueError:
                explicit = None
            if explicit:
                evidence = failure_evidence({'error': explicit['error_code'], 'status': response.status_code})
                evidence.update(token_exchange_started=True, token_exchange_rejected=True)
                raise ReauthError(explicit['message'], evidence=evidence)
        response.raise_for_status()
        tokens = response.json()
        if not tokens.get("access_token") or not tokens.get("refresh_token"):
            raise ValueError("OAuth exchange missing credentials")
        return tokens
