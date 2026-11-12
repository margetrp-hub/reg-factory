"""platforms — 平台层。

对外只暴露三个东西：
    dispatcher.run()        执行一步
    registry.capabilities() 能力自述
    registry.script_names() 派单名契约

细节见各部分 docstring。
"""
from platforms import dispatcher, registry  # noqa: F401

__all__ = ["dispatcher", "registry"]
