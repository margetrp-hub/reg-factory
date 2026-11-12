"""Read the built-in mailbox pool through regd; never copy its login settings."""
from __future__ import annotations

import asyncio
import ipaddress
import os
import re
import time
from urllib.parse import urlsplit

import requests


class MailboxError(RuntimeError):
    pass


def _get(path, **params):
    address = os.environ.get("REG_FACTORY_ICMAIL_ADDR") or os.environ.get("REGD_ADDR") or "127.0.0.1:8800"
    parsed = urlsplit("http://" + address)
    host = parsed.hostname or ""
    if host in {"0.0.0.0", "::", "localhost"}:
        host = "127.0.0.1"
    try:
        local = ipaddress.ip_address(host).is_loopback
    except ValueError:
        local = False
    if not local or parsed.username or parsed.password or parsed.path:
        raise MailboxError("内置邮箱只连接本节点 regd，请检查节点监听地址")
    host = f"[{host}]" if ":" in host else host
    key = os.environ.get("REG_FACTORY_ICMAIL_KEY") or os.environ.get("REG_FACTORY_PANEL_PASSWORD", "")
    if not key and os.environ.get("REG_FACTORY_ALLOW_ANONYMOUS", "").lower() not in {"1", "true"}:
        raise MailboxError("内置邮箱缺少本节点访问凭据，请从 SG 任务入口执行")
    with requests.Session() as session:
        session.trust_env = False
        try:
            response = session.get(f"http://{host}:{parsed.port or 8800}/api/apps/icmail{path}",
                params=params, headers={"X-API-Key": key}, timeout=(3, 8), allow_redirects=False)
        except requests.RequestException:
            raise MailboxError("内置邮箱连接失败，请检查本节点服务后重试") from None
        if response.status_code in {401, 403}:
            raise MailboxError("内置邮箱访问未授权，请检查节点配置")
        if response.status_code == 409:
            raise MailboxError("邮箱未登记、已暂停或来源不可用，请在邮箱池重配原地址与收件来源")
        if response.status_code != 200:
            raise MailboxError(f"内置邮箱读取失败（HTTP {response.status_code}），请重试")
        try:
            body = response.json()
            if body.get("ok") is not True or not isinstance(body.get("data"), dict):
                raise ValueError()
            return body["data"]
        except (ValueError, AttributeError):
            raise MailboxError("内置邮箱返回无效，请检查本节点服务") from None


def bind(email):
    """Bind an existing identity before sending OTP; this never allocates mail."""
    identity = str(email or "").strip().lower()
    snapshot = _get("/workflow/mailbox", email=identity)
    if snapshot.get("email") != identity:
        raise MailboxError("内置邮箱返回的身份不一致")
    print("  [mail] 使用内置邮箱池，已绑定原邮箱；等待本次新邮件", flush=True)
    return {"id": identity, "email": identity, "provider": "icmail",
            "cursor": int(snapshot["cursor"]), "source_id": int(snapshot["source_id"]),
            "started_at": time.time()}


def allocate(platform="chatgpt", email=""):
    """Reserve once using the same durable, cross-process ledger as registration."""
    from common import emails
    from common.file_lock import file_lock

    candidates = []
    if email:
        candidates = [str(email).strip().lower()]
    else:
        offset = 0
        while True:
            page = _get("/pool", state="active", limit=100, offset=offset)
            items = page.get("items") or []
            candidates.extend(item["email"] for item in items
                if item.get("sourceEnabled") and item.get("sourceId") and item.get("origin") != "discovered")
            offset += len(items)
            if not items or offset >= int(page.get("total", 0)):
                break
    with emails._lock, file_lock(f"{emails.EMAILS_FILE}.icmail.reserve"):
        used = emails._load_used(platform)
        for identity in candidates:
            if identity.lower() in used:
                continue
            mailbox = bind(identity)
            emails.mark_registration_started(platform, identity)
            print(f"  [mail] 内置邮箱已登记本次注册占用：{identity}", flush=True)
            return mailbox
    raise MailboxError("内置邮箱池没有未使用的启用地址；请导入地址并绑定收件来源。已尝试注册的原邮箱请走重新授权")


def read_messages(mailbox, received_after=None):
    # Mail Date headers have second precision; the captured ID excludes old mail
    # even when a newly sent message shares this same second.
    after = int(max(float(mailbox.get("started_at") or 0), float(received_after or 0)))
    snapshot = _get("/workflow/mailbox", email=mailbox["email"], cursor=mailbox.get("cursor", 0), after=after)
    if snapshot.get("email") != mailbox["email"] or snapshot.get("source_id") != mailbox.get("source_id"):
        raise MailboxError("邮箱收件来源已变化，请在原任务重新授权后取码")
    return snapshot.get("messages") or []


async def poll_code(mailbox, *, received_after=None, max_wait=120, poll_interval=5, exclude_codes=()):
    deadline = time.monotonic() + max(0, float(max_wait))
    excluded = set(str(code) for code in exclude_codes)
    print("  [mail] 内置邮箱等待新验证码", flush=True)
    while time.monotonic() < deadline:
        messages = await asyncio.to_thread(read_messages, mailbox, received_after)
        for message in messages:
            sender = str(message.get("from") or "").lower()
            subject = str(message.get("subject") or "").lower()
            if not any(hint in sender or hint in subject for hint in ("openai", "chatgpt")):
                continue
            for text in (message.get("subject"), message.get("text"), message.get("snippet")):
                for code in re.findall(r"\b(\d{6})\b", str(text or "")):
                    if code not in excluded:
                        print("  [mail] 已取得本次验证码", flush=True)
                        return code
        remaining = deadline - time.monotonic()
        if remaining > 0:
            await asyncio.sleep(min(max(0.05, poll_interval), remaining))
    print("  [mail] 内置邮箱等待超时，请检查收件来源后重试原任务", flush=True)
    return None
