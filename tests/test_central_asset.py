import copy
import os
import sys
import time
import unittest
from unittest.mock import patch
from pathlib import Path

sys.path[:0] = [str(Path(__file__).resolve().parents[1]), str(Path(__file__).resolve().parents[1] / 'shared/node-sdk/python')]
from tools.central_asset import execute


class CentralExecutionTests(unittest.TestCase):
    def test_at_receives_no_other_credentials_and_does_not_scan_pool(self):
        seen = []
        material = {'document': {'email': 'fixture@example.invalid', 'access_token': 'fixture-at', 'refresh_token': 'never-send'}}
        with patch('common.asset_scanner._scan_chatgpt', side_effect=lambda record, timeout: seen.append(record) or {'status': 'normal'}):
            result = execute({'action': 'asset.liveness', 'expires_at': time.time()+100}, material)
        self.assertEqual(seen, [{'_token': {'access_token': 'fixture-at'}}])
        self.assertEqual(result['liveness'], 'alive')
        self.assertIsNone(result['document'])

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
