#!/usr/bin/env bash
# reg-factory 甲骨文云（OCI）新加坡实例一键部署
#
# 适用：Ubuntu 22.04/24.04、Oracle Linux 8/9（x86_64 或 A1 ARM64）
# 做四件事：装 Docker -> 放行本机防火墙 -> 生成面板密码 -> 构建并启动 -> 冒烟自检
#
# 用法：
#   scp -r reg-factory ubuntu@<实例IP>:~/ && ssh ubuntu@<实例IP>
#   cd ~/reg-factory/deploy && sudo ./deploy-oci-sg.sh
#   sudo ./deploy-oci-sg.sh --publish 8800        # 直接暴露 8800（无 HTTPS）
#
# 注意：OCI 还有一层「安全列表 / NSG」，必须在控制台放行 80/443（或 8800），
# 脚本会打印对应 ingress 规则，但不会替你改云上配置。
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${HERE}"

PUBLISH=""
ASSUME_YES=0
while [ $# -gt 0 ]; do
  case "$1" in
    --publish) PUBLISH="${2:?--publish 需要宿主端口或 IP:端口}"; shift 2 ;;
    --yes|-y) ASSUME_YES=1; shift ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) echo "未知参数: $1" >&2; exit 2 ;;
  esac
done

log()  { printf '\033[1;36m[deploy]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[warn]\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m[error]\033[0m %s\n' "$*" >&2; exit 1; }

as_root() {
  if [ "$(id -u)" = "0" ]; then "$@"; else sudo "$@"; fi
}

# ------------------------------------------------------------ 0. 基本检查
[ -f ./Dockerfile ] || fail "请在 reg-factory/deploy 目录内运行"
MEM_MB=$(awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo)
CPUS=$(nproc)
log "实例规格：${CPUS} vCPU / ${MEM_MB} MiB 内存 / $(uname -m)"
if [ "${MEM_MB}" -lt 3500 ]; then
  warn "内存 < 3.5GB：同时跑 1-2 个 Chromium 就会 OOM。建议 A1.Flex 2C8G 及以上，"
  warn "或先在 deploy/.env 与 /data/.env 里把 REG_FACTORY_MAX_CONCURRENCY 设为 1。"
fi

# ------------------------------------------------------------ 1. 装 Docker
if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
  log "Docker 已就绪：$(docker --version)"
else
  log "安装 Docker Engine + compose 插件"
  . /etc/os-release
  case "${ID}" in
    ubuntu|debian)
      as_root apt-get update -y
      as_root apt-get install -y ca-certificates curl gnupg git
      as_root install -m 0755 -d /etc/apt/keyrings
      if [ ! -f /etc/apt/keyrings/docker.asc ]; then
        curl -fsSL https://download.docker.com/linux/${ID}/gpg -o /tmp/docker.asc
        as_root install -m 0644 /tmp/docker.asc /etc/apt/keyrings/docker.asc
      fi
      echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/${ID} ${VERSION_CODENAME} stable" \
        | as_root tee /etc/apt/sources.list.d/docker.list >/dev/null
      as_root apt-get update -y
      as_root apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
      ;;
    ol|rhel|rocky|almalinux|centos)
      as_root dnf install -y dnf-utils git
      as_root dnf config-manager --add-repo https://download.docker.com/linux/centos/docker-ce.repo
      as_root dnf install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
      ;;
    *)
      fail "未识别的发行版 ${ID}，请手动安装 Docker 后重跑"
      ;;
  esac
  as_root systemctl enable --now docker
  log "Docker 安装完成：$(docker --version)"
fi

if ! docker info >/dev/null 2>&1; then
  warn "当前用户不能直接访问 docker，后续命令使用 sudo"
  DOCKER=(sudo docker)
else
  DOCKER=(docker)
fi

# ------------------------------------------------------------ 2. 本机防火墙
open_port() {
  local port="$1"
  if command -v firewall-cmd >/dev/null 2>&1 && systemctl is-active --quiet firewalld; then
    as_root firewall-cmd --permanent --add-port="${port}/tcp" >/dev/null || true
    as_root firewall-cmd --reload >/dev/null || true
    return
  fi
  if command -v iptables >/dev/null 2>&1; then
    if ! as_root iptables -C INPUT -p tcp --dport "${port}" -j ACCEPT 2>/dev/null; then
      # OCI 官方镜像在 INPUT 末尾有一条 REJECT，插入到它前面
      local reject_line
      reject_line=$(as_root iptables -L INPUT --line-numbers -n 2>/dev/null | awk '/REJECT/ {print $1; exit}')
      if [ -n "${reject_line}" ]; then
        as_root iptables -I INPUT "${reject_line}" -p tcp --dport "${port}" -j ACCEPT
      else
        as_root iptables -I INPUT 1 -p tcp --dport "${port}" -j ACCEPT
      fi
    fi
    if command -v netfilter-persistent >/dev/null 2>&1; then
      as_root netfilter-persistent save >/dev/null 2>&1 || true
    elif [ -d /etc/iptables ]; then
      as_root sh -c 'iptables-save > /etc/iptables/rules.v4' || true
    fi
  fi
}

log "放行本机防火墙 80/443"
open_port 80
open_port 443
if [ -n "${PUBLISH}" ]; then
  PUBLISH_PORT="${PUBLISH##*:}"
  log "额外放行 ${PUBLISH_PORT}"
  open_port "${PUBLISH_PORT}"
fi

# ------------------------------------------------------------ 3. 生成配置
if [ ! -f .env ]; then
  log "生成 deploy/.env 并创建随机面板密码"
  cp .env.server.example .env
  PASSWORD="$(openssl rand -base64 24 2>/dev/null || head -c 32 /dev/urandom | base64 | tr -d '\n')"
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$PASSWORD" <<'PY'
import pathlib, sys
path = pathlib.Path(".env")
text = path.read_text(encoding="utf-8")
text = text.replace("CHANGE_ME_TO_A_LONG_RANDOM_STRING", sys.argv[1])
path.write_text(text, encoding="utf-8")
PY
  else
    sed -i "s|CHANGE_ME_TO_A_LONG_RANDOM_STRING|${PASSWORD}|" .env
  fi
  chmod 600 .env
  log "面板密码：${PASSWORD}"
  log "（已写入 deploy/.env，稍后也会打印在部署摘要里）"
else
  log "deploy/.env 已存在，保留现有密码"
fi

if [ -n "${PUBLISH}" ]; then
  # RF_PUBLISH 只保存宿主地址和端口，容器 8800 由 compose 追加。
  case "${PUBLISH}" in
    *:*) PUBLISH_BINDING="${PUBLISH}" ;;
    *) PUBLISH_BINDING="0.0.0.0:${PUBLISH}" ;;
  esac
  if grep -q '^RF_PUBLISH=' .env; then
    sed -i "s|^RF_PUBLISH=.*|RF_PUBLISH=${PUBLISH_BINDING}|" .env
  else
    printf '\nRF_PUBLISH=%s\n' "${PUBLISH_BINDING}" >> .env
  fi
  log "端口发布改为 ${PUBLISH_BINDING} -> 8800"
fi

# ------------------------------------------------------------ 4. 构建 & 启动
log "构建镜像（首次需下载 Playwright Chromium，5-15 分钟）"
"${DOCKER[@]}" compose build panel

log "启动服务"
"${DOCKER[@]}" compose up -d --no-build panel

log "等待健康检查"
for i in $(seq 1 60); do
  status="$("${DOCKER[@]}" inspect --format '{{.State.Health.Status}}' reg-factory 2>/dev/null || echo starting)"
  [ "${status}" = "healthy" ] && break
  [ "${status}" = "unhealthy" ] && { "${DOCKER[@]}" compose logs --tail 80 panel; fail "容器不健康，见上方日志"; }
  sleep 3
done
if [ "${status}" != "healthy" ]; then
  "${DOCKER[@]}" compose logs --tail 80 panel
  fail "等待健康检查超时（最后状态 ${status}）"
fi

# ------------------------------------------------------------ 5. 冒烟
PUBLISHED="$("${DOCKER[@]}" compose port panel 8800 | head -1)"
case "${PUBLISHED}" in
  0.0.0.0:*) PUBLISHED="127.0.0.1:${PUBLISHED##*:}" ;;
esac
RF_BASE="${RF_BASE:-http://${PUBLISHED}}" bash ./smoke-test.sh \
  || fail "冒烟测试未全部通过，请检查 docker compose logs -f panel"

# ------------------------------------------------------------ 6. 摘要
PUBLIC_IP="$(curl -fsS --max-time 5 https://api.ipify.org 2>/dev/null || echo '<实例公网IP>')"
PUBLISH_LINE="$(grep -E '^RF_PUBLISH=' .env | head -1 || true)"
echo
echo "============================================================"
echo " reg-factory 部署完成"
echo " 实例 IP        : ${PUBLIC_IP}"
echo " 端口发布       : ${PUBLISH_LINE#RF_PUBLISH=}"
echo " 面板账号       : $(grep -E '^REG_FACTORY_PANEL_USER=' .env | cut -d= -f2)"
echo " 面板密码       : $(grep -E '^REG_FACTORY_PANEL_PASSWORD=' .env | cut -d= -f2)"
echo " 资产 API Key   : sudo docker exec reg-factory cat /data/runtime/asset-api-key.txt"
echo " 日志           : sudo docker compose -f ${HERE}/docker-compose.yml logs -f panel"
echo " 数据卷         : rf-data (/data)"
echo "------------------------------------------------------------"
echo " 还差一步：在 OCI 控制台 实例 -> 子网 -> 安全列表 放行 ingress"
echo "   TCP 80  / 0.0.0.0/0"
echo "   TCP 443 / 0.0.0.0/0"
echo " 然后配置 deploy/Caddyfile 域名，取消 docker-compose.yml 里 caddy 段注释，"
echo " docker compose up -d 即可获得 HTTPS。"
echo "============================================================"
