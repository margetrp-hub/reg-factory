# -*- coding: utf-8 -*-
"""deploy/serve.py — 服务器部署入口（生产用，替代 start.sh）。

和 start.sh 的差别：
  1. 监听 0.0.0.0（容器内），对外暴露由 compose 端口绑定 / 反向代理决定；
  2. 前置 PanelGate 鉴权，避免把可编辑 .env、可执行任务的 WebUI 裸奔到公网；
  3. 先加载 REG_FACTORY_ENV_FILE 再 import webui.server，保证配置一致；
  4. 单 worker + 关闭 access log 泛洪（任务内存态 RUNS/SMS_RENTS 都在进程内）；
  5. 打印一份启动摘要，便于在 docker logs 里核对版本 / 数据目录 / 浏览器 provider。

用法：
  python -m deploy.serve                 # 正常启动
  python -m deploy.serve --check         # 只做配置与导入自检，不监听端口
"""

from __future__ import annotations

import argparse
import os
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))


def load_env_file(path: Path) -> int:
    """把 .env 注入进程环境；真实环境变量优先（与 config._load_dotenv 语义一致）。"""
    if not path.is_file():
        return 0
    loaded = 0
    try:
        for raw in path.read_text(encoding="utf-8", errors="replace").splitlines():
            line = raw.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, _, value = line.partition("=")
            key = key.strip()
            if key and key not in os.environ:
                os.environ[key] = value.strip().strip('"').strip("'")
                loaded += 1
    except OSError as exc:
        print(f"[serve] 读取 {path} 失败: {exc}", flush=True)
    return loaded


def _summary() -> str:
    data_dir = os.environ.get("REG_FACTORY_DATA_DIR") or "/data"
    lines = [
        "=" * 62,
        "  reg-factory 服务器版",
        f"  数据目录   : {data_dir}",
        f"  环境文件   : {os.environ.get('REG_FACTORY_ENV_FILE') or os.path.join(data_dir, '.env')}",
        f"  浏览器     : {os.environ.get('FINGERPRINT_BROWSER', 'bundled')}",
        f"  出口模式   : {os.environ.get('PROXY_MODE', 'residential')}",
        f"  Display    : {os.environ.get('DISPLAY') or '(headless)'}",
        f"  监听       : {os.environ.get('REG_FACTORY_HOST', '0.0.0.0')}:{os.environ.get('REG_FACTORY_PORT', '8799')}",
        f"  面板鉴权   : {'关闭(内网)' if os.environ.get('REG_FACTORY_ALLOW_ANONYMOUS', '').lower() in {'1', 'true', 'yes', 'on'} else '开启'}",
        "=" * 62,
    ]
    return "\n".join(lines)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="reg-factory server entrypoint")
    parser.add_argument("--check", action="store_true", help="只验证配置与导入，不启动服务")
    args = parser.parse_args(argv)

    data_dir = Path(os.environ.setdefault("REG_FACTORY_DATA_DIR", "/data"))
    os.environ.setdefault("REG_FACTORY_ENV_FILE", str(data_dir / ".env"))
    data_dir.mkdir(parents=True, exist_ok=True)
    loaded = load_env_file(Path(os.environ["REG_FACTORY_ENV_FILE"]))

    os.chdir(ROOT)
    sys.path.insert(0, str(ROOT / "webui"))

    try:
        from webui.server import app as raw_app, WEBUI_VERSION  # noqa: WPS433 (延迟导入)
    except Exception as exc:  # pragma: no cover - 启动期依赖缺失时给出可读错误
        print(f"[serve] 导入 webui.server 失败: {type(exc).__name__}: {exc}", flush=True)
        raise

    from deploy.gate import gate_from_env
    from deploy.memory_guard import guard_from_env

    # 顺序：gate 先认证，memory_guard 再判断内存水位（都不动业务代码）
    app = gate_from_env(guard_from_env(raw_app))

    print(_summary(), flush=True)
    print(f"[serve] .env 注入 {loaded} 项；WebUI 版本 {WEBUI_VERSION}", flush=True)

    if args.check:
        print("[serve] --check 通过：配置与 FastAPI 应用可加载。", flush=True)
        return 0

    import uvicorn

    host = os.environ.get("REG_FACTORY_HOST", "0.0.0.0")
    port = int(os.environ.get("REG_FACTORY_PORT", "8799") or 8799)
    log_level = os.environ.get("REG_FACTORY_LOG_LEVEL", "info").strip().lower() or "info"

    uvicorn.run(
        app,
        host=host,
        port=port,
        log_level=log_level,
        access_log=False,
        proxy_headers=True,
        forwarded_allow_ips=os.environ.get("REG_FACTORY_FORWARDED_ALLOW_IPS", "127.0.0.1,::1"),
        timeout_keep_alive=int(os.environ.get("REG_FACTORY_KEEPALIVE", "75") or 75),
        # 注册任务本身会并发拉多个浏览器，连接数不是瓶颈；过高的上限只会放大风险。
        limit_concurrency=int(os.environ.get("REG_FACTORY_LIMIT_CONCURRENCY", "200") or 200),
        server_header=False,
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
