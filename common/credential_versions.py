"""Current credential selection shared by executors and producers.

Old references identify the account; only a successful atomic save advances the
current credential timestamp. Never splice tokens from different results.
"""
import base64
import copy
from contextlib import contextmanager
from datetime import datetime
import hashlib
import json
import os
from pathlib import Path


def claims(token):
    try:
        part = token.split('.')[1]
        return json.loads(base64.urlsafe_b64decode(part + '=' * (-len(part) % 4)))
    except (ValueError, IndexError, AttributeError):
        return {}


def tokens(document):
    found = []
    def visit(value):
        if not isinstance(value, dict):
            return
        copy = dict(value)
        for key, alias in [('access_token', 'accessToken'), ('refresh_token', 'refreshToken'), ('id_token', 'idToken')]:
            copy[key] = copy.get(key) or copy.get(alias) or ''
        if copy['access_token']:
            found.append(copy)
        for key in ('payload', 'credentials', 'oauth', 'token', 'tokens'):
            visit(value.get(key))
    visit(document)
    expected = str(document.get('email') or document.get('payload', {}).get('email') or '').strip().lower()
    if expected:
        def belongs(item):
            emails = set()
            for key in ('access_token', 'id_token'):
                identity = claims(item[key])
                email = identity.get('email') or (identity.get('https://api.openai.com/profile') or {}).get('email')
                if email:
                    emails.add(str(email).strip().lower())
            return not emails or emails == {expected}
        found = [item for item in found if belongs(item)]
    if not found:
        return None
    complete = [item for item in found if item['refresh_token']]
    found = complete or found
    selected = found[0]
    for item in found[1:]:
        if fingerprint(item) == fingerprint(selected):
            continue
        before, after = claims(selected['access_token']), claims(item['access_token'])
        subject_before = claims(selected['id_token']).get('sub') or before.get('sub')
        subject_after = claims(item['id_token']).get('sub') or after.get('sub')
        if (not before.get('iat') or not after.get('iat') or before['iat'] == after['iat']
                or not subject_before or subject_before != subject_after):
            raise ValueError('CREDENTIAL_RESULT_AMBIGUOUS')
        if after['iat'] > before['iat']:
            selected = item
    # A complete chosen bundle must not carry a stale nested bundle with it.
    return {k: v for k, v in selected.items() if k not in ('payload', 'credentials', 'oauth', 'token', 'tokens')}


def fingerprint(document):
    return tuple(str(document.get(k) or '') for k in ('access_token', 'refresh_token', 'id_token'))


def scope(document):
    doc = tokens(document) or document.get('payload', document)
    auth = claims(doc.get('access_token', '')).get('https://api.openai.com/auth') or {}
    subject = (claims(doc.get('id_token', '')).get('sub') or claims(doc.get('access_token', '')).get('sub')
               or doc.get('credential_subject') or doc.get('chatgpt_user_id') or doc.get('user_id') or '')
    workspace = (doc.get('workspace_account_id') or doc.get('chatgpt_account_id') or doc.get('account_id')
                 or auth.get('chatgpt_account_id') or '')
    return str(workspace), str(subject)


def saved_time(document):
    try:
        return datetime.fromisoformat(document.get('credential_updated_at', '').replace('Z', '+00:00')).timestamp()
    except (ValueError, TypeError):
        return float(claims((tokens(document) or {}).get('access_token', '')).get('iat') or 0)


def newest(items):
    latest = max(saved_time(d) for _, d in items)
    finalists = [(p, d) for p, d in items if saved_time(d) == latest]
    if len({fingerprint(tokens(d) or {}) for _, d in finalists}) > 1:
        raise ValueError('CREDENTIAL_RESULT_AMBIGUOUS')
    return finalists[0]


def compatible(anchor, candidate):
    a, b = scope(anchor), scope(candidate)
    if a[1] and b[1] and a[1] == b[1]:
        return True  # the same upstream user may be reused in a new workspace
    return all(not left or not right or left == right for left, right in zip(a, b))


def reference_stub(document, current_ref):
    """Retain login material and identity, but never a retired token bundle."""
    result = copy.deepcopy(document)
    workspace, subject = scope(document)
    secret_keys = {'access_token', 'refresh_token', 'id_token', 'accessToken',
                   'refreshToken', 'idToken', 'oauth_access_token', 'oauth_refresh_token'}
    def strip(value):
        if isinstance(value, dict):
            for key in list(value):
                if key in secret_keys:
                    del value[key]
                else:
                    strip(value[key])
        elif isinstance(value, list):
            for item in value:
                strip(item)
    strip(result)
    payload = result.setdefault('payload', {})
    if subject:
        payload['credential_subject'] = subject
    if workspace:
        payload['workspace_account_id'] = workspace
    result['credential_ref'] = current_ref
    return result


def select(anchor, candidates):
    candidates = [(p, d) for p, d in candidates if compatible(anchor, d)]
    for axis in (0, 1):
        if axis == 0 and scope(anchor)[1] and all(scope(d)[1] == scope(anchor)[1] for _, d in candidates):
            continue
        if len({scope(d)[axis] for _, d in candidates} - {''}) > 1:
            raise ValueError('CREDENTIAL_IDENTITY_AMBIGUOUS')
    return newest(candidates) if candidates else None


@contextmanager
def account_lock(root, email, ref):
    identity = email.strip().lower() or ref
    key = hashlib.sha256(('chatgpt\n' + identity).encode()).hexdigest()
    path = Path(root) / 'credential-locks' / (key + '.lock')
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    with path.open('a+b') as handle:
        os.chmod(path, 0o600)
        if os.name == 'nt':
            import msvcrt
            if path.stat().st_size == 0:
                handle.write(b'0'); handle.flush()
            handle.seek(0)
            try:
                msvcrt.locking(handle.fileno(), msvcrt.LK_NBLCK, 1)
            except OSError:
                raise BlockingIOError('CREDENTIAL_BUSY') from None
        else:
            import fcntl
            fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        try:
            yield
        finally:
            if os.name == 'nt':
                handle.seek(0); msvcrt.locking(handle.fileno(), msvcrt.LK_UNLCK, 1)
            else:
                fcntl.flock(handle.fileno(), fcntl.LOCK_UN)
