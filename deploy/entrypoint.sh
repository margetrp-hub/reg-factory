#!/usr/bin/env bash
# reg-factory 服务器容器入口：准备数据目录 -> 生成 .env -> 起 Xvfb -> 降权启动面板
set -euo pipefail

APP_DIR="${APP_DIR:-/app}"
DATA_DIR="${REG_FACTORY_DATA_DIR:-/data}"
ENV_FILE="${REG_FACTORY_ENV_FILE:-${DATA_DIR}/.env}"
RUN_USER="${REG_FACTORY_RUN_USER:-rf}"
PORT="${REG_FACTORY_PORT:-8799}"

log() { printf '[entrypoint] %s\n' "$*"; }

# ------------------------------------------------------------ 数据目录
mkdir -p \
  "${DATA_DIR}" \
  "${DATA_DIR}/runtime" \
  "${DATA_DIR}/runtime/logs" \
  "${DATA_DIR}/runtime/state" \
  "${DATA_DIR}/runtime/secrets" \
  "${DATA_DIR}/runtime/gopay" \
  "${DATA_DIR}/logs" \
  "${DATA_DIR}/cookies" \
  "${DATA_DIR}/tokens" \
  "${DATA_DIR}/screenshots" \
  "${DATA_DIR}/browser-profiles" \
  "${DATA_DIR}/outlook_accounts" \
  "${DATA_DIR}/_outlook_pool" \
  "${DATA_DIR}/tri_register_logs"

# ------------------------------------------------------------ 首次生成 .env
if [ ! -f "${ENV_FILE}" ]; then
  log "初始化 ${ENV_FILE}"
  if [ -f "${APP_DIR}/deploy/env.app.example" ]; then
    cp "${APP_DIR}/deploy/env.app.example" "${ENV_FILE}"
  else
    : > "${ENV_FILE}"
  fi
  chmod 600 "${ENV_FILE}" || true
fi

# 用 python 读写 .env：key 存在则就地替换，不存在则追加（首个匹配优先语义）
env_set() {
  local key="$1" value="$2" file="$3"
  "${PYTHON:-python}" - "$key" "$value" "$file" <<'PY'
import sys, pathlib
key, value, path = sys.argv[1], sys.argv[2], pathlib.Path(sys.argv[3])
lines = path.read_text(encoding="utf-8", errors="replace").splitlines() if path.exists() else []
out, replaced = [], False
for line in lines:
    stripped = line.strip()
    if stripped and not stripped.startswith("#") and "=" in stripped:
        name = stripped.split("=", 1)[0].strip()
        if name == key:
            if not replaced:
                out.append(f"{key}={value}")
                replaced = True
            continue
    out.append(line)
if not replaced:
    out.append(f"{key}={value}")
path.write_text("\n".join(out) + "\n", encoding="utf-8")
PY
}

env_get() {
  local key="$1" file="$2"
  "${PYTHON:-python}" - "$key" "$file" <<'PY'
import sys, pathlib
key, path = sys.argv[1], pathlib.Path(sys.argv[2])
if path.exists():
    for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
        s = line.strip()
        if s and not s.startswith("#") and "=" in s:
            k, _, v = s.partition("=")
            if k.strip() == key:
                print(v.strip().strip('"').strip("'"))
                break
PY
}

# 面板密码：只从进程环境读取（compose env_file），绝不写进 /data/.env，
# 避免通过面板的「环境配置」页或任务子进程环境泄漏。
ALLOW_ANON="${REG_FACTORY_ALLOW_ANONYMOUS:-0}"
if [ -z "${REG_FACTORY_PANEL_PASSWORD:-}" ] && [ "${ALLOW_ANON}" != "1" ]; then
  log "缺少 REG_FACTORY_PANEL_PASSWORD：请写入 deploy/.env 后重启容器"
  exit 78
fi

# 资产 API Key：未提供时自动生成，保证公网入口不能匿名领取账号
if [ -z "${REG_FACTORY_ASSET_API_KEY:-}" ] && [ -z "$(env_get REG_FACTORY_ASSET_API_KEY "${ENV_FILE}")" ]; then
  ASSET_KEY="$("${PYTHON:-python}" -c 'import secrets; print("rf_" + secrets.token_urlsafe(32))')"
  env_set REG_FACTORY_ASSET_API_KEY "${ASSET_KEY}" "${ENV_FILE}"
  umask 077
  printf '%s\n' "${ASSET_KEY}" > "${DATA_DIR}/runtime/asset-api-key.txt"
  chmod 600 "${DATA_DIR}/runtime/asset-api-key.txt" || true
  log "已生成资产 API Key -> ${DATA_DIR}/runtime/asset-api-key.txt"
fi

# ------------------------------------------------------------ 权限
if [ "$(id -u)" = "0" ]; then
  # 只在整个树里还有非目标属主的文件时才全量 chown，避免每次重启扫大盘
  if [ -n "$(find "${DATA_DIR}" ! -user "${RUN_USER}" -print -quit 2>/dev/null || true)" ]; then
    chown -R "${RUN_USER}:${RUN_USER}" "${DATA_DIR}" 2>/dev/null || true
  fi
  DROP_TO=(setpriv --reuid="${RUN_USER}" --regid="${RUN_USER}" --init-groups --inh-caps=-all --)
else
  DROP_TO=()
fi

# ------------------------------------------------------------ 显示环境
# 无头/有头由 /data/.env 决定（面板里改完重启容器即生效）；进程环境可强制覆盖。
if [ -n "${REG_FACTORY_BROWSER_HEADLESS:-}" ]; then
  BROWSER_HEADLESS="${REG_FACTORY_BROWSER_HEADLESS}"
else
  BROWSER_HEADLESS="$(env_get REG_FACTORY_BROWSER_HEADLESS "${ENV_FILE}")"
  BROWSER_HEADLESS="${BROWSER_HEADLESS:-0}"
fi
if [ -n "${REG_FACTORY_XVFB:-}" ]; then
  USE_XVFB="${REG_FACTORY_XVFB}"
else
  USE_XVFB="$(env_get REG_FACTORY_XVFB "${ENV_FILE}")"
  USE_XVFB="${USE_XVFB:-1}"
fi

if [ "${USE_XVFB}" = "1" ] && [ "${BROWSER_HEADLESS}" != "1" ]; then
  if command -v Xvfb >/dev/null 2>&1; then
    export DISPLAY="${DISPLAY:-:99}"
    log "启动 Xvfb ${DISPLAY} (有头 Chromium，抗检测更好)"
    Xvfb "${DISPLAY}" -screen 0 1600x1000x24 -nolisten tcp >/tmp/xvfb.log 2>&1 &
    for _ in $(seq 1 50); do
      [ -S "/tmp/.X11-unix/X${DISPLAY#:}" ] && break
      sleep 0.1
    done
    # root 起的 Xvfb 用默认 umask 建 socket，降权后的 rf 需要写权限才能连
    chmod 0666 "/tmp/.X11-unix/X${DISPLAY#:}" 2>/dev/null || true
  else
    log "未安装 Xvfb，退回无头模式"
    export REG_FACTORY_BROWSER_HEADLESS=1
  fi
else
  unset DISPLAY || true
  log "无头模式运行 Chromium"
fi

export REG_FACTORY_DATA_DIR="${DATA_DIR}"
export REG_FACTORY_ENV_FILE="${ENV_FILE}"
export REG_FACTORY_HOST="${REG_FACTORY_HOST:-0.0.0.0}"
export REG_FACTORY_PORT="${PORT}"
export PYTHONUNBUFFERED=1
export PYTHONPATH="${APP_DIR}${PYTHONPATH:+:${PYTHONPATH}}"

log "启动面板 :${PORT}（数据目录 ${DATA_DIR}）"
cd "${APP_DIR}"
exec "${DROP_TO[@]}" "${PYTHON:-python}" -m deploy.serve
