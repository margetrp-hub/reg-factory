import base64
import json
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

import pytest

from tools import asset_ops
from tools.chatgpt_reauthorize import login_record, validate_identity


def jwt(claims):
    return 'head.' + base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip('=') + '.sig'


def test_authorize_does_not_replay_old_refresh_grant():
    args = SimpleNamespace(platform='chatgpt', allow_relogin=True, ref='original', email='', timeout=60)
    with patch.object(asset_ops, '_refresh_chatgpt') as refresh, patch.object(asset_ops, '_reauthorize_chatgpt', return_value={'result': 'refreshed'}) as authorize:
        assert asset_ops.cmd_refresh(args) == 0
        refresh.assert_not_called()
        authorize.assert_called_once_with('original', '', 60)


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


def test_fresh_authorization_persists_original_copies_but_unknown_preserves_them(tmp_path, monkeypatch):
    monkeypatch.setenv('REG_FACTORY_DATA_DIR', str(tmp_path))
    directory = tmp_path / 'assets' / 'chatgpt' / 'oauth'
    directory.mkdir(parents=True)
    path = directory / 'original.json'
    doc = {'ref': 'import:original', 'payload': {'email': 'original@icloud.com', 'refresh_token': 'old', 'access_token': 'old-access', 'oauth_access_token': 'old-access'}}
    path.write_text(json.dumps(doc))
    imported = tmp_path / 'assets' / 'chatgpt' / 'imported'
    imported.mkdir()
    (imported / 'original.json').write_text(json.dumps({'ref': doc['ref'], 'payload': {'password': 'original-password', 'totp': 'JBSWY3DPEHPK3PXP'}}))
    (imported / 'decoy.json').write_text(json.dumps({'ref': 'another-ref', 'payload': {'email': 'original@icloud.com', 'password': 'wrong-password'}}))
    with patch('tools.chatgpt_reauthorize.authorize_document', new=AsyncMock(side_effect=TimeoutError)):
        assert asset_ops._reauthorize_chatgpt('import:original')['result'] == 'unknown'
    assert json.loads(path.read_text()) == doc
    with patch('tools.chatgpt_reauthorize.authorize_document', new=AsyncMock(return_value={'refresh_token': 'new', 'access_token': 'new-access'})) as authorize:
        assert asset_ops._reauthorize_chatgpt('import:original')['result'] == 'refreshed'
        source = authorize.call_args.args[0]['payload']
        assert source['password'] == 'original-password'
        assert source['totp'] == 'JBSWY3DPEHPK3PXP'
    payload = json.loads(path.read_text())['payload']
    assert payload['refresh_token'] == 'new'
    assert payload['access_token'] == payload['oauth_access_token'] == 'new-access'
