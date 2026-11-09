"""Exercise the same login boundary through retries, failure evidence and recovery."""
from types import SimpleNamespace
from unittest.mock import Mock, patch

import pytest

from vendor.reauth_web.engine import failure_evidence
from vendor.reauth_web.lib.oauth_client import OAuthClient
from vendor.reauth_web.lib.sentinel_token import fetch_sentinel_challenge


class TransportFailure(Exception):
    def __init__(self, code):
        self.code = code
        super().__init__('private-proxy-password must never be logged')


def response(status=200):
    return SimpleNamespace(status_code=status, json=lambda: {'token': 'private-challenge'})


def test_transient_tls_retries_same_step_then_continues_without_replaying_login():
    session = Mock()
    session.post.side_effect = [TransportFailure(35), TransportFailure(28), response()]
    details, retries = {}, []
    with patch('vendor.reauth_web.lib.sentinel_token.time.sleep') as pause:
        assert fetch_sentinel_challenge(session, 'device', failure_details=details, on_retry=retries.append) == {'token': 'private-challenge'}
    assert session.post.call_count == 3 and details == {}
    assert [call.args for call in pause.call_args_list] == [(1,), (3,)]
    assert [item['attempt'] for item in retries] == [1, 2]
    assert all(item['token_exchange_started'] is False for item in retries)
    assert 'private-' not in str(retries)


@pytest.mark.parametrize('error,count,expected', [(35, 3, 'oauth_tls_handshake_failed'), (60, 1, 'oauth_certificate_verification_failed')])
def test_exhausted_or_certificate_failure_preserves_real_cause_without_more_requests(error, count, expected):
    session = Mock()
    session.post.side_effect = TransportFailure(error)
    details, retries = {}, []
    with patch('vendor.reauth_web.lib.sentinel_token.time.sleep'):
        assert fetch_sentinel_challenge(session, 'device', failure_details=details, on_retry=retries.append) is None
    assert session.post.call_count == count
    assert details == {'error_code': expected, 'http_status': 0, 'page_type': 'login-check', 'attempts': count}
    assert len(retries) == count-1 and 'private-' not in str(details)


def test_http_rejection_is_not_retried_or_misreported_as_prior_redirect():
    client = OAuthClient({}, verbose=False)
    client.session.close()
    client.session = Mock()
    client.session.post.return_value = response(403)
    with patch('vendor.reauth_web.lib.sentinel_token.time.sleep') as pause:
        result = client._follow_external_authorize_for_callback('https://auth.openai.com/log-in',
            expected_state='state', redirect_uri='http://localhost:1455/auth/callback',
            user_agent='test', impersonate=None, sms_client=None, email='original@example.test',
            password='private-password', mail_client=None, mfa_client=None, device_id='device',
            max_hops=2, oauth_protocol=(protocol := {'last_http_status': 302}))
    result['oauth_protocol'] = protocol
    assert failure_evidence(result) == {'stage': 'authorize', 'error_code': 'oauth_login_check_http_error',
        'http_status': 403, 'page_type': 'login-check', 'token_exchange_started': False}
    assert client.session.post.call_count == 1 and not pause.called
    client.session.request.assert_not_called()
