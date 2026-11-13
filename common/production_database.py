"""The production executor shares Go's database, with explicit short transactions.

SQLite is supported only as an explicitly supplied local test database. A PG
connection failure never falls back to a file or an old migration snapshot.
"""
from pathlib import Path
import re
from urllib.parse import parse_qsl, urlencode, urlsplit, urlunsplit


class Row(dict):
    def __getitem__(self, key):
        return tuple(self.values())[key] if isinstance(key, int) else super().__getitem__(key)


def row_factory(cursor):
    names = [column.name for column in cursor.description or ()]
    return lambda values: Row(zip(names, values))


class Postgres:
    postgres = True

    def __init__(self, dsn):
        import psycopg
        # pgx accepts search_path as a URI runtime parameter; libpq expects options.
        parsed = urlsplit(dsn)
        query = dict(parse_qsl(parsed.query))
        schema = query.pop('search_path', '')
        if schema:
            if not re.fullmatch('[A-Za-z_][A-Za-z0-9_]*', schema):
                raise ValueError('PRODUCTION_DATABASE_SCHEMA_INVALID')
            query['options'] = (query.get('options', '') + ' -csearch_path=' + schema).strip()
            dsn = urlunsplit(parsed._replace(query=urlencode(query)))
        self.connection = psycopg.connect(dsn, autocommit=True, row_factory=row_factory, connect_timeout=10)
        self.connection.execute('SET statement_timeout=15000')
        self.connection.execute('SET lock_timeout=10000')

    @property
    def in_transaction(self):
        from psycopg.pq import TransactionStatus
        return self.connection.info.transaction_status != TransactionStatus.IDLE

    def execute(self, statement, parameters=()):
        if statement.strip().upper() == 'BEGIN IMMEDIATE':
            self.connection.execute('BEGIN')
            # Matches the original short single-writer section for account choice;
            # no provider call is made while this lock or transaction is held.
            return self.connection.execute('SELECT pg_advisory_xact_lock(hashtext(current_schema()), 914027)')
        return self.connection.execute(statement.replace('?', '%s'), parameters)

    def __enter__(self):
        if not self.in_transaction:
            self.connection.execute('BEGIN')
        return self

    def __exit__(self, kind, value, trace):
        self.rollback() if kind else self.commit()

    def commit(self):
        self.connection.commit()

    def rollback(self):
        self.connection.rollback()

    def close(self):
        self.connection.close()


def connect(dsn):
    if dsn.startswith(('postgres://', 'postgresql://')) or 'dbname=' in dsn:
        return Postgres(dsn)
    # Only an existing database explicitly passed by Go/test callers is allowed.
    # Diagnostic identifiers such as regfactory/public must not create a file.
    import sqlite3
    path = Path(dsn).resolve()
    db = sqlite3.connect(path.as_uri() + '?mode=rw', uri=True, timeout=15, check_same_thread=False)
    db.row_factory = sqlite3.Row
    return db
