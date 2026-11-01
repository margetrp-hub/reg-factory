#!/usr/bin/env bash
# reg-factory 服务器版冒烟测试：鉴权闸门 + 面板接口 + 资产 API 配置
set -uo pipefail

# 在 Windows 的 Git Bash / MSYS 下跑时，避免 /data 这类容器内路径被改写成 C:\...
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE="${RF_BASE:-http://127.0.0.1:8799}"
ENV_FILE="${HERE}/.env"

pass=0
fail=0
ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; pass=$((pass + 1)); }
bad()  { printf '  \033[31m✗\033[0m %s\n' "$*"; fail=$((fail + 1)); }
info() { printf '  · %s\n' "$*"; }

if [ -f "${ENV_FILE}" ]; then
  USER_NAME="$(grep -E '^REG_FACTORY_PANEL_USER=' "${ENV_FILE}" | head -1 | cut -d= -f2-)"
  PASSWORD="$(grep -E '^REG_FACTORY_PANEL_PASSWORD=' "${ENV_FILE}" | head -1 | cut -d= -f2-)"
fi
USER_NAME="${USER_NAME:-admin}"
PASSWORD="${PASSWORD:-}"

echo "== reg-factory 服务器冒烟 =="
info "目标 ${BASE}"

# 1. 容器
if command -v docker >/dev/null 2>&1 && docker inspect reg-factory >/dev/null 2>&1; then
  state="$(docker inspect --format '{{.State.Status}}/{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' reg-factory)"
  case "${state}" in
    running/healthy) ok "容器状态 ${state}" ;;
    *) bad "容器状态 ${state}" ;;
  esac
else
  info "跳过容器状态检查（docker 不可用或容器名不同）"
fi

# 2. 健康检查
if curl -fsS --max-time 5 "${BASE}/__rf/health" | grep -q '"ok":true'; then
  ok "健康检查 /__rf/health"
else
  bad "健康检查 /__rf/health 失败"
fi

# 3. 未认证页面访问应跳登录页
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "${BASE}/")"
loc="$(curl -s -o /dev/null -D - --max-time 5 "${BASE}/" | tr -d '\r' | awk 'tolower($1)=="location:" {print $2}')"
if [ "${code}" = "302" ] && printf '%s' "${loc}" | grep -q '__rf/login'; then
  ok "未认证访问被导向登录页"
else
  bad "未认证访问未拦截（code=${code} location=${loc}）—— 检查 REG_FACTORY_ALLOW_ANONYMOUS"
fi

# 4. 未认证 API 应 401
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "${BASE}/api/status")"
if [ "${code}" = "401" ]; then
  ok "未认证调用 /api/status 返回 401"
else
  bad "未认证调用 /api/status 返回 ${code}（应为 401）"
fi

# 5. 登录后应能读面板状态
if [ -n "${PASSWORD}" ]; then
  jar="$(mktemp)"
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
    -c "${jar}" -d "username=${USER_NAME}&password=${PASSWORD}&next=/" "${BASE}/__rf/login")"
  if [ "${code}" = "302" ] || [ "${code}" = "200" ]; then
    body="$(curl -s --max-time 10 -b "${jar}" "${BASE}/api/status")"
    if printf '%s' "${body}" | grep -q '"browser_provider"'; then
      provider="$(printf '%s' "${body}" | sed -n 's/.*"browser_provider": *"\([^"]*\)".*/\1/p')"
      ok "面板登录成功，browser_provider=${provider:-unknown}"
    else
      bad "登录后 /api/status 未返回预期结构：$(printf '%s' "${body}" | head -c 200)"
    fi
  else
    bad "登录请求返回 ${code}"
  fi
  rm -f "${jar}"
else
  info "未读取到面板密码，跳过登录链路检查"
fi

# 6. 资产 API key
if docker inspect reg-factory >/dev/null 2>&1; then
  if docker exec reg-factory test -s /data/runtime/asset-api-key.txt 2>/dev/null; then
    ok "资产 API Key 已生成（/data/runtime/asset-api-key.txt）"
  else
    bad "资产 API Key 未生成"
  fi
fi

# 7. 浏览器内核
if docker inspect reg-factory >/dev/null 2>&1; then
  if docker exec reg-factory test -x /usr/local/bin/chromium 2>/dev/null; then
    ok "Chromium 可执行（/usr/local/bin/chromium）"
  else
    bad "Chromium 缺失"
  fi
fi

echo
echo "通过 ${pass} 项，失败 ${fail} 项"
[ "${fail}" -eq 0 ]
