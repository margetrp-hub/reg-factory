#!/usr/bin/env bash
# reg-factory Go 控制面容器入口：数据目录 -> .env -> Xvfb -> 降权启动 regd
set -euo pipefail

APP_DIR="${APP_DIR:-/app}"
DATA_DIR="${REG_FACTORY_DATA_DIR:-/data}"
ENV_FILE="${REG_FACTORY_ENV_FILE:-${DATA_DIR}/.env}"
RUN_USER="${REG_FACTORY_RUN_USER:-rf}"
ADDR="${REGD_ADDR:-127.0.0.1:8800}"

log() { printf '[entrypoint-go] %s\n' "$*"; }

# ------------------------------------------------------------ 数据目录
mkdir -p \
  "${DATA_DIR}/runtime" "${DATA_DIR}/runtime/logs" "${DATA_DIR}/runtime/state" \
  "${DATA_DIR}/runtime/secrets" "${DATA_DIR}/logs" "${DATA_DIR}/cookies" \
  "${DATA_DIR}/tokens" "${DATA_DIR}/screenshots" "${DATA_DIR}/browser-profiles" \
  "${DATA_DIR}/outlook_accounts" "${DATA_DIR}/_outlook_pool" "${DATA_DIR}/tri_register_logs"

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

# 面板密码：只从进程环境读取（compose env_file），不写进 /data/.env
if [ -z "${REG_FACTORY_PANEL_PASSWORD:-}" ] && [ "${REG_FACTORY_ALLOW_ANONYMOUS:-0}" != "1" ]; then
  log "缺少 REG_FACTORY_PANEL_PASSWORD：请写入 deploy/.env 后重启容器"
  exit 78
fi

# 资产 API Key：未提供时自动生成，供下游拉号
if [ -z "${REG_FACTORY_ASSET_API_KEY:-}" ] && ! grep -qE '^REG_FACTORY_ASSET_API_KEY=.+' "${ENV_FILE}" 2>/dev/null; then
  ASSET_KEY="$("${PYTHON:-python}" -c 'import secrets; print("rf_" + secrets.token_urlsafe(32))')"
  printf '\nREG_FACTORY_ASSET_API_KEY=%s\n' "${ASSET_KEY}" >> "${ENV_FILE}"
  umask 077
  printf '%s\n' "${ASSET_KEY}" > "${DATA_DIR}/runtime/asset-api-key.txt"
  chmod 600 "${DATA_DIR}/runtime/asset-api-key.txt" || true
  log "已生成资产 API Key -> ${DATA_DIR}/runtime/asset-api-key.txt"
fi

# ------------------------------------------------------------ 权限
if [ "$(id -u)" = "0" ]; then
  if [ -n "$(find "${DATA_DIR}" ! -user "${RUN_USER}" -print -quit 2>/dev/null || true)" ]; then
    chown -R "${RUN_USER}:${RUN_USER}" "${DATA_DIR}" 2>/dev/null || true
  fi
  DROP_TO=(setpriv --reuid="${RUN_USER}" --regid="${RUN_USER}" --init-groups --inh-caps=-all --)
else
  DROP_TO=()
fi

# ------------------------------------------------------------ 显示
HEADLESS="${REG_FACTORY_BROWSER_HEADLESS:-0}"
if [ "${REG_FACTORY_XVFB:-1}" = "1" ] && [ "${HEADLESS}" != "1" ]; then
  if command -v Xvfb >/dev/null 2>&1; then
    export DISPLAY="${DISPLAY:-:99}"
    log "启动 Xvfb ${DISPLAY}（有头 Chromium）"
    Xvfb "${DISPLAY}" -screen 0 1600x1000x24 -nolisten tcp >/tmp/xvfb.log 2>&1 &
    for _ in $(seq 1 50); do
      [ -S "/tmp/.X11-unix/X${DISPLAY#:}" ] && break
      sleep 0.1
    done
    chmod 0666 "/tmp/.X11-unix/X${DISPLAY#:}" 2>/dev/null || true
  else
    log "未安装 Xvfb，退回无头模式"
    export REG_FACTORY_BROWSER_HEADLESS=1
  fi
else
  unset DISPLAY || true
fi

export REG_FACTORY_DATA_DIR="${DATA_DIR}"
export REG_FACTORY_ENV_FILE="${ENV_FILE}"
export REGD_APP_ROOT="${APP_DIR}"
export REGD_ADDR="${ADDR}"
export PYTHONUNBUFFERED=1
export PYTHONPATH="${APP_DIR}${PYTHONPATH:+:${PYTHONPATH}}"

log "启动 regd ${ADDR}（数据目录 ${DATA_DIR}）"
cd "${APP_DIR}"
exec "${DROP_TO[@]}" /usr/local/bin/regd
