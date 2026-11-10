import json
from unittest.mock import AsyncMock, patch

from tools import asset_ops


def test_import_authorization_publishes_complete_credentials_and_keeps_login(tmp_path, monkeypatch):
    monkeypatch.setenv("REG_FACTORY_DATA_DIR", str(tmp_path))
    monkeypatch.setenv("REG_FACTORY_RUN_ID", "import-authorization")
    source = tmp_path / "assets/chatgpt/imported/first.json"
    source.parent.mkdir(parents=True)
    original = {"ref": "import:one@example.test", "platform": "chatgpt", "kind": "imported",
                "email": "one@example.test", "payload": {"password": "fixture-password", "totp": "fixture-totp"}}
    source.write_text(json.dumps(original))
    tokens = {"access_token": "new-at", "refresh_token": "new-rt", "id_token": "new-id"}
    with patch("tools.chatgpt_reauthorize.authorize_document", new=AsyncMock(return_value=tokens)) as authorize:
        result = asset_ops._reauthorize_chatgpt(original["ref"])
    assert result["result"] == "refreshed"
    assert authorize.call_args.args[0]["payload"]["password"] == "fixture-password"
    oauth = list((tmp_path / "assets/chatgpt/oauth").glob("*.json"))
    assert len(oauth) == 1
    saved = json.loads(oauth[0].read_text())
    assert saved["ref"] == original["ref"] and saved["kind"] == "oauth"
    assert all(saved["payload"][key] == value for key, value in tokens.items())
    assert "password" not in saved["payload"] and "totp" not in saved["payload"]
    assert json.loads(source.read_text())["payload"]["password"] == "fixture-password"
    assert asset_ops._find_credential(original["ref"])[0] == oauth[0]
    # A subsequent explicit RT refresh sees this exact canonical result.
    assert len(list((tmp_path / "oauth-results").glob("*.json"))) == 1


def test_import_failure_then_repair_uses_same_account_and_preserves_material(tmp_path, monkeypatch):
    from vendor.reauth_web.engine import ReauthError
    monkeypatch.setenv("REG_FACTORY_DATA_DIR", str(tmp_path))
    source = tmp_path / "assets/chatgpt/imported/first.json"
    source.parent.mkdir(parents=True)
    source.write_text(json.dumps({"ref": "import:one", "platform": "chatgpt", "kind": "imported",
        "email": "one@example.test", "payload": {"password": "fixture-password", "totp": "fixture-totp"}}))
    before = source.read_bytes()
    failure = ReauthError("private", evidence={"stage": "authorize", "token_exchange_started": False,
                                             "error_code": "oauth_tls_handshake_failed"})
    with patch("tools.chatgpt_reauthorize.authorize_document", new=AsyncMock(side_effect=failure)):
        assert asset_ops._reauthorize_chatgpt("import:one")["result"] == "failed"
    assert source.read_bytes() == before and not (tmp_path / "assets/chatgpt/oauth").exists()
    tokens = {"access_token": "new-at", "refresh_token": "new-rt", "id_token": "new-id"}
    with patch("tools.chatgpt_reauthorize.authorize_document", new=AsyncMock(return_value=tokens)):
        assert asset_ops._reauthorize_chatgpt("import:one")["result"] == "refreshed"
    assert len(list((tmp_path / "assets/chatgpt/oauth").glob("*.json"))) == 1
