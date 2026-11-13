import copy
import os
import sys
import time
import unittest
from unittest.mock import patch
from pathlib import Path

sys.path[:0] = [str(Path(__file__).resolve().parents[1]), str(Path(__file__).resolve().parents[1] / 'shared/node-sdk/python')]
from tools.central_asset import execute
from unittest.mock import Mock
import base64
import json


def jwt(**claims):
    return 'fixture.' + base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip('=') + '.fixture'


class CentralExecutionTests(unittest.TestCase):
    def test_authorization_retains_explicit_state_but_not_unconfirmed_tokens(self):
        from vendor.reauth_web.engine import ReauthError, failure_evidence
        for code in ('account_banned', 'workspace_deactivated', 'deactivated_workspace', 'mfa_totp_secret_required'):
            error = ReauthError('private-body', evidence=failure_evidence({'error': code, 'status': 401}))
            with self.subTest(code=code), patch('tools.chatgpt_reauthorize.authorize_document', side_effect=error):
                result = execute({'action': 'account.authorize', 'expires_at': time.time()+600}, {'document': {'email': 'fixture@example.invalid'}})
                self.assertEqual(result['status'], 'failed')
                self.assertEqual(result['error_code'], code.upper())
                self.assertNotIn('private-body', str(result))
        async def received_then_failed(document, **kwargs):
            kwargs['on_tokens']({'access_token': 'unconfirmed-at'})
            raise error
        with patch('tools.chatgpt_reauthorize.authorize_document', side_effect=received_then_failed):
            result = execute({'action': 'account.authorize', 'expires_at': time.time()+600}, {'document': {'email': 'fixture@example.invalid'}})
        self.assertEqual(result['status'], 'unknown')
        self.assertEqual(result['unapplied_result']['access_token'], 'unconfirmed-at')

    def test_nested_refresh_preserves_shape_and_rotates_internal_credentials(self):
        tokens = {'access_token': jwt(email='fixture@example.invalid'), 'refresh_token': 'new-rt', 'expires_in': 3600}
        document = {'accounts': [{'email': 'fixture@example.invalid', 'credentials': {'access_token': 'old-at', 'refresh_token': 'old-rt', 'expires_at': 1}, 'name': 'keep'}], 'type': 'export'}
        before = copy.deepcopy(document)
        response = Mock(status_code=200, content=b'json')
        response.json.return_value = tokens
        with patch('requests.Session') as session:
            session.return_value.__enter__.return_value.post.return_value = response
            result = execute({'action': 'account.refresh', 'expires_at': time.time()+60}, {'document': document})
            self.assertEqual(session.return_value.__enter__.return_value.post.call_args.kwargs['data']['refresh_token'], 'old-rt')
        self.assertEqual(result['status'], 'completed')
        self.assertEqual(result['document']['accounts'][0]['credentials']['refresh_token'], 'new-rt')
        self.assertGreater(result['document']['accounts'][0]['credentials']['expires_at'], time.time())
        self.assertNotIn('access_token', result['document'])
        self.assertEqual(document, before)

    def test_explicit_account_states_and_generic_401(self):
        for code, expected in [('account_banned', 'account_banned'), ('account_deactivated', 'account_deactivated'), ('workspace_deactivated', 'workspace_deactivated'), ('deactivated_workspace', 'deactivated_workspace'), ('invalid_grant', 'RT_REJECTED')]:
            with self.subTest(code=code), patch('requests.Session') as session:
                response = Mock(status_code=401, content=b'json')
                response.json.return_value = {'error': {'code': code}}
                session.return_value.__enter__.return_value.post.return_value = response
                result = execute({'action': 'account.refresh', 'expires_at': time.time()+60}, {'document': {'refresh_token': 'old-rt'}})
                self.assertEqual(result['status'], 'failed')
                self.assertEqual(result['error_code'], expected.upper())
                self.assertIsNone(result['document'])

    def test_received_rotation_is_retained_if_projection_cannot_finish(self):
        response = Mock(status_code=200, content=b'json')
        response.json.return_value = {'access_token': jwt(email='fixture@example.invalid'), 'refresh_token': 'rotated-rt', 'expires_in': 'invalid-expiry'}
        with patch('requests.Session') as session:
            session.return_value.__enter__.return_value.post.return_value = response
            result = execute({'action': 'account.refresh', 'expires_at': time.time()+60}, {'document': {'email': 'fixture@example.invalid', 'refresh_token': 'old-rt'}})
        self.assertEqual(result['status'], 'unknown')
        self.assertEqual(result['unapplied_result']['refresh_token'], 'rotated-rt')

    def test_ambiguous_account_document_does_not_exchange(self):
        with patch('requests.Session') as session:
            result = execute({'action': 'account.refresh', 'expires_at': time.time()+60}, {'document': {'accounts': [{'refresh_token': 'a'}, {'refresh_token': 'b'}]}})
        session.assert_not_called()
        self.assertEqual(result['error_code'], 'ASSET_DOCUMENT_AMBIGUOUS')
        self.assertEqual(result['status'], 'failed')

    def test_reauthorization_receives_central_recovery_without_local_lookup(self):
        seen = []
        async def authorize(document, **kwargs):
            seen.append((document, kwargs))
            return {'access_token': 'new-at', 'refresh_token': 'new-rt'}
        material = {'document': {'email': 'fixture@example.invalid', 'access_token': 'old-at'},
                    'recovery': {'login': {'password': 'fixture-password', 'totp_secret': 'fixture-totp'},
                                 'mailbox': {'email': 'mail@example.invalid', 'provider': 'imap', 'material': {'host': 'imap.example.invalid'}}}}
        before = copy.deepcopy(material)
        with patch('tools.chatgpt_reauthorize.authorize_document', side_effect=authorize), patch('tools.asset_ops._find_credential', side_effect=AssertionError('no local read')):
            result = execute({'action': 'account.authorize', 'expires_at': time.time()+600}, material)
        self.assertEqual(result['status'], 'completed')
        self.assertEqual(seen[0][0]['password'], 'fixture-password')
        self.assertEqual(seen[0][1]['mailbox'], material['recovery']['mailbox'])
        self.assertEqual(material, before)
