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
    assert json.loads(old_path.read_text())["refresh_token"] == "rotated-refresh"
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
