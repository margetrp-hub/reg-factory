#!/usr/bin/env bash
# 代理池体检：SG 机房 IP 直连 claude.ai / chatgpt.com / grok.com 会被 Cloudflare 403，
# 上任务前先用这个脚本确认住宅代理确实能过，而不是等到浏览器里看到 "Just a moment"。
#
#   ./proxy-check.sh                                   # 读容器 /data/.env 里的代理池
#   ./proxy-check.sh "http://u:p@h1:1234,http://u:p@h2:1234"
#   ./proxy-check.sh --targets https://claude.ai,https://chatgpt.com
#
# 判读：
#   * 出口 IP 与原机相同 → 代理没生效
#   * 200/302 → 可用
#   * 403 + 耗时 < 0.3s → 大概率被 Cloudflare 直接拦（数据中心 IP 段）
#   * 403 + 耗时 > 1s → 可能只是需要 JS 挑战，有头浏览器仍有机会
set -uo pipefail

CONTAINER="${CONTAINER:-reg-factory}"
UA="Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
TARGETS_DEFAULT="https://claude.ai,https://chatgpt.com,https://grok.com"

PROXIES=""
TARGETS="${TARGETS_DEFAULT}"
while [ $# -gt 0 ]; do
  case "$1" in
    --targets) TARGETS="$(echo "${2:-}" | tr ',' ' ')"; shift 2 ;;
    -h|--help) sed -n '2,17p' "$0"; exit 0 ;;
    *) PROXIES="${1}"; shift ;;
  esac
done

if [ -z "${PROXIES}" ]; then
  PROXIES="$(docker exec "${CONTAINER}" sh -c \
    'grep -hE "^REG_FACTORY_PROXY(_POOL)?=" /data/.env 2>/dev/null | cut -d= -f2-' 2>/dev/null \
    | tr ',' '\n' | tr -d ' "' | grep -v '^$' | paste -sd, -)"
fi

echo "== reg-factory 代理池体检 =="
BASE_IP="$(curl -fsS --max-time 8 https://api.ipify.org 2>/dev/null || echo unknown)"
echo "本机直连出口：${BASE_IP}"
echo "目标站点：${TARGETS}"
echo

if [ -z "${PROXIES}" ]; then
  echo "未配置代理（/data/.env 的 REG_FACTORY_PROXY_POOL 为空）。"
  echo "直连基线检查："
  for target in ${TARGETS}; do
    read -r code secs < <(curl -s -o /dev/null -A "${UA}" -w '%{http_code} %{time_total}' --max-time 12 "${target}" 2>/dev/null)
    verdict="可用"
    [ "${code}" = "403" ] && verdict="被拦（需住宅代理）"
    printf '  %-28s %s %6.2fs  %s\n' "${target}" "${code}" "${secs:-0}" "${verdict}"
  done
  exit 0
fi

OK=0
TOTAL=0
IFS=',' read -ra LIST <<< "${PROXIES}"
for proxy in "${LIST[@]}"; do
  [ -n "${proxy}" ] || continue
  TOTAL=$((TOTAL + 1))
  redacted="$(printf '%s' "${proxy}" | sed -E 's#(://)[^@/]+@#\1***:***@#')"
  echo "--- ${redacted}"
  ip="$(curl -fsS --max-time 12 -x "${proxy}" https://api.ipify.org 2>/dev/null || echo FAIL)"
  if [ "${ip}" = "FAIL" ]; then
    echo "    代理不可用（连不上或认证失败）"
    continue
  fi
  if [ "${ip}" = "${BASE_IP}" ]; then
    echo "    出口 IP ${ip}（与本机相同，代理未生效）"
  else
    echo "    出口 IP ${ip}"
  fi
  alive=0
  for target in ${TARGETS}; do
    read -r code secs < <(curl -s -o /dev/null -x "${proxy}" -A "${UA}" -w '%{http_code} %{time_total}' --max-time 15 "${target}" 2>/dev/null)
    mark="✓"
    case "${code}" in
      200|301|302) alive=$((alive + 1)) ;;
      403) mark="403" ;;
      *) mark="?" ;;
    esac
    printf '    %-28s %s %6.2fs %s\n' "${target}" "${code:-ERR}" "${secs:-0}" "${mark}"
  done
  [ "${alive}" -gt 0 ] && OK=$((OK + 1))
done

echo
echo "可用代理：${OK}/${TOTAL}（判定标准：至少一个目标站点返回 200/30x）"
[ "${OK}" -eq 0 ] && {
  echo "建议：换住宅供应商或换地区；SG 机房段 IP 对 Cloudflare 站点基本不可用。"
  exit 1
}
exit 0
