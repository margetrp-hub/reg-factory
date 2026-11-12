"""Read-only IMAP adapter for a task's explicit central mailbox material."""
import email
from email import policy
from email.utils import getaddresses, parsedate_to_datetime
import imaplib
import re
import ssl
import time

from .assets import AssetProtocolError


class IMAPMailbox:
    supports_poll_windows = True

    def __init__(self, mailbox, *, stopped, connect=imaplib.IMAP4_SSL):
        self.mailbox, self.stopped, self.connect = mailbox, stopped, connect
        self.seen = set()
        self.primed = False

    def _read(self, *, baseline=False):
        material = self.mailbox.get('material') or {}
        defaults = {'qq': 'imap.qq.com', 'icloud': 'imap.mail.me.com', 'outlook': 'outlook.office365.com'}
        host = material.get('host') or defaults.get(self.mailbox.get('provider'))
        username = material.get('username') or self.mailbox.get('email')
        password = material.get('password') or material.get('app_password')
        if (not isinstance(host, str) or not re.fullmatch(r'[a-zA-Z0-9.-]{1,253}', host)
                or not isinstance(username, str) or not username or not isinstance(password, str) or not password
                or self.stopped.is_set()):
            raise AssetProtocolError('ASSET_MAILBOX_UNAVAILABLE')
        client = self.connect(host, port=993, ssl_context=ssl.create_default_context(), timeout=8)
        try:
            client.login(username, password)
            if client.select('INBOX', readonly=True)[0] != 'OK':
                raise AssetProtocolError('ASSET_MAILBOX_UNAVAILABLE')
            status, rows = client.uid('search', None, 'ALL')
            if status != 'OK':
                raise AssetProtocolError('ASSET_MAILBOX_UNAVAILABLE')
            ids = (rows[0] or b'').split()[-50:]
            if baseline:
                self.seen.update(ids)
                return []
            messages = []
            for uid in reversed(ids):
                if self.stopped.is_set():
                    break
                if uid in self.seen:
                    continue
                status, body = client.uid('fetch', uid, '(BODY.PEEK[]<0.65536>)')
                if status != 'OK':
                    continue
                for item in body:
                    if isinstance(item, tuple) and isinstance(item[1], bytes):
                        messages.append((uid, email.message_from_bytes(item[1][:65536], policy=policy.default)))
            return messages
        finally:
            try:
                client.logout()
            except Exception:
                pass

    def prime(self):
        self._read(baseline=True)
        self.primed = True

    def wait_for_otp(self, email_address, timeout=600, issued_after=None, poll_window=None):
        if not self.primed:
            raise AssetProtocolError('ASSET_MAILBOX_NOT_PRIMED')
        duration = min(float(timeout), float(poll_window) if poll_window is not None else float(timeout))
        deadline = time.monotonic() + max(0, duration)
        while not self.stopped.is_set() and time.monotonic() < deadline:
            for uid, message in self._read():
                self.seen.add(uid)
                senders = [v.lower().rsplit('@', 1)[-1] for _, v in getaddresses(message.get_all('From', []))]
                if not any(v in {'openai.com', 'chatgpt.com', 'tm.openai.com'} or v.endswith('.openai.com') for v in senders):
                    continue
                recipients = [v.lower() for _, v in getaddresses(message.get_all('To', []) + message.get_all('Delivered-To', []))]
                if recipients and email_address.lower() not in recipients:
                    continue
                try:
                    sent = parsedate_to_datetime(message['Date']).timestamp()
                    cutoff = issued_after.timestamp() if hasattr(issued_after, 'timestamp') else float(issued_after or 0)
                    if sent < cutoff - 30:
                        continue
                except (ValueError, TypeError, AttributeError, OverflowError):
                    continue
                parts = [str(message.get('Subject', ''))]
                for part in message.walk():
                    if part.get_content_type() in {'text/plain', 'text/html'} and part.get_content_disposition() != 'attachment':
                        parts.append(str(part.get_content()))
                match = re.search(r'(?<!\d)(\d{6})(?!\d)', '\n'.join(parts))
                if match:
                    return match.group(1)
            self.stopped.wait(min(2, max(0, deadline - time.monotonic())))
        return ''
