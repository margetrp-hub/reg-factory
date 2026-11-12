"""A durable central-asset operation for nodes with their own task scheduler.

The scheduler owns admission and terminal persistence. This adapter never
repeats an external call after the durable intent, including after a restart.
"""
import json
import re
import uuid

from .assets import AssetProtocolError, result_observation, validate_result_receipt
from .journal import ResultJournal


class AssetOperation:
    def __init__(self, directory, key, context, *, claim, send):
        self.context, self.claim, self.send = context, claim, send
        self.task_id = context['attempt_id']
        self.intents = ResultJournal(directory / 'intents', key)
        self.results = ResultJournal(directory / 'results', key)

    def saved(self):
        value = self.results.get(self.task_id)
        if value and value['context'] != self.context:
            raise AssetProtocolError('ASSET_TASK_SCOPE_CHANGED')
        return value

    def stage(self, result, *, preflight=False):
        old = self.saved()
        if old:
            return old
        state = result.get('status')
        if state not in {'completed', 'failed', 'unknown'} or not re.fullmatch(r'[A-Z0-9_]{0,95}', result.get('error_code', '')):
            raise AssetProtocolError('ASSET_EXECUTOR_RESULT_INVALID')
        mutation = self.context['action'] in {'account.refresh', 'account.authorize'}
        if state == 'completed' and mutation and not isinstance(result.get('document'), dict):
            raise AssetProtocolError('ASSET_EXECUTOR_RESULT_INVALID')
        message = {'action': 'result', 'task_id': self.task_id, 'result': {
            'result_id': str(uuid.uuid4()), 'outcome': {'completed': 'success', 'failed': 'failed', 'unknown': 'unknown'}[state],
            'document': result.get('document') if mutation and state == 'completed' else None,
            **result_observation(result)}}
        value = {'task_id': self.task_id, 'context': self.context, 'message': message,
                 'preflight': preflight, 'result': result}
        self.results.put(self.task_id, value)
        return value

    def run(self, execute, *, replay=False):
        if self.saved():
            return self.deliver()
        intent = self.intents.get(self.task_id)
        if intent:
            if intent['context'] != self.context:
                raise AssetProtocolError('ASSET_TASK_SCOPE_CHANGED')
            self.stage({'status': 'unknown', 'document': None, 'error_code': 'NODE_EXECUTION_INTERRUPTED'})
            return self.deliver()
        if replay:
            raise AssetProtocolError('ASSET_RESULT_NOT_AVAILABLE')
        invoked, result = False, None
        try:
            self.results.verify_writable()
            self.intents.verify_writable()
            receipt = self.claim()
            if (receipt.get('task_id') != self.task_id or receipt.get('account_id') != self.context['subject_id']
                    or receipt.get('node_id') != self.context['node_id'] or receipt.get('state') != 'claimed'
                    or receipt.get('expected_revision') != self.context['resource_version']):
                raise AssetProtocolError('ASSET_CLAIM_RECEIPT_INVALID')
            material = self.send({'action': 'read', 'task_id': self.task_id})
            if (material.get('account_id') != self.context['subject_id']
                    or material.get('credential_revision') != self.context['resource_version']):
                raise AssetProtocolError('ASSET_READ_SCOPE_INVALID')
            self.intents.put(self.task_id, {'task_id': self.task_id, 'context': self.context})
            invoked = True
            result = execute(material)
            self.stage(result)
        except Exception as exc:
            code = str(exc) if isinstance(exc, AssetProtocolError) else 'ASSET_EXECUTION_UNKNOWN' if invoked else 'ASSET_PREPARATION_FAILED'
            if not re.fullmatch(r'[A-Z0-9_]{1,95}', code):
                code = 'ASSET_EXECUTION_UNKNOWN' if invoked else 'ASSET_PREPARATION_FAILED'
            failure = {'status': 'unknown' if invoked else 'failed', 'document': None, 'error_code': code}
            if result is not None:
                # A returned token may already have consumed the previous RT.
                # Keep the rejected result encrypted; do not silently discard it.
                failure['unapplied_result'] = result
            self.stage(failure, preflight=not invoked)
        return self.deliver()

    def validate(self, receipt, value):
        r = value['message']['result']
        return validate_result_receipt(receipt, task_id=self.task_id, account_id=self.context['subject_id'],
            result_id=r['result_id'], outcome=r['outcome'], expected_revision=self.context['resource_version'],
            refresh=self.context['action'] in {'account.refresh', 'account.authorize'})

    def deliver(self):
        value = self.saved()
        if not value:
            raise AssetProtocolError('ASSET_RESULT_NOT_AVAILABLE')
        state, receipt, code = 'pending', {}, ''
        try:
            receipt = self.send(value['message'])
            state = self.validate(receipt, value)
        except Exception:
            try:
                receipt = self.send({'action': 'receipt', 'task_id': self.task_id})
                if receipt.get('task_id') == self.task_id and receipt.get('state') == 'absent' and value['preflight']:
                    state = 'failed'
                else:
                    state = self.validate(receipt, value)
            except Exception as exc:
                code = str(exc) if isinstance(exc, AssetProtocolError) else 'ASSET_RESULT_DELIVERY_UNAVAILABLE'
                receipt = {}
        return {'task_id': self.task_id, 'account_id': self.context['subject_id'],
                'result_id': value['message']['result']['result_id'], 'state': state,
                'error_code': code or ('ASSET_CHECKOUT_CLOSED' if state == 'closed' else value['result'].get('error_code', '')),
                'outcome': value['message']['result']['outcome'], 'expected_revision': self.context['resource_version'],
                'credential_revision': receipt.get('credential_revision', self.context['resource_version']),
                'receipt': receipt, 'preflight': value['preflight'],
                'observation': {k: value['message']['result'][k] for k in ('source_status', 'source_liveness', 'checked_at')}}

    def acknowledge(self, receipt):
        """Only call after the node's terminal task transaction has committed."""
        value = self.saved()
        if not value:
            return
        if not (value['preflight'] and receipt == {'task_id': self.task_id, 'state': 'absent'}):
            if self.validate(receipt, value) not in {'completed', 'failed', 'closed'}:
                raise AssetProtocolError('ASSET_RESULT_NOT_TERMINAL')
        intent = self.intents.get(self.task_id)
        if intent:
            self.intents.acknowledge(self.task_id, intent)
        self.results.acknowledge(self.task_id, value)
