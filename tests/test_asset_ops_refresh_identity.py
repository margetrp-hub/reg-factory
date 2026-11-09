import json
from unittest.mock import Mock, patch

from tools import asset_ops


def test_refresh_uses_original_ref_and_preserves_other_workspace(tmp_path, monkeypatch):
    monkeypatch.setenv("REG_FACTORY_DATA_DIR", str(tmp_path))
    directory = tmp_path / "assets" / "chatgpt" / "oauth"
    directory.mkdir(parents=True)
    email = "same@example.test"
    original = {"email": email, "refresh_token": "original-refresh", "access_token": "old-access",
                "workspace_account_id": "original-workspace"}
    selected = {"ref": "import:" + email, "payload": original}
    decoy = {"ref": "creds:" + email, "payload": {**original, "refresh_token": "wrong-refresh",
                                                    "workspace_account_id": "other-workspace"}}
    chosen_path, decoy_path = directory / "z-original.json", directory / "a-decoy.json"
    chosen_path.write_text(json.dumps(selected))
    decoy_path.write_text(json.dumps(decoy))
    old_directory = tmp_path / "plus-import" / "team-json"
    old_directory.mkdir(parents=True)
    old_path = old_directory / "original.json"
    old_path.write_text(json.dumps(original))
    response = Mock(status_code=200, content=b"json")
    response.json.return_value = {"access_token": "rotated-access", "refresh_token": "rotated-refresh"}
    with patch("requests.Session") as factory:
        factory.return_value.post.return_value = response
        result = asset_ops._refresh_chatgpt(selected["ref"], email)
        assert factory.return_value.post.call_args.kwargs["data"]["refresh_token"] == "original-refresh"
    assert result["result"] == "refreshed"
    assert json.loads(chosen_path.read_text())["payload"]["refresh_token"] == "rotated-refresh"
    assert asset_ops._token_map(asset_ops._find_credential('original')[1])["refresh_token"] == "rotated-refresh"
    assert json.loads(decoy_path.read_text()) == decoy


def test_missing_subject_does_not_fall_back_to_same_email(tmp_path, monkeypatch):
    monkeypatch.setenv("REG_FACTORY_DATA_DIR", str(tmp_path))
    directory = tmp_path / "assets" / "chatgpt" / "oauth"
    directory.mkdir(parents=True)
    (directory / "decoy.json").write_text(json.dumps({"ref": "creds:other", "email": "same@example.test",
                                                    "refresh_token": "wrong-refresh"}))
    with patch("requests.Session") as factory:
        result = asset_ops._refresh_chatgpt("missing-original-subject", "same@example.test")
        factory.assert_not_called()
    assert result["result"] == "relogin_required"


def test_refresh_uses_production_route_and_keeps_unrotated_id_token(tmp_path, monkeypatch):
    monkeypatch.setenv("REG_FACTORY_DATA_DIR", str(tmp_path))
    monkeypatch.setenv("REG_FACTORY_PROXY_URL", "http://registration-proxy.invalid:8080")
    monkeypatch.setenv("REG_FACTORY_PRODUCTION_PROXY", "http://production-proxy.invalid:8080")
    directory = tmp_path / "assets" / "chatgpt" / "oauth"
    directory.mkdir(parents=True)
    path = directory / "one.json"
    path.write_text(json.dumps({"ref": "one", "payload": {
        "access_token": "fixture-old-at", "refresh_token": "fixture-rt", "id_token": "fixture-id"
    }}))
    response = Mock(status_code=200, content=b"json")
    response.json.return_value = {"access_token": "fixture-new-at", "expires_in": 3600}
    with patch("requests.Session") as factory:
        factory.return_value.post.return_value = response
        assert asset_ops._refresh_chatgpt("one")["result"] == "refreshed"
        factory.return_value.proxies.update.assert_called_once_with({
            "http": "http://production-proxy.invalid:8080", "https": "http://production-proxy.invalid:8080"
        })
        assert factory.return_value.trust_env is False
        assert factory.return_value.post.call_count == 1
    credentials = json.loads(path.read_text())["payload"]
    assert credentials["id_token"] == "fixture-id"
    assert credentials["refresh_token"] == "fixture-rt"


def test_old_reference_uses_last_success_failure_preserves_it_and_retry_advances(tmp_path, monkeypatch):
    monkeypatch.setenv('REG_FACTORY_DATA_DIR', str(tmp_path))
    monkeypatch.setenv('REG_FACTORY_RUN_ID', 'work-first')
    directory = tmp_path / 'assets/chatgpt/oauth'
    directory.mkdir(parents=True)
    for ref, stamp in [('old', '2026-11-01T00:00:00Z'), ('current', '2026-11-02T00:00:00Z')]:
        (directory / (ref + '.json')).write_text(json.dumps({'ref': ref, 'email': 'same@example.test',
            'credential_updated_at': stamp, 'payload': {'access_token': ref, 'refresh_token': ref + '-rt',
            'workspace_account_id': 'one'}}))
    response = Mock(status_code=400, content=b'json')
    response.json.return_value = {'error': 'invalid_grant'}
    before = (directory / 'current.json').read_bytes()
    with patch('requests.Session') as factory:
        factory.return_value.post.return_value = response
        result = asset_ops._refresh_chatgpt('old')
        assert factory.return_value.post.call_args.kwargs['data']['refresh_token'] == 'current-rt'
    assert asset_ops._emit(result) == 1
    assert (directory / 'current.json').read_bytes() == before
    assert not (tmp_path / 'credential-task-results').exists()
    response.status_code = 200
    response.json.return_value = {'access_token': 'new', 'refresh_token': 'new-rt'}
    with patch('requests.Session') as factory:
        factory.return_value.post.return_value = response
        assert asset_ops._refresh_chatgpt('old')['result'] == 'refreshed'
    for ref in ('old', 'current'):
        saved = asset_ops._find_credential(ref)[1]
        assert asset_ops._token_map(saved)['refresh_token'] == 'new-rt'
        assert saved['credential_updated_at'] > '2026-11-02'
    snapshot = next((tmp_path / 'credential-task-results').glob('*.json'))
    original = snapshot.read_bytes()
    monkeypatch.setenv('REG_FACTORY_RUN_ID', 'work-reuse')
    response.json.return_value = {'access_token': 'reused', 'refresh_token': 'reused-rt'}
    with patch('requests.Session') as factory:
        factory.return_value.post.return_value = response
        assert asset_ops._refresh_chatgpt('old')['result'] == 'refreshed'
    assert snapshot.read_bytes() == original
    assert asset_ops._token_map(asset_ops._find_credential('old')[1])['access_token'] == 'reused'


def test_stale_save_and_parallel_alias_cannot_replace_latest(tmp_path, monkeypatch):
    import pytest
    from common.credential_versions import account_lock
    monkeypatch.setenv('REG_FACTORY_DATA_DIR', str(tmp_path))
    directory = tmp_path / 'assets/chatgpt/oauth'
    directory.mkdir(parents=True)
    path = directory / 'one.json'
    source = {'ref': 'one', 'email': 'same@example.test', 'payload': {'access_token': 'old', 'refresh_token': 'old-rt'}}
    path.write_text(json.dumps(source))
    with account_lock(tmp_path, source['email'], 'one'), patch('requests.Session') as factory:
        assert asset_ops._refresh_chatgpt('one')['result'] == 'failed'
        factory.assert_not_called()
    path.write_text(json.dumps({**source, 'payload': {'access_token': 'new', 'refresh_token': 'new-rt'}}))
    before = path.read_bytes()
    with pytest.raises(OSError, match='CREDENTIAL_CHANGED'):
        asset_ops._refresh_document_copies('one', source['email'], path, source, {'access_token': 'stale', 'refresh_token': 'stale-rt'})
    assert path.read_bytes() == before


def test_failed_exchange_retry_reauthorizes_across_alias_without_replaying_rt(tmp_path, monkeypatch):
    import sqlite3
    monkeypatch.setenv('REG_FACTORY_DATA_DIR', str(tmp_path))
    monkeypatch.setenv('REG_FACTORY_RUN_ID', 'new-attempt')
    email = 'retry@example.test'
    directory = tmp_path / 'assets/chatgpt/oauth'
    directory.mkdir(parents=True)
    (directory / 'one.json').write_text(json.dumps({'ref': 'current', 'email': email,
        'payload': {'access_token': 'old-at', 'refresh_token': 'possibly-consumed-rt'}}))
    with sqlite3.connect(tmp_path / 'regd.db') as db:
        db.executescript('''
            CREATE TABLE accounts (ref TEXT, identity TEXT, platform TEXT);
            CREATE TABLE tasks (id TEXT, script TEXT, args_json TEXT, status TEXT,
                exit_code INTEGER, finished_at TEXT, created_at TEXT);
        ''')
        db.executemany('INSERT INTO accounts VALUES (?, ?, ?)',
                       [(ref, email, 'chatgpt') for ref in ('current', 'old-reference')])
        db.executemany('INSERT INTO tasks VALUES (?, ?, ?, ?, ?, ?, ?)', [
            ('lost-response', 'asset_ops', json.dumps(['refresh', '--ref', 'old-reference']),
             'failed', 75, '2026-11-10T01:00:00Z', '2026-11-10T01:00:00Z'),
            ('failed-retry', 'asset_ops', json.dumps(['refresh', '--ref', 'current']),
             'failed', 1, '2026-11-10T02:00:00Z', '2026-11-10T02:00:00Z')])
    with patch('requests.Session') as exchange, patch.object(asset_ops._reauthorize_chatgpt, '__wrapped__') as authorize:
        authorize.return_value = {'ref': 'current', 'result': 'refreshed'}
        assert asset_ops._refresh_chatgpt('current', email)['result'] == 'refreshed'
        authorize.assert_called_once_with('current', email)
        exchange.assert_not_called()
    with sqlite3.connect(tmp_path / 'regd.db') as db:
        db.execute('INSERT INTO tasks VALUES (?, ?, ?, ?, ?, ?, ?)',
                   ('recovered', 'asset_ops', json.dumps(['refresh', '--ref', 'current', '--allow-relogin']),
                    'succeeded', 0, '2026-11-10T03:00:00Z', '2026-11-10T03:00:00Z'))
    assert not asset_ops._retry_needs_authorization(tmp_path, email, 'old-reference')
