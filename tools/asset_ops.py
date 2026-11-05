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
  * unknown（超时/断连）不自动重试，留给人工或对账。
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
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

PLATFORMS = ("claude", "chatgpt", "grok", "kiro", "outlook")
OPENAI_TOKEN_URL = "https://auth.openai.com/oauth/token"
OPENAI_CLIENT_ID = "app_EMoamEEZ73f0CkXaXp7hrann"


def _emit(payload: dict) -> int:
    print(json.dumps(payload, ensure_ascii=False))
    # The runner maps the process exit code to the Work receipt.  Returning
    # zero for relogin/unknown made a refresh look successful even though no
    # new credential document was written.
    if payload.get("result") in {"unknown", "relogin_required", "banned", "failed"}:
        return 1
    if "result" not in payload and payload.get("status") in {"unknown", "error", "expired", "banned"}:
        return 1
    return 0


def _normalize_status(status: str) -> str:
    """把 asset_scanner 的状态口径收敛成 Team 认的四个值 + unknown。"""
    value = str(status or "").strip().lower()
    if value in {"ok", "normal"}:
        return "ok"
    if value in {"expired", "restricted", "abuse_locked"}:
        return "expired"
    if value in {"banned", "invalid", "dead"}:
        return "banned"
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
        return _emit(_refresh_chatgpt(args.ref, args.email))

    return _emit({
        "ref": args.ref,
        "result": "relogin_required",
        "message": f"{platform or '未知平台'} 需要邮箱+密码重新登录，v1 未自动执行",
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


def _json_email(value: object) -> str:
    if not isinstance(value, dict):
        return ""
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
    for key in ("payload", "credentials", "oauth", "token", "tokens"):
        nested = value.get(key)
        if isinstance(nested, dict) and any(
            nested.get(name) for name in ("refresh_token", "refreshToken", "oauth_refresh_token")
        ):
            return nested
    return value if any(value.get(name) for name in ("refresh_token", "refreshToken", "oauth_refresh_token")) else None


def _credential_dirs() -> list[Path]:
    root = Path(os.environ.get("REG_FACTORY_DATA_DIR") or ROOT).resolve()
    return [
        root / "assets" / "chatgpt" / "oauth",
        root / "assets" / "chatgpt" / "imported",
        root / "plus-import" / "team-json",
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


def _find_credential(ref: str) -> tuple[Path, dict] | None:
    for directory in _credential_dirs():
        if not directory.is_dir():
            continue
        for path in sorted(directory.glob("*.json")):
            try:
                document = json.loads(path.read_text(encoding="utf-8"))
            except (OSError, UnicodeError, json.JSONDecodeError):
                continue
            if isinstance(document, dict) and _credential_matches(ref, path, document):
                return path, document
    return None


def _atomic_json_write(path: Path, document: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    try:
        os.chmod(temporary, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
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
    primary = document.get("payload") if isinstance(document.get("payload"), dict) else document
    targets: list[dict] = []
    for candidate in (primary, document):
        if isinstance(candidate, dict) and candidate not in targets:
            targets.append(candidate)
    for parent in list(targets):
        for key in ("credentials", "oauth", "token", "tokens"):
            nested = parent.get(key)
            if isinstance(nested, dict) and nested not in targets:
                targets.append(nested)
    for target in targets:
        access_key = "accessToken" if "accessToken" in target and "access_token" not in target else "access_token"
        refresh_key = "refreshToken" if "refreshToken" in target and "refresh_token" not in target else "refresh_token"
        if "access_token" in target or "accessToken" in target or target is primary:
            target[access_key] = tokens["access_token"]
        if tokens.get("refresh_token") and ("refresh_token" in target or "refreshToken" in target or target is primary):
            target[refresh_key] = tokens["refresh_token"]
        if tokens.get("id_token") and (target is primary or "id_token" in target or "idToken" in target):
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


def _refresh_document_copies(ref: str, email: str, selected_path: Path,
                             selected_document: dict, updates: dict) -> list[str]:
    """Persist one refresh result to every local copy of the account JSON.

    The unified asset store and the historical ``plus-import/team-json`` tree
    are both read by deployed SG images. Updating only the first match makes a
    customer download the old document after a successful 401 repair. Match
    by the original source credentials, never email alone, then write atomically.
    """
    targets: list[tuple[Path, dict]] = [(selected_path, selected_document)]
    seen = {str(selected_path.resolve())}
    for directory in _credential_dirs():
        if not directory.is_dir():
            continue
        for path in sorted(directory.glob("*.json")):
            try:
                key = str(path.resolve())
            except OSError:
                key = str(path)
            if key in seen:
                continue
            try:
                document = json.loads(path.read_text(encoding="utf-8"))
            except (OSError, UnicodeError, json.JSONDecodeError):
                continue
            if not isinstance(document, dict):
                continue
            if _same_oauth_source(selected_document, document):
                targets.append((path, document))
                seen.add(key)

    written: list[str] = []
    for path, document in targets:
        updated = _update_oauth_document(copy.deepcopy(document), updates)
        _atomic_json_write(path, updated)
        written.append(str(path))
    return written


def _same_oauth_source(selected: dict, candidate: dict) -> bool:
    left = selected.get("payload") if isinstance(selected.get("payload"), dict) else selected
    right = candidate.get("payload") if isinstance(candidate.get("payload"), dict) else candidate
    # A shared email can represent distinct workspaces and OAuth sessions.
    for key in ("workspace_account_id", "chatgpt_account_id", "account_id", "chatgpt_user_id", "user_id"):
        if left.get(key) and right.get(key) and left[key] != right[key]:
            return False
    left_ref = selected.get("ref") or left.get("ref")
    right_ref = candidate.get("ref") or right.get("ref")
    if left_ref and right_ref:
        return left_ref == right_ref
    left_tokens, right_tokens = _token_map(selected) or {}, _token_map(candidate) or {}
    left_refresh = left_tokens.get("refresh_token") or left_tokens.get("refreshToken")
    right_refresh = right_tokens.get("refresh_token") or right_tokens.get("refreshToken")
    return bool(left_refresh and left_refresh == right_refresh)


def _refresh_chatgpt(ref: str, email: str = "") -> dict:
    """Refresh a ChatGPT OAuth record and atomically persist the new JSON."""
    import requests

    found = _find_credential(ref or email)
    if not found:
        return {"ref": ref, "result": "relogin_required", "message": "找不到可刷新凭据"}
    path, document = found
    token = _token_map(document)
    refresh_token = str((token or {}).get("refresh_token") or (token or {}).get("refreshToken") or "").strip()
    if not refresh_token:
        return {"ref": ref, "result": "relogin_required", "message": "账号 JSON 缺少 refresh_token"}
    client_id = str((token or {}).get("client_id") or (token or {}).get("clientId") or OPENAI_CLIENT_ID).strip()
    try:
        session = requests.Session()
        session.trust_env = False
        proxy = str(os.environ.get("REG_FACTORY_PROXY_URL") or "").strip()
        if proxy:
            session.proxies.update({"http": proxy, "https": proxy})
        response = session.post(
            OPENAI_TOKEN_URL,
            data={"client_id": client_id, "grant_type": "refresh_token", "refresh_token": refresh_token,
                  "scope": "openid profile email offline_access"},
            timeout=int(os.environ.get("REG_FACTORY_REFRESH_TIMEOUT", "30")),
        )
    except requests.RequestException as exc:
        return {"ref": ref, "result": "unknown", "message": f"{type(exc).__name__}"}
    try:
        payload = response.json() if response.content else {}
    except (ValueError, json.JSONDecodeError):
        payload = {}
    if response.status_code == 200 and isinstance(payload, dict) and payload.get("access_token"):
        expires_in = int(payload.get("expires_in") or 0)
        updates = {
            "access_token": str(payload["access_token"]),
            "refresh_token": str(payload.get("refresh_token") or refresh_token),
            "id_token": str(payload.get("id_token") or ""),
            "expires_in": expires_in,
            "expires_at": int(__import__("time").time()) + expires_in if expires_in > 0 else 0,
        }
        try:
            written = _refresh_document_copies(ref, _json_email(document) or email, path, document, updates)
        except OSError as exc:
            return {"ref": ref, "result": "unknown", "message": f"刷新成功但 JSON 写回失败：{type(exc).__name__}"}
        return {"ref": ref, "result": "refreshed", "expires_in": expires_in,
                "updated_files": len(written), "message": "ChatGPT OAuth 已刷新并写回全部 JSON"}
    code = str(payload.get("error") or "") if isinstance(payload, dict) else ""
    if response.status_code in {400, 401} and code in {"invalid_grant", "invalid_token", "account_deactivated", "account_banned"}:
        return {"ref": ref, "result": "relogin_required", "message": f"refresh_token 无效（{code or response.status_code}）"}
    return {"ref": ref, "result": "unknown", "message": f"OAuth HTTP {response.status_code}"}


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

    args = parser.parse_args()
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
