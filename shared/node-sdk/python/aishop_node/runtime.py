"""Public capability/version facts. Never reports secrets or host paths."""
import re

from .assets import AssetProtocolError, IDENTIFIER

VERSION = '1.1.0'
CONTRACT = 1
CAPABILITIES = ['asset_claim', 'durable_result', 'scoped_ack', 'repair_observation', 'runtime_versions', 'asset_handoff', 'asset_receive']


def runtime(*, center_id='', configuration_revision='', materials=()):
    if len(materials) > 8:
        raise AssetProtocolError('SDK_MATERIAL_VERSIONS_INVALID')
    applied = []
    for item in materials:
        if (not isinstance(item, dict) or set(item) != {'kind', 'digest', 'revision'}
                or not isinstance(item['kind'], str) or not re.fullmatch(r'[a-z][a-z0-9_]{0,63}', item['kind'])
                or not isinstance(item['digest'], str) or not re.fullmatch(r'[a-f0-9]{64}', item['digest'])
                or type(item['revision']) is not int or not 1 <= item['revision'] <= 2147483647
                or any(old['kind'] == item['kind'] for old in applied)):
            raise AssetProtocolError('SDK_MATERIAL_VERSIONS_INVALID')
        applied.append(dict(item))
    value = {'node_sdk': {'version': VERSION, 'contract': CONTRACT, 'capabilities': list(CAPABILITIES)},
             'material_versions': applied}
    if center_id or configuration_revision:
        if (not isinstance(center_id, str) or not IDENTIFIER.fullmatch(center_id)
                or not isinstance(configuration_revision, str) or not re.fullmatch(r'[a-f0-9]{16}', configuration_revision)):
            raise AssetProtocolError('SDK_CONFIGURATION_INVALID')
        value['central_assets'] = {'configured': True, 'contract': CONTRACT, 'center_id': center_id,
                                  'configuration_revision': configuration_revision}
    return value
