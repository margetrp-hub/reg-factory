"""platforms/outlook — Outlook"""
from platforms.base import PlatformSpec, register_step  # noqa: F401

SPEC = PlatformSpec(
    name="outlook",
    label="Outlook",
    modes=('browser', 'batch'),
    steps=('register', 'unlock', 'verify'),
    script_names=('unlock_outlook',),
    requires=('proxy', 'captcha'),
    notes='standalone 与 unlock 已挂；批量待合并',
)

ALL = [SPEC]

# 挂步骤实现。导入失败的平台不影响其他平台。
try:
    from platforms.outlook.register import register
    register_step('outlook', 'register', register)
except Exception:
    pass
try:
    from platforms.outlook.unlock import unlock
    register_step('outlook', 'unlock', unlock)
except Exception:
    pass

# verify 步骤：跨平台通用，用 platforms._verify.verify
try:
    from platforms._verify import verify as _verify_fn
    register_step('outlook', 'verify', _verify_fn)
except Exception:
    pass
