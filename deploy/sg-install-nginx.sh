#!/usr/bin/env bash
# 把 reg-factory 面板挂到 SG 现有 Nginx 上（阶段 1：仅 80 端口反代）
#
#   sudo ./sg-install-nginx.sh [域名]
#
# 行为：
#   1. 备份 /etc/nginx/nginx.conf 到 /etc/nginx/nginx.conf.bak-rf-<时间戳>
#   2. 安装 /etc/nginx/reg-factory.conf（域名可替换，默认 reg.yhoo.lol）
#   3. 在 nginx.conf 顶层 include（幂等，重复执行不会重复插入）
#   4. nginx -t 通过才 reload；失败自动回滚
#
# 证书升级：DNS 生效后执行 sudo ./sg-enable-https.sh <域名> <邮箱>
set -euo pipefail

DOMAIN="${1:-reg.yhoo.lol}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC="${HERE}/nginx/sg-reg-factory.conf"
DEST="/etc/nginx/reg-factory.conf"
MAIN="/etc/nginx/nginx.conf"
STAMP="$(date +%Y%m%d-%H%M%S)"
INCLUDE_LINE="    include /etc/nginx/reg-factory.conf;"

[ "${EUID}" -eq 0 ] || { echo "需要 root（sudo）" >&2; exit 2; }

echo "[1/5] 确认面板在线"
curl -fsS --max-time 5 http://127.0.0.1:8800/api/health >/dev/null || {
  echo "面板未就绪：cd /opt/reg-factory/deploy && sudo docker compose -f docker-compose.yml -f docker-compose.sg.yml up -d" >&2
  exit 1
}
[ -f "${SRC}" ] || { echo "缺少站点模板 ${SRC}" >&2; exit 1; }

echo "[2/5] 备份并安装站点配置（域名 ${DOMAIN}）"
cp -a "${MAIN}" "${MAIN}.bak-rf-${STAMP}"
HAD_DEST=0
if [ -e "${DEST}" ]; then
  cp -a "${DEST}" "${DEST}.bak-rf-${STAMP}"
  HAD_DEST=1
fi
sed "s/reg\.yhoo\.lol/${DOMAIN}/g" "${SRC}" > "${DEST}"
chmod 644 "${DEST}"

echo "[3/5] 写入 include（幂等）"
if grep -q "include /etc/nginx/reg-factory.conf;" "${MAIN}"; then
  echo "     已存在，跳过"
else
  # 插到 http { 块内第一条 include 之前，保持顺序稳定
  python3 - "${MAIN}" <<'PY'
import pathlib, sys, re
path = pathlib.Path(sys.argv[1])
text = path.read_text(encoding="utf-8")
line = "    include /etc/nginx/reg-factory.conf;\n"
if line not in text:
    match = re.search(r"^    include /etc/nginx/mime\.types;\n", text, re.M)
    if match:
        text = text[:match.start()] + line + text[match.start():]
    else:
        raise SystemExit("找不到插入点：请手工在 http{} 内加入 include")
    path.write_text(text, encoding="utf-8")
    print("     inserted before mime.types")
PY
fi

echo "[4/5] 校验配置"
if ! nginx -t; then
  echo "校验失败，回滚 nginx.conf" >&2
  cp -a "${MAIN}.bak-rf-${STAMP}" "${MAIN}"
  if [ "${HAD_DEST}" = "1" ]; then
    cp -a "${DEST}.bak-rf-${STAMP}" "${DEST}"
  else
    rm -f "${DEST}"
  fi
  nginx -t || true
  exit 1
fi

echo "[5/5] reload 并自测"
systemctl reload nginx
sleep 1
CODE_LOCAL="$(curl -s -o /dev/null -w '%{http_code}' --max-time 8 -H "Host: ${DOMAIN}" http://127.0.0.1/api/health)"
CODE_PUBLIC="$(curl -s -o /dev/null -w '%{http_code}' --max-time 8 "http://${DOMAIN}/api/health" || echo "unreachable")"
echo "     本地带 Host 访问: ${CODE_LOCAL}"
echo "     公网域名访问    : ${CODE_PUBLIC}（DNS 未指向本机时必然失败）"
[ "${CODE_LOCAL}" = "200" ] || { echo "本机反代健康检查失败" >&2; exit 1; }
echo
echo "回滚：cp -a ${MAIN}.bak-rf-${STAMP} ${MAIN} && nginx -t && systemctl reload nginx"
if [ "${HAD_DEST}" = "1" ]; then
  echo "站点配置备份：${DEST}.bak-rf-${STAMP}（回滚时一并恢复）"
fi
