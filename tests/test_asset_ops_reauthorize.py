import base64
import json
from types import SimpleNamespace
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from tools import asset_ops
from tools.chatgpt_reauthorize import login_record, validate_identity
from tools.chatgpt_reauthorize import authorize_document
import asyncio


@pytest.mark.parametrize('code', ['account_banned', 'workspace_deactivated', 'rate_limit_exceeded'])
def test_browser_refusal_ends_original_flow_without_token_exchange(code):
    from tools.chatgpt_reauthorize import _drive_authorization
    from vendor.reauth_web.engine import ReauthError
    async def run():
        callbacks = {}
        page = SimpleNamespace(on=lambda event, cb: callbacks.update({event: cb}), remove_listener=lambda event, cb: callbacks.pop(event))
        cancelled = []
        async def drive(*args, **kwargs):
            # Provider prose is not evidence, nor is a third-party response.
            await callbacks['response'](SimpleNamespace(url='https://other.invalid/', status=401, json=AsyncMock(return_value={'error': {'code': code}})))
            body = AsyncMock(side_effect=ValueError('non-json rate limit')) if code == 'rate_limit_exceeded' else AsyncMock(return_value={'error': {'code': code}})
            await callbacks['response'](SimpleNamespace(url='https://auth.openai.com/api/accounts/password/verify', status=429 if code == 'rate_limit_exceeded' else 401, json=body))
            try:
                await asyncio.Future()
            finally:
                cancelled.append(True)
        with patch('common.oauth_codex.drive_authorize', side_effect=drive):
            with pytest.raises(ReauthError) as caught:
                await asyncio.wait_for(_drive_authorization(page, 'https://auth.openai.com/oauth/authorize'), 2)
        assert caught.value.evidence['error_code'] == code
        assert caught.value.evidence['token_exchange_started'] is False
        assert cancelled == [True] and not callbacks
    asyncio.run(run())


def jwt(claims):
    return 'head.' + base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip('=') + '.sig'


def test_authorize_does_not_replay_old_refresh_grant():
    args = SimpleNamespace(platform='chatgpt', allow_relogin=True, ref='original', email='', timeout=60, login_mode='browser')
    with patch.object(asset_ops, '_refresh_chatgpt') as refresh, patch.object(asset_ops, '_reauthorize_chatgpt', return_value={'result': 'refreshed'}) as authorize:
        assert asset_ops.cmd_refresh(args) == 0
        refresh.assert_not_called()
        authorize.assert_called_once_with('original', '', 60, login_mode='browser')


def test_browser_start_failure_returns_reason_without_touching_credentials(tmp_path, monkeypatch):
    monkeypatch.setenv('REG_FACTORY_DATA_DIR', str(tmp_path))
    directory = tmp_path / 'assets/chatgpt/oauth'
    directory.mkdir(parents=True)
    path = directory / 'original.json'
    path.write_text(json.dumps({'ref': 'import:original', 'payload': {
        'email': 'original@icloud.com', 'password': 'fixture-password',
        'access_token': 'original-access', 'refresh_token': 'original-refresh'}}))
    before = path.read_bytes()
    with patch('patchright.async_api.async_playwright'), \
            patch('common.browser.open_and_connect', new=AsyncMock(side_effect=RuntimeError('private browser diagnostic'))), \
            patch('tools.chatgpt_reauthorize._exchange') as exchange, \
            patch.object(asset_ops, '_refresh_document_copies') as save:
        result = asset_ops._reauthorize_chatgpt('import:original')
    assert result['result'] == 'failed'
    assert result['error_code'] == 'authorization_browser_error'
    assert result['evidence']['token_exchange_started'] is False
    assert result['message'] == '授权浏览器启动或运行失败'
    assert 'private browser diagnostic' not in json.dumps(result)
    assert path.read_bytes() == before
    exchange.assert_not_called()
    save.assert_not_called()
    assert asset_ops._emit(result) == 1


def test_unknown_provider_result_has_distinct_exit_code(capsys):
    assert asset_ops._emit({'result': 'unknown'}) == 75
    assert json.loads(capsys.readouterr().out)['result'] == 'failed'
    assert asset_ops._emit({'result': 'relogin_required'}) == 1


@pytest.mark.parametrize('code', ['workspace_deactivated', 'refresh_token_invalidated'])
def test_token_exchange_explicit_rejection_preserves_state(code):
    from tools.chatgpt_reauthorize import _exchange
    from vendor.reauth_web.engine import ReauthError
    from unittest.mock import MagicMock
    response = MagicMock(status_code=401)
    response.json.return_value = {'error': {'code': code, 'message': 'private-response'}}
    with patch('requests.Session') as session:
        session.return_value.__enter__.return_value.post.return_value = response
        with pytest.raises(ReauthError) as caught:
            _exchange('code', 'verifier', 'client', 'http://localhost:1455/auth/callback')
    assert caught.value.evidence['error_code'] == code
    assert caught.value.evidence['token_exchange_rejected'] is True
    assert 'private-response' not in str(caught.value)


@pytest.mark.parametrize('exchange_started,expected,exit_code', [(False, 'failed', 1), (True, 'failed', 75)])
def test_authorization_failure_releases_task_only_when_no_tokens_exchanged(tmp_path, monkeypatch, exchange_started, expected, exit_code):
    from vendor.reauth_web.engine import ReauthError
    monkeypatch.setenv('REG_FACTORY_DATA_DIR', str(tmp_path))
    directory = tmp_path / 'assets/chatgpt/oauth'
    directory.mkdir(parents=True)
    path = directory / 'original.json'
    original = {'ref': 'import:original', 'payload': {'email': 'original@example.test', 'refresh_token': 'original-refresh', 'access_token': 'original-access'}}
    path.write_text(json.dumps(original))
    before = path.read_bytes()
    failure = ReauthError('private-provider-body', evidence={'stage': 'authorize',
        'error_code': 'oauth_tls_handshake_failed', 'token_exchange_started': exchange_started})
    with patch('tools.chatgpt_reauthorize.authorize_document', new=AsyncMock(side_effect=failure)):
        result = asset_ops._reauthorize_chatgpt('import:original')
    assert result['result'] == expected and asset_ops._emit(result) == exit_code
    if expected == 'failed':
        assert result['error_schema'] == 'account.failure.v2'
    else:
        assert 'error_schema' not in result
    assert 'private-provider-body' not in json.dumps(result) and path.read_bytes() == before
    with patch('tools.chatgpt_reauthorize.authorize_document', new=AsyncMock(return_value={'refresh_token': 'new-refresh', 'access_token': 'new-access'})):
        assert asset_ops._reauthorize_chatgpt('import:original')['result'] == 'refreshed'
    assert json.loads(path.read_text())['payload']['refresh_token'] == 'new-refresh'


def test_icloud_login_does_not_reuse_openai_refresh_token():
    record = login_record({'payload': {'email': 'original@icloud.com', 'refresh_token': 'old-openai', 'session_token': 'old-session', 'chatgpt_password': 'password'}})
    assert record['source_type'] == 'mailbox'
    assert record['account_password'] == 'password'
    assert not record['refresh_token']
    assert not record['session_token']


def test_workspace_identity_is_checked_before_persistence():
    doc = {'email': 'original@example.test', 'workspace_account_id': 'workspace-original'}
    token = jwt({'email': doc['email'], 'https://api.openai.com/auth': {'chatgpt_account_id': 'workspace-other'}})
    with pytest.raises(ValueError, match='workspace'):
        validate_identity(doc, {'access_token': token})
    token = jwt({'email': doc['email'], 'https://api.openai.com/auth': {'chatgpt_account_id': 'workspace-original'}})
    validate_identity(doc, {'access_token': token})
    with pytest.raises(ValueError, match='workspace'):
        validate_identity(doc, {'access_token': jwt({'email': doc['email'], 'https://api.openai.com/auth': {'chatgpt_account_id': 'workspace-other'}}), 'id_token': token})


def test_protocol_is_explicit_and_requires_its_own_material():
    from vendor.reauth_web.engine import ReauthError
    doc = {'email': 'original@icloud.com', 'password': 'fixture-password', 'totp': 'JBSWY3DPEHPK3PXP', 'workspace_account_id': 'workspace'}
    token = jwt({'email': doc['email'], 'exp': 4102444800, 'https://api.openai.com/auth': {'chatgpt_account_id': 'workspace'}})
    with patch('vendor.reauth_web.engine.reauth_account', return_value={'payloads': {'cpa': {'access_token': token, 'refresh_token': 'fresh'}}}) as protocol, patch('common.browser.open_and_connect') as browser:
        assert asyncio.run(authorize_document(doc, login_mode='protocol'))['refresh_token'] == 'fresh'
        assert protocol.call_args.args[1].extra['workspace_id'] == 'workspace'
        browser.assert_not_called()
        with pytest.raises(ReauthError) as missing:
            asyncio.run(authorize_document({'email':doc['email']}, login_mode='protocol'))
        assert missing.value.evidence['error_code'] == 'credential_login_material_invalid'
        assert protocol.call_count == 1


def test_login_options_use_original_identity_and_never_expose_material(tmp_path, monkeypatch, capsys):
    import io
    monkeypatch.setenv('REG_FACTORY_DATA_DIR', str(tmp_path))
    folder = tmp_path / 'assets/chatgpt/imported'
    folder.mkdir(parents=True)
    records = {
        'rt': {'email':'token@example.test', 'refresh_token':'fixture-rt', 'access_token':'old'},
        'password': {'email':'password@example.test', 'password':'fixture-password'},
        'twofactor': {'email':'mfa@example.test', 'password':'fixture-password', 'totp':'JBSWY3DPEHPK3PXP'},
        'mail': {'email':'mail@example.test', 'reauthorization':{'email':'mail@example.test','provider':'icmail'}},
    }
    for ref, payload in records.items():
        (folder / (ref+'.json')).write_text(json.dumps({'ref':ref,'payload':payload}))
    monkeypatch.setattr('sys.stdin', io.StringIO(json.dumps(list(records))))
    with patch('requests.Session') as network:
        assert asset_ops.cmd_login_options(SimpleNamespace()) == 0
        network.assert_not_called()
    raw = capsys.readouterr().out
    result = json.loads(raw)
    assert result['rt']['has_refresh_token'] and result['rt']['modes'] == []
    assert result['password']['modes'] == ['browser']
    assert result['twofactor']['modes'] == ['browser','protocol']
    assert result['mail']['modes'] == ['browser']
    assert all(secret not in raw for secret in ('fixture-rt','fixture-password','JBSWY3DPEHPK3PXP'))


@pytest.mark.parametrize('production_proxy', ['', 'http://production.example.test:8888'])
@pytest.mark.parametrize('needs_email', [False, True])
def test_authorization_follows_challenges_and_mail_outage_does_not_block_password(monkeypatch, production_proxy, needs_email):
    from urllib.parse import urlsplit, parse_qs
    monkeypatch.setenv('REG_FACTORY_PRODUCTION_PROXY', production_proxy)
    monkeypatch.setenv('PROXY_MODE', 'residential')
    monkeypatch.setenv('REG_FACTORY_PROXY', 'http://registration.example.test:8889')
    doc = {'payload': {'email': 'original@icloud.com', 'password': 'original-password',
        'totp': 'JBSWY3DPEHPK3PXP', 'workspace_account_id': 'original-workspace'}}
    token = jwt({'email': doc['payload']['email'], 'exp': 4102444800,
        'https://api.openai.com/auth': {'chatgpt_account_id': 'original-workspace'}})
    mail = AsyncMock(return_value='123456')
    if not needs_email:
        mail.prepare.side_effect = RuntimeError('mailbox offline')
    async def drive(page, url, **kwargs):
        assert kwargs['account_password'] == 'original-password'
        assert kwargs['totp_secret'] == 'JBSWY3DPEHPK3PXP'
        assert kwargs['workspace_id'] == 'original-workspace'
        if needs_email:
            assert await kwargs['email_code_provider'](doc['payload']['email'], 1) == '123456'
        return 'fresh-code', parse_qs(urlsplit(url).query)['state'][0], 'ok'
    with patch('patchright.async_api.async_playwright') as playwright, \
            patch('common.browser.open_and_connect', new=AsyncMock(return_value=(object(), 'profile', None, object(), object()))) as connect, \
            patch('common.browser.teardown', new=AsyncMock()) as close, \
            patch('tools.import_plus_codex.MailCodeProvider', return_value=mail), \
            patch('tools.chatgpt_reauthorize._drive_authorization', new=AsyncMock(side_effect=drive)), \
            patch('tools.chatgpt_reauthorize._exchange', return_value={'access_token': token, 'refresh_token': 'fresh'}), \
            patch('vendor.reauth_web.engine.reauth_account') as protocol:
        result = asyncio.run(authorize_document(doc))
    assert result['refresh_token'] == 'fresh'
    mail.prepare.assert_awaited_once()
    protocol.assert_not_called()
    close.assert_awaited_once()
    options = connect.await_args.kwargs['browser_options']
    if production_proxy:
        assert options['host'] == 'production.example.test' and options['port'] == '8888'
    else:
        assert options['proxyType'] == 'noproxy'


def test_central_mailbox_authorizes_without_node_local_mailbox_or_password():
    from urllib.parse import urlsplit, parse_qs
    doc = {'email': 'central@example.test', 'password': '', 'totp': ''}
    mailbox = {'provider': 'imap', 'email': 'inbox@example.test', 'material': {'host': 'imap.example.test'}}
    token = jwt({'email': doc['email']})
    async def drive(page, url, **kwargs):
        assert kwargs['account_password'] == ''
        assert await kwargs['email_code_provider'](doc['email'], 123) == '123456'
        return 'code', parse_qs(urlsplit(url).query)['state'][0], 'ok'
    with patch('patchright.async_api.async_playwright'), \
            patch('common.browser.open_and_connect', new=AsyncMock(return_value=(object(), 'profile', None, object(), object()))), \
            patch('common.browser.teardown', new=AsyncMock()), \
            patch('aishop_node.mailbox.IMAPMailbox') as provider, \
            patch('tools.import_plus_codex.MailCodeProvider') as local_mail, \
            patch('tools.chatgpt_reauthorize._drive_authorization', new=AsyncMock(side_effect=drive)), \
            patch('tools.chatgpt_reauthorize._exchange', return_value={'access_token': token, 'refresh_token': 'fresh'}):
        provider.return_value.wait_for_otp.return_value = '123456'
        assert asyncio.run(authorize_document(doc, mailbox=mailbox))['refresh_token'] == 'fresh'
        provider.assert_called_once()
        provider.return_value.prime.assert_called_once()
        provider.return_value.wait_for_otp.assert_called_once_with(doc['email'], timeout=150, issued_after=123)
        local_mail.assert_not_called()


def test_exchange_uses_same_production_proxy_and_no_ambient_proxy(monkeypatch):
    from unittest.mock import MagicMock
    from tools.chatgpt_reauthorize import _exchange
    monkeypatch.setenv('REG_FACTORY_PRODUCTION_PROXY', 'http://production.example.test:8888')
    monkeypatch.setenv('REG_FACTORY_PROXY_URL', 'http://registration.example.test:8889')
    session = MagicMock()
    session.proxies = {}
    session.post.return_value.json.return_value = {'access_token': 'new-access', 'refresh_token': 'new-refresh'}
    with patch('requests.Session') as factory:
        factory.return_value.__enter__.return_value = session
        assert _exchange('auth-code', 'verifier', 'client', 'http://localhost:1455/auth/callback')['refresh_token'] == 'new-refresh'
    assert session.trust_env is False
    assert session.proxies == {'http': 'http://production.example.test:8888', 'https': 'http://production.example.test:8888'}


@pytest.mark.parametrize('ref,kind', [('import:original', 'oauth'), ('production:original-operation', 'team')])
def test_fresh_authorization_persists_original_copies_but_unknown_preserves_them(tmp_path, monkeypatch, ref, kind):
    monkeypatch.setenv('REG_FACTORY_DATA_DIR', str(tmp_path))
    directory = tmp_path / 'assets' / 'chatgpt' / kind
    directory.mkdir(parents=True)
    path = directory / 'original.json'
    doc = {'ref': ref, 'payload': {'email': 'original@icloud.com', 'refresh_token': 'old', 'access_token': 'old-access', 'oauth_access_token': 'old-access'}}
    path.write_text(json.dumps(doc))
    imported = tmp_path / 'assets' / 'chatgpt' / 'imported'
    imported.mkdir()
    (imported / 'original.json').write_text(json.dumps({'ref': doc['ref'], 'payload': {'password': 'original-password', 'totp': 'JBSWY3DPEHPK3PXP'}}))
    (imported / 'decoy.json').write_text(json.dumps({'ref': 'another-ref', 'payload': {'email': 'original@icloud.com', 'password': 'wrong-password'}}))
    with patch('tools.chatgpt_reauthorize.authorize_document', new=AsyncMock(side_effect=TimeoutError)):
        failure = asset_ops._reauthorize_chatgpt(ref)
        assert failure['result'] == 'failed' and failure['error_code'] == 'oauth_network_timeout'
    assert json.loads(path.read_text()) == doc
    with patch('tools.chatgpt_reauthorize.authorize_document', new=AsyncMock(return_value={'refresh_token': 'new', 'access_token': 'new-access'})) as authorize:
        assert asset_ops._reauthorize_chatgpt(ref)['result'] == 'refreshed'
        source = authorize.call_args.args[0]['payload']
        assert source['password'] == 'original-password'
        assert source['totp'] == 'JBSWY3DPEHPK3PXP'
    payload = json.loads(path.read_text())['payload']
    assert payload['refresh_token'] == 'new'
    assert payload['access_token'] == payload['oauth_access_token'] == 'new-access'


def test_produced_result_reuses_proven_login_identity_after_repeated_rotations(tmp_path, monkeypatch):
    monkeypatch.setenv('REG_FACTORY_DATA_DIR', str(tmp_path))
    email = 'original@icloud.com'
    identity = jwt({'sub': 'original-openai-subject', 'email': email})
    original = {'email': email, 'workspace_account_id': 'original-workspace',
                'access_token': 'old-access', 'refresh_token': 'old-refresh', 'id_token': identity}
    def save(kind, ref, payload):
        path = tmp_path / 'assets' / 'chatgpt' / kind / (ref.replace(':', '_') + '.json')
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps({'ref': ref, 'email': email, 'payload': payload}))
        return path
    produced = save('team', 'production:original-operation', original)
    source = save('oauth', 'import:original', original)
    login = save('imported', 'import:original', {'email': email,
        'reauthorization': {'account_password': 'original-password', 'two_factor': 'JBSWY3DPEHPK3PXP'}})
    source_before, login_before = source.read_bytes(), login.read_bytes()
    for revision in (1, 2):
        rotated = {'access_token': f'new-access-{revision}', 'refresh_token': f'new-refresh-{revision}',
                   'id_token': jwt({'sub': 'original-openai-subject', 'email': email, 'iat': revision})}
        with patch('tools.chatgpt_reauthorize.authorize_document', new=AsyncMock(return_value=rotated)) as authorize:
            assert asset_ops._reauthorize_chatgpt('production:original-operation')['result'] == 'refreshed'
            record = login_record(authorize.call_args.args[0])
            assert record['account_password'] == 'original-password'
            assert record['two_factor'] == 'JBSWY3DPEHPK3PXP'
            assert authorize.call_args.args[0]['payload']['workspace_account_id'] == 'original-workspace'
        current = asset_ops._token_map(asset_ops._find_credential('production:original-operation')[1])
        assert current['refresh_token'] == rotated['refresh_token']
        assert 'reauthorization' not in current and 'password' not in current
        # Old references follow current credentials; login metadata stays separate.
        assert asset_ops._token_map(asset_ops._find_credential('import:original')[1])['refresh_token'] == rotated['refresh_token']
        assert login.read_bytes() == login_before


@pytest.mark.parametrize('sub,email', [('other-subject', 'same@icloud.com'), ('original-subject', 'other@icloud.com'), ('', 'same@icloud.com')])
def test_login_material_never_links_by_email_alone(tmp_path, monkeypatch, sub, email):
    monkeypatch.setenv('REG_FACTORY_DATA_DIR', str(tmp_path))
    selected = {'ref': 'production:original', 'payload': {'email': 'same@icloud.com',
        'refresh_token': 'original-refresh', 'id_token': jwt({'sub': 'original-subject', 'email': 'same@icloud.com'})}}
    for kind, payload in [('oauth', {'email': 'same@icloud.com', 'refresh_token': 'other-refresh',
                                   'id_token': jwt({'sub': sub, 'email': email})}),
                          ('imported', {'email': 'same@icloud.com', 'password': 'must-not-use'})]:
        path = tmp_path / 'assets/chatgpt' / kind / 'decoy.json'
        path.parent.mkdir(parents=True)
        path.write_text(json.dumps({'ref': 'import:decoy', 'payload': payload}))
    assert not login_record(asset_ops._reauthorization_document(selected))['account_password']
