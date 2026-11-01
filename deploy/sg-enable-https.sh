#!/usr/bin/env bash
# 给 reg-factory 面板签发证书并挂上 Nginx 反代（SG 主机，复用现有 certbot webroot）
#
#   sudo ./sg-enable-https.sh reg.ohlao.cfd admin@ohlao.cfd
#
# 顺序刻意是「先签发成功，再安装配置」：证书拿不到时不动现有 Nginx，
# 避免 nginx -t 失败波及这台机上的其它 9 个站点。
set -euo pipefail

DOMAIN="${1:-}"
EMAIL="${2:-}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC="${HERE}/nginx/reg-factory.conf"
DEST="/etc/nginx/sites-available/reg-factory.conf"
LINK="/etc/nginx/sites-enabled/reg-factory.conf"
WEBROOT="/var/www/letsencrypt"
PANEL="http://127.0.0.1:8799/__rf/health"

[ -n "${DOMAIN}" ] || { echo "用法: $0 <域名> [邮箱]" >&2; exit 2; }
[ "${EUID}" -eq 0 ] || { echo "需要 root（sudo）" >&2; exit 2; }

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
  certbot renew --cert-name "${DOMAIN}" --quiet || true
else
  certbot certonly --webroot -w "${WEBROOT}" -d "${DOMAIN}" \
    --non-interactive --agree-tos --keep-until-expiring \
    ${EMAIL:+--email "${EMAIL}"} ${EMAIL:---register-unsafely-without-email}
fi

echo "[4/5] 安装 Nginx 配置"
sed "s/reg\.ohlao\.cfd/${DOMAIN}/g" "${SRC}" > "${DEST}"
ln -sfn "${DEST}" "${LINK}"
if ! nginx -t; then
  echo "配置校验失败，已回滚本次 include" >&2
  rm -f "${LINK}" "${DEST}"
  nginx -t || true
  exit 1
fi
systemctl reload nginx

echo "[5/5] 公网验证"
sleep 1
echo "     https://${DOMAIN}/__rf/health -> $(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "https://${DOMAIN}/__rf/health")"
echo "     未认证访问首页应 302 到登录页：$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' --max-time 10 "https://${DOMAIN}/")"
echo
echo "完成。面板地址 https://${DOMAIN}/ ，账号密码在 ${HERE}/.env"
echo "证书续期沿用本机 certbot timer，无需额外配置。"
