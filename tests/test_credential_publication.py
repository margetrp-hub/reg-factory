import json
from unittest.mock import patch

import pytest

from tools import asset_ops


@pytest.fixture
def publication(tmp_path, monkeypatch):
    monkeypatch.setenv('REG_FACTORY_DATA_DIR', str(tmp_path))
    monkeypatch.setenv('REG_FACTORY_RUN_ID', 'original-task')
    path = tmp_path / 'assets/chatgpt/oauth/account.json'
    path.parent.mkdir(parents=True)
    original = {'ref': 'one', 'platform': 'chatgpt', 'kind': 'oauth', 'email': 'one@example.test',
                'payload': {'access_token': 'old-at', 'refresh_token': 'old-rt', 'id_token': 'old-id'}}
    path.write_text(json.dumps(original))
    updates = {'access_token': 'new-at', 'refresh_token': 'new-rt', 'id_token': 'new-id'}
    return tmp_path, path, original, updates


@pytest.mark.parametrize('boundary', ['receipt', 'current', 'after-current'])
def test_saved_result_recovers_without_another_provider_call(publication, boundary):
    root, path, original, updates = publication
    write = asset_ops._atomic_json_write

    def crash(destination, document):
        if boundary == 'receipt' and destination.parent.name == 'credential-task-results':
            raise OSError('receipt interrupted')
        if boundary == 'current' and destination == path:
            raise OSError('publication interrupted')
        write(destination, document)

    with patch.object(asset_ops, '_atomic_json_write', side_effect=crash):
        if boundary == 'after-current':
            asset_ops._refresh_document_copies('one', original['email'], path, original, updates)
        else:
            with pytest.raises(OSError):
                asset_ops._refresh_document_copies('one', original['email'], path, original, updates)
    with patch('requests.Session') as network:
        assert asset_ops._recover_publication('one')['result'] == 'refreshed'
        saved = path.read_bytes()
        assert asset_ops._recover_publication('one')['result'] == 'refreshed'
        assert path.read_bytes() == saved
        network.assert_not_called()
    assert json.loads(saved)['payload']['refresh_token'] == 'new-rt'
    assert len(list((root / 'oauth-results').glob('*.json'))) == 1


def test_recovery_cannot_overwrite_a_later_account_reuse(publication):
    _, path, original, updates = publication
    asset_ops._refresh_document_copies('one', original['email'], path, original, updates)
    newer = json.loads(path.read_text())
    newer['credential_updated_at'] = '2099-01-01T00:00:00Z'
    newer['payload']['refresh_token'] = 'later-rt'
    path.write_text(json.dumps(newer))
    before = path.read_bytes()
    assert asset_ops._recover_publication('one')['result'] == 'failed'
    assert path.read_bytes() == before


def test_recovery_is_bound_to_original_task_and_account(publication, monkeypatch):
    _, path, original, updates = publication
    asset_ops._refresh_document_copies('one', original['email'], path, original, updates)
    assert asset_ops._recover_publication('different-account')['result'] == 'failed'
    monkeypatch.setenv('REG_FACTORY_RUN_ID', 'different-task')
    assert asset_ops._recover_publication('one')['result'] == 'not_saved'


def test_retry_same_task_returns_saved_result_without_using_rt(publication):
    _, path, original, updates = publication
    asset_ops._refresh_document_copies('one', original['email'], path, original, updates)
    with patch('requests.Session') as network:
        assert asset_ops._refresh_chatgpt('one')['result'] == 'refreshed'
        network.assert_not_called()
