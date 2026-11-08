"""Produce TEAM credentials from one existing Free account, with durable steps.

Only the signed Go node admits work. This process never registers an account,
buys seats/phone numbers or changes the selected account. Bounded retries first
reconcile invitations and saved outputs. Secrets never enter task logs.
"""
from __future__ import annotations

import asyncio
import base64
import contextlib
import hashlib
import io
import json
import os
from pathlib import Path
import sqlite3
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from cryptography.fernet import Fernet

AUTH = 'https://api.openai.com/auth'
PROFILE = 'https://api.openai.com/profile'
BASE = 'https://chatgpt.com/backend-api'


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':'), ensure_ascii=True)


def claims(token):
    try:
        part = token.split('.')[1]
        return json.loads(base64.urlsafe_b64decode(part + '=' * (-len(part) % 4)))
    except (ValueError, IndexError, TypeError):
        return {}


class Pending(Exception):
    def __init__(self, code, *, manual=False):
        self.code, self.manual = code, manual


def email_of(row):
    return str(row.get('email') or row.get('email_address') or row.get('user_email')
               or (row.get('profile') or {}).get('email') or '').strip().lower()


def document_metadata(doc):
    payload = doc.get('payload', doc)
    c = claims(payload.get('access_token', ''))
    email = str(payload.get('email', '')).strip().lower()
    return payload, email, c, c.get(AUTH, {})


def candidates(db, root):
    """Conservatively reconcile every alias and retained result, not filenames."""
    aliases = list(db.execute('SELECT a.*,c.password,c.totp FROM accounts a LEFT JOIN account_credentials c ON c.ref=a.ref WHERE platform=?', ('chatgpt',)))
    blocked = {str(r['identity']).lower() for r in aliases if r['claimed'] or r['join_status'] in ('joined', 'switched', 'verified')}
    blocked |= {r[0].lower() for r in db.execute('SELECT email FROM production_results')}
    blocked |= {r[0].lower() for r in db.execute("SELECT email FROM production_runs WHERE email<>'' AND NOT (status='cancelled' AND stop_effect='none' AND external_started=0)")}
    paths = list((root / 'plus-import').glob('*.json'))
    paths += list((root / 'plus-import' / 'team-json').glob('*.json'))
    paths += list((root / 'assets' / 'chatgpt').glob('*/*.json'))
    paths += list((root / 'asset-results').glob('*.json'))
    found = {}
    for path in sorted(set(paths)):
        if path.is_symlink() or path.stat().st_size > 2 * 1024 * 1024:
            continue
        try:
            payload, email, c, auth = document_metadata(json.loads(path.read_text()))
        except (ValueError, TypeError, AttributeError):
            continue
        claim_email = str((c.get(PROFILE) or {}).get('email') or c.get('email') or '').lower()
        if not email or claim_email != email:
            continue
        plan = auth.get('chatgpt_plan_type')
        if plan and plan != 'free':
            blocked.add(email)
        if (plan != 'free' or c.get('exp', 0) <= time.time() or not payload.get('refresh_token')):
            continue
        logins = [r for r in aliases if str(r['identity']).lower() == email and r['password'] and r['totp']
                  and r['status'] in ('normal', 'active')]
        if not logins:
            continue
        raw = canonical(payload)
        item = {'email': email, 'source_ref': str(path.relative_to(root)), 'source_sha': hashlib.sha256(raw.encode()).hexdigest(),
                'document': payload, 'password': logins[0]['password'], 'totp': logins[0]['totp']}
        if email not in found or c.get('iat', 0) > claims(found[email]['document']['access_token']).get('iat', 0):
            found[email] = item
    return [found[email] for email in sorted(found) if email not in blocked]


class Backend:
    def __init__(self, material):
        from curl_cffi import requests
        proxy = os.environ.get('REG_FACTORY_PRODUCTION_PROXY', '').strip()
        self.http = requests.Session(impersonate='chrome', trust_env=False,
            proxies={'http': proxy, 'https': proxy} if proxy else None)
        self.workspace = material['workspace']

    def request(self, method, path, *, token=None, discovery=False, **kwargs):
        headers = {'Authorization': 'Bearer ' + (token or self.workspace['access_token'])}
        if not discovery:
            headers['ChatGPT-Account-Id'] = self.workspace['account_id']
        try:
            response = self.http.request(method, BASE + path, headers=headers, timeout=20, **kwargs)
            if not 200 <= response.status_code < 300:
                raise Pending('PRODUCTION_UPSTREAM_HTTP_' + str(response.status_code))
            data = response.json()
            if not isinstance(data, dict):
                raise ValueError()
            return data
        except Pending:
            raise
        except Exception:
            raise Pending('PRODUCTION_UPSTREAM_UNAVAILABLE') from None

    def page(self, kind):
        rows, expected = [], None
        while len(rows) <= 2000:
            data = self.request('GET', '/accounts/' + self.workspace['account_id'] + '/' + kind + '?limit=50&offset=' + str(len(rows)))
            items, total = data.get('items'), data.get('total')
            if (not isinstance(items, list) or type(total) is not int or not 0 <= total <= 2000
                    or any(not isinstance(r, dict) for r in items) or expected is not None and expected != total):
                raise Pending('PRODUCTION_ROSTER_INCOMPLETE')
            expected = total
            rows.extend(items)
            if len(rows) == total:
                return rows
            if not items or len(rows) > total:
                break
        raise Pending('PRODUCTION_ROSTER_INCOMPLETE')

    def snapshot(self, email, seat):
        members, invites = self.page('users'), self.page('invites')
        member = [r for r in members if email_of(r) == email]
        pending = []
        for r in invites:
            status = next((r[k] for k in ('status', 'state', 'invite_status') if r.get(k) is not None), None)
            if status is None:
                raise Pending('PRODUCTION_INVITE_STATE_UNKNOWN')
            if str(status).lower() in {'2', 'pending', 'invited', 'sent', 'open', 'active'}:
                pending.append(r)
        existing = member or [r for r in pending if email_of(r) == email]
        if len(existing) > 1 or any(r.get('seat_type') != seat for r in existing):
            raise Pending('PRODUCTION_MEMBER_SEAT_CONFLICT', manual=True)
        return members, pending, bool(member), bool(existing)

    def preflight(self, candidate, seat):
        child = self.request('GET', '/accounts/check/v4-2023-04-27', token=candidate['document']['access_token'], discovery=True)
        accounts = child.get('accounts')
        if not isinstance(accounts, dict) or not accounts:
            raise Pending('PRODUCTION_SOURCE_DISCOVERY_UNKNOWN')
        if any(v.get('account', {}).get('plan_type') != 'free' for v in accounts.values()):
            raise Pending('PRODUCTION_SOURCE_ALREADY_USED', manual=True)
        members, invites, joined, present = self.snapshot(candidate['email'], seat)
        if present:
            raise Pending('PRODUCTION_SOURCE_ALREADY_USED', manual=True)
        sub = self.request('GET', '/subscriptions?account_id=' + self.workspace['account_id'])
        # Never purchase or expand seats. Require a positive paid entitlement.
        capacities = sub.get('seat_capacity')
        if not isinstance(capacities, list):
            raise Pending('PRODUCTION_PAID_CAPACITY_UNKNOWN')
        selected = [r for r in capacities if isinstance(r, dict) and r.get('type') == seat]
        if len(selected) != 1:
            raise Pending('PRODUCTION_PAID_CAPACITY_UNKNOWN')
        cap = selected[0]
        paid, available, held = cap.get('paid'), cap.get('available'), cap.get('held', 0)
        occupied = sum(r.get('seat_type') == seat for r in members) + sum(r.get('seat_type') == seat for r in invites)
        if any(type(x) is not int or x < 0 for x in (paid, available, held)):
            raise Pending('PRODUCTION_PAID_CAPACITY_UNKNOWN')
        if min(available, paid - occupied - held) < 1:
            raise Pending('PRODUCTION_NO_PAID_SEAT', manual=True)

    def invite(self, email, seat):
        self.request('POST', '/accounts/' + self.workspace['account_id'] + '/invites',
            json={'email_addresses': [email], 'role': 'standard-user', 'resend_emails': False, 'seat_type': seat})

    def authorize(self, candidate, workspace, persist, progress):
        from tools.chatgpt_reauthorize import authorize_document
        # No old personal token can overwrite the new workspace result.
        document = {'email': candidate['email'], 'password': candidate['password'],
                    'totp': candidate['totp'], 'workspace_account_id': workspace}
        return asyncio.run(authorize_document(document, timeout=420, on_tokens=persist, on_progress=progress))


class Run:
    def __init__(self, db, operation, key, root):
        self.db, self.operation, self.box, self.root = db, operation, Fernet(key), root

    def row(self):
        return self.db.execute('SELECT * FROM production_runs WHERE operation_id=?', (self.operation,)).fetchone()

    def seal(self, value):
        return self.box.encrypt(canonical(value).encode()).decode()

    def open(self, value):
        return json.loads(self.box.decrypt(value.encode()))

    def progress(self, stage, status='running', code='', diagnostic=None, **fields):
        self.db.execute('BEGIN IMMEDIATE')
        with self.db:
            if self.row()['status'] == 'cancelled':
                raise Pending('PRODUCTION_CANCELLED', manual=True)
            values = {'stage': stage, 'status': status, 'error_code': code, 'updated_at': int(time.time()),
                      'next_at': int(time.time()) + (30 if status == 'unknown' else 0), **fields}
            if stage != self.row()['stage']:
                values['retry_attempts'] = 0
            self.db.execute('UPDATE production_runs SET ' + ','.join(k + '=?' for k in values) + ' WHERE operation_id=?', (*values.values(), self.operation))
            finished = time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()) if status in ('unknown', 'waiting_user') else ''
            self.db.execute('UPDATE production_admissions SET state=?,error_code=?,finished_at=? WHERE operation_id=?', (status, code, finished, self.operation))
            self.db.execute('UPDATE tasks SET status=?,error=? WHERE id=?', (status, code, 'work-' + self.operation))
            action = '自动重试已用完，管理员可继续或终止本项；其他数量继续执行' if status == 'waiting_user' else '沿原任务自动推进或退避重试；保留原账号和已保存结果'
            event = {'stage': stage, 'status': status, 'error_code': code, 'processing_owner': 'node_admin'}
            if diagnostic:
                # Normalize through the login engine's allowlist again. Neither
                # exception text nor arbitrary provider fields enter task logs.
                from vendor.reauth_web.engine import failure_evidence
                safe = failure_evidence({'error': diagnostic.get('error_code'),
                    'page_type': diagnostic.get('page_type'), 'status': diagnostic.get('http_status')})
                event['diagnostic'] = safe
                action += f"；授权诊断 {safe['error_code']}；页面 {safe['page_type']}；HTTP {safe['http_status']}"
            event['action'] = action
            self.db.execute("INSERT INTO events(task_id,ts,stream,line) VALUES(?,?,'stage',?)",
                ('work-' + self.operation, time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()), canonical(event)))

    def oauth_progress(self, stage):
        # Only known phase names enter logs; provider messages may contain tokens.
        if stage in {'login', 'authorize', 'mfa', 'workspace', 'exchange', 'done', 'session', 'password', 'email', 'consent'}:
            self.progress('oauth_started', code='PRODUCTION_OAUTH_' + stage.upper())

    def save_output(self, output):
        row = self.row()
        material = self.open(row['material'])
        workspace = material['workspace']['account_id']
        doc = {k: output[k] for k in ('access_token', 'refresh_token', 'id_token') if output.get(k)}
        doc.update(email=row['email'], account_id=workspace, workspace_account_id=workspace,
                   seat_type=material['parameters']['seat_type'])
        with self.db:
            # A process can be stopped after issuance. Retention must still win.
            saved = self.db.execute('SELECT artifact FROM production_outputs WHERE operation_id=? AND attempt=?',
                (self.operation, row['oauth_attempts'])).fetchone()
            sealed = saved[0] if saved else self.seal(doc)
            if not saved:
                self.db.execute('INSERT INTO production_outputs(operation_id,attempt,artifact,created_at) VALUES(?,?,?,?)',
                    (self.operation,row['oauth_attempts'],sealed,int(time.time())))
            self.db.execute('UPDATE production_runs SET output_artifact=? WHERE operation_id=?',(sealed,self.operation))

    def restart_input(self):
        """A fresh signed generation can reclaim only its own absent member.

        Ordinary candidate selection still excludes all issued TEAM history.
        The prior executor must be fenced and its stop readback settled; successful
        or uncertain outputs can never become input to another authorization.
        """
        material = self.open(self.row()['material'])
        params = material['parameters']
        if params.get('mode') != 'produce' or not params.get('production_ref'):
            return None
        rows = self.db.execute('''SELECT r.*,a.parameters FROM production_runs r
            JOIN production_admissions a ON a.operation_id=r.operation_id
            LEFT JOIN production_results p ON p.operation_id=r.operation_id
            WHERE r.operation_id<>? AND r.stop_requested=1 AND r.status='cancelled'
              AND r.stop_effect='none' AND r.input_artifact<>''
              AND (p.operation_id IS NULL OR p.state='failed')
            ORDER BY r.updated_at DESC''', (self.operation,))
        for prior in rows:
            if json.loads(prior['parameters']) != params:
                continue
            old_material = self.open(prior['material'])
            if old_material['workspace']['account_id'] != material['workspace']['account_id']:
                continue
            occupied = self.db.execute('''SELECT 1 FROM production_runs WHERE email=? AND operation_id<>?
                AND NOT (status='cancelled' AND stop_effect='none' AND stop_requested=1)''',
                (prior['email'], self.operation)).fetchone()
            accepted = self.db.execute("SELECT 1 FROM production_results WHERE email=? AND state<>'failed'",
                                       (prior['email'],)).fetchone()
            if occupied or accepted:
                continue
            selected = self.open(prior['input_artifact'])
            if (selected.get('email') != prior['email'] or selected.get('source_sha') != prior['source_sha']
                    or selected.get('source_ref') != prior['source_ref']
                    or hashlib.sha256(canonical(selected.get('document')).encode()).hexdigest() != prior['source_sha']):
                raise Pending('PRODUCTION_RESTART_INPUT_INVALID', manual=True)
            return selected
        return None

    def choose(self):
        self.db.execute('BEGIN IMMEDIATE')
        try:
            if self.row()['stop_requested']:
                raise Pending('PRODUCTION_CANCELLED', manual=True)
            if self.row()['input_artifact']:
                self.db.commit()
                return
            original = self.restart_input()
            found = [original] if original else candidates(self.db, self.root)
            if not found:
                raise Pending('PRODUCTION_EXISTING_FREE_UNAVAILABLE', manual=True)
            selected = found[0]
            self.db.execute('UPDATE production_runs SET email=?,input_artifact=?,source_ref=?,source_sha=? WHERE operation_id=?',
                (selected['email'], self.seal(selected), selected['source_ref'], selected['source_sha'], self.operation))
            self.db.execute('UPDATE accounts SET claimed=1 WHERE lower(identity)=?', (selected['email'],))
            self.db.commit()
        except BaseException:
            self.db.rollback()
            raise

    def advance(self, backend_factory=Backend):
        row = self.row()
        if row['stop_requested']:
            return self.reconcile_stop(backend_factory)
        if row['status'] in {'waiting_user', 'failed', 'cancelled', 'delivered'}:
            return {'status': row['status'], 'error_code': row['error_code']}
        try:
            if row['output_artifact']:
                return self.output(backend_factory(self.open(row['material'])))
            self.progress(row['stage'])
            material = self.open(row['material'])
            self.choose()
            row = self.row()
            candidate = self.open(row['input_artifact'])
            backend = backend_factory(material)
            seat = 'default' if material['parameters']['seat_type'] == 'standard' else 'prolite'
            workspace = material['workspace']['account_id']
            if row['stage'] == 'oauth_started':
                if row['oauth_attempts']-row['oauth_retry_base'] >= 3:
                    raise Pending('PRODUCTION_OAUTH_RETRIES_EXHAUSTED', manual=True)
                # Check original membership/invitation before retrying the same
                # account authorization. Never replace an already saved output.
                _, _, _, present = backend.snapshot(candidate['email'], seat)
                if not present:
                    raise Pending('PRODUCTION_INVITE_RESULT_UNKNOWN')
                self.progress('invited')
            if material['readback_until'] <= time.time():
                raise Pending('PRODUCTION_AUTHORIZATION_EXPIRED', manual=True)
            if row['stage'] == 'admitted':
                backend.preflight(candidate, seat)
                self.progress('invite_started', external_started=1, invite_attempts=row['invite_attempts']+1,
                              invite_observed_at=0)
                backend.invite(candidate['email'], seat)
                self.progress('invited')
            row = self.row()
            if row['stage'] in {'invite_started', 'invited'}:
                _, _, joined, present = backend.snapshot(candidate['email'], seat)
                if not present:
                    stamp = int(time.time())
                    if not row['invite_observed_at']:
                        self.progress('invite_started', invite_observed_at=stamp)
                        raise Pending('PRODUCTION_INVITE_READBACK_PENDING')
                    if stamp-row['invite_observed_at'] < 30:
                        raise Pending('PRODUCTION_INVITE_READBACK_PENDING')
                    if row['invite_attempts'] >= 3:
                        raise Pending('PRODUCTION_INVITE_RETRIES_EXHAUSTED', manual=True)
                    backend.preflight(candidate, seat)
                    self.progress('invite_started', invite_attempts=row['invite_attempts']+1, invite_observed_at=0)
                    backend.invite(candidate['email'], seat)
                    raise Pending('PRODUCTION_INVITE_READBACK_PENDING')
                self.progress('oauth_started', external_started=1, oauth_attempts=self.row()['oauth_attempts'] + 1)
                output = backend.authorize(candidate, workspace, self.save_output, self.oauth_progress)
                self.save_output(output)
                self.progress('output_saved')
            return self.output(backend)
        except Pending as exc:
            if self.row()['stop_requested']:
                return {'status':'cancelled','error_code':'PRODUCTION_ADMIN_STOPPED'}
            attempts = self.row()['retry_attempts']+1
            manual = exc.manual or attempts >= 3 and exc.code not in {'PRODUCTION_INVITE_READBACK_PENDING', 'PRODUCTION_MEMBERSHIP_PENDING'}
            self.progress(self.row()['stage'], 'waiting_user' if manual else 'unknown', exc.code,
                          retry_attempts=attempts, next_at=int(time.time())+min(300,30*2**min(attempts-1,3)))
            return {'status': self.row()['status'], 'error_code': exc.code}
        except Exception as exc:
            if self.row()['stop_requested']:
                return {'status':'cancelled','error_code':'PRODUCTION_ADMIN_STOPPED'}
            stage = self.row()['stage']
            code = 'PRODUCTION_OAUTH_RESULT_UNKNOWN' if stage == 'oauth_started' else 'PRODUCTION_EXECUTION_UNKNOWN'
            attempts = self.row()['retry_attempts']+1
            exhausted = (stage == 'oauth_started' and self.row()['oauth_attempts']-self.row()['oauth_retry_base'] >= 3) or attempts >= 3
            delay = min(300,30 * 2**max(0,self.row()['oauth_attempts']-1))
            diagnostic = getattr(exc, 'evidence', None)
            self.progress(stage, 'waiting_user' if exhausted else 'unknown', code,
                          diagnostic=diagnostic if isinstance(diagnostic, dict) else None,
                          next_at=int(time.time())+delay, retry_attempts=attempts)
            return {'status': self.row()['status'], 'error_code': code}

    def reconcile_stop(self, backend_factory=Backend):
        row = self.row()
        effect = row['stop_effect']
        if not effect:
            if not row['external_started']:
                effect = 'none'
            else:
                material = self.open(row['material'])
                backend = backend_factory(material)
                try:
                    _, _, _, present = backend.snapshot(row['email'], 'default' if material['parameters']['seat_type']=='standard' else 'prolite')
                except Exception:
                    code = 'PRODUCTION_STOP_READBACK_PENDING'
                    with self.db:
                        self.db.execute('UPDATE production_runs SET error_code=?,next_at=? WHERE operation_id=?',
                                        (code,int(time.time())+30,self.operation))
                        self.db.execute('UPDATE production_admissions SET error_code=? WHERE operation_id=?', (code,self.operation))
                        self.db.execute('UPDATE tasks SET error=? WHERE id=?', (code,'work-'+self.operation))
                        self.db.execute("INSERT INTO events(task_id,ts,stream,line) VALUES(?,?,'stage',?)",
                            ('work-'+self.operation,time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime()),
                             canonical({'stage':'stop_readback','status':'unknown','error_code':code,
                                        'action':'执行已停止；30秒后只读核对原邀请及席位，保留已产出凭据'})))
                    return {'status':'cancelled','error_code':code}
                if present:
                    effect = 'occupied'
                elif row['stop_observed_at'] and time.time()-row['stop_observed_at'] >= 30:
                    effect = 'none'
            with self.db:
                self.db.execute('UPDATE production_runs SET stop_effect=?,stop_observed_at=CASE WHEN stop_observed_at=0 THEN ? ELSE stop_observed_at END,next_at=? WHERE operation_id=?',
                    (effect,int(time.time()),int(time.time())+30,self.operation))
                if effect == 'none' and not row['external_started']:
                    self.db.execute('UPDATE accounts SET claimed=0 WHERE lower(identity)=?',(row['email'],))
        return {'status':'cancelled','error_code':'PRODUCTION_ADMIN_STOPPED'}

    def output(self, backend=None):
        row = self.row()
        doc, material = self.open(row['output_artifact']), self.open(row['material'])
        backend = backend or Backend(material)
        workspace, seat = material['workspace']['account_id'], material['parameters']['seat_type']
        for kind in ('access_token', 'id_token'):
            c = claims(doc.get(kind, ''))
            email = str(c.get('email') or (c.get(PROFILE) or {}).get('email') or '').lower()
            if c.get('exp', 0) <= time.time() or email != row['email'] or c.get(AUTH, {}).get('chatgpt_account_id') != workspace:
                raise Pending('PRODUCTION_OUTPUT_IDENTITY_INVALID', manual=True)
        if not doc.get('refresh_token'):
            raise Pending('PRODUCTION_OUTPUT_INCOMPLETE', manual=True)
        auth = claims(doc['access_token']).get(AUTH, {})
        valid_plans = {'team', 'business', 'self_serve_business'} if seat == 'standard' else {'team_prolite', 'business_prolite', 'self_serve_business_prolite'}
        if auth.get('chatgpt_plan_type') not in valid_plans:
            raise Pending('PRODUCTION_OUTPUT_SEAT_INVALID', manual=True)
        _, _, joined, _ = backend.snapshot(row['email'], 'default' if seat == 'standard' else 'prolite')
        if not joined:
            raise Pending('PRODUCTION_MEMBERSHIP_PENDING')
        self.progress('result_ready', 'running')
        return {'status': 'ready', 'email': row['email'], 'artifact': row['output_artifact'],
                'source_ref': row['source_ref'], 'source_sha256': hashlib.sha256(canonical(doc).encode()).hexdigest(), 'workspace_id': workspace,
                'created_at': self.db.execute('SELECT min(created_at) FROM production_outputs WHERE operation_id=?', (self.operation,)).fetchone()[0]}


def main(request):
    import fcntl
    root = Path(request['root'])
    operation = request['operation_id']
    db = sqlite3.connect(request['db_path'], timeout=15, check_same_thread=False)
    db.row_factory = sqlite3.Row
    run = Run(db, operation, request['artifact_key'].encode(), root)
    locks = root / 'runtime' / 'production-locks'
    locks.mkdir(parents=True, exist_ok=True)
    with (locks / (operation + '.lock')).open('a') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return {'status': 'running', 'error_code': ''}
        if request['action'] == 'admit':
            db.execute('BEGIN IMMEDIATE')
            if run.row():
                db.commit()
                return {'status': run.row()['status'], 'error_code': run.row()['error_code']}
            with db:
                if db.execute('SELECT 1 FROM production_stops WHERE operation_id=?', (operation,)).fetchone():
                    return {'status':'cancelled','error_code':'PRODUCTION_ADMIN_STOPPED'}
                db.execute("INSERT INTO production_runs(operation_id,material,stage,status,updated_at) VALUES(?,?,'admitted','queued',?) ON CONFLICT(operation_id) DO NOTHING",
                           (operation, run.seal(request['material']), int(time.time())))
            run.progress('admitted', 'queued')
            return {'status': 'queued', 'error_code': ''}
        try:
            return run.advance()
        except Pending as exc:
            if run.row()['stop_requested']:
                return {'status':'cancelled','error_code':'PRODUCTION_STOP_READBACK_PENDING'}
            run.progress(run.row()['stage'], 'waiting_user' if exc.manual else 'unknown', exc.code)
            return {'status': run.row()['status'], 'error_code': exc.code}


if __name__ == '__main__':
    try:
        request = json.load(sys.stdin)
        # The reused login library writes diagnostics. Keep raw provider output
        # out of both the structured response and durable task logs.
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            result = main(request)
    except Exception:
        result = {'status': 'unknown', 'error_code': 'PRODUCTION_DRIVER_UNAVAILABLE'}
    print(canonical(result))
