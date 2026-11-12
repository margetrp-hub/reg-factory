"""Authenticated, opaque asset messages shared with the Go node SDK.

The node's asset key is independent of its Work transport key and the storage
master key. Work relays the envelope; only the node and Production open it.
"""
import base64
import json
import os
import re
import time
import uuid

from cryptography.exceptions import InvalidTag
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

SCHEMA = "aishop.node-assets.v1"
IDENTIFIER = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$")


class AssetProtocolError(ValueError):
    pass


def strict_json(raw):
    def pairs(items):
        result = {}
        for key, value in items:
            if key in result:
                raise AssetProtocolError("ASSET_DUPLICATE_FIELD")
            result[key] = value
        return result
    try:
        value = json.loads(raw, object_pairs_hook=pairs,
                           parse_constant=lambda _: (_ for _ in ()).throw(ValueError()))
        def depth(item, level=0):
            if level > 32:
                raise ValueError()
            if isinstance(item, dict):
                for nested in item.values():
                    depth(nested, level + 1)
            elif isinstance(item, list):
                for nested in item:
                    depth(nested, level + 1)
        depth(value)
        return value
    except (ValueError, TypeError, UnicodeError, RecursionError) as exc:
        raise AssetProtocolError("ASSET_MESSAGE_INVALID") from exc


def pack(node_id, key, body, *, direction="request", request_id=None, now=None):
    stamp = int(time.time()) if now is None else now
    request_id = request_id or str(uuid.uuid4())
    if (not isinstance(node_id, str) or not IDENTIFIER.fullmatch(node_id)
            or not isinstance(request_id, str) or not IDENTIFIER.fullmatch(request_id)
            or direction not in {"request", "response"} or not isinstance(body, dict) or type(stamp) is not int):
        raise AssetProtocolError("ASSET_SCOPE_INVALID")
    payload = {"schema": SCHEMA, "node_id": node_id, "request_id": request_id,
               "direction": direction, "issued_at": stamp, "expires_at": stamp + 120, "body": body}
    raw = json.dumps(payload, ensure_ascii=False, sort_keys=True, separators=(",", ":"), allow_nan=False).encode()
    if len(raw) > 256 * 1024:
        raise AssetProtocolError("ASSET_MESSAGE_TOO_LARGE")
    nonce = os.urandom(12)
    aad = f"{SCHEMA}\n{node_id}\n{direction}".encode()
    sealed = base64.urlsafe_b64encode(nonce + AESGCM(decode_key(key)).encrypt(nonce, raw, aad)).decode()
    return {"schema": SCHEMA, "node_id": node_id, "sealed": sealed}


def unpack(envelope, key, *, direction="request", node_id=None, request_id=None, now=None):
    stamp = int(time.time()) if now is None else now
    if (not isinstance(envelope, dict) or set(envelope) != {"schema", "node_id", "sealed"}
            or envelope["schema"] != SCHEMA or not isinstance(envelope["node_id"], str)
            or not IDENTIFIER.fullmatch(envelope["node_id"]) or not isinstance(envelope["sealed"], str)
            or len(envelope["sealed"]) > 360 * 1024):
        raise AssetProtocolError("ASSET_MESSAGE_INVALID")
    try:
        token = base64.b64decode(envelope["sealed"], altchars=b"-_", validate=True)
        if base64.urlsafe_b64encode(token).decode() != envelope['sealed']:
            raise ValueError()
        aad = f"{SCHEMA}\n{envelope['node_id']}\n{direction}".encode()
        raw = AESGCM(decode_key(key)).decrypt(token[:12], token[12:], aad)
        if len(raw) > 256 * 1024:
            raise ValueError()
    except (ValueError, InvalidTag, UnicodeError):
        raise AssetProtocolError("ASSET_AUTHENTICATION_FAILED") from None
    payload = strict_json(raw)
    if (not isinstance(payload, dict) or set(payload) != {"schema", "node_id", "request_id", "direction", "issued_at", "expires_at", "body"}
            or payload["schema"] != SCHEMA or payload["node_id"] != envelope["node_id"]
            or node_id is not None and payload["node_id"] != node_id
            or request_id is not None and payload["request_id"] != request_id
            or payload["direction"] != direction or not isinstance(payload["body"], dict)
            or not isinstance(payload["request_id"], str) or not IDENTIFIER.fullmatch(payload["request_id"])
            or type(payload["issued_at"]) is not int or type(payload["expires_at"]) is not int
            or not stamp - 120 <= payload["issued_at"] <= stamp + 30
            or not stamp < payload["expires_at"] <= payload["issued_at"] + 120):
        raise AssetProtocolError("ASSET_SCOPE_INVALID")
    return payload


def decode_key(value):
    try:
        key = base64.b64decode(value, altchars=b"-_", validate=True)
        if len(key) != 32 or base64.urlsafe_b64encode(key).decode() != value:
            raise ValueError()
        return key
    except (ValueError, TypeError, UnicodeError):
        raise AssetProtocolError("ASSET_KEY_INVALID") from None


def exchange(send, node_id, key, body):
    """Send once. On a lost ACK, callers resend their *persisted* original result.

    send is the owning node's authenticated Work relay. No external operation,
    token refresh or payment is ever retried by this helper.
    """
    request_id = str(uuid.uuid4())
    envelope = pack(node_id, key, body, request_id=request_id)
    response = unpack(send(envelope), key, direction='response', node_id=node_id, request_id=request_id)
    answer = response['body']
    if answer.get('task_id') != body.get('task_id'):
        raise AssetProtocolError('ASSET_RECEIPT_SCOPE_INVALID')
    if body.get('action') == 'result' and answer.get('result_id') != body['result'].get('result_id'):
        raise AssetProtocolError('ASSET_RECEIPT_SCOPE_INVALID')
    return answer


def validate_result_receipt(receipt, *, task_id, account_id, result_id, outcome, expected_revision, refresh=False):
    """Validate durable confirmation before a node discards its saved result.

    Unknown evidence stays pending. A closed checkout is an explicit owner's
    resolution, never permission to execute the original operation again.
    """
    states = {'success': 'completed', 'failed': 'failed', 'unknown': 'unknown'}
    if (not isinstance(receipt, dict) or outcome not in states or type(refresh) is not bool
            or type(expected_revision) is not int or expected_revision < 1
            or not all(isinstance(v, str) and IDENTIFIER.fullmatch(v) for v in (task_id, account_id, result_id))
            or receipt.get('task_id') != task_id or receipt.get('account_id') != account_id):
        raise AssetProtocolError('ASSET_ACK_SCOPE_INVALID')
    if receipt.get('state') == 'closed':
        if type(receipt.get('expected_revision')) is not int or receipt['expected_revision'] != expected_revision:
            raise AssetProtocolError('ASSET_ACK_SCOPE_INVALID')
        return 'closed'
    if (receipt.get('state') != states[outcome]
            or receipt.get('result_id', receipt.get('last_result_id')) != result_id):
        raise AssetProtocolError('ASSET_ACK_SCOPE_INVALID')
    if outcome != 'unknown':
        revision = receipt.get('credential_revision')
        if (type(revision) is not int or not expected_revision <= revision <= expected_revision + int(refresh and outcome == 'success')):
            raise AssetProtocolError('ASSET_ACK_SCOPE_INVALID')
    return states[outcome]


def result_observation(result):
    """Keep node facts; classify only explicit, structured failure codes.

    This does not turn a failed repair into a successful check, advance a
    credential revision, or infer an account ban from an HTTP status/message.
    """
    if not isinstance(result, dict):
        raise AssetProtocolError('ASSET_STATE_INVALID')
    status = result.get('source_status')
    liveness = result.get('liveness')
    code = result.get('error_code')
    status = 'unknown' if status is None or status == '' else status
    liveness = 'unknown' if liveness is None or liveness == '' else liveness
    code = '' if code is None else code
    checked_at = result.get('checked_at')
    if not isinstance(code, str) or checked_at is not None and not isinstance(checked_at, str):
        raise AssetProtocolError('ASSET_STATE_INVALID')
    if status == 'unknown' and liveness == 'unknown':
        if code.lower() in {'account_deactivated', 'account_banned'}:
            status = 'banned'
        elif code.lower() in {'workspace_deactivated', 'deactivated_workspace'}:
            status = 'workspace_deactivated'
    account_state(status, liveness, checked_at=checked_at)
    return {'source_status': status, 'source_liveness': liveness, 'checked_at': checked_at}


def account_state(status="unknown", liveness="unknown", *, checked_at=None):
    """Preserve REG facts without equating workspace loss or stale flags with a ban."""
    for value in (status, liveness):
        if not isinstance(value, str) or not re.fullmatch(r"[a-z][a-z0-9_]{0,63}", value):
            raise AssetProtocolError("ASSET_STATE_INVALID")
    facts = {status, liveness}
    account = "banned" if "banned" in facts else "unknown"
    workspace = "deactivated" if "workspace_deactivated" in facts else "unknown"
    credential = "email_otp_required" if "email_otp_required" in facts else (
        "needs_refresh" if facts & {"needs_refresh", "expired", "relogin_required"} else "unknown")
    health = "alive" if liveness == "alive" and checked_at else "unknown"
    if facts & {"banned", "workspace_deactivated", "invalid", "dead"}:
        health = "unavailable"
    elif credential != "unknown":
        health = "needs_attention"
    return {"source_status": status, "source_liveness": liveness, "account": account,
            "workspace": workspace, "credential": credential, "health": health,
            "checked_at": checked_at, "automatic_refresh_allowed": not bool(
                facts & {"banned", "workspace_deactivated", "email_otp_required"})}
