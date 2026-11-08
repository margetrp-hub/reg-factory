import json
import hashlib
from unittest.mock import Mock, patch

from tools import asset_ops, team_production_check


def test_producer_retains_each_original_and_current_export(tmp_path, monkeypatch):
    from common import session_export
    monkeypatch.setenv('REG_FACTORY_DATA_DIR', str(tmp_path))
    monkeypatch.setattr(session_export, 'TOKEN_OUTPUT_DIR', str(tmp_path / 'tokens'))
    for version in ('first', 'second'):
        assert session_export.save_codex_oauth_credentials({'access_token': version,
            'refresh_token': version+'-rt', 'workspace_account_id': 'workspace'}, 'child@example.test')
    versions = [json.loads(p.read_text(encoding='utf-8')) for p in (tmp_path / 'asset-results').glob('*.json')]
    assert {value['payload']['access_token'] for value in versions} == {'first', 'second'}
    current = json.loads(next((tmp_path / 'assets/chatgpt/oauth').glob('*.json')).read_text(encoding='utf-8'))
    assert current['payload']['access_token'] == 'second'
    assert current['payload']['workspace_account_id'] == 'workspace'
    for path in (tmp_path / 'asset-results').glob('*.json'):
        assert hashlib.sha256(path.read_bytes()).hexdigest() == path.stem


def test_partial_projection_failure_keeps_supplier_result_and_both_versions(tmp_path, monkeypatch):
    monkeypatch.setenv('REG_FACTORY_DATA_DIR', str(tmp_path))
    path = tmp_path / 'assets/chatgpt/oauth/child.json'
    path.parent.mkdir(parents=True)
    original = {'ref': 'import:child@example.test', 'platform': 'chatgpt', 'kind': 'oauth',
                'payload': {'email': 'child@example.test', 'access_token': 'before', 'refresh_token': 'before-rt'}}
    path.write_text(json.dumps(original))
    real_write = asset_ops._atomic_json_write
    def fail_projection(destination, doc):
        if destination == path:
            raise OSError('simulated projection failure')
        real_write(destination, doc)
    with patch.object(asset_ops, '_atomic_json_write', side_effect=fail_projection):
        try:
            asset_ops._refresh_document_copies(original['ref'], 'child@example.test', path, original,
                {'access_token': 'after', 'refresh_token': 'after-rt'})
        except OSError:
            pass
        else:
            raise AssertionError('did not reach failing projection')
    assert json.loads(path.read_text()) == original
    manifest = json.loads(next((tmp_path / 'oauth-results').glob('*.json')).read_text())
    assert manifest['updates']['refresh_token'] == 'after-rt'
    versions = [json.loads(p.read_text())['payload']['access_token'] for p in (tmp_path / 'asset-results').glob('*.json')]
    assert set(versions) == {'before', 'after'}
    for path in (tmp_path / 'asset-results').glob('*.json'):
        assert hashlib.sha256(path.read_bytes()).hexdigest() == path.stem


def test_same_ref_different_session_is_not_overwritten(tmp_path, monkeypatch):
    monkeypatch.setenv('REG_FACTORY_DATA_DIR', str(tmp_path))
    directory = tmp_path / 'assets/chatgpt/oauth'
    directory.mkdir(parents=True)
    old = {'ref': 'import:child', 'payload': {'access_token': 'a', 'refresh_token': 'rt-a'}}
    other = {'ref': 'import:child', 'payload': {'access_token': 'b', 'refresh_token': 'rt-b'}}
    (directory / 'a.json').write_text(json.dumps(old))
    (directory / 'b.json').write_text(json.dumps(other))
    with patch('requests.Session') as request:
        result = asset_ops._refresh_chatgpt('import:child')
    assert result['result'] == 'unknown'
    request.assert_not_called()
    assert not asset_ops._same_oauth_source(old, other)


def test_unexplained_401_is_unknown_and_revoked_is_exact_failure():
    import base64
    token = 'e30.' + base64.urlsafe_b64encode(json.dumps({'exp': 4102444800}).encode()).decode().rstrip('=') + '.test'
    response = Mock(status_code=401)
    response.json.return_value = {'error': 'proxy authorization'}
    get = Mock(return_value=response)
    assert team_production_check.verify({'access_token': token}, get)['status'] == 'unknown'
    response.json.return_value = {'error': {'code': 'token_revoked'}}
    assert team_production_check.verify({'access_token': token}, get)['error_code'] == 'PRODUCTION_CREDENTIAL_TOKEN_REVOKED'
    assert all(call.args[0].endswith('/wham/usage') for call in get.call_args_list)
