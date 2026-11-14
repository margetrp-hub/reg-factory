"""协议重登：邮箱----密码----2fa -> 全新 codex tokens -> 四种交付格式。

流程（不依赖任何 CPA / sub2api 后端，纯协议自闭环）：
  1. 本地生成 PKCE + state，拼出 auth.openai.com/oauth/authorize 授权链接
  2. 用账号密码 + 本地 2FA 动态码走完登录与授权，拿到 callback code
  3. 用 code_verifier 换 access/refresh/id token
  4. 组装 cpa / sub2api / cockpit 文件
"""

from __future__ import annotations

import json
import random
import secrets
import time
from urllib.parse import urlencode

from .lib.chatgpt_client import ChatGPTClient
from .lib.oauth_client import (
    OAUTH_CLIENT_ID,
    OAUTH_ISSUER,
    OAUTH_REDIRECT_URI,
    OAuthClient,
)
from .lib.utils import generate_pkce

from .config import Settings
from .formats import session_to_payloads
from .parsing import AccountLine
from .totp import TotpMfaClient

OAUTH_SCOPE = "openid profile email offline_access"

# 与前端共享的步骤定义（/api/meta 会把标签下发给页面）
STEPS_LOGIN = [
    ("read", "解析卡密"),
    ("session", "建立干净会话"),
    ("login", "登录 ChatGPT"),
    ("mfa", "2FA 动态码校验"),
    ("token", "换取 token"),
    ("build", "生成 Sub2 文件"),
    ("finish", "整理交付文件"),
]

STEPS_REDEEM = [
    ("read", "解析卡密"),
    ("stock", "查询主站库存"),
    ("fetch", "取回成品文件"),
    ("finish", "整理交付文件"),
]

STEPS_CONVERT = [
    ("read", "读取 sub2 文件"),
    ("build", "转格式"),
    ("finish", "整理交付文件"),
]


class ReauthError(RuntimeError):
    """单个账号重登失败。"""

    def __init__(self, message, *, evidence=None):
        super().__init__(message)
        self.evidence = evidence or {}


def _oauth_config(settings: Settings) -> dict:
    return {
        "oauth_phone_strategy": "sms",
        "sub2api_oauth_max_hops": 20,
        "oauth_email_otp_max_attempts": 3,
        "oauth_email_otp_timeout": 150,
        "sub2api_oauth_impersonates": settings.impersonates,
        "sub2api_redirect_uri": OAUTH_REDIRECT_URI,
        "workspace_id": str(settings.extra.get("workspace_id") or ""),
    }


def build_authorize_url(code_challenge: str, state: str) -> str:
    params = {
        "response_type": "code",
        "client_id": OAUTH_CLIENT_ID,
        "redirect_uri": OAUTH_REDIRECT_URI,
        "scope": OAUTH_SCOPE,
        "code_challenge": code_challenge,
        "code_challenge_method": "S256",
        "state": state,
    }
    return f"{OAUTH_ISSUER}/oauth/authorize?{urlencode(params)}"


PHONE_FAILURES = {
    "add_phone",
    "add-phone",
    "phone_required",
    "add_phone_failed",
    "add_phone_requires_sms_client",
    "phone_verification",
    "phone_verification_required",
    "phone_number_required",
}

PHONE_MESSAGE = "该账号被要求手机号验证｜本站不提供手机号，请换其他方式处理"

ERROR_HINTS = {
    "authorization_browser_error": "授权浏览器启动或运行失败",
    "oauth_cloudflare_challenge": "上游安全验证未通过，未更新账号凭据",
    "account_deactivated": "上游明确返回账号封禁/停用，刷新不能恢复账号",
    "account_banned": "上游明确返回账号封禁，刷新不能恢复账号",
    "workspace_deactivated": "上游明确返回空间停用，请核对原工作区",
    "deactivated_workspace": "上游明确返回空间停用，请核对原工作区",
    "mfa_totp_verification_failed": "账号两步验证未通过，请核对当前有效的 2FA 资料",
    "mfa_challenge_rejected": "上游在 2FA 提交环节直接拒绝（403/409），通常为账号停用或受限；请先核对该账号是否被封禁",
    "oauth_tls_handshake_failed": "登录校验连接的 TLS 握手失败，网络重试已用尽",
    "oauth_certificate_verification_failed": "登录校验连接的证书验证失败，请检查出口证书",
    "oauth_network_timeout": "登录校验连接超时，网络重试已用尽",
    "oauth_network_unavailable": "登录校验服务暂时不可达，请检查网络出口",
    "oauth_login_check_http_error": "登录校验服务返回异常状态，请查看 HTTP 状态",
    "oauth_login_check_invalid_response": "登录校验服务未返回有效校验结果",
    "phone_required": PHONE_MESSAGE,
    "add_phone_failed": PHONE_MESSAGE,
    "add_phone_requires_sms_client": PHONE_MESSAGE,
    "mfa_totp_secret_required": "这个账号需要 2FA 密钥，请检查第三列是否填了正确的 base32 密钥",
    "mfa_totp_factor_not_available": "该账号没有可用的 TOTP 验证器，无法自动通过 2FA",
    "mfa_totp_code_unavailable": "2FA 动态码未通过，可稍后重试",
    "mfa_primary_factor_required": "该账号要求主验证方式（非 TOTP）确认，本站无法自动通过",
    "email_otp_rejected_429": "邮箱验证码被限流，请稍后重试",
    "email_otp_not_found": "没有等到邮箱验证码",
    "invalid_username_or_password": (
        "邮箱或密码不正确｜若你这行里出现超过 4 个连续短横线，"
        "请确认密码里是不是真的带 '-'"
    ),
    "rate_limit_exceeded": "登录被 OpenAI 限流了，等几分钟再试这个账号",
}


def is_phone_required(reason: str) -> bool:
    text = str(reason or "").lower()
    return any(code in text for code in PHONE_FAILURES) or (
        "phone" in text and "sms" in text
    )


def _failure_reason(result: dict) -> str:
    detail = str(result.get("error") or "").strip()
    page = str(result.get("page_type") or "").strip()
    protocol = result.get("oauth_protocol") or {}
    last_page = str(protocol.get("last_page_type") or "").strip()
    status = protocol.get("last_status") or result.get("status") or ""
    hint = ERROR_HINTS.get(detail)
    if hint:
        return hint
    if (
        is_phone_required(detail)
        or is_phone_required(page)
        or is_phone_required(last_page)
    ):
        return PHONE_MESSAGE
    parts = [item for item in (detail, page or last_page) if item]
    if status:
        parts.append(f"HTTP {status}")
    return " / ".join(parts) or "授权未完成"


def failure_evidence(result: dict) -> dict:
    """Allowlisted diagnostics only: never return provider text, URLs or secrets."""
    from common.oauth_errors import REFRESH_RELOGIN_CODES
    protocol = result.get("oauth_protocol") or {}
    known = set(ERROR_HINTS) | REFRESH_RELOGIN_CODES | {
        "oauth_cloudflare_challenge", "oauth_callback_not_found", "oauth_callback_timeout",
        "email_otp_provider_required", "email_otp_verification_failed", "login_continue_failed",
        "mfa_totp_verification_failed", "choose_account_failed", "about_you_failed",
    }
    code = result.get("error")
    page = protocol.get("last_page_type") or result.get("page_type")
    pages = {"start", "log-in", "login-check", "login_password", "mfa-challenge", "email-verification", "workspace",
             "consent", "edge-challenge", "http-page", "choose-an-account", "add-phone"}
    status = protocol.get("last_http_status") or protocol.get("last_status") or result.get("status")
    return {"stage": "authorize", "error_code": code if code in known else "oauth_authorization_failed",
            "page_type": page if page in pages else "unknown",
            "http_status": status if type(status) is int and 100 <= status <= 599 else 0,
            "token_exchange_started": False}


def reauth_account(
    account: AccountLine,
    settings: Settings,
    *,
    on_step=None,
    on_retry=None,
) -> dict:
    """跑完一个账号的协议重登，返回 payloads。"""
    impersonate = random.choice(settings.impersonate_pool)
    proxy = settings.proxy or ""
    chatgpt_client = ChatGPTClient(
        proxy=proxy,
        verbose=False,
        impersonate=impersonate,
    )
    chatgpt_client.current_email = account.email
    oauth_client = None
    try:
        oauth_client = OAuthClient(_oauth_config(settings), proxy=proxy, verbose=False)
        oauth_client.on_login_retry = on_retry
        oauth_client.session.close()
        oauth_client.session = chatgpt_client.session
        return _reauth_with_clients(account, settings, chatgpt_client, oauth_client, on_step=on_step)
    finally:
        sessions = [chatgpt_client.session]
        if oauth_client is not None:
            sessions.append(oauth_client.session)
        for session in {id(item): item for item in sessions}.values():
            try:
                session.close()
            except Exception:
                pass


def _reauth_with_clients(account, settings, chatgpt_client, oauth_client, *, on_step=None):
    def emit(step: str, message: str = "") -> None:
        if on_step is not None:
            on_step(step, message)

    # 共用同一个干净会话，保证登录 cookie 与授权流程连续
    oauth_client.session = chatgpt_client.session

    emit("session", "生成 PKCE 与授权链接")
    code_verifier, code_challenge = generate_pkce()
    state = secrets.token_urlsafe(32)
    authorize_url = build_authorize_url(code_challenge, state)
    mfa_client = TotpMfaClient(
        account.totp_secret,
        period=settings.totp_period,
    )

    started = time.time()
    emit("login", "提交邮箱与密码，跟随登录页")
    emit("mfa", "本地生成 2FA 动态码并提交")
    try:
        result = oauth_client.authorize_external_oauth_url(
            authorize_url,
            chatgpt_client,
            expected_state=state,
            user_agent=getattr(chatgpt_client, "ua", "") or None,
            impersonate=getattr(chatgpt_client, "impersonate", "") or None,
            email=account.email,
            password=account.password,
            mfa_client=mfa_client,
        )
    except Exception as exc:  # noqa: BLE001 - 统一转成站点可读错误
        if is_phone_required(exc):
            raise ReauthError(PHONE_MESSAGE, evidence=failure_evidence({"error": "phone_required"})) from exc
        raise ReauthError(f"登录流程异常：{type(exc).__name__}", evidence=failure_evidence({})) from exc

    if not isinstance(result, dict) or not result.get("ok"):
        reason = _failure_reason(result if isinstance(result, dict) else {})
        raise ReauthError(f"授权失败：{reason}", evidence=failure_evidence(result if isinstance(result, dict) else {}))
    code = str(result.get("code") or "").strip()
    callback_state = str(result.get("state") or "").strip()
    if not code:
        raise ReauthError("授权失败：没有拿到回调 code")
    if callback_state and callback_state != state:
        raise ReauthError("授权失败：state 校验不通过")
    emit("token", f"授权通过，换取 token（耗时 {int(time.time() - started)}s）")

    tokens = oauth_client._exchange_code_for_tokens(  # noqa: SLF001 - 协议库内部接口
        code,
        code_verifier,
        getattr(chatgpt_client, "ua", "") or None,
        getattr(chatgpt_client, "impersonate", "") or None,
    )
    if not isinstance(tokens, dict) or not tokens.get("access_token"):
        from common.oauth_errors import ACCOUNT_STATES
        code = getattr(oauth_client, 'token_exchange_rejection', '')
        if code in ACCOUNT_STATES:
            evidence = failure_evidence({'error': code, 'status': getattr(oauth_client, 'last_oauth_status', 0)})
            evidence.update(token_exchange_started=True, token_exchange_rejected=True)
            raise ReauthError(ERROR_HINTS[code], evidence=evidence)
        raise ReauthError("token 交换失败，请稍后重试")

    session = {
        "accessToken": tokens.get("access_token"),
        "refreshToken": tokens.get("refresh_token"),
        "idToken": tokens.get("id_token"),
        "email": account.email,
    }
    emit("build", "生成 cpa / sub2api / cockpit 结构")
    payloads = session_to_payloads(session, account.email)
    item = payloads.get("item") or {}
    plan_type = str(item.get("plan_type") or "unknown").lower() or "unknown"
    if not item.get("refresh_token"):
        raise ReauthError("没有拿到 refresh_token，本次结果不可用")
    return {
        "email": account.email,
        "plan_type": plan_type,
        "account_id": item.get("account_id") or "",
        "expires_at": item.get("expires_at") or "",
        "payloads": {
            "cpa": payloads.get("cpa") or {},
            "sub2api": payloads.get("sub2api") or {},
            "cockpit": payloads.get("cockpit") or {},
        },
    }


def probe_proxy(settings: Settings, *, timeout: int = 20) -> dict:
    """探测 OpenAI 认证入口连通性（给管理页自检用）。"""
    from .lib.http_client import create_http_session

    session = create_http_session(
        proxy=settings.proxy or None,
        impersonate=settings.impersonate,
    )
    try:
        resp = session.get(
            f"{OAUTH_ISSUER}/",
            timeout=timeout,
            allow_redirects=True,
        )
        return {"ok": True, "status": resp.status_code}
    except Exception as exc:  # noqa: BLE001
        return {"ok": False, "error": f"{type(exc).__name__}: {exc}"}
    finally:
        try:
            session.close()
        except Exception:  # noqa: BLE001
            pass


def describe_payloads(payloads: dict) -> str:
    """调试用：只输出结构，不输出任何令牌。"""
    return json.dumps(
        {key: sorted((value or {}).keys()) for key, value in payloads.items()},
        ensure_ascii=False,
    )
