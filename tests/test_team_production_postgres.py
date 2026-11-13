"""Run the existing production state journeys against disposable PostgreSQL."""
import os
import unittest
import uuid
from urllib.parse import urlsplit, urlunsplit, parse_qsl, urlencode
from unittest.mock import patch

from common.production_database import Postgres
from tests import test_team_production as examples


@unittest.skipUnless(os.environ.get('REG_FACTORY_TEST_POSTGRES_DSN'), 'disposable PG required')
class PostgresProductionTests(examples.ProductionTests):
    def setUp(self):
        import psycopg
        dsn = os.environ['REG_FACTORY_TEST_POSTGRES_DSN']
        parsed = urlsplit(dsn)
        if parsed.hostname not in {'127.0.0.1', 'localhost'} or parsed.path != '/sg_release_test':
            raise ValueError('disposable local sg_release_test required')
        schema = 'production_py_' + uuid.uuid4().hex
        admin = psycopg.connect(dsn, autocommit=True)
        admin.execute('CREATE SCHEMA ' + schema)
        def cleanup():
            admin.execute('DROP SCHEMA ' + schema + ' CASCADE')
            admin.close()
        self.addCleanup(cleanup)
        query = dict(parse_qsl(parsed.query), search_path=schema)
        db = Postgres(urlunsplit(parsed._replace(query=urlencode(query))))
        db.executescript = lambda statements: db.connection.execute(statements.replace('id INTEGER PRIMARY KEY AUTOINCREMENT', 'id BIGSERIAL PRIMARY KEY'))
        with patch.object(examples.sqlite3, 'connect', return_value=db):
            super().setUp()
