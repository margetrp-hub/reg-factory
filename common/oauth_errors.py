"""Explicit provider state only; HTTP codes and prose cannot establish a ban."""

ACCOUNT_STATES = {
    'account_deactivated': 'banned',
    'account_banned': 'banned',
    'workspace_deactivated': 'workspace_deactivated',
    'deactivated_workspace': 'workspace_deactivated',
}


def error_code(payload):
    if not isinstance(payload, dict):
        return ''
    error = payload.get('error')
    if isinstance(error, str):
        return error.strip().lower()
    for item in (error, payload.get('detail'), payload):
        if isinstance(item, dict):
            for key in ('code', 'error_code', 'type'):
                value = item.get(key)
                if isinstance(value, str) and value.strip():
                    return value.strip().lower()
    return ''


def account_failure(payload):
    code = error_code(payload)
    status = ACCOUNT_STATES.get(code)
    if not status:
        return None
    return {'error_code': code, 'source_status': status,
            'message': '上游明确返回账号封禁/停用' if status == 'banned' else '上游明确返回空间停用'}
