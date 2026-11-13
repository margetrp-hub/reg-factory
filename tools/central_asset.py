"""Platform executor for the Go central-asset host; one bounded stdin/stdout call.

Work transport, admission, durable intent, result delivery and ACK belong to Go.
Credentials travel only through the private process pipes, never argv or logs.
"""
import asyncio
import contextlib
import json
import os
from pathlib import Path
import sys
import time

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))


def execute(context, material):
    from tools.asset_ops import _token_map, _update_oauth_document, _primary_account, OPENAI_TOKEN_URL, OPENAI_CLIENT_ID
    from tools.chatgpt_reauthorize import authorize_document, validate_identity
    from common.oauth_errors import account_failure
    from vendor.reauth_web.engine import ReauthError, ERROR_HINTS
    import requests
    document = json.loads(json.dumps(material['document']))
    try:
        primary = _primary_account(document)
        token = _token_map(document) or primary
    except ValueError:
        return {'status': 'failed', 'document': None, 'error_code': 'ASSET_DOCUMENT_AMBIGUOUS'}
    action = context['action']
    timeout = min(600 if action == 'account.authorize' else 40, context['expires_at'] - int(time.time()))
    if timeout <= 0:
        return {'status': 'failed', 'document': None, 'error_code': 'WORK_AUTHORIZATION_EXPIRED'}
    if action == 'account.authorize':
        recovery = material.get('recovery') or {}
        login = recovery.get('login') or {}
        primary = json.loads(json.dumps(primary))
        primary.update(password=login.get('password', ''), totp=login.get('totp_secret', ''))
        mailbox = recovery.get('mailbox') or {}
        mail = mailbox.get('material') or {}
        primary['reauthorization'] = {'mail_api_url': mail.get('url') or mail.get('verification_url') or ''}
        # The existing authorization engine emits progress only. Keep it out of
        # the result protocol; the runner records stderr as redacted live logs.
        received = {}
        try:
            with contextlib.redirect_stdout(sys.stderr):
                updates = asyncio.run(authorize_document(primary, timeout=timeout, mailbox=mailbox or None, on_tokens=lambda v: received.update(v)))
        except Exception as exc:
            evidence = exc.evidence if isinstance(exc, ReauthError) else {}
            code = evidence.get('error_code')
            if not received and (evidence.get('token_exchange_started') is False or evidence.get('token_exchange_rejected') is True) and code in ERROR_HINTS:
                return {'status': 'failed', 'document': None, 'error_code': code.upper()}
            return {'status': 'unknown', 'document': None, 'error_code': 'AUTHORIZATION_UNCONFIRMED', 'unapplied_result': received}
        try:
            return {'status': 'completed', 'document': _update_oauth_document(document, updates), 'error_code': ''}
        except (ValueError, TypeError, KeyError):
            return {'status': 'unknown', 'document': None, 'error_code': 'ASSET_RESULT_UNAPPLIED', 'unapplied_result': updates}
    refresh_token = token.get('refresh_token') or token.get('refreshToken') or token.get('oauth_refresh_token')
    if action != 'account.refresh' or not refresh_token:
        return {'status': 'failed', 'document': None, 'error_code': 'ASSET_REFRESH_TOKEN_REQUIRED'}
    with requests.Session() as session:
        session.trust_env = False
        proxy = os.getenv('REG_FACTORY_PRODUCTION_PROXY', '').strip()
        if proxy:
            session.proxies.update(http=proxy, https=proxy)
        response = session.post(OPENAI_TOKEN_URL, data={'client_id': token.get('client_id') or OPENAI_CLIENT_ID,
            'grant_type': 'refresh_token', 'refresh_token': refresh_token, 'scope': 'openid profile email offline_access'}, timeout=timeout)
        try:
            data = response.json() if response.content else {}
        except ValueError:
            data = {}
    explicit = account_failure(data) if response.status_code >= 400 else None
    if explicit:
        return {'status': 'failed', 'document': None, 'error_code': explicit['error_code'].upper()}
    if response.status_code in {400, 401}:
        return {'status': 'failed', 'document': None, 'error_code': 'RT_REJECTED'}
    if response.status_code != 200 or not isinstance(data, dict) or not data.get('access_token'):
        return {'status': 'unknown', 'document': None, 'error_code': 'RT_RESULT_UNKNOWN'}
    updates = {k: data[k] for k in ('access_token', 'refresh_token', 'id_token', 'expires_in') if k in data}
    combined = {**token, **updates}
    try:
        validate_identity(primary, combined)
    except ValueError:
        return {'status': 'unknown', 'document': None, 'error_code': 'ASSET_IDENTITY_MISMATCH', 'unapplied_result': updates}
    try:
        if data.get('expires_in'):
            updates['expires_at'] = int(time.time()) + int(data['expires_in'])
        return {'status': 'completed', 'document': _update_oauth_document(document, updates), 'error_code': ''}
    except (ValueError, TypeError, KeyError):
        return {'status': 'unknown', 'document': None, 'error_code': 'ASSET_RESULT_UNAPPLIED', 'unapplied_result': updates}


def main():
    raw = sys.stdin.buffer.read(1024 * 1024 + 1)
    if len(raw) > 1024 * 1024 or len(sys.argv) != 1:
        raise ValueError('invalid executor input')
    value = json.loads(raw)
    if value['context']['action'] not in {'account.refresh', 'account.authorize'}:
        raise ValueError('unsupported executor action')
    with contextlib.redirect_stdout(sys.stderr):
        result = execute(value['context'], value['material'])
    print(json.dumps(result, separators=(',', ':'), allow_nan=False))


if __name__ == '__main__':
    try:
        main()
    except Exception:
        print(json.dumps({'status': 'unknown', 'document': None, 'error_code': 'ASSET_EXECUTION_UNKNOWN'}))
        sys.exit(75)
