"""Transport validation only. Handoff fields are optional; tasks validate inputs."""
import json
from importlib.resources import files
from .assets import AssetProtocolError

REGISTRY = json.loads(files(__package__).joinpath('asset-profiles.v1.json').read_text(encoding='utf-8'))
SCHEMA = REGISTRY['schema']


def missing_fields(entry):
    if not isinstance(entry, dict):
        return ['entry']
    errors = []
    if entry.get('schema') != SCHEMA:
        errors.append('schema')
    if not isinstance(entry.get('source_ref'), str) or not 1 <= len(entry['source_ref']) <= 255:
        errors.append('source_ref')
    if type(entry.get('source_revision')) is not int or not 1 <= entry['source_revision'] <= 2147483647:
        errors.append('source_revision')
    if entry.get('type') not in {'account', 'mailbox', 'credential'}:
        errors.append('type')
    for name, limit in [('platform',80),('kind',80),('email',190),('role',80),('usage',500)]:
        if name in entry and (not isinstance(entry[name],str) or len(entry[name]) > limit):
            errors.append(name)
    if not isinstance(entry.get('document'), dict):
        errors.append('document')
    if 'state' in entry and not isinstance(entry['state'],dict):
        errors.append('state')
    return sorted(errors)


def validate(entry):
    invalid = missing_fields(entry)
    if invalid:
        raise AssetProtocolError('ASSET_ENVELOPE_INVALID:' + ','.join(invalid))
    return entry


def verify_receipt(request, response, node_id, account_id=''):
    if (response.get('state') == 'rejected' and request.get('action') == 'snapshot'
            and response.get('task_id') == request.get('task_id') and response.get('node_id') == node_id
            and response.get('source_ref') == request.get('entry', {}).get('source_ref')):
        raise AssetProtocolError('ASSET_HANDOFF_REJECTED')
    share = response.get('sharing', {})
    entry = request.get('entry', {})
    if (not isinstance(share,dict) or not isinstance(share.get('account_id'),str) or not share['account_id']
            or request.get('action') != 'snapshot' or response.get('state') != 'accepted'
            or response.get('task_id') != request.get('task_id') or share.get('node_id') != node_id
            or share.get('source_ref') != entry.get('source_ref') or share.get('state') != 'transferred'
            or type(share.get('source_version')) is not int or share['source_version'] != entry.get('source_revision')
            or account_id and share['account_id'] != account_id):
        raise AssetProtocolError('ASSET_HANDOFF_ACK_INVALID')
    return share
