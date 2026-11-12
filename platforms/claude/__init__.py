"""platforms/claude — Claude"""
from platforms.base import PlatformSpec, register_step  # noqa: F401

SPEC = PlatformSpec(
    name="claude",
    label="Claude",
    modes=('browser', 'http'),
    steps=('register', 'verify'),
    script_names=('register_three_platforms',),
    requires=('email', 'proxy', 'captcha', 'sms'),
    notes='register.py 是统一入口，--protocol 切换 browser/http',
)

ALL = [SPEC]

# 挂步骤实现。导入失败的平台不影响其他平台。
try:
    from platforms.claude.register import register
    register_step('claude', 'register', register)
except Exception:
    pass

# verify 步骤：跨平台通用，用 platforms._verify.verify
try:
    from platforms._verify import verify as _verify_fn
    register_step('claude', 'verify', _verify_fn)
except Exception:
    pass
