#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""按 Work/面板动作对单个或整批账号做测活与凭据修复，输出单行 JSON。

用法：
    python tools/asset_ops.py liveness --ref <ref> [--platform claude]
    python tools/asset_ops.py scan --platform claude [--account-concurrency 2]
    python tools/asset_ops.py refresh --ref <ref> --platform outlook [--allow-relogin]

设计原则（对齐 account-node 的治理）：
  * 测活默认低频：单平台串行，命中风控就停，不硬刷；
  * 刷新只处理“可修”的类型，封禁类直接返回 banned，不做无意义重试；
  * 结果只做结构化输出，不打印任何 cookie / token；
  * 超时/断连由任务记为失败；新尝试重新授权，不重放可能已消耗的 RT。
"""
from __future__ import annotations

import argparse
import copy
import hashlib
import json
import os
import sys
import tempfile
import time
from functools import wraps
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from common import credential_versions as versions
from common.oauth_errors import account_failure, error_code
from common.proxy_switch import production_proxy

PLATFORMS = ("claude", "chatgpt", "grok", "kiro", "outlook")
OPENAI_TOKEN_URL = "https://auth.openai.com/oauth/token"
OPENAI_CLIENT_ID = "app_EMoamEEZ73f0CkXaXp7hrann"


def _emit(payload: dict) -> int:
    if payload.get("result") in {"unknown", "relogin_required", "banned", "workspace_deactivated", "failed"}:
        payload["error_schema"] = "account.failure.v2"
        payload.setdefault("error_code", "credential_result_incomplete")
        payload["result"] = "failed"
    print(json.dumps(payload, ensure_ascii=False))
    # The runner maps the process exit code to the Work receipt.  Returning
    # zero for relogin/unknown made a refresh look successful even though no
    # new credential document was written.
    if payload.get("result") == "failed":
        return 1
    if "result" not in payload and payload.get("status") in {"unknown", "error", "expired", "banned", "workspace_deactivated"}:
        return 1
    return 0


def _stage(stage: str, message: str) -> None:
    if os.environ.get('REG_FACTORY_RUN_ID'):
        print(json.dumps({'schema': 'credential.stage.v1', 'stage': stage,
                          'status': 'running', 'next_action': message}, ensure_ascii=False), flush=True)


def _normalize_status(status: str) -> str:
    """把 asset_scanner 的状态口径收敛成 Team 认的四个值 + unknown。"""
    value = str(status or "").strip().lower()
    if value in {"ok", "normal"}:
        return "ok"
    if value in {"expired", "restricted", "abuse_locked"}:
        return "expired"
    if value in {"banned", "workspace_deactivated", "invalid", "dead"}:
        return value
    if value in {"error"}:
        return "unknown"
    return "unknown"


def _find_item(report: dict, ref: str) -> dict | None:
    target = str(ref or "").strip().lower()
    if not target:
        return None
    for item in report.get("items", []) or []:
        candidates = {
            str(item.get("id") or "").lower(),
            str(item.get("email") or "").lower(),
            str(item.get("ref") or "").lower(),
        }
        if target in candidates:
            return item
    # 退一步：ref 里带路径时用 basename 比对
    base = target.rsplit("/", 1)[-1]
    for item in report.get("items", []) or []:
        if base and base in str(item.get("id") or "").lower():
            return item
    return None


def cmd_liveness(args) -> int:
    from common import asset_scanner

    platform = (args.platform or "").strip().lower()
    if args.refresh_scan and platform in PLATFORMS:
        asset_scanner.scan_pool(
            platforms=[platform], concurrency=1, account_concurrency=1,
            timeout=int(os.environ.get("REG_FACTORY_LIVENESS_TIMEOUT", "15")), force=True,
        )
    report = asset_scanner.get_report()
    item = _find_item(report, args.ref)
    if not item:
        return _emit({
            "ref": args.ref, "platform": platform, "status": "unknown",
            "checked_at": report.get("last_scan_at", ""),
            "message": "号池报告里没有该账号，先跑一次「号池扫描」",
            "source": "report",
        })
    return _emit({
        "ref": args.ref,
        "platform": item.get("platform", platform),
        "status": _normalize_status(item.get("status")),
        "raw_status": item.get("status", ""),
        "checked_at": item.get("checked_at", "") or report.get("last_scan_at", ""),
        "message": str(item.get("detail") or "")[:200],
        "evidence": str(item.get("evidence") or "")[:120],
        "source": "report",
    })


def cmd_scan(args) -> int:
    from common import asset_scanner

    platforms = [p.strip().lower() for p in (args.platform or "").split(",") if p.strip()]
    if not platforms:
        platforms = ["claude"]
    concurrency = max(1, min(2, int(args.account_concurrency or 1)))
    result = asset_scanner.scan_pool(
        platforms=platforms, concurrency=1, account_concurrency=concurrency,
        timeout=int(os.environ.get("REG_FACTORY_LIVENESS_TIMEOUT", "15")),
        force=bool(args.force),
        include_plus_trial=bool(args.include_plus_trial),
    )
    summary = {}
    if isinstance(result, dict):
        summary = result.get("summary") or {}
    return _emit({
        "platforms": platforms,
        "scanned": True,
        "summary": summary,
        "finished_at": (result or {}).get("last_scan_at", "") if isinstance(result, dict) else "",
    })


def cmd_refresh(args) -> int:
    platform = (args.platform or "").strip().lower()
    # A requested ChatGPT refresh must reach the OAuth endpoint even when the
    # last liveness report is stale or says ``banned``.  The refresh result
    # itself is the evidence; a scan must not become a hidden business gate.

    # 只有明确持有可刷新凭据且被授权时才动手
    if platform == "outlook":
        result = _refresh_outlook_token(args.ref)
        if result.get("result") == "refreshed":
            return _emit(result)
        if not args.allow_relogin:
            result.setdefault("message", "需要 email----password 重新授权；加 --allow-relogin 执行")
            return _emit(result)
        return _refresh_outlook(args)

    if platform == "chatgpt":
        if args.allow_relogin:
            return _emit(_reauthorize_chatgpt(args.ref, args.email, args.timeout))
        return _emit(_refresh_chatgpt(args.ref, args.email))

    return _emit({
        "ref": args.ref,
        "result": "failed",
        "error_code": "platform_authorization_unsupported",
        "message": f"{platform or '未知平台'} 尚未支持自动授权，本次未执行",
    })


GRAPH_TOKEN_URL = "https://login.microsoftonline.com/consumers/oauth2/v2.0/token"
GRAPH_CLIENT_ID = "9e5f94bc-e8a4-4e73-b8be-63364c29d753"


def _subject_id(ref: str) -> str:
    """Match the deterministic UUID used by the Go node for account refs."""
    digest = bytearray(hashlib.sha1(("account-node:" + str(ref)).encode()).digest()[:16])
    digest[6] = (digest[6] & 0x0F) | 0x40
    digest[8] = (digest[8] & 0x3F) | 0x80
    raw = digest.hex()
    return f"{raw[:8]}-{raw[8:12]}-{raw[12:16]}-{raw[16:20]}-{raw[20:32]}"


def _primary_account(value: dict) -> dict:
    if not isinstance(value, dict):
        raise ValueError('ASSET_DOCUMENT_AMBIGUOUS')
    if 'accounts' in value:
        accounts = value['accounts']
        if not isinstance(accounts, list) or len(accounts) != 1 or not isinstance(accounts[0], dict):
            raise ValueError('ASSET_DOCUMENT_AMBIGUOUS')
        return _primary_account(accounts[0])
    if isinstance(value.get('payload'), dict):
        return _primary_account(value['payload'])
    return value


def _json_email(value: object) -> str:
    if not isinstance(value, dict):
        return ""
    if 'accounts' in value:
        return _json_email(_primary_account(value))
    for key in ("email", "account_email", "user_email", "username", "login"):
        candidate = value.get(key)
        if isinstance(candidate, str) and "@" in candidate:
            return candidate.strip()
    for key in ("payload", "credentials", "oauth", "token", "tokens", "account", "user", "profile"):
        nested = value.get(key)
        found = _json_email(nested)
        if found:
            return found
    return ""


def _token_map(value: object) -> dict | None:
    if not isinstance(value, dict):
        return None
    if 'accounts' in value:
        return _token_map(_primary_account(value))
    candidates = []
    for key in ("payload", "credentials", "oauth", "token", "tokens"):
        nested = value.get(key)
        found = _token_map(nested)
        if found is not None:
            candidates.append(found)
    if any(value.get(name) for name in ("refresh_token", "refreshToken", "oauth_refresh_token")):
        candidates.append(value)
    if not candidates:
        return None
    def bundle(item):
        return tuple(item.get(k) or item.get(alias) or item.get('oauth_' + k) for k, alias in (
            ('access_token', 'accessToken'), ('refresh_token', 'refreshToken'), ('id_token', 'idToken')))
    if any(bundle(item) != bundle(candidates[0]) for item in candidates[1:]):
        raise ValueError('ASSET_DOCUMENT_AMBIGUOUS')
    return candidates[0]


def _credential_dirs(root=None) -> list[Path]:
    root = Path(root or os.environ.get("REG_FACTORY_DATA_DIR") or ROOT).resolve()
    return [
        root / "assets" / "chatgpt" / "oauth",
        root / "assets" / "chatgpt" / "team",
        root / "assets" / "chatgpt" / "imported",
        root / "assets" / "chatgpt" / "free",
        root / "plus-import" / "team-json",
        root / "plus-import",
        root / "creds",
        root / "tokens" / "chatgpt",
    ]


def _credential_matches(ref: str, path: Path, document: dict) -> bool:
    target = str(ref or "").strip().lower()
    if not target:
        return False
    payload = document.get("payload") if isinstance(document.get("payload"), dict) else document
    candidates = {
        path.stem.lower(),
        path.name.lower(),
        str(document.get("ref") or "").strip().lower(),
        str(payload.get("ref") or "").strip().lower(),
        _json_email(document).lower(),
    }
    email = _json_email(document)
    explicit_ref = str(document.get("ref") or payload.get("ref") or "").strip()
    if email and not explicit_ref:
        candidates.update({
            email.lower(),
            f"creds:{email}".lower(),
            f"import:{email}".lower(),
            _subject_id(email).lower(),
            _subject_id(f"creds:{email}").lower(),
            _subject_id(f"import:{email}").lower(),
        })
    refs = {
        str(document.get("ref") or "").strip(),
        str(payload.get("ref") or "").strip(),
        path.stem,
    }
    for value in refs:
        if value:
            candidates.add(_subject_id(value).lower())
    for value in (document, payload):
        if isinstance(value, dict):
            for key in ("account_id", "chatgpt_account_id", "user_id", "source_asset_id"):
                item = str(value.get(key) or "").strip().lower()
                if item:
                    candidates.add(item)
    return target in candidates


def _find_credential(ref: str, root=None, database=None) -> tuple[Path, dict] | None:
    matches, all_documents = [], []
    for directory in _credential_dirs(root):
        if not directory.is_dir():
            continue
        for path in sorted(directory.glob("*.json")):
            try:
                document = json.loads(path.read_text(encoding="utf-8"))
            except (OSError, UnicodeError, json.JSONDecodeError):
                continue
            if isinstance(document, dict):
                all_documents.append((path, document))
                if _credential_matches(ref, path, document):
                    matches.append((path, document))
    # Explicit unified OAuth refs are the selected projection. Legacy files
    # are evidence only; they do not become current by directory order.
    selected = [(p, d) for p, d in matches if p.parent.name in {"oauth", "team"}
                and p.parent.parent.name == "chatgpt"
                and (d.get("ref") == ref or _subject_id(str(d.get("ref") or "")) == ref)]
    candidates = selected or [(p, d) for p, d in matches if _token_map(d)] or matches
    if not candidates:
        # A retired source file may still have an authorized Core subject.
        # Resolve only an existing account row, never a caller-supplied email.
        from contextlib import closing
        from common.production_database import connect
        explicit_root = root is not None
        root = Path(root or os.environ.get('REG_FACTORY_DATA_DIR') or ROOT)
        dsn = database or os.environ.get('REGD_DB')
        if not dsn and explicit_root and (root / 'regd.db').is_file():
            dsn = str(root / 'regd.db')  # explicitly supplied offline test fixture
        if not dsn:
            return None
        with closing(connect(dsn)) as db:
            rows = [(r[0], r[1]) for r in db.execute("SELECT ref,identity FROM accounts WHERE platform='chatgpt'").fetchall()]
        verified = [(key, identity) for key, identity in rows if key == ref or _subject_id(key) == ref]
        if len(verified) != 1 or not verified[0][1]:
            return None
        key, identity = verified[0]
        candidates = [(root / 'assets/chatgpt/imported' / (hashlib.sha256(key.encode()).hexdigest() + '.json'),
                       {'ref': key, 'email': identity, 'kind': 'imported', 'platform': 'chatgpt', 'payload': {}})]
    complete = [(p, d) for p, d in candidates if (versions.tokens(d) or {}).get('refresh_token')]
    anchor_path, anchor = versions.newest(complete) if complete else candidates[0]
    email = _json_email(anchor).lower()
    related = [(p, d) for p, d in all_documents
               if (anchor.get('ref') and d.get('ref') == anchor.get('ref') or email and _json_email(d).lower() == email)
               and (versions.tokens(d) or {}).get('refresh_token')]
    # Canonical assets outrank legacy export copies. The latter never regain
    # authority because an old exporter rewrote their filesystem timestamp.
    unified = [(p, d) for p, d in related if p.parent.parent.name == 'chatgpt' and p.parent.parent.parent.name == 'assets']
    path, document = versions.select(anchor, unified or related) or (anchor_path, anchor)
    if document.get('ref') and isinstance(document.get('payload'), dict):
        bundle = versions.tokens(document)
        if bundle:
            document = copy.deepcopy(document)
            for key in ('access_token', 'refresh_token', 'id_token', 'accessToken', 'refreshToken', 'idToken'):
                document.pop(key, None)
            document['payload'] = bundle
    return path, document


def _atomic_json_write(path: Path, document: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    try:
        os.chmod(temporary, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8", newline="\n") as handle:
            json.dump(document, handle, ensure_ascii=False, indent=2)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass


def _update_oauth_document(document: dict, tokens: dict) -> dict:
    """Update the existing JSON shape without dropping mailbox metadata."""
    primary = _primary_account(document)
    _token_map(document)  # Reject conflicting sessions before changing any field.
    targets: list[dict] = []
    def visit(parent):
        if any(key in parent for key in ('access_token', 'accessToken', 'oauth_access_token', 'refresh_token', 'refreshToken', 'oauth_refresh_token')):
            targets.append(parent)
        for key in ('payload', 'credentials', 'oauth', 'token', 'tokens'):
            if isinstance(parent.get(key), dict):
                visit(parent[key])
        if 'accounts' in parent:
            visit(_primary_account(parent))
    visit(document)
    if not targets:
        targets.append(primary.get('credentials') if isinstance(primary.get('credentials'), dict) else primary)
    for target in targets:
        access_key = "accessToken" if "accessToken" in target and "access_token" not in target else "access_token"
        refresh_key = "refreshToken" if "refreshToken" in target and "refresh_token" not in target else "refresh_token"
        target[access_key] = tokens["access_token"]
        if "oauth_access_token" in target:
            target["oauth_access_token"] = tokens["access_token"]
        if tokens.get("refresh_token"):
            target[refresh_key] = tokens["refresh_token"]
            if 'oauth_refresh_token' in target:
                target['oauth_refresh_token'] = tokens['refresh_token']
        if tokens.get("id_token"):
            target["id_token" if "id_token" in target or "idToken" not in target else "idToken"] = tokens["id_token"]
        if "expires_in" in target:
            target["expires_in"] = tokens.get("expires_in")
        if tokens.get("expires_at"):
            epoch = int(tokens["expires_at"])
            iso = datetime.fromtimestamp(epoch, timezone.utc).isoformat().replace("+00:00", "Z")
            for key in ("expires_at", "expires", "expired"):
                if key not in target:
                    continue
                target[key] = iso if isinstance(target[key], str) else epoch
    return document


def _retire_credential_copies(root, selected_path, document):
    """Called under the account lock, after the complete current save."""
    current = versions.tokens(document)
    if not current or not current.get('refresh_token'):
        return
    email = _json_email(document).strip().lower()
    for directory in _credential_dirs(root):
        for path in directory.glob('*.json'):
            if path == selected_path or path.is_symlink():
                continue
            old = json.loads(path.read_text(encoding='utf-8'))
            if not email or _json_email(old).strip().lower() != email or not versions.compatible(old, document):
                continue
            bundle = versions.tokens(old)
            if not bundle or versions.saved_time(old) > versions.saved_time(document):
                continue
            if versions.saved_time(old) == versions.saved_time(document) and versions.fingerprint(bundle) != versions.fingerprint(current):
                continue
            _atomic_json_write(path, versions.reference_stub(old, document.get('ref', '')))


def _refresh_document_copies(ref: str, email: str, selected_path: Path,
                             selected_document: dict, updates: dict) -> list[str]:
    """Publish one complete result atomically; every alias resolves this version."""
    _stage('credential_save', '正在保存本次完整凭据与恢复材料')
    current = _find_credential(ref)
    if current is None or versions.fingerprint(versions.tokens(current[1]) or {}) != versions.fingerprint(versions.tokens(selected_document) or {}):
        raise OSError('CREDENTIAL_CHANGED_BEFORE_SAVE')
    root = Path(os.environ.get("REG_FACTORY_DATA_DIR") or ROOT).resolve()
    updated = _update_oauth_document(copy.deepcopy(selected_document), updates)
    updated['credential_updated_at'] = datetime.now(timezone.utc).isoformat()
    updated.setdefault('platform', 'chatgpt')
    updated.setdefault('kind', 'oauth')
    updated.setdefault('email', email)
    if not versions.compatible(selected_document, updated):
        raise OSError('CREDENTIAL_IDENTITY_CHANGED')
    prepared = [(selected_path, selected_document, updated)]
    # First authorization of imported login material publishes a complete OAuth
    # asset. Keep the original login record and use the same stable account ref.
    if selected_document.get("kind") in {"imported", "free", "password"} or root / 'assets' not in selected_path.parents:
        oauth_path = root / "assets" / "chatgpt" / "oauth" / (hashlib.sha256(ref.encode()).hexdigest() + ".json")
        oauth = {"ref": ref, "platform": "chatgpt", "kind": "oauth", "email": email,
                 "source": "authorized:import", "created_at": datetime.now(timezone.utc).isoformat(),
                 "payload": {**updates, "email": email}}
        for key in ('workspace_account_id', 'account_id', 'chatgpt_account_id', 'chatgpt_user_id', 'user_id', 'client_id'):
            if updated.get('payload', updated).get(key):
                oauth['payload'].setdefault(key, updated.get('payload', updated)[key])
        oauth['credential_updated_at'] = datetime.now(timezone.utc).isoformat()
        prepared = [(oauth_path, None, oauth)]
    # Persist the entire supplier response and intended write set first. If a
    # projection fails halfway through, the new token is still recoverable
    # without replaying the consumed refresh grant.
    task_id = os.environ.get('REG_FACTORY_RUN_ID', '').strip()
    result = {"ref": ref, "email": email, "task_id": task_id,
              "received_at": datetime.now(timezone.utc).isoformat(),
              "source": selected_document,
              "updates": updates, "files": [{"path": str(p.relative_to(root)),
               "before": json.loads(p.read_text(encoding='utf-8')) if p.exists() else None,
              "after": new} for p, old, new in prepared]}
    result_raw = json.dumps(result, ensure_ascii=False, sort_keys=True).encode()
    result_id = hashlib.sha256(result_raw).hexdigest()
    _atomic_json_write(root / "oauth-results" / (result_id + ".json"), result)
    written: list[str] = []
    # Freeze the task output before publishing. A later account reuse must not
    # change this task's signed receipt or the artifact being received by Core.
    if task_id:
        document = versions.tokens(prepared[-1][2])
        if not document or not document.get('refresh_token'):
            raise OSError('CREDENTIAL_RESULT_INCOMPLETE')
        task_result = {'task_id': task_id, 'ref': ref, 'email': email, 'publication_id': result_id,
                       'credential_updated_at': prepared[-1][2]['credential_updated_at'], 'document': document}
        name = hashlib.sha256(task_id.encode()).hexdigest() + '.json'
        _atomic_json_write(root / 'credential-task-results' / name, task_result)
    for path, _, updated in prepared:
        _atomic_json_write(path, updated)
        written.append(str(path))
    _retire_credential_copies(root, prepared[-1][0], prepared[-1][2])
    # The runner acknowledges only after the task/account transaction commits.
    # Keep this journal through process exit and service restart.
    if not task_id:
        (root / "oauth-results" / (result_id + ".json")).unlink()
    return written


def _recover_publication(ref: str) -> dict:
    """Finish this task's saved write set locally, without contacting a provider."""
    root = Path(os.environ.get('REG_FACTORY_DATA_DIR') or ROOT).resolve()
    task_id = os.environ.get('REG_FACTORY_RUN_ID', '').strip()
    if not task_id:
        return {'ref': ref, 'result': 'failed', 'message': '缺少原任务编号'}
    try:
        name = hashlib.sha256(task_id.encode()).hexdigest() + '.json'
        receipt_path = root / 'credential-task-results' / name
        if not receipt_path.exists():
            # A crash can occur between journal fsync and receipt publication.
            matches = []
            for path in (root / 'oauth-results').glob('*.json'):
                try:
                    saved = json.loads(path.read_text(encoding='utf-8'))
                except (OSError, ValueError):
                    continue
                if saved.get('task_id') == task_id and saved.get('ref') == ref:
                    matches.append((path.stem, saved))
            if not matches:
                return {'ref': ref, 'result': 'not_saved', 'message': '原任务没有已保存结果'}
            if len(matches) != 1:
                raise ValueError('CREDENTIAL_PUBLICATION_MISSING')
            publication_id, saved = matches[0]
            after = saved['files'][-1]['after']
            receipt = {'task_id': task_id, 'ref': ref, 'email': saved['email'],
                       'publication_id': publication_id, 'credential_updated_at': after['credential_updated_at'],
                       'document': versions.tokens(after)}
            _atomic_json_write(receipt_path, receipt)
        receipt = json.loads(receipt_path.read_text(encoding='utf-8'))
        if receipt['task_id'] != task_id or receipt['ref'] != ref:
            raise ValueError('CREDENTIAL_TASK_MISMATCH')
        document = versions.tokens(receipt['document'])
        if not document or not document.get('refresh_token'):
            raise ValueError('CREDENTIAL_RESULT_INCOMPLETE')
        with versions.account_lock(root, receipt['email'], ref):
            current = _find_credential(ref, root)
            if (current and versions.fingerprint(versions.tokens(current[1]) or {}) == versions.fingerprint(document)
                    and current[1].get('credential_updated_at') == receipt['credential_updated_at']):
                return {'ref': ref, 'result': 'refreshed', 'message': '原任务最新凭据已保存'}
            publication_id = receipt.get('publication_id', '')
            if len(publication_id) != 64 or any(c not in '0123456789abcdef' for c in publication_id):
                raise ValueError('CREDENTIAL_PUBLICATION_MISSING')
            journal = json.loads((root / 'oauth-results' / (publication_id + '.json')).read_text(encoding='utf-8'))
            if journal.get('task_id') != task_id or journal.get('ref') != ref or journal.get('email') != receipt['email']:
                raise ValueError('CREDENTIAL_TASK_MISMATCH')
            if (not current or versions.fingerprint(versions.tokens(current[1]) or {}) !=
                    versions.fingerprint(versions.tokens(journal['source']) or {})
                    or current[1].get('credential_updated_at') != journal['source'].get('credential_updated_at')):
                raise ValueError('CREDENTIAL_CHANGED_BEFORE_SAVE')
            # Validate the entire write set before changing any current file.
            writes = []
            for item in journal['files']:
                path = (root / item['path']).resolve()
                if path.suffix != '.json' or root / 'assets' not in path.parents:
                    raise ValueError('CREDENTIAL_PUBLICATION_PATH_INVALID')
                after = item['after']
                if (versions.fingerprint(versions.tokens(after) or {}) != versions.fingerprint(document)
                        or after.get('credential_updated_at') != receipt['credential_updated_at']
                        or not versions.compatible(journal['source'], after)):
                    raise ValueError('CREDENTIAL_RESULT_MISMATCH')
                before = json.loads(path.read_text(encoding='utf-8')) if path.exists() else None
                if before != item['before'] and before != after:
                    raise ValueError('CREDENTIAL_CHANGED_BEFORE_SAVE')
                writes.append((path, after))
            if not writes:
                raise ValueError('CREDENTIAL_PUBLICATION_EMPTY')
            for path, after in writes:
                _atomic_json_write(path, after)
        return {'ref': ref, 'result': 'refreshed', 'message': '已恢复原任务保存结果，未重复请求上游'}
    except BlockingIOError:
        return {'ref': ref, 'result': 'failed', 'error_code': 'CREDENTIAL_BUSY', 'message': '凭据正在写入，稍后重试本地收尾'}
    except (OSError, ValueError, KeyError, TypeError):
        return {'ref': ref, 'result': 'failed', 'error_code': 'CREDENTIAL_PUBLICATION_UNCONFIRMED',
                'message': '本次凭据保存未确认或已被更新；原资料保留，可重新授权'}


def _same_oauth_source(selected: dict, candidate: dict) -> bool:
    left = selected.get("payload") if isinstance(selected.get("payload"), dict) else selected
    right = candidate.get("payload") if isinstance(candidate.get("payload"), dict) else candidate
    # A shared email can represent distinct workspaces and OAuth sessions.
    for key in ("workspace_account_id", "chatgpt_account_id", "account_id", "chatgpt_user_id", "user_id"):
        if left.get(key) and right.get(key) and left[key] != right[key]:
            return False
    left_ref = selected.get("ref") or left.get("ref")
    right_ref = candidate.get("ref") or right.get("ref")
    left_tokens, right_tokens = _token_map(selected) or {}, _token_map(candidate) or {}
    left_refresh = left_tokens.get("refresh_token") or left_tokens.get("refreshToken")
    right_refresh = right_tokens.get("refresh_token") or right_tokens.get("refreshToken")
    if left_refresh and right_refresh:
        return left_refresh == right_refresh and (not left_ref or not right_ref or left_ref == right_ref)
    if left_ref and right_ref:
        return left_ref == right_ref and not right_refresh
    return bool(left_refresh and left_refresh == right_refresh)


def _credential_operation(fn):
    @wraps(fn)
    def guarded(ref, email='', *args, **kwargs):
        try:
            if os.environ.get('REG_FACTORY_RUN_ID'):
                saved = _recover_publication(ref)
                if saved['result'] != 'not_saved':
                    return saved
            found = _find_credential(ref or email)
            identity = _json_email(found[1]) if found else email
            root = Path(os.environ.get('REG_FACTORY_DATA_DIR') or ROOT)
            with versions.account_lock(root, identity, ref):
                # Go checked current task history before spawning this executor.
                # This file lock still protects publication across worker processes.
                _stage('credential_rt' if fn.__name__ == '_refresh_chatgpt' else 'credential_authorize',
                       '使用 RT 获取新凭据' if fn.__name__ == '_refresh_chatgpt' else '使用原账号资料重新登录授权')
                return fn(ref, email, *args, **kwargs)
        except BlockingIOError:
            return {'ref': ref, 'result': 'failed', 'error_code': 'credential_task_busy', 'message': '该账号已有凭据任务执行中，本次未执行'}
        except ValueError:
            return {'ref': ref, 'result': 'failed', 'error_code': 'credential_document_ambiguous', 'message': '账号工作区或凭据版本不一致'}
    return guarded


@_credential_operation
def _refresh_chatgpt(ref: str, email: str = "") -> dict:
    """Refresh a ChatGPT OAuth record and atomically persist the new JSON."""
    import requests

    found = _find_credential(ref or email)
    if not found:
        return {"ref": ref, "result": "relogin_required", "error_code": "credential_not_found", "message": "找不到可刷新凭据"}
    path, document = found
    token = _token_map(document)
    refresh_token = str((token or {}).get("refresh_token") or (token or {}).get("refreshToken") or "").strip()
    if not refresh_token:
        return {"ref": ref, "result": "relogin_required", "error_code": "refresh_token_required", "message": "账号 JSON 缺少 refresh_token"}
    client_id = str((token or {}).get("client_id") or (token or {}).get("clientId") or OPENAI_CLIENT_ID).strip()
    try:
        session = requests.Session()
        session.trust_env = False
        proxy = production_proxy()
        if proxy:
            session.proxies.update({"http": proxy, "https": proxy})
        response = session.post(
            OPENAI_TOKEN_URL,
            data={"client_id": client_id, "grant_type": "refresh_token", "refresh_token": refresh_token,
                  "scope": "openid profile email offline_access"},
            timeout=int(os.environ.get("REG_FACTORY_REFRESH_TIMEOUT", "30")),
        )
    except requests.RequestException as exc:
        code = "oauth_network_timeout" if isinstance(exc, requests.Timeout) else "oauth_tls_handshake_failed" if isinstance(exc, requests.exceptions.SSLError) else "oauth_network_unavailable"
        return {"ref": ref, "result": "failed", "error_code": code, "message": type(exc).__name__}
    try:
        payload = response.json() if response.content else {}
    except (ValueError, json.JSONDecodeError):
        payload = {}
    if response.status_code == 200 and isinstance(payload, dict) and payload.get("access_token"):
        expires_in = int(payload.get("expires_in") or 0)
        updates = {
            "access_token": str(payload["access_token"]),
            "refresh_token": str(payload.get("refresh_token") or refresh_token),
            "id_token": str(payload.get("id_token") or (token or {}).get("id_token") or ""),
            "expires_in": expires_in,
            "expires_at": int(__import__("time").time()) + expires_in if expires_in > 0 else 0,
        }
        try:
            written = _refresh_document_copies(ref, _json_email(document) or email, path, document, updates)
        except OSError as exc:
            return {"ref": ref, "result": "failed", "error_code": "credential_save_failed", "message": f"刷新成功但 JSON 写回失败：{type(exc).__name__}"}
        return {"ref": ref, "result": "refreshed", "expires_in": expires_in,
                "updated_files": len(written), "message": "最新凭据已保存，原账号引用已同步"}
    explicit = account_failure(payload) if response.status_code >= 400 else None
    if explicit:
        return {'ref': ref, 'result': 'failed', 'error_schema': 'account.failure.v1', **explicit}
    code = error_code(payload)
    if response.status_code in {400, 401} and code in {"invalid_grant", "invalid_token", "refresh_token_reused", "refresh_token_expired", "refresh_token_revoked"}:
        return {"ref": ref, "result": "relogin_required", "error_code": code, "message": f"刷新授权被拒绝（{code}）"}
    code = "rate_limit_exceeded" if response.status_code == 429 else "oauth_service_unavailable" if response.status_code >= 500 else "oauth_response_invalid" if response.status_code == 200 else "oauth_request_rejected"
    return {"ref": ref, "result": "failed", "error_code": code, "message": f"OAuth HTTP {response.status_code}"}


def _reauthorization_document(document: dict) -> dict:
    """Resolve login metadata without joining independent OAuth results.

    Production gives the result a new ref. Password/TOTP still belong to the
    original OpenAI identity. Prove that relationship with both ID-token subject
    and email; email alone never grants access to another record's secrets.
    """
    from tools.chatgpt_reauthorize import _claims

    candidates = []
    for directory in _credential_dirs():
        if not directory.is_dir():
            continue
        for path in sorted(directory.glob("*.json")):
            try:
                candidate = json.loads(path.read_text(encoding="utf-8"))
            except (OSError, ValueError):
                continue
            if isinstance(candidate, dict):
                candidates.append(candidate)

    identity = _claims(str((_token_map(document) or {}).get("id_token") or ""))
    email = _json_email(document).strip().lower()
    source_refs = set()
    if email and identity.get("sub") and str(identity.get("email") or "").lower() == email:
        for candidate in candidates:
            token = _claims(str((_token_map(candidate) or {}).get("id_token") or ""))
            ref = candidate.get("ref")
            candidate_subject = token.get('sub') or versions.scope(candidate)[1]
            candidate_email = token.get('email') or _json_email(candidate)
            if (isinstance(ref, str) and ref and candidate_subject == identity["sub"]
                    and str(candidate_email or "").lower() == email
                    and _json_email(candidate).strip().lower() == email):
                source_refs.add(ref)

    result = copy.deepcopy(document)
    payload = result.get("payload", result)
    keys = ("password", "account_password", "chatgpt_password", "totp", "two_factor",
            "otp_secret", "reauthorization", "notes", "mail_api_url", "mail_api_key")
    for candidate in candidates:
        same_source = _same_oauth_source(document, candidate)
        same_identity = (candidate.get("ref") in source_refs
                         and _json_email(candidate).strip().lower() == email)
        if not same_source and not same_identity:
            continue
        original = candidate.get("payload", candidate)
        for key in keys:
            if original.get(key) and not payload.get(key):
                payload[key] = original[key]
    return result


@_credential_operation
def _reauthorize_chatgpt(ref: str, email: str = "", timeout: int = 600) -> dict:
    """An authorize command must perform a fresh supplier-owned OAuth login."""
    import asyncio
    from tools.chatgpt_reauthorize import authorize_document
    from vendor.reauth_web.engine import ERROR_HINTS, ReauthError
    from requests import RequestException, Timeout
    from requests.exceptions import SSLError

    found = _find_credential(ref or email)
    if not found:
        return {"ref": ref, "result": "relogin_required", "error_code": "credential_not_found", "message": "找不到原账号登录资料"}
    path, document = found
    try:
        login_document = _reauthorization_document(document)
        updates = asyncio.run(authorize_document(login_document, timeout=timeout))
    except ReauthError as exc:
        evidence = exc.evidence
        code = evidence.get("error_code") or "credential_result_incomplete"
        return {"ref": ref, "result": "failed", "error_schema": "account.failure.v2", "error_code": code,
                "message": ERROR_HINTS.get(code, type(exc).__name__), "evidence": evidence}
    except (TimeoutError, RequestException) as exc:
        code = "oauth_network_timeout" if isinstance(exc, (TimeoutError, Timeout)) else "oauth_tls_handshake_failed" if isinstance(exc, SSLError) else "oauth_network_unavailable"
        return {"ref": ref, "result": "failed", "error_code": code, "message": type(exc).__name__}
    try:
        written = _refresh_document_copies(ref, _json_email(document), path, document, updates)
    except OSError as exc:
        return {"ref": ref, "result": "failed", "error_code": "credential_save_failed", "message": type(exc).__name__}
    return {"ref": ref, "result": "refreshed", "updated_files": len(written),
            "message": "SG已重新登录并获取全新OAuth凭据，原账号JSON已更新"}


def _lookup_mailbox(email: str) -> list[str] | None:
    """从 emails.txt 找 email----password----refresh_token----client_id。"""
    data_root = Path(os.environ.get("REG_FACTORY_DATA_DIR") or ROOT)
    path = data_root / "emails.txt"
    if not path.is_file():
        return None
    target = email.strip().lower()
    for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
        text = line.strip()
        if not text or text.startswith("#"):
            continue
        parts = text.replace("-----", "----").split("----")
        if parts and parts[0].strip().lower() == target:
            return [item.strip() for item in parts]
    return None


def _refresh_outlook_token(ref: str) -> dict:
    """用 refresh_token 直接续期 Graph token（不重登、不打印 token）。"""
    import requests

    email = ref.split("/", 1)[-1] if "/" in ref else ref
    email = email.split(":", 1)[-1] if email.startswith("emails:") else email
    parts = _lookup_mailbox(email)
    if not parts or len(parts) < 3 or not parts[2]:
        return {"ref": ref, "result": "relogin_required",
                "message": "emails.txt 里没有该账号的 refresh_token，需要重新授权"}
    refresh_token = parts[2]
    client_id = parts[3] if len(parts) > 3 and parts[3] else GRAPH_CLIENT_ID
    try:
        response = requests.post(
            GRAPH_TOKEN_URL,
            data={
                "client_id": client_id,
                "grant_type": "refresh_token",
                "refresh_token": refresh_token,
                "scope": "https://graph.microsoft.com/Mail.Read",
            },
            timeout=20,
        )
    except requests.RequestException as exc:
        return {"ref": ref, "result": "unknown", "message": f"{type(exc).__name__}: {exc}"[:180]}

    if response.status_code == 200:
        payload = response.json() if response.content else {}
        new_rt = str(payload.get("refresh_token") or "")
        if new_rt and new_rt != refresh_token:
            _rewrite_mailbox_rt(email, new_rt)
        return {"ref": ref, "result": "refreshed",
                "expires_in": payload.get("expires_in"),
                "message": "Graph token 续期成功"}
    body = (response.text or "")[:160]
    code = ""
    try:
        code = str(response.json().get("error") or "")
    except Exception:
        pass
    if code in {"invalid_grant", "interaction_required", "consent_required"}:
        return {"ref": ref, "result": "relogin_required", "message": f"refresh_token 失效（{code}）"}
    return {"ref": ref, "result": "unknown", "message": f"HTTP {response.status_code}: {body}"}


def _rewrite_mailbox_rt(email: str, new_rt: str) -> None:
    """把新 refresh_token 写回 emails.txt（同邮箱那一行）。"""
    data_root = Path(os.environ.get("REG_FACTORY_DATA_DIR") or ROOT)
    path = data_root / "emails.txt"
    if not path.is_file():
        return
    lines = path.read_text(encoding="utf-8", errors="replace").splitlines()
    out = []
    for line in lines:
        parts = line.replace("-----", "----").split("----")
        if parts and parts[0].strip().lower() == email.strip().lower() and len(parts) >= 3:
            parts[2] = new_rt
            out.append("----".join(parts))
        else:
            out.append(line)
    path.write_text("\n".join(out) + "\n", encoding="utf-8")


def _refresh_outlook(args) -> int:
    """用 authorize_outlook 的 HTTP 授权路径刷新 Graph token。"""
    account = str(args.account or "").strip()
    if "----" not in account:
        return _emit({"ref": args.ref, "result": "relogin_required",
                      "message": "缺少 email----password，无法重新授权"})
    with tempfile.NamedTemporaryFile("w", suffix=".txt", delete=False, encoding="utf-8") as handle:
        handle.write(account + "\n")
        path = handle.name
    try:
        import subprocess

        completed = subprocess.run(
            [sys.executable, str(ROOT / "tools" / "authorize_outlook.py"),
             "--input", path, "--concurrency", "1", "--method", "http"],
            capture_output=True, text=True, timeout=int(args.timeout or 600), cwd=str(ROOT),
            env={**os.environ, "PYTHONIOENCODING": "utf-8"},
        )
        ok = completed.returncode == 0
        return _emit({
            "ref": args.ref,
            "result": "refreshed" if ok else "unknown",
            "message": (completed.stdout or completed.stderr or "")[-300:],
        })
    except Exception as exc:  # pragma: no cover
        return _emit({"ref": args.ref, "result": "unknown", "message": f"{type(exc).__name__}: {exc}"[:200]})
    finally:
        try:
            os.unlink(path)
        except OSError:
            pass


def main() -> int:
    parser = argparse.ArgumentParser(description="reg-factory 账号测活 / 修复")
    sub = parser.add_subparsers(dest="command", required=True)

    live = sub.add_parser("liveness")
    live.add_argument("--ref", required=True)
    live.add_argument("--platform", default="")
    live.add_argument("--refresh-scan", action="store_true", help="先触发一次单平台扫描再取结果")
    live.set_defaults(func=cmd_liveness)

    scan = sub.add_parser("scan")
    scan.add_argument("--platform", default="")
    scan.add_argument("--account-concurrency", type=int, default=1)
    scan.add_argument("--force", action="store_true")
    scan.add_argument("--include-plus-trial", action="store_true")
    scan.set_defaults(func=cmd_scan)

    refresh = sub.add_parser("refresh")
    refresh.add_argument("--ref", required=True)
    refresh.add_argument("--email", default="", help="节点本地账号映射出的邮箱（不含密码）")
    refresh.add_argument("--platform", default="")
    refresh.add_argument("--allow-relogin", action="store_true")
    refresh.add_argument("--account", default="", help="email----password（仅 outlook 重授权用）")
    refresh.add_argument("--timeout", type=int, default=600)
    refresh.set_defaults(func=cmd_refresh)

    recover = sub.add_parser('recover-publication', help=argparse.SUPPRESS)
    recover.add_argument('--ref', required=True)
    recover.set_defaults(func=lambda args: _emit(_recover_publication(args.ref)))

    args = parser.parse_args()
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
