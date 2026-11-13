"""Read-only verification of a frozen TEAM JSON; stdout contains no credentials."""
import base64
import json
import os
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from common.oauth_errors import account_failure

def verify(document, get=None):
    token = document['access_token']
    segment = token.split('.')[1]
    claims = json.loads(base64.urlsafe_b64decode(segment+'='*(-len(segment)%4)))
    auth = claims.get('https://api.openai.com/auth', {})
    workspace = auth.get('chatgpt_account_id')
    if claims.get('exp', 0) <= int(time.time()):
        return {'status': 'failed', 'error_code': 'PRODUCTION_CREDENTIAL_EXPIRED'}
    if get is None:
        from curl_cffi import requests
        # Verification has its own route. Registration's residential proxy is
        # neither necessary nor an authority to change the retained artifact.
        proxy = os.environ.get('REG_FACTORY_PRODUCTION_PROXY', '').strip()
        session = requests.Session(impersonate='chrome', trust_env=False,
            proxies={'http': proxy, 'https': proxy} if proxy else None)
        get = session.get
    response = get('https://chatgpt.com/backend-api/wham/usage',
        headers={'Authorization': 'Bearer '+token, 'ChatGPT-Account-Id': workspace}, timeout=12)
    if 400 <= response.status_code < 500:
        try:
            explicit = account_failure(response.json())
        except ValueError:
            explicit = None
        if explicit:
            return {'status': 'failed', 'error_code': 'PRODUCTION_CREDENTIAL_' + explicit['error_code'].upper()}
    if response.status_code == 401:
        try:
            failure = response.json().get('error', {})
            code = failure.get('code') if isinstance(failure, dict) else ''
        except (ValueError, AttributeError):
            code = ''
        if code in {'token_revoked', 'token_expired', 'invalid_token', 'account_deactivated'}:
            return {'status': 'failed', 'error_code': 'PRODUCTION_CREDENTIAL_' + code.upper()}
        return {'status': 'unknown', 'error_code': 'PRODUCTION_CHECK_HTTP_401'}
    if response.status_code != 200:
        return {'status': 'unknown', 'error_code': 'PRODUCTION_CHECK_HTTP_'+str(response.status_code)}
    value = response.json()
    email = str(value.get('email', '')).lower()
    plans = {'team': 'standard', 'business': 'standard', 'team_standard': 'standard',
        'self_serve_business': 'standard', 'self_serve_business_prolite': 'premium', 'team_prolite': 'premium',
        'business_prolite': 'premium', 'team_premium': 'premium'}
    expected = plans.get(str(auth.get('chatgpt_plan_type', '')).lower())
    live = plans.get(str(value.get('plan_type', '')).lower())
    if email and email != str(document.get('email', '')).lower():
        return {'status': 'failed', 'error_code': 'PRODUCTION_IDENTITY_MISMATCH'}
    # Some usage responses report only the workspace's team plan, while the
    # signed token identifies its member seat. Neither may be free/unknown.
    if not expected or not live:
        return {'status': 'failed', 'error_code': 'PRODUCTION_TEAM_PLAN_REQUIRED'}
    return {'status': 'completed', 'error_code': '', 'valid_until': claims['exp']}

if __name__ == '__main__':
    try:
        result = verify(json.load(sys.stdin))
    except Exception:
        result = {'status': 'unknown', 'error_code': 'PRODUCTION_CHECK_UNAVAILABLE'}
    print(json.dumps(result, separators=(',', ':')))
