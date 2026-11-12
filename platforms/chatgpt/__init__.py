"""platforms/chatgpt — ChatGPT 四步流水线

Step 1  register  浏览器注册          register_chatgpt.py（Python）
Step 2  login     登录刷新 AT         login_export.py（Python）
Step 3  checkout  提链                pluscheck（Go）
Step 4  pay       支付                plusbind（Go）

前三步中 1/2 需要浏览器，因为注册和登录都有 Cloudflare 挑战。
Step 3/4 是纯协议，Go 实现能省掉解释器启动并拿到结构化错误。
"""
from platforms.base import PlatformSpec, register_step  # noqa: F401

SPEC = PlatformSpec(
    name="chatgpt",
    label="ChatGPT",
    modes=("browser", "http"),
    steps=("register", "login", "checkout", "pay", "verify"),
    script_names=("register_chatgpt", "register_three_platforms"),
    requires=("email", "proxy"),
    notes="四步流水线；Step 3/4 已 Go 化，Step 1/2 保持 Python 浏览器",
)

ALL = [SPEC]

# 挂步骤实现。单个失败不影响其他步骤。
try:
    from platforms.chatgpt.register import register
    register_step("chatgpt", "register", register)
except Exception:
    pass

try:
    from platforms.chatgpt.login import login
    register_step("chatgpt", "login", login)
except Exception:
    pass

try:
    from platforms.chatgpt.checkout import checkout
    register_step("chatgpt", "checkout", checkout)
except Exception:
    pass

try:
    from platforms.chatgpt.pay import pay
    register_step("chatgpt", "pay", pay)
except Exception:
    pass

try:
    from platforms._verify import verify as _verify_fn
    register_step("chatgpt", "verify", _verify_fn)
except Exception:
    pass
