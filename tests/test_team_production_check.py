import base64
import json
import time
from unittest.mock import Mock
import pytest

from tools.team_production_check import verify


@pytest.mark.parametrize('http,code', [(401, 'account_banned'), (401, 'workspace_deactivated'), (403, 'deactivated_workspace')])
def test_explicit_disabled_result_is_terminal_without_refresh(http, code):
    claims = {'exp': int(time.time())+600, 'https://api.openai.com/auth': {'chatgpt_account_id': 'fixture-workspace'}}
    token = 'fixture.' + base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip('=') + '.fixture'
    response = Mock(status_code=http)
    response.json.return_value = {'error': {'code': code}}
    get = Mock(return_value=response)
    assert verify({'access_token': token}, get=get) == {'status': 'failed', 'error_code': 'PRODUCTION_CREDENTIAL_' + code.upper()}
    assert get.call_count == 1
