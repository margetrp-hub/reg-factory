import base64
import json
from pathlib import Path
import sqlite3
import tempfile
import time
import unittest

from cryptography.fernet import Fernet
from tools.team_production import Run, Pending, canonical, candidates


def token(email, workspace, plan='free'):
    value = {'exp': int(time.time())+86400, 'iat': int(time.time()), 'email': email,
             'https://api.openai.com/profile': {'email': email},
             'https://api.openai.com/auth': {'chatgpt_account_id': workspace, 'chatgpt_plan_type': plan}}
    return 'e30.' + base64.urlsafe_b64encode(canonical(value).encode()).decode().rstrip('=') + '.test'


class ProductionTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        (self.root/'plus-import').mkdir()
        self.email, self.workspace = 'unused@example.test', 'target-workspace'
        self.db = sqlite3.connect(':memory:')
        self.db.row_factory = sqlite3.Row
        self.addCleanup(self.db.close)
        self.db.executescript('''
          CREATE TABLE accounts(ref TEXT,platform TEXT,identity TEXT,claimed INT,join_status TEXT,status TEXT);
          CREATE TABLE account_credentials(ref TEXT,password TEXT,totp TEXT);
          CREATE TABLE production_results(operation_id TEXT PRIMARY KEY,email TEXT,state TEXT,artifact TEXT);
          CREATE TABLE production_runs(operation_id TEXT PRIMARY KEY,material TEXT,email TEXT DEFAULT '',input_artifact TEXT DEFAULT '',
            output_artifact TEXT DEFAULT '',source_ref TEXT DEFAULT '',source_sha TEXT DEFAULT '',stage TEXT,status TEXT,error_code TEXT DEFAULT '',
            updated_at INT,next_at INT DEFAULT 0,external_started INT DEFAULT 0,oauth_attempts INT DEFAULT 0,
            stop_requested INT DEFAULT 0,stop_effect TEXT DEFAULT '',stop_observed_at INT DEFAULT 0,
            retry_attempts INT DEFAULT 0,oauth_retry_base INT DEFAULT 0,invite_attempts INT DEFAULT 0,invite_observed_at INT DEFAULT 0);
          CREATE UNIQUE INDEX production_run_email ON production_runs(email)
            WHERE email<>'' AND NOT (status='cancelled' AND stop_effect='none' AND stop_requested=1);
          CREATE TABLE production_admissions(operation_id TEXT,state TEXT,error_code TEXT,finished_at TEXT DEFAULT '',parameters TEXT);
          CREATE TABLE production_outputs(operation_id TEXT,attempt INT,artifact TEXT,created_at INT);
          CREATE TABLE tasks(id TEXT,status TEXT,error TEXT);
          CREATE TABLE events(task_id TEXT,ts TEXT,stream TEXT,line TEXT);
        ''')
        self.db.execute("INSERT INTO accounts VALUES('free','chatgpt',?,0,'none','active')", (self.email,))
        self.db.execute("INSERT INTO account_credentials VALUES('free','synthetic-password','synthetic-totp')")
        doc = {'email': self.email, 'access_token': token(self.email,'personal'), 'refresh_token':'synthetic-refresh'}
        (self.root/'plus-import'/'free.json').write_text(json.dumps(doc))
        self.key = Fernet.generate_key()
        self.run = Run(self.db,'original',self.key,self.root)
        self.material = {'parameters': {'seat_type':'standard','mode':'produce','production_ref':'slot','order_ref':'order','product_id':34}, 'workspace': {'account_id': self.workspace},
                         'expires_at': int(time.time())+900, 'readback_until':int(time.time())+86400}
        self.db.execute("INSERT INTO production_runs(operation_id,material,stage,status) VALUES('original',?,'admitted','queued')", (self.run.seal(self.material),))
        self.db.execute("INSERT INTO production_admissions(operation_id,state,error_code,parameters) VALUES('original','queued','',?)", (canonical(self.material['parameters']),))
        self.db.execute("INSERT INTO tasks VALUES('work-original','queued','')")
        self.db.commit()
        owner = self
        class Backend:
            invites = 0
            logins = 0
            present = False
            joined = False
            lose_invite = False
            lose_oauth = False
            def __init__(self, material): pass
            def preflight(self, candidate, seat): pass
            def snapshot(self, email, seat): return [],[],self.joined,self.present
            def invite(self, email, seat):
                Backend.invites+=1
                Backend.present=True
                if self.lose_invite: raise Pending('PRODUCTION_UPSTREAM_UNAVAILABLE')
            def authorize(self, candidate, workspace, persist, progress):
                Backend.logins+=1
                if self.lose_oauth: raise RuntimeError('private upstream text')
                Backend.joined=True
                t=token(owner.email,workspace,'team')
                return {'access_token':t,'id_token':t,'refresh_token':'new-synthetic-refresh'}
        self.backend=Backend

    def restart(self, *, slot='slot'):
        material = {**self.material, 'parameters': {**self.material['parameters'], 'production_ref':slot}}
        self.db.execute("INSERT INTO production_runs(operation_id,material,stage,status) VALUES('restart',?,'admitted','queued')", (self.run.seal(material),))
        self.db.execute("INSERT INTO production_admissions(operation_id,state,error_code,parameters) VALUES('restart','queued','',?)", (canonical(material['parameters']),))
        self.db.execute("INSERT INTO tasks VALUES('work-restart','queued','')")
        self.db.commit()
        return Run(self.db,'restart',self.key,self.root)

    def failed_output(self):
        self.assertEqual(self.run.advance(self.backend)['status'],'ready')
        self.db.execute("INSERT INTO production_results VALUES('original',?,'failed',?)", (self.email,self.run.row()['output_artifact']))
        self.db.execute("UPDATE production_runs SET status='cancelled',stop_requested=1,stop_effect='none' WHERE operation_id='original'")
        self.backend.present = self.backend.joined = False
        self.db.commit()

    def test_confirmed_stop_can_restart_original_input_without_replacing_failed_output(self):
        self.failed_output()
        original = dict(self.run.row())
        self.assertEqual(candidates(self.db,self.root),[])
        restarted = self.restart()
        self.assertEqual(restarted.advance(self.backend)['status'],'ready')
        self.assertEqual(restarted.row()['email'], self.email)
        self.assertEqual(restarted.open(restarted.row()['input_artifact']), self.run.open(original['input_artifact']))
        self.assertEqual(dict(self.run.row()), original)
        self.assertEqual(self.db.execute('SELECT artifact FROM production_results').fetchone()[0],original['output_artifact'])
        self.assertEqual(self.db.execute('SELECT count(*) FROM production_outputs').fetchone()[0],2)
        self.assertEqual((self.backend.invites,self.backend.logins),(2,2))
        restarted.advance(self.backend)
        self.assertEqual((self.backend.invites,self.backend.logins),(2,2))

    def test_restart_refuses_another_slot_or_unsettled_successful_or_active_account(self):
        self.failed_output()
        restarted = self.restart(slot='another-slot')
        with self.assertRaises(Pending): restarted.choose()
        self.db.execute("UPDATE production_runs SET material=? WHERE operation_id='restart'", (self.run.seal(self.material),))
        self.db.commit()
        for effect in ('', 'occupied'):
            self.db.execute("UPDATE production_runs SET stop_effect=? WHERE operation_id='original'", (effect,))
            self.db.commit()
            with self.assertRaises(Pending): restarted.choose()
        self.db.execute("UPDATE production_runs SET stop_effect='none' WHERE operation_id='original'")
        self.db.execute("UPDATE production_results SET state='completed'")
        self.db.commit()
        with self.assertRaises(Pending): restarted.choose()
        self.db.execute("UPDATE production_results SET state='failed'")
        self.db.execute("INSERT INTO production_runs(operation_id,email,stage,status) VALUES('competing',?,'admitted','queued')", (self.email,))
        self.db.commit()
        with self.assertRaises(Pending): restarted.choose()
        self.assertFalse(restarted.row()['input_artifact'])

    def test_stop_readback_failure_keeps_unknown_with_backoff(self):
        self.db.execute("UPDATE production_runs SET status='cancelled',stop_requested=1,external_started=1 WHERE operation_id='original'")
        self.db.commit()
        class Unavailable(self.backend):
            def snapshot(self, *args):
                raise Pending('PRODUCTION_UPSTREAM_UNAVAILABLE')
        self.assertEqual(self.run.reconcile_stop(Unavailable)['error_code'],'PRODUCTION_STOP_READBACK_PENDING')
        self.assertEqual(self.run.row()['stop_effect'],'')
        self.assertGreaterEqual(self.run.row()['next_at'],int(time.time())+29)

    def test_unstarted_stop_releases_account_for_a_new_quantity_attempt(self):
        self.db.execute("UPDATE production_runs SET email=?,status='cancelled',stop_requested=1 WHERE operation_id='original'",(self.email,))
        self.db.execute("UPDATE accounts SET claimed=1")
        self.db.commit()
        self.run.reconcile_stop(self.backend)
        self.assertEqual(self.run.row()['stop_effect'],'none')
        self.assertEqual([item['email'] for item in candidates(self.db,self.root)],[self.email])
        self.db.execute("INSERT INTO production_runs(operation_id,email,stage,status) VALUES('replacement',?,'selected','queued')",(self.email,))
        self.db.commit()

    def test_lost_invitation_ack_reconciles_then_issues_once(self):
        self.backend.lose_invite=True
        self.assertEqual(self.run.advance(self.backend)['status'],'unknown')
        self.assertEqual(self.run.row()['stage'],'invite_started')
        result=self.run.advance(self.backend)
        self.assertEqual(result['status'],'ready')
        self.assertEqual((self.backend.invites,self.backend.logins),(1,1))
        self.assertEqual(self.db.execute('SELECT claimed FROM accounts').fetchone()[0],1)
        self.assertEqual(self.run.output(self.backend(self.material))['artifact'],result['artifact'])
        self.assertEqual(self.backend.logins,1)
        self.assertNotIn('new-synthetic-refresh',''.join(r[0] for r in self.db.execute('SELECT line FROM events')))

    def test_interrupted_oauth_retries_same_account_then_can_be_stopped(self):
        self.backend.lose_oauth=True
        self.assertEqual(self.run.advance(self.backend)['status'],'unknown')
        self.assertEqual(self.run.advance(self.backend)['status'],'unknown')
        self.assertEqual(self.run.advance(self.backend)['status'],'waiting_user')
        self.assertEqual(self.run.advance(self.backend)['status'],'waiting_user')
        self.assertEqual((self.backend.invites,self.backend.logins),(1,3))
        self.assertEqual(self.run.row()['email'],self.email)
        self.db.execute("UPDATE production_runs SET stop_requested=1,status='cancelled'")
        self.db.commit()
        self.assertEqual(self.run.advance(self.backend)['status'],'cancelled')
        self.assertEqual(self.run.row()['stop_effect'],'occupied')
        self.assertEqual(self.backend.logins,3)

    def test_saved_output_survives_failure_before_membership_confirmation(self):
        normal=self.backend.authorize
        def authorize(backend,candidate,workspace,persist,progress):
            out=normal(backend,candidate,workspace,persist,progress)
            self.backend.joined=False
            return out
        self.backend.authorize=authorize
        self.assertEqual(self.run.advance(self.backend)['status'],'unknown')
        self.assertTrue(self.run.row()['output_artifact'])
        self.backend.joined=True
        self.assertEqual(self.run.output(self.backend(self.material))['status'],'ready')
        self.assertEqual((self.backend.invites,self.backend.logins),(1,1))

    def test_alias_and_retained_team_history_exclude_an_account(self):
        self.assertEqual(len(candidates(self.db,self.root)),1)
        self.db.execute("INSERT INTO accounts VALUES('other','chatgpt',?,1,'none','active')", (self.email,))
        self.assertEqual(candidates(self.db,self.root),[])
        self.db.execute("DELETE FROM accounts WHERE ref='other'")
        folder=self.root/'asset-results'; folder.mkdir()
        (folder/'old.json').write_text(json.dumps({'payload':{'email':self.email,'access_token':token(self.email,'old-workspace','team')}}))
        self.assertEqual(candidates(self.db,self.root),[])

    def test_issued_output_is_retained_when_post_exchange_validation_raises(self):
        normal = self.backend.authorize
        def authorize(backend,candidate,workspace,persist,progress):
            doc = normal(backend,candidate,workspace,persist,progress)
            persist(doc)
            raise RuntimeError('validation interrupted after issuance')
        self.backend.authorize = authorize
        self.assertEqual(self.run.advance(self.backend)['status'],'unknown')
        self.assertTrue(self.run.row()['output_artifact'])
        self.assertEqual(self.run.advance(self.backend)['status'],'ready')
        self.assertEqual(self.backend.logins,1)
        self.assertEqual(self.db.execute('SELECT count(*) FROM production_outputs').fetchone()[0],1)

    def test_missing_invitation_is_read_back_before_retrying_same_email(self):
        def invite(backend,email,seat):
            self.assertEqual(email,self.email)
            self.backend.invites += 1
            if self.backend.invites == 1:
                raise Pending('PRODUCTION_UPSTREAM_UNAVAILABLE')
            self.backend.present = True
        self.backend.invite = invite
        self.assertEqual(self.run.advance(self.backend)['status'],'unknown')
        self.assertEqual(self.run.advance(self.backend)['status'],'unknown')
        self.assertEqual(self.backend.invites,1)
        self.db.execute('UPDATE production_runs SET invite_observed_at=?',(int(time.time())-31,))
        self.db.commit()
        self.assertEqual(self.run.advance(self.backend)['status'],'unknown')
        self.assertEqual(self.run.advance(self.backend)['status'],'ready')
        self.assertEqual((self.backend.invites,self.backend.logins),(2,1))

    def test_stop_before_mutation_and_stop_absence_require_distinct_evidence(self):
        self.db.execute("UPDATE production_runs SET stop_requested=1,status='cancelled'")
        self.db.commit()
        self.assertEqual(self.run.advance(self.backend)['status'],'cancelled')
        self.assertEqual(self.run.row()['stop_effect'],'none')
        self.assertEqual((self.backend.invites,self.backend.logins),(0,0))
        self.db.execute("UPDATE production_runs SET external_started=1,stop_effect='',email=?",(self.email,))
        self.db.commit()
        self.run.advance(self.backend)
        self.assertEqual(self.run.row()['stop_effect'],'')
        self.db.execute('UPDATE production_runs SET stop_observed_at=?',(int(time.time())-31,))
        self.db.commit()
        self.run.advance(self.backend)
        self.assertEqual(self.run.row()['stop_effect'],'none')


if __name__ == '__main__': unittest.main()
