import base64
import json
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

import pytest

from tools import asset_ops
from tools.chatgpt_reauthorize import login_record, validate_identity
from tools.chatgpt_reauthorize import authorize_document
import asyncio


def jwt(claims):
    return 'head.' + base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip('=') + '.sig'


def test_authorize_does_not_replay_old_refresh_grant():
    args = SimpleNamespace(platform='chatgpt', allow_relogin=True, ref='original', email='', timeout=60)
    with patch.object(asset_ops, '_refresh_chatgpt') as refresh, patch.object(asset_ops, '_reauthorize_chatgpt', return_value={'result': 'refreshed'}) as authorize:
        assert asset_ops.cmd_refresh(args) == 0
        refresh.assert_not_called()
        authorize.assert_called_once_with('original', '', 60)


def test_unknown_provider_result_has_distinct_exit_code():
    assert asset_ops._emit({'result': 'unknown'}) == 75
    assert asset_ops._emit({'result': 'relogin_required'}) == 1


@pytest.mark.parametrize('exchange_started,expected,exit_code', [(False, 'failed', 1), (True, 'unknown', 75)])
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


def test_password_totp_uses_supplier_protocol_and_preserves_workspace():
    doc = {'payload': {'email': 'original@icloud.com', 'password': 'original-password', 'totp': 'JBSWY3DPEHPK3PXP', 'workspace_account_id': 'original-workspace'}}
    token = jwt({'email': doc['payload']['email'], 'exp': 4102444800, 'https://api.openai.com/auth': {'chatgpt_account_id': 'original-workspace'}})
    with patch('vendor.reauth_web.engine.reauth_account', return_value={'payloads': {'cpa': {'access_token': token, 'refresh_token': 'fresh'}}}) as reauth:
        result = asyncio.run(authorize_document(doc))
    account, settings = reauth.call_args.args
    assert account.password == 'original-password'
    assert account.totp_secret == 'JBSWY3DPEHPK3PXP'
    assert settings.extra['workspace_id'] == 'original-workspace'
    assert result['refresh_token'] == 'fresh'


@pytest.mark.parametrize('production_proxy', ['', 'http://production.example.test:8888'])
def test_work_authorization_uses_production_egress_without_inheriting_registration_proxy(monkeypatch, production_proxy):
    monkeypatch.setenv('REG_FACTORY_PRODUCTION_PROXY', production_proxy)
    monkeypatch.setenv('PROXY_MODE', 'residential')
    monkeypatch.setenv('REG_FACTORY_PROXY', 'http://registration.example.test:8889')
    doc = {'payload': {'email': 'original@icloud.com', 'password': 'original-password', 'totp': 'JBSWY3DPEHPK3PXP'}}
    token = jwt({'email': doc['payload']['email'], 'exp': 4102444800})
    with patch('vendor.reauth_web.engine.reauth_account', return_value={'payloads': {'cpa': {'access_token': token, 'refresh_token': 'fresh'}}}) as reauth:
        asyncio.run(authorize_document(doc))
    assert reauth.call_args.args[1].proxy == production_proxy
    assert callable(reauth.call_args.kwargs['on_retry'])


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
        assert asset_ops._reauthorize_chatgpt(ref)['result'] == 'unknown'
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
        current = json.loads(produced.read_text())['payload']
        assert current['refresh_token'] == rotated['refresh_token']
        assert 'reauthorization' not in current and 'password' not in current
        # Other references remain independent OAuth results and login metadata.
        assert source.read_bytes() == source_before
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
