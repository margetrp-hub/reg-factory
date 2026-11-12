"""platforms/replit — Replit"""
from platforms.base import PlatformSpec, register_step  # noqa: F401

SPEC = PlatformSpec(
    name="replit",
    label="Replit",
    modes=('browser',),
    steps=('register', 'verify'),
    script_names=(),
    requires=('email',),
    notes='逻辑仍在 register.py 内，接口已声明',
)

ALL = [SPEC]

# 挂步骤实现。导入失败的平台不影响其他平台。
try:
    from platforms.replit.register import register
    register_step('replit', 'register', register)
except Exception:
    pass

# verify 步骤：跨平台通用，用 platforms._verify.verify
try:
    from platforms._verify import verify as _verify_fn
    register_step('replit', 'verify', _verify_fn)
except Exception:
    pass
