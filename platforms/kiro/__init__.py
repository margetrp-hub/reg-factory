"""platforms/kiro — Kiro"""
from platforms.base import PlatformSpec, register_step  # noqa: F401

SPEC = PlatformSpec(
    name="kiro",
    label="Kiro",
    modes=('browser',),
    steps=('register', 'verify'),
    script_names=(),
    requires=('email', 'proxy'),
    notes='单实现',
)

ALL = [SPEC]

# 挂步骤实现。导入失败的平台不影响其他平台。
try:
    from platforms.kiro.register import register
    register_step('kiro', 'register', register)
except Exception:
    pass

# verify 步骤：跨平台通用，用 platforms._verify.verify
try:
    from platforms._verify import verify as _verify_fn
    register_step('kiro', 'verify', _verify_fn)
except Exception:
    pass
