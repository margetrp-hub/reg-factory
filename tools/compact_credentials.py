"""Consolidate current credentials without deleting account/task identities.

Run with workers stopped, after retaining a short-lived operational rollback
snapshot. Default is a dry run. Source stubs contain identity/reference metadata;
running tasks keep their exact source evidence. No credential is printed.
"""
import argparse
import copy
import hashlib
import json
import os
from pathlib import Path
from contextlib import closing
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from common import credential_versions as versions
from common.production_database import connect
from tools.asset_ops import _atomic_json_write, _credential_dirs, _find_credential, _json_email

LOGIN_KEYS = ('password', 'account_password', 'chatgpt_password', 'totp', 'two_factor',
              'otp_secret', 'reauthorization', 'mail_api_url', 'mail_api_key')


def compact(root, *, apply=False, database=None):
    root = Path(root).resolve()
    dsn = database or os.environ.get('REGD_DB')
    if not dsn:
        raise RuntimeError('CREDENTIAL_DATABASE_REQUIRED')
    with closing(connect(dsn)) as db:
        accounts = [(r[0], r[1]) for r in db.execute("SELECT ref,identity FROM accounts WHERE platform='chatgpt' ORDER BY ref").fetchall()]
        active = db.execute("SELECT count(*) FROM tasks WHERE status IN ('running','queued','stopping')").fetchone()[0]
        if active:
            raise RuntimeError('CREDENTIAL_TASK_ACTIVE')
        protected = {row[i] for row in db.execute("SELECT r.source_sha,r.result_sha FROM account_refreshes r JOIN tasks t ON t.id=r.task_id WHERE t.status NOT IN ('succeeded','failed')").fetchall() for i in (0, 1) if row[i]}
        task_states = {r[0]: r[1] for r in db.execute('SELECT id,status FROM tasks').fetchall()}
    before, current, writes = {}, {}, {}
    for ref, email in accounts:
        selected = _find_credential(ref, root=root, database=dsn)
        if not selected:
            raise RuntimeError('CREDENTIAL_REFERENCE_MISSING')
        path, document = selected
        bundle = versions.tokens(document)
        if not bundle or not bundle.get('refresh_token'):
            continue  # login-only material remains available for authorization
        before[ref] = versions.fingerprint(bundle)
        key = str(path.resolve())
        if key in current:
            continue
        canonical = copy.deepcopy(document)
        canonical_ref = document.get('ref') or ref
        canonical.update(ref=canonical_ref, platform='chatgpt', kind=document.get('kind') if document.get('kind') in ('oauth','team') else 'oauth',
                         email=email, payload=bundle)
        for token_key in ('access_token','refresh_token','id_token','accessToken','refreshToken','idToken'):
            canonical.pop(token_key,None)
        if root / 'assets' not in path.parents:
            path = root / 'assets/chatgpt/oauth' / (hashlib.sha256(canonical_ref.encode()).hexdigest()+'.json')
        current[key] = (path,canonical)
        writes[path] = canonical
    canonical_paths = {path for path,_ in current.values()}
    for directory in _credential_dirs(root):
        for path in directory.glob('*.json'):
            if path in canonical_paths or path.is_symlink():
                continue
            document=json.loads(path.read_text(encoding='utf-8'))
            email=_json_email(document).strip().lower()
            matches=[(p,d) for p,d in current.values() if _json_email(d).strip().lower()==email and versions.compatible(document,d)]
            if len(matches)!=1 or not versions.tokens(document):
                continue
            target, canonical=matches[0]
            writes[path]=versions.reference_stub(document, canonical['ref'])
    retired=[]
    for directory in (root/'asset-results',root/'oauth-results'):
        for path in directory.glob('*.json'):
            if path.is_symlink() or path.stem in protected:
                continue
            # Recent/unsettled responses may still be in transit; old task rows
            # retain metadata. Pending refresh source digests are always pinned.
            if time.time()-path.stat().st_mtime < 600:
                continue
            if directory.name=='oauth-results':
                document=json.loads(path.read_text())
                task_id=document.get('task_id','')
                if task_id and (task_id.startswith('work-') or task_states.get(task_id) not in ('succeeded','failed')):
                    continue  # Unknown or not-yet-acknowledged task evidence is separate.
                if any(item.get('after',{}).get('credential_updated_at') is None for item in document.get('files',[])):
                    continue  # legacy unresolved write evidence needs explicit review
            retired.append(path)
    if apply:
        # Canonical records first; interruption cannot strand an alias.
        for path in canonical_paths:
            _atomic_json_write(path,writes[path])
        for path,document in writes.items():
            if path not in canonical_paths:
                _atomic_json_write(path,document)
        for ref, expected in before.items():
            selected=_find_credential(ref,root=root,database=dsn)
            if not selected or versions.fingerprint(versions.tokens(selected[1]) or {})!=expected:
                raise RuntimeError('CREDENTIAL_COMPACTION_VERIFICATION_FAILED')
        for path in retired:
            path.unlink()
    return {'account_sources':len(accounts),'accounts':len({email.strip().lower() for _,email in accounts}),
            'verified_references':len(before),'current_documents':len(canonical_paths),
            'retired_copies':len(writes)-len(canonical_paths),'retired_result_files':len(retired),'applied':apply}


if __name__=='__main__':
    parser=argparse.ArgumentParser()
    parser.add_argument('--root',default=os.environ.get('REG_FACTORY_DATA_DIR','/data'))
    parser.add_argument('--apply',action='store_true')
    args=parser.parse_args()
    print(json.dumps(compact(args.root,apply=args.apply)))
