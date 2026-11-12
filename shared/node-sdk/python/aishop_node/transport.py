"""Bounded node -> Work transport for encrypted asset messages; never retries."""
import hashlib
import hmac
import json
import time
import uuid
from urllib.error import HTTPError, URLError
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener

from .assets import AssetProtocolError, IDENTIFIER, strict_json

DOMAIN = b'aishop-node-assets-relay-v1\n'
PATH = '/internal/work/v1/assets'


def relay_request(envelope, center_id, receipt_key, *, now=None):
    if (not isinstance(center_id, str) or not IDENTIFIER.fullmatch(center_id)
            or not isinstance(receipt_key, bytes) or len(receipt_key) < 32):
        raise AssetProtocolError('ASSET_RELAY_CONFIG_INVALID')
    stamp = int(time.time()) if now is None else now
    payload = {'schema': 'aishop.node-assets-relay.v1', 'node_id': envelope['node_id'],
               'center_id': center_id, 'request_id': str(uuid.uuid4()), 'issued_at': stamp,
               'expires_at': stamp + 120, 'envelope': envelope}
    raw = json.dumps(payload, ensure_ascii=True, sort_keys=True, separators=(',', ':'), allow_nan=False).encode()
    return {'payload': payload, 'signature': hmac.new(receipt_key, DOMAIN + raw, hashlib.sha256).hexdigest()}


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def work_sender(origin, center_id, receipt_key):
    post = work_transport(origin)
    def send(envelope):
        return post(PATH, relay_request(envelope, center_id, receipt_key))
    return send


def work_transport(origin):
    parsed = urlsplit(origin)
    if (parsed.scheme not in {'http', 'https'} or not parsed.hostname or parsed.username or parsed.password
            or parsed.query or parsed.fragment or parsed.path not in {'', '/'}
            or any(ord(c) <= 32 or ord(c) == 127 for c in origin)):
        raise AssetProtocolError('ASSET_RELAY_ORIGIN_INVALID')
    opener = build_opener(ProxyHandler({}), NoRedirect())
    def send(path, document):
        request = Request(origin.rstrip('/') + path, json.dumps(document).encode(),
                          {'Content-Type': 'application/json'}, method='POST')
        try:
            with opener.open(request, timeout=20) as response:
                raw = response.read(380 * 1024 + 1)
                if response.status != 200 or len(raw) > 380 * 1024:
                    raise AssetProtocolError('ASSET_RELAY_RESPONSE_INVALID')
            return strict_json(raw)
        except HTTPError as exc:
            raise AssetProtocolError('ASSET_RELAY_REJECTED' if 400 <= exc.code < 500 and exc.code != 429 else 'ASSET_RELAY_UNAVAILABLE') from None
        except (URLError, TimeoutError, OSError):
            raise AssetProtocolError('ASSET_RELAY_UNAVAILABLE') from None
    return send


def work_claim(origin, node_id, center_id, receipt_key, request_key, attempt_id):
    domain = b'aishop-node-asset-claim-v1\n'
    stamp, request_id = int(time.time()), str(uuid.uuid4())
    p = {'schema': 'aishop.node-asset-claim.v1', 'node_id': node_id, 'center_id': center_id,
         'attempt_id': attempt_id, 'request_id': request_id, 'issued_at': stamp, 'expires_at': stamp+120}
    def sign(value, key):
        raw = json.dumps(value, ensure_ascii=True, sort_keys=True, separators=(',', ':'), allow_nan=False).encode()
        return hmac.new(key, domain+raw, hashlib.sha256).hexdigest()
    if len(receipt_key) < 32 or len(request_key) < 32:
        raise AssetProtocolError('ASSET_RELAY_CONFIG_INVALID')
    doc = work_transport(origin)('/internal/work/v1/asset-claim', {'payload': p, 'signature': sign(p, receipt_key)})
    r = doc.get('payload')
    if (not isinstance(r, dict) or set(doc) != {'payload', 'signature'}
            or set(r) != {'schema', 'node_id', 'center_id', 'attempt_id', 'request_id', 'issued_at', 'expires_at', 'result'}
            or not isinstance(doc['signature'], str) or not hmac.compare_digest(doc['signature'], sign(r, request_key))
            or r['schema'] != 'aishop.node-asset-claim-receipt.v1'
            or any(r[name] != p[name] for name in ('node_id', 'center_id', 'attempt_id', 'request_id'))
            or type(r['issued_at']) is not int or type(r['expires_at']) is not int
            or not stamp-120 <= r['issued_at'] <= stamp+30 or not stamp < r['expires_at'] <= r['issued_at']+120
            or not isinstance(r['result'], dict)):
        raise AssetProtocolError('ASSET_CLAIM_RECEIPT_INVALID')
    return r['result']
