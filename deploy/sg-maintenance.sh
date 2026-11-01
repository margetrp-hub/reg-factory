#!/usr/bin/env bash
# SG 主机上的 reg-factory 日常维护（建议由 systemd timer 每日执行）
#
# 做四件事：
#   1. 清理浏览器 profile / 调试截图（默认保留 3 天）
#   2. 备份 /data 到 /opt/reg-factory/backups（默认保留 7 份）
#   3. 磁盘与容器健康检查，异常退出码非 0，便于接入告警
#   4. 打印一份当前占用摘要
#
# 用法：
#   sudo ./sg-maintenance.sh            # 正常维护
#   sudo ./sg-maintenance.sh --dry-run  # 只报告不删除
#   sudo ./sg-maintenance.sh --backup-only
set -uo pipefail

APP_DIR="${APP_DIR:-/opt/reg-factory}"
DATA_VOLUME="${DATA_VOLUME:-reg-factory_rf-data}"
BACKUP_DIR="${BACKUP_DIR:-/opt/reg-factory/backups}"
PROFILE_DAYS="${PROFILE_DAYS:-3}"
BACKUP_KEEP="${BACKUP_KEEP:-7}"
CONTAINER="${CONTAINER:-reg-factory}"

DRY=0
BACKUP_ONLY=0
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY=1 ;;
    --backup-only) BACKUP_ONLY=1 ;;
    -h|--help) sed -n '2,18p' "$0"; exit 0 ;;
  esac
done

log() { printf '[%s] %s\n' "$(date '+%F %T')" "$*"; }
DOCKER=(docker)

# ------------------------------------------------------------ 1. 清理浏览器资料
if [ "$BACKUP_ONLY" = "0" ]; then
  # 孤儿 Chromium：父任务被 SIGKILL 后浏览器会被 init 收养，常驻吃掉 600MB+
  ORPHANS="$("${DOCKER[@]}" exec "${CONTAINER}" sh -c \
    "ps -eo pid=,ppid=,comm= 2>/dev/null | awk '\$3 ~ /chrome|chromium/ && \$2 == 1 {print \$1}'" 2>/dev/null || true)"
  if [ -n "${ORPHANS}" ]; then
    COUNT="$(printf '%s\n' "${ORPHANS}" | wc -l)"
    if [ "$DRY" = "1" ]; then
      log "dry-run：发现 ${COUNT} 个孤儿浏览器进程（${ORPHANS//$'\n'/,}）"
    else
      log "清理 ${COUNT} 个孤儿浏览器进程"
      # shellcheck disable=SC2086
      "${DOCKER[@]}" exec "${CONTAINER}" sh -c "kill -TERM ${ORPHANS//$'\n'/ } 2>/dev/null || true"
    fi
  else
    log "无孤儿浏览器进程"
  fi

  log "清理 ${PROFILE_DAYS} 天前的 browser-profiles / screenshots"
  for sub in browser-profiles screenshots screenshots_github screenshots_outlook tri_register_logs; do
    if [ "$DRY" = "1" ]; then
      "${DOCKER[@]}" run --rm -v "${DATA_VOLUME}:/data" alpine \
        sh -c "find /data/${sub} -mindepth 1 -maxdepth 2 -mtime +${PROFILE_DAYS} 2>/dev/null | head -20" || true
    else
      "${DOCKER[@]}" run --rm -v "${DATA_VOLUME}:/data" alpine \
        sh -c "find /data/${sub} -mindepth 1 -maxdepth 2 -mtime +${PROFILE_DAYS} -exec rm -rf {} + 2>/dev/null || true" || true
    fi
  done
fi

# ------------------------------------------------------------ 2. 备份 /data
mkdir -p "${BACKUP_DIR}"
STAMP="$(date +%Y%m%d-%H%M)"
TARGET="${BACKUP_DIR}/rf-data-${STAMP}.tar.gz"
log "备份数据卷 -> ${TARGET}"
if [ "$DRY" = "1" ]; then
  log "dry-run：跳过备份"
else
  if "${DOCKER[@]}" run --rm -v "${DATA_VOLUME}:/data:ro" -v "${BACKUP_DIR}:/backup" alpine \
      tar czf "/backup/$(basename "${TARGET}")" --exclude 'browser-profiles/*' --exclude 'screenshots*/*' -C /data . ; then
    chmod 600 "${TARGET}" 2>/dev/null || true
    ln -sfn "$(basename "${TARGET}")" "${BACKUP_DIR}/latest.tar.gz"
    log "备份完成：$(du -h "${TARGET}" | awk '{print $1}')"
  else
    log "警告：备份失败"
  fi
  # 只保留最近 N 份
  mapfile -t olds < <(ls -1t "${BACKUP_DIR}"/rf-data-*.tar.gz 2>/dev/null | tail -n +$((BACKUP_KEEP + 1)))
  for old in "${olds[@]:-}"; do
    [ -n "${old}" ] && rm -f "${old}" && log "清理旧备份 $(basename "${old}")"
  done
fi

# ------------------------------------------------------------ 3. 健康与水位
FAIL=0
STATUS="$("${DOCKER[@]}" inspect --format '{{.State.Status}}/{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "${CONTAINER}" 2>/dev/null || echo missing)"
case "${STATUS}" in
  running/healthy) log "容器状态 ${STATUS}" ;;
  *) log "异常：容器状态 ${STATUS}"; FAIL=1 ;;
esac

DISK_USE="$(df --output=pcent / | tail -1 | tr -dc '0-9')"
MEM_AVAIL_MB="$(awk '/MemAvailable/ {print int($2/1024)}' /proc/meminfo)"
LOAD1="$(awk '{print $1}' /proc/loadavg)"
log "磁盘 ${DISK_USE}% / 可用内存 ${MEM_AVAIL_MB}MB / load1 ${LOAD1}"
[ "${DISK_USE:-0}" -ge 85 ] && { log "警告：磁盘使用率 ${DISK_USE}%"; FAIL=1; }
[ "${MEM_AVAIL_MB:-0}" -le 1500 ] && { log "警告：可用内存仅 ${MEM_AVAIL_MB}MB"; FAIL=1; }

# ------------------------------------------------------------ 4. 摘要
log "容器资源占用：$("${DOCKER[@]}" stats --no-stream --format '{{.Name}} {{.CPUPerc}} {{.MemUsage}}' "${CONTAINER}" 2>/dev/null | tail -1)"
log "数据卷大小：$("${DOCKER[@]}" system df -v 2>/dev/null | awk -v v="${DATA_VOLUME}" '$1==v {print $2" "$3}' | head -1)"
exit "${FAIL}"
