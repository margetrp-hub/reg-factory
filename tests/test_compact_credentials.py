import hashlib
import json
import os
import sqlite3
import time
import uuid
from urllib.parse import urlsplit, urlunsplit, parse_qsl, urlencode

import pytest

from tools.compact_credentials import compact
from tools.asset_ops import _find_credential, _reauthorization_document
from common.credential_versions import tokens


def test_compaction_keeps_current_login_and_old_refs_but_retires_old_tokens(tmp_path):
    with sqlite3.connect(tmp_path/'regd.db') as db:
        db.executescript('''CREATE TABLE accounts(ref TEXT,identity TEXT,platform TEXT);
            CREATE TABLE tasks(id TEXT,script TEXT,status TEXT);
            CREATE TABLE account_refreshes(task_id TEXT,source_sha TEXT,result_sha TEXT);''')
        for ref in ('old','current','missing-file'):
            db.execute("INSERT INTO accounts VALUES(?,?,'chatgpt')",(ref,'same@example.test'))
        db.execute("INSERT INTO tasks VALUES('unresolved','asset_ops','unknown')")
        db.execute("INSERT INTO account_refreshes VALUES('unresolved','pinned','')")
    directory=tmp_path/'assets/chatgpt/oauth'
    directory.mkdir(parents=True)
    for ref,stamp in [('old','2026-11-01T00:00:00Z'),('current','2026-11-02T00:00:00Z')]:
        (directory/(ref+'.json')).write_text(json.dumps({'ref':ref,'platform':'chatgpt','kind':'oauth','email':'same@example.test',
          'credential_updated_at':stamp,'payload':{'email':'same@example.test','access_token':ref,'refresh_token':ref+'-rt'}}))
    login=tmp_path/'assets/chatgpt/imported/old.json'
    login.parent.mkdir(parents=True)
    login.write_text(json.dumps({'ref':'old','kind':'imported','email':'same@example.test','payload':{'password':'fixture-password','totp':'fixture-totp'}}))
    before=login.read_bytes()
    results=tmp_path/'asset-results';results.mkdir()
    for name in ('pinned','obsolete'):
        path=results/(name+'.json');path.write_text('{}');os.utime(path,(time.time()-1000,)*2)
    dry=compact(tmp_path,database=str(tmp_path/'regd.db'))
    assert not dry['applied'] and 'old-rt' in (directory/'old.json').read_text()
    result=compact(tmp_path,apply=True,database=str(tmp_path/'regd.db'))
    assert result['current_documents']==1 and result['verified_references']==3
    assert 'old-rt' not in (directory/'old.json').read_text()
    assert (results/'pinned.json').exists() and not (results/'obsolete.json').exists()
    assert login.read_bytes()==before
    for ref in ('old','current','missing-file'):
        _,doc=_find_credential(ref,root=tmp_path)
        assert tokens(doc)['refresh_token']=='current-rt'
        assert doc['credential_updated_at']=='2026-11-02T00:00:00Z'
    assert compact(tmp_path,apply=True,database=str(tmp_path/'regd.db'))['current_documents']==1


def test_compaction_reads_live_postgres_and_preserves_unknown_task(tmp_path):
    dsn = os.environ.get('REG_FACTORY_TEST_POSTGRES_DSN')
    if not dsn:
        pytest.skip('disposable PostgreSQL required')
    import psycopg
    parsed = urlsplit(dsn)
    assert parsed.hostname in {'127.0.0.1', 'localhost'} and parsed.path == '/sg_release_test'
    schema = 'compact_' + uuid.uuid4().hex
    with psycopg.connect(dsn, autocommit=True) as db:
        db.execute('CREATE SCHEMA ' + schema)
        try:
            db.execute('SET search_path TO ' + schema)
            db.execute('CREATE TABLE accounts(ref TEXT,identity TEXT,platform TEXT); CREATE TABLE tasks(id TEXT,script TEXT,status TEXT); CREATE TABLE account_refreshes(task_id TEXT,source_sha TEXT,result_sha TEXT)')
            db.execute("INSERT INTO accounts VALUES('one','one@example.test','chatgpt'); INSERT INTO tasks VALUES('unknown-task','asset_ops','unknown'); INSERT INTO account_refreshes VALUES('unknown-task','pinned','')")
            (tmp_path/'regd.db').write_text('retired snapshot must never be read')
            current=tmp_path/'assets/chatgpt/oauth/one.json';current.parent.mkdir(parents=True)
            current.write_text(json.dumps({'ref':'one','platform':'chatgpt','kind':'oauth','email':'one@example.test','payload':{'access_token':'current','refresh_token':'current-rt'}}))
            pinned=tmp_path/'asset-results/pinned.json';pinned.parent.mkdir();pinned.write_text('{}');os.utime(pinned,(time.time()-1000,)*2)
            url=urlunsplit(parsed._replace(query=urlencode(dict(parse_qsl(parsed.query),search_path=schema))))
            result=compact(tmp_path,database=url,apply=True)
            assert result['current_documents']==1 and pinned.exists()
            db.execute("UPDATE tasks SET status='running'")
            with pytest.raises(RuntimeError,match='CREDENTIAL_TASK_ACTIVE'):
                compact(tmp_path,database=url,apply=True)
        finally:
            db.execute('DROP SCHEMA '+schema+' CASCADE')
