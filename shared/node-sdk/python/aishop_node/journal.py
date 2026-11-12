"""Immutable encrypted result journal. Replays persistence, never external work."""
import json
import os
from pathlib import Path
import uuid

from cryptography.fernet import Fernet

from .assets import AssetProtocolError, strict_json


class ResultJournal:
    def __init__(self, directory, key):
        self.directory = Path(directory)
        self.directory.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.cipher = Fernet(key.encode())

    def path(self, task_id):
        if str(uuid.UUID(task_id)) != task_id:
            raise AssetProtocolError('ASSET_JOURNAL_ID_INVALID')
        return self.directory / (task_id + '.result')

    def get(self, task_id):
        path = self.path(task_id)
        if not path.exists():
            return None
        if path.is_symlink() or path.stat().st_size > 2 * 1024 * 1024:
            raise AssetProtocolError('ASSET_JOURNAL_INVALID')
        try:
            value = strict_json(self.cipher.decrypt(path.read_bytes()))
            if not isinstance(value, dict) or value.get('task_id') != task_id:
                raise ValueError()
            return value
        except Exception:
            raise AssetProtocolError('ASSET_JOURNAL_UNREADABLE') from None

    def put(self, task_id, value):
        raw = json.dumps(value, sort_keys=True, ensure_ascii=True, separators=(',', ':'), allow_nan=False).encode()
        if value.get('task_id') != task_id or len(raw) > 1024 * 1024:
            raise AssetProtocolError('ASSET_JOURNAL_INVALID')
        target = self.path(task_id)
        temporary = self.directory / (str(uuid.uuid4()) + '.tmp')
        try:
            with temporary.open('xb') as stream:
                if os.name != 'nt':
                    os.chmod(temporary, 0o600)
                stream.write(self.cipher.encrypt(raw))
                stream.flush()
                os.fsync(stream.fileno())
            try:
                # Publish a complete file atomically without replacing evidence.
                os.link(temporary, target)
            except FileExistsError:
                if self.get(task_id) != value:
                    raise AssetProtocolError('ASSET_JOURNAL_CONFLICT') from None
            self._sync()
        finally:
            temporary.unlink(missing_ok=True)

    def pending(self):
        return sorted(path.stem for path in self.directory.glob('*.result'))

    def verify_writable(self):
        """Verify the actual durable storage boundary before external work."""
        probe = self.directory / (str(uuid.uuid4()) + '.probe')
        try:
            with probe.open('xb') as stream:
                stream.write(b'asset-result-journal')
                stream.flush()
                os.fsync(stream.fileno())
            self._sync()
        finally:
            probe.unlink(missing_ok=True)
            self._sync()

    def acknowledge(self, task_id, value):
        if self.get(task_id) != value:
            raise AssetProtocolError('ASSET_JOURNAL_CONFLICT')
        self.path(task_id).unlink()
        self._sync()

    def _sync(self):
        if os.name != 'nt':
            fd = os.open(self.directory, os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(fd)
            finally:
                os.close(fd)
