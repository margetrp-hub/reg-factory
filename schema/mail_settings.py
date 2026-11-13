"""Mail defaults shared by the execution code and the exported UI catalog.

Only these non-secret settings interpret an explicit blank as the default.
Passwords, API keys and optional custom endpoints remain blank when cleared.
"""
import os

# Provider identifiers are persisted in account material. icmail denotes the
# built-in address pool; it is independent of the external icloud API provider.
MAIL_PROVIDERS = {
    "icmail": "内置邮箱池",
    "moemail": "MoeMail",
    "yyds": "YYDS Mail",
    "gptmail": "GPTMail",
    "cfmail": "CFMail",
    "icloud": "iCloud API",
    "remail": "Remail",
    "custom": "自定义邮箱 API",
}

# ChatGPT's existing registration/re-authorization chain supports these sources.
# Keep its advertised choices aligned with that chain, not every temp API.
CHATGPT_MAIL_PROVIDERS = {"pool": "Outlook 邮箱池", **{
    key: MAIL_PROVIDERS[key] for key in ("icmail", "icloud", "remail")
}}

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
