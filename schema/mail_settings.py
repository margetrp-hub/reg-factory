"""Mail defaults shared by the execution code and the exported UI catalog.

Only these non-secret settings interpret an explicit blank as the default.
Passwords, API keys and optional custom endpoints remain blank when cleared.
"""
import os

MAIL_DEFAULTS = {
    "CHATGPT_EMAIL_PROVIDER": "pool",
    "TEMP_EMAIL_PROVIDER": "gptmail",
    "YYDS_BASE_URL": "https://maliapi.215.im",
    "ICLOUD_MAIL_API_BASE": "https://mail.no-replyca.xyz",
    "ICLOUD_MAIL_TYPE": "icloud-code",
    "ICLOUD_MAIL_SERVICE": "openai",
}


def mail_setting(key):
    return os.environ.get(key, "").strip() or MAIL_DEFAULTS[key]
