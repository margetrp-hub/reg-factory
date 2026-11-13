from unittest.mock import patch

import pytest

from common.proxy_switch import production_proxy


def test_shared_proxy_uses_current_pool_and_keeps_explicit_direct(tmp_path):
    state = tmp_path / "pool-index"
    state.write_text("1")
    values = {"REG_FACTORY_PRODUCTION_PROXY_SOURCE": "residential",
              "REG_FACTORY_PRODUCTION_PROXY": "http://unused.test:1",
              "REG_FACTORY_PROXY_POOL": "http://first.test:80;http://second.test:80",
              "REG_FACTORY_PROXY_STATE_FILE": str(state)}
    assert production_proxy(values) == "http://second.test:80"
    assert production_proxy({**values, "REG_FACTORY_PRODUCTION_PROXY_SOURCE": "direct"}) == ""
    with pytest.raises(ValueError, match="未配置"):
        production_proxy({"REG_FACTORY_PRODUCTION_PROXY_SOURCE": "residential"})


@pytest.mark.parametrize("source,base_key,key_key", [
    ("primary", "VISION_API_BASE", "VISION_API_KEY"),
    ("vote", "ZZ_BASE", "ZZ_KEY"),
])
def test_claude_reuses_selected_gateway_without_discovery_or_custom_credentials(source, base_key, key_key):
    import register
    from common import agent_captcha
    with patch.object(register, "CLAUDE_VISION_SOURCE", source), \
         patch.object(register, "CLAUDE_VISION_API_BASE", "https://unused.test"), \
         patch.object(register, "CLAUDE_VISION_API_KEY", "unused"), \
         patch.object(register, "CLAUDE_VISION_MODEL", "fixture-model"), \
         patch.object(agent_captcha, base_key, "http://shared.test"), \
         patch.object(agent_captcha, key_key, "shared-key"), \
         patch.object(register.requests, "get") as discover:
        assert register._select_claude_vision_voter() == ("http://shared.test", "shared-key", "fixture-model")
        discover.assert_not_called()
        with patch.object(agent_captcha, key_key, ""):
            with pytest.raises(ValueError, match="所选连接未配置"):
                register._select_claude_vision_voter()
        assert register._select_claude_vision_voter()[1] == "shared-key"
