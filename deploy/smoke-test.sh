#!/usr/bin/env bash
# reg-factory 统一控制面冒烟：健康、静态页面、鉴权、会话与 worker 运行时
set -uo pipefail

# Git Bash / MSYS 不转换容器内路径。
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE="${RF_BASE:-http://127.0.0.1:8800}"
ENV_FILE="${HERE}/.env"
CONTAINER="${CONTAINER:-reg-factory}"
USER_NAME="${REG_FACTORY_PANEL_USER:-}"
PASSWORD="${REG_FACTORY_PANEL_PASSWORD:-}"

pass=0
fail=0
ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; pass=$((pass + 1)); }
bad()  { printf '  \033[31m✗\033[0m %s\n' "$*"; fail=$((fail + 1)); }
info() { printf '  · %s\n' "$*"; }

if [ -f "${ENV_FILE}" ]; then
  USER_NAME="${USER_NAME:-$(grep -E '^REG_FACTORY_PANEL_USER=' "${ENV_FILE}" | head -1 | cut -d= -f2-)}"
  PASSWORD="${PASSWORD:-$(grep -E '^REG_FACTORY_PANEL_PASSWORD=' "${ENV_FILE}" | head -1 | cut -d= -f2-)}"
fi
USER_NAME="${USER_NAME:-admin}"

# 不依赖宿主 Python/jq；密码中的引号、反斜杠与换行按 JSON 编码。
json_escape() {
  local value="$1"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  value="${value//$'\n'/\\n}"
  value="${value//$'\r'/\\r}"
  value="${value//$'\t'/\\t}"
  value="${value//$'\b'/\\b}"
  value="${value//$'\f'/\\f}"
  printf '%s' "${value}"
}

echo "== reg-factory 统一控制面冒烟 =="
info "目标 ${BASE}"

HAS_CONTAINER=0
if command -v docker >/dev/null 2>&1 && docker inspect "${CONTAINER}" >/dev/null 2>&1; then
  HAS_CONTAINER=1
  # Compose 已处理 .env 的引号和插值；优先验证容器实际收到的凭据。
  # 显式传给本脚本的环境值仍可覆盖，用于诊断失败路径。
  runtime_user="$(docker exec "${CONTAINER}" printenv REG_FACTORY_PANEL_USER 2>/dev/null || true)"
  runtime_password="$(docker exec "${CONTAINER}" printenv REG_FACTORY_PANEL_PASSWORD 2>/dev/null || true)"
  USER_NAME="${REG_FACTORY_PANEL_USER:-${runtime_user:-${USER_NAME}}}"
  PASSWORD="${REG_FACTORY_PANEL_PASSWORD:-${runtime_password:-${PASSWORD}}}"
  state="$(docker inspect --format '{{.State.Status}}/{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "${CONTAINER}")"
  case "${state}" in
    running/healthy) ok "容器状态 ${state}" ;;
    *) bad "容器状态 ${state}" ;;
  esac
else
  info "跳过容器检查（docker 不可用或未找到 ${CONTAINER}）"
fi

if curl -fsS --max-time 5 "${BASE}/api/health" | grep -q '"service"[[:space:]]*:[[:space:]]*"regd"'; then
  ok "regd 健康检查 /api/health"
else
  bad "regd 健康检查 /api/health 失败"
fi

# 静态页面公开，由前端根据 /api/session 跳转；服务端 API 必须返回 401。
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "${BASE}/login/")"
if [ "${code}" = "200" ]; then
  ok "登录页面可访问"
else
  bad "登录页面返回 ${code}（应为 200）"
fi
for route in /api/session /api/system; do
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "${BASE}${route}")"
  if [ "${code}" = "401" ]; then
    ok "未认证 ${route} 返回 401"
  else
    bad "未认证 ${route} 返回 ${code}（应为 401）"
  fi
done

if [ -n "${PASSWORD}" ]; then
  jar="$(mktemp)" || exit 1
  trap 'rm -f "${jar}"' EXIT
  payload="$(printf '{"username":"%s","password":"%s"}' "$(json_escape "${USER_NAME}")" "$(json_escape "${PASSWORD}")")"
  code="$(printf '%s' "${payload}" | curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
    -c "${jar}" -H 'Content-Type: application/json' --data-binary @- "${BASE}/api/login")"
  if [ "${code}" = "200" ]; then
    body="$(curl -fsS --max-time 10 -b "${jar}" "${BASE}/api/system")"
    if printf '%s' "${body}" | grep -q '"max_parallel"[[:space:]]*:'; then
      ok "JSON 登录与 Cookie 会话可读取 /api/system"
    else
      bad "登录后 /api/system 未返回控制面状态"
    fi
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 -X POST \
      -b "${jar}" -c "${jar}" "${BASE}/api/logout")"
    session_code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -b "${jar}" "${BASE}/api/session")"
    if [ "${code}" = "200" ] && [ "${session_code}" = "401" ]; then
      ok "登出后会话恢复未认证状态"
    else
      bad "登出后会话状态异常（logout=${code} session=${session_code}）"
    fi
  else
    bad "登录请求返回 ${code}"
  fi
else
  bad "未读取到面板密码，无法验证登录链路"
fi

if [ "${HAS_CONTAINER}" = "1" ]; then
  if docker exec "${CONTAINER}" sh -c '[ -n "${REG_FACTORY_ASSET_API_KEY:-}" ] || test -s /data/runtime/asset-api-key.txt || grep -qE "^REG_FACTORY_ASSET_API_KEY=.+" /data/.env'; then
    ok "资产 API Key 已配置或自动生成"
  else
    bad "资产 API Key 未配置"
  fi
  if docker exec "${CONTAINER}" test -x /usr/local/bin/chromium; then
    ok "Chromium 可执行"
  else
    bad "Chromium 缺失"
  fi
  if docker exec "${CONTAINER}" python3 -c 'import playwright, requests, PIL, aiohttp, curl_cffi, cryptography'; then
    ok "Python worker 运行依赖可导入"
  else
    bad "Python worker 运行依赖缺失"
  fi
fi

echo
echo "通过 ${pass} 项，失败 ${fail} 项"
[ "${fail}" -eq 0 ]
