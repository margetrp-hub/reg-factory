"""platforms/grok — Grok"""
from platforms.base import PlatformSpec, register_step  # noqa: F401

SPEC = PlatformSpec(
    name="grok",
    label="Grok",
    modes=('browser', 'http'),
    steps=('register', 'verify'),
    script_names=(),
    requires=('proxy', 'captcha'),
    notes='http 版已挂；browser 版 register_grok.py 待条件切换',
)

ALL = [SPEC]

# 挂步骤实现。导入失败的平台不影响其他平台。
try:
    from platforms.grok.register import register
    register_step('grok', 'register', register)
except Exception:
    pass

# verify 步骤：跨平台通用，用 platforms._verify.verify
try:
    from platforms._verify import verify as _verify_fn
    register_step('grok', 'verify', _verify_fn)
except Exception:
    pass
