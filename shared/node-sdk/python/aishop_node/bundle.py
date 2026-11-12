"""Verify an exported SDK bundle before packaging or starting a node."""
import hashlib
import json
from pathlib import Path


def verify(directory):
    directory = Path(directory).resolve()
    manifest = json.loads((directory / 'manifest.json').read_text('utf-8'))
    if manifest.get('schema') != 'aishop.node-sdk-bundle.v1' or manifest.get('contract') != 1:
        raise ValueError('NODE_SDK_MANIFEST_INVALID')
    files = manifest['files']
    if hashlib.sha256(json.dumps(files, sort_keys=True).encode()).hexdigest() != manifest['source_sha256']:
        raise ValueError('NODE_SDK_MANIFEST_INVALID')
    for name, digest in files.items():
        path = directory / name
        if path.is_symlink() or not path.resolve().is_relative_to(directory) or hashlib.sha256(path.read_bytes()).hexdigest() != digest:
            raise ValueError('NODE_SDK_CONTENT_MISMATCH')
    actual = {p.relative_to(directory).as_posix() for p in directory.rglob('*') if p.is_file()
              and p.suffix in {'.py', '.go', '.mod', '.json'} and p.name != 'manifest.json'}
    if actual != set(files):
        raise ValueError('NODE_SDK_CONTENT_MISMATCH')
    release = json.loads((directory / 'contracts/sdk-release.v1.json').read_text('utf-8'))
    if manifest.get('sdk_version') != release.get('version') or manifest['contract'] != release.get('contract'):
        raise ValueError('NODE_SDK_VERSION_MISMATCH')
    return manifest['source_sha256']


if __name__ == '__main__':
    import sys
    print(verify(sys.argv[1]))
