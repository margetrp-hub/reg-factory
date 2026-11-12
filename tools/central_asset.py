"""Execute one Work-authorized central asset; replay means delivery only."""
import argparse
import asyncio
import contextlib
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import sys
import time
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[1]
sys.path[:0] = [str(ROOT), str(ROOT / 'shared/node-sdk/python')]

from aishop_node.assets import AssetProtocolError, decode_key, exchange
from aishop_node.execution import AssetOperation
from aishop_node.transport import work_claim, work_sender


def configuration():
    names = ('NODE_ID', 'NODE_KEY', 'NODE_RECEIPT_KEY', 'WORK_ORIGIN', 'ASSET_CENTER_ID', 'ASSET_KEY', 'ASSET_JOURNAL_KEY')
    cfg = {key: os.getenv('REG_FACTORY_' + key, '') for key in names}
    if not all(cfg.values()) or min(len(cfg[k]) for k in ('NODE_KEY', 'NODE_RECEIPT_KEY')) < 32:
        raise AssetProtocolError('ASSET_CONFIGURATION_REQUIRED')
    if cfg['ASSET_KEY'] in {cfg['NODE_KEY'], cfg['NODE_RECEIPT_KEY'], cfg['ASSET_JOURNAL_KEY']}:
        raise AssetProtocolError('ASSET_KEY_ISOLATION_REQUIRED')
    decode_key(cfg['ASSET_KEY'])
    decode_key(cfg['ASSET_JOURNAL_KEY'])
    parsed = urlsplit(cfg['WORK_ORIGIN'])
    if (parsed.scheme not in {'https', 'http'} or not parsed.hostname or parsed.username or parsed.password
            or parsed.query or parsed.fragment or parsed.path not in {'', '/'}):
        raise AssetProtocolError('ASSET_WORK_ORIGIN_INVALID')
    return cfg


def execute(context, material):
    from tools.asset_ops import _token_map, _update_oauth_document, OPENAI_TOKEN_URL, OPENAI_CLIENT_ID
    from tools.chatgpt_reauthorize import authorize_document, validate_identity
    import requests
    document = json.loads(json.dumps(material['document']))
    token = _token_map(document) or document
    action = context['action']
    timeout = min(600 if action == 'account.authorize' else 40, context['expires_at'] - int(time.time()))
    if timeout <= 0:
        return {'status': 'failed', 'document': None, 'error_code': 'WORK_AUTHORIZATION_EXPIRED'}
    if action == 'asset.liveness':
        from common.asset_scanner import _scan_chatgpt
        observed = _scan_chatgpt({'_token': {'access_token': token.get('access_token', '')}}, min(15, timeout))
        state = {'normal': 'alive', 'ok': 'alive', 'expired': 'needs_refresh', 'banned': 'banned'}.get(observed['status'], 'unknown')
        return {'status': 'completed' if state != 'unknown' else 'failed', 'document': None,
                'liveness': state, 'source_status': state, 'error_code': '' if state != 'unknown' else 'AT_CHECK_UNAVAILABLE',
                'checked_at': datetime.now(timezone.utc).isoformat()}
    if action == 'account.authorize':
        recovery = material.get('recovery') or {}
        login = recovery.get('login') or {}
        value = json.loads(json.dumps(document))
        primary = value.get('accounts', [value])[0]
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
        except Exception:
            return {'status': 'unknown', 'document': None, 'error_code': 'AUTHORIZATION_UNCONFIRMED', 'unapplied_result': received}
        return {'status': 'completed', 'document': _update_oauth_document(document, updates), 'error_code': ''}
    if action != 'account.refresh' or not token.get('refresh_token'):
        return {'status': 'failed', 'document': None, 'error_code': 'ASSET_REFRESH_TOKEN_REQUIRED'}
    with requests.Session() as session:
        session.trust_env = False
        proxy = os.getenv('REG_FACTORY_PRODUCTION_PROXY', '').strip()
        if proxy:
            session.proxies.update(http=proxy, https=proxy)
        response = session.post(OPENAI_TOKEN_URL, data={'client_id': token.get('client_id') or OPENAI_CLIENT_ID,
            'grant_type': 'refresh_token', 'refresh_token': token['refresh_token'], 'scope': 'openid profile email offline_access'}, timeout=timeout)
        data = response.json() if response.content else {}
    if response.status_code in {400, 401}:
        return {'status': 'failed', 'document': None, 'error_code': 'RT_REJECTED'}
    if response.status_code != 200 or not isinstance(data, dict) or not data.get('access_token'):
        return {'status': 'unknown', 'document': None, 'error_code': 'RT_RESULT_UNKNOWN'}
    updates = {k: data[k] for k in ('access_token', 'refresh_token', 'id_token', 'expires_in') if k in data}
    combined = {**token, **updates}
    try:
        validate_identity(document.get('accounts', [document])[0], combined)
    except ValueError:
        return {'status': 'unknown', 'document': None, 'error_code': 'ASSET_IDENTITY_MISMATCH', 'unapplied_result': updates}
    return {'status': 'completed', 'document': _update_oauth_document(document, updates), 'error_code': ''}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--assignment')
    parser.add_argument('--check-config', action='store_true')
    parser.add_argument('--replay', action='store_true')
    parser.add_argument('--interrupted', choices=('unstarted', 'started'))
    parser.add_argument('--ack')
    args = parser.parse_args()
    cfg = configuration()
    if args.check_config:
        from aishop_node.bundle import verify
        from aishop_node.journal import ResultJournal
        verify(ROOT / 'shared/node-sdk')
        journal = Path(os.environ['REG_FACTORY_DATA_DIR']) / 'central-assets'
        for part in ('intents', 'results'):
            ResultJournal(journal / part, cfg['ASSET_JOURNAL_KEY']).verify_writable()
        print(json.dumps({'configured': True, 'contract': 1}))
        return
    if not args.assignment:
        raise AssetProtocolError('ASSET_SCOPE_INVALID')
    context = json.loads(args.assignment)
    if (context['node_id'] != cfg['NODE_ID'] or context['parameters'] != {'asset_ref': {'center_id': cfg['ASSET_CENTER_ID']}}
            or context['action'] not in {'asset.liveness', 'account.refresh', 'account.authorize'}):
        raise AssetProtocolError('ASSET_SCOPE_INVALID')
    sender = work_sender(cfg['WORK_ORIGIN'], cfg['ASSET_CENTER_ID'], cfg['NODE_RECEIPT_KEY'].encode())
    operation = AssetOperation(Path(os.environ['REG_FACTORY_DATA_DIR']) / 'central-assets', cfg['ASSET_JOURNAL_KEY'], context,
        claim=lambda: work_claim(cfg['WORK_ORIGIN'], cfg['NODE_ID'], cfg['ASSET_CENTER_ID'], cfg['NODE_RECEIPT_KEY'].encode(),
            cfg['NODE_KEY'].encode(), context['attempt_id']),
        send=lambda body: exchange(sender, cfg['NODE_ID'], cfg['ASSET_KEY'], body))
    if args.ack:
        operation.acknowledge(json.loads(args.ack))
        return
    if args.interrupted and not args.replay:
        raise AssetProtocolError('ASSET_RECOVERY_SCOPE_INVALID')
    if args.interrupted and not operation.saved() and not operation.intents.get(context['attempt_id']):
        unstarted = args.interrupted == 'unstarted'
        operation.stage({'status': 'failed' if unstarted else 'unknown', 'document': None,
            'error_code': 'NODE_TASK_NOT_STARTED' if unstarted else 'NODE_EXECUTION_EVIDENCE_MISSING'}, preflight=unstarted)
    if not args.replay and context['expires_at'] <= int(time.time()) and not operation.saved():
        operation.stage({'status': 'failed', 'document': None, 'error_code': 'WORK_AUTHORIZATION_EXPIRED'}, preflight=True)
    result = operation.run(lambda material: execute(context, material), replay=args.replay)
    print(json.dumps({'schema': 'aishop.asset-delivery.v1', **result}, separators=(',', ':')))


if __name__ == '__main__':
    try:
        main()
    except Exception as exc:
        code = str(exc) if isinstance(exc, AssetProtocolError) else 'ASSET_ADAPTER_UNAVAILABLE'
        print(json.dumps({'schema': 'aishop.asset-delivery-error.v1', 'error_code': code}))
        sys.exit(75)
