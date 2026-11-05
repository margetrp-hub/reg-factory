#!/usr/bin/env bash
# 给 reg-factory 面板签发证书并挂上 Nginx 反代（SG 主机，复用现有 certbot webroot）
#
#   sudo ./sg-enable-https.sh reg.yhoo.lol you@example.com
#
# 顺序刻意是「先签发成功，再安装配置」：证书拿不到时不动现有 Nginx，
# 避免 nginx -t 失败波及这台机上的其它 9 个站点。
set -euo pipefail

DOMAIN="${1:-}"
EMAIL="${2:-}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC="${HERE}/nginx/sg-reg-factory-https.conf"
# 与 sg-install-nginx.sh 使用同一个站点文件，避免留下两套 server block。
DEST="/etc/nginx/reg-factory.conf"
MAIN="/etc/nginx/nginx.conf"
STAMP="$(date +%Y%m%d-%H%M%S)"
WEBROOT="/var/www/letsencrypt"
PANEL="http://127.0.0.1:8800/api/health"

[ -n "${DOMAIN}" ] || { echo "用法: $0 <域名> [邮箱]" >&2; exit 2; }
[ "${EUID}" -eq 0 ] || { echo "需要 root（sudo）" >&2; exit 2; }
[ -f "${SRC}" ] || { echo "缺少站点模板 ${SRC}" >&2; exit 1; }
grep -q 'include /etc/nginx/reg-factory.conf;' "${MAIN}" || {
  echo "请先运行 sg-install-nginx.sh ${DOMAIN} 安装同一站点入口" >&2
  exit 1
}

echo "[1/5] 确认面板在本机可用"
curl -fsS --max-time 5 "${PANEL}" >/dev/null || { echo "面板未就绪：先 docker compose up -d" >&2; exit 1; }

echo "[2/5] 检查 DNS"
RESOLVED="$(getent ahostsv4 "${DOMAIN}" | awk '{print $1}' | head -1 || true)"
PUBLIC="$(curl -fsS --max-time 8 https://api.ipify.org || true)"
echo "     ${DOMAIN} -> ${RESOLVED:-未解析}；本机出口 ${PUBLIC}"
if [ "${RESOLVED}" != "${PUBLIC}" ]; then
  if command -v dig >/dev/null 2>&1; then
    echo "     提示：若记录在 Cloudflare 橙云后面，解析到的是 CF 边缘 IP，属正常。" >&2
  fi
fi

echo "[3/5] 申请证书（webroot ${WEBROOT}）"
mkdir -p "${WEBROOT}/.well-known/acme-challenge"
if [ -f "/etc/letsencrypt/live/${DOMAIN}/fullchain.pem" ]; then
  echo "     已存在证书，执行续期检查"
  certbot renew --cert-name "${DOMAIN}" --quiet
else
  CERTBOT_EMAIL=(--register-unsafely-without-email)
  if [ -n "${EMAIL}" ]; then CERTBOT_EMAIL=(--email "${EMAIL}"); fi
  certbot certonly --webroot -w "${WEBROOT}" -d "${DOMAIN}" \
    --non-interactive --agree-tos --keep-until-expiring \
    "${CERTBOT_EMAIL[@]}"
fi

echo "[4/5] 安装 Nginx 配置"
HAD_DEST=0
if [ -e "${DEST}" ]; then
  cp -a "${DEST}" "${DEST}.bak-rf-${STAMP}"
  HAD_DEST=1
fi
sed "s/reg\.yhoo\.lol/${DOMAIN}/g" "${SRC}" > "${DEST}"
chmod 644 "${DEST}"
if ! nginx -t; then
  echo "配置校验失败，恢复原站点配置" >&2
  if [ "${HAD_DEST}" = "1" ]; then
    cp -a "${DEST}.bak-rf-${STAMP}" "${DEST}"
  else
    rm -f "${DEST}"
  fi
  nginx -t || true
  exit 1
fi
systemctl reload nginx

echo "[5/5] 公网验证"
sleep 1
curl -fsS --max-time 10 "https://${DOMAIN}/api/health" >/dev/null
echo "     https://${DOMAIN}/api/health -> 200"
SESSION_CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "https://${DOMAIN}/api/session")"
echo "     未认证 /api/session -> ${SESSION_CODE}"
[ "${SESSION_CODE}" = "401" ] || { echo "API 鉴权检查失败" >&2; exit 1; }
echo
echo "完成。面板地址 https://${DOMAIN}/ ，账号密码在 ${HERE}/.env"
echo "证书续期沿用本机 certbot timer，无需额外配置。"
