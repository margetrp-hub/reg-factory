"""Platform executor for the Go central-asset host; one bounded stdin/stdout call.

Work transport, admission, durable intent, result delivery and ACK belong to Go.
Credentials travel only through the private process pipes, never argv or logs.
"""
import asyncio
import contextlib
import json
from pathlib import Path
import sys
import time

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from common.proxy_switch import production_proxy


def execute(context, material):
    from tools.asset_ops import _token_map, _update_oauth_document, _primary_account, OPENAI_TOKEN_URL, OPENAI_CLIENT_ID
    from tools.chatgpt_reauthorize import authorize_document, validate_identity
    from common.oauth_errors import account_failure, error_code
    from vendor.reauth_web.engine import ReauthError
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
        except ReauthError as exc:
            code = exc.evidence.get('error_code') or 'credential_result_incomplete'
            return {'status': 'unknown' if received else 'failed', 'document': None,
                    'error_code': code.upper(), 'unapplied_result': received}
        except (TimeoutError, requests.RequestException) as exc:
            code = 'OAUTH_NETWORK_TIMEOUT' if isinstance(exc, (TimeoutError, requests.Timeout)) else 'OAUTH_TLS_HANDSHAKE_FAILED' if isinstance(exc, requests.exceptions.SSLError) else 'OAUTH_NETWORK_UNAVAILABLE'
            return {'status': 'unknown' if received else 'failed', 'document': None,
                    'error_code': code, 'unapplied_result': received}
        try:
            return {'status': 'completed', 'document': _update_oauth_document(document, updates), 'error_code': ''}
        except (ValueError, TypeError, KeyError):
            return {'status': 'unknown', 'document': None, 'error_code': 'ASSET_RESULT_UNAPPLIED', 'unapplied_result': updates}
    refresh_token = token.get('refresh_token') or token.get('refreshToken') or token.get('oauth_refresh_token')
    if action != 'account.refresh' or not refresh_token:
        return {'status': 'failed', 'document': None, 'error_code': 'ASSET_REFRESH_TOKEN_REQUIRED'}
    with requests.Session() as session:
        session.trust_env = False
        proxy = production_proxy()
        if proxy:
            session.proxies.update(http=proxy, https=proxy)
        try:
            response = session.post(OPENAI_TOKEN_URL, data={'client_id': token.get('client_id') or OPENAI_CLIENT_ID,
                'grant_type': 'refresh_token', 'refresh_token': refresh_token, 'scope': 'openid profile email offline_access'}, timeout=timeout)
        except requests.RequestException as exc:
            code = 'OAUTH_NETWORK_TIMEOUT' if isinstance(exc, requests.Timeout) else 'OAUTH_TLS_HANDSHAKE_FAILED' if isinstance(exc, requests.exceptions.SSLError) else 'OAUTH_NETWORK_UNAVAILABLE'
            return {'status': 'failed', 'document': None, 'error_code': code}
        try:
            data = response.json() if response.content else {}
        except ValueError:
            data = {}
    explicit = account_failure(data) if response.status_code >= 400 else None
    if explicit:
        return {'status': 'failed', 'document': None, 'error_code': explicit['error_code'].upper()}
    code = error_code(data)
    if response.status_code in {400, 401} and code in {'invalid_grant', 'invalid_token', 'refresh_token_reused', 'refresh_token_expired', 'refresh_token_revoked'}:
        return {'status': 'failed', 'document': None, 'error_code': code.upper()}
    if response.status_code != 200 or not isinstance(data, dict) or not data.get('access_token'):
        code = 'RATE_LIMIT_EXCEEDED' if response.status_code == 429 else 'OAUTH_SERVICE_UNAVAILABLE' if response.status_code >= 500 else 'OAUTH_RESPONSE_INVALID' if response.status_code == 200 else 'OAUTH_REQUEST_REJECTED'
        return {'status': 'failed', 'document': None, 'error_code': code}
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
    main()
