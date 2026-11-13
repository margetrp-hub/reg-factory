# syntax=docker/dockerfile:1.7
#
# reg-factory 统一镜像：regd 控制面 + 内嵌前端 + Python worker
# linux/amd64 + linux/arm64；从干净 checkout 一次构建，无需预先准备项目镜像。
#
# 设计要点：
#   * 基础镜像 Debian bookworm slim，Playwright 官方支持，依赖库齐全
#   * Chromium 由 playwright install 提供，做成 /usr/local/bin/chromium 供项目
#     的 bundled 浏览器适配器发现（common/bundled_browser.py: find_browser_path）
#   * 同时装 Xvfb：有头 Chromium 在 Cloudflare / Arkose 面前明显更耐打
#   * 容器以非 root 用户 rf 跑浏览器；entrypoint 以 root 做目录 chown 后降权
#   * 数据全部落 /data（单卷即可备份/迁移）

ARG PYTHON_IMAGE=python:3.11-slim-bookworm
ARG GO_IMAGE=golang:1.26-bookworm
ARG NODE_IMAGE=node:22-bookworm-slim

# ---------------------------------------------------------------- 前端
FROM ${NODE_IMAGE} AS web
WORKDIR /web
ENV NEXT_TELEMETRY_DISABLED=1 CI=1
RUN corepack enable && corepack prepare pnpm@10.18.2 --activate
COPY server/web/package.json server/web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile --config.minimumReleaseAge=0
COPY server/web/ ./
RUN pnpm build

# ---------------------------------------------------------------- Go
FROM ${GO_IMAGE} AS api
WORKDIR /src
ENV CGO_ENABLED=0 GOPROXY=https://goproxy.cn,direct
COPY server/go.mod server/go.sum ./
COPY shared/node-sdk/go /shared/node-sdk/go
RUN go mod download
COPY server/ ./
COPY --from=web /web/out ./webdist
RUN go build -trimpath -ldflags="-s -w" -o /out/regd ./cmd/regd

# ---------------------------------------------------------------- Python worker 运行时
FROM ${PYTHON_IMAGE} AS runtime

ARG DEBIAN_FRONTEND=noninteractive
ARG INSTALL_CLOAK=0
ARG APT_MIRROR=""
ARG PIP_INDEX_URL_ARG=""

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONPATH=/app/shared/node-sdk/python \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    PLAYWRIGHT_SKIP_BROWSER_GC=1 \
    XDG_CACHE_HOME=/opt/browser-cache \
    TZ=Asia/Singapore \
    LANG=C.UTF-8 \
    LC_ALL=C.UTF-8 \
    DEBIAN_FRONTEND=noninteractive

# ---------------------------------------------------------------- 系统依赖
RUN set -eux; \
    if [ -n "${APT_MIRROR}" ]; then \
        sed -i "s|deb.debian.org|${APT_MIRROR}|g; s|security.debian.org|${APT_MIRROR}|g" \
            /etc/apt/sources.list.d/debian.sources 2>/dev/null || true; \
    fi; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates curl tini util-linux libatomic1 \
        xvfb x11-utils xauth \
        tzdata procps \
        fonts-liberation2 fonts-noto-color-emoji fonts-noto-cjk \
        libasound2 libatk-bridge2.0-0 libatk1.0-0 libatspi2.0-0 libcairo2 \
        libcups2 libdbus-1-3 libdrm2 libgbm1 libglib2.0-0 libnspr4 libnss3 \
        libpango-1.0-0 libx11-6 libxcb1 libxcomposite1 libxdamage1 libxext6 \
        libxfixes3 libxkbcommon0 libxrandr2 libgtk-3-0 libdbus-glib-1-2; \
    rm -rf /var/lib/apt/lists/*

# ---------------------------------------------------------------- Python 依赖
COPY deploy/requirements-server.txt /tmp/requirements-server.txt
RUN set -eux; \
    if [ -n "${PIP_INDEX_URL_ARG}" ]; then export PIP_INDEX_URL="${PIP_INDEX_URL_ARG}"; fi; \
    python -m pip install --upgrade pip; \
    python -m pip install -r /tmp/requirements-server.txt; \
    if [ "${INSTALL_CLOAK}" = "1" ]; then \
        python -m pip install "cloakbrowser[geoip]>=0.4.10"; \
    fi

# Playwright 浏览器 + 系统库（--with-deps 会自动补齐发行版依赖）
RUN set -eux; \
    python -m patchright install --with-deps chromium; \
    python -m playwright install chromium; \
    python -m camoufox fetch; \
    CHROME="$(python -c 'from patchright.sync_api import sync_playwright as s; p=s().start(); print(p.chromium.executable_path); p.stop()')"; \
    test -x "${CHROME}"; \
    ln -sf "${CHROME}" /usr/local/bin/chromium; \
    chmod -R a+rX /ms-playwright; \
    python -c 'import os; print("[build] arch:", os.uname().machine)'

# ---------------------------------------------------------------- 运行用户
RUN set -eux; \
    groupadd -g 10001 rf; \
    useradd -m -d /home/rf -u 10001 -g 10001 -s /bin/bash rf; \
    mkdir -p /data /app; \
    chown -R rf:rf /data /opt/browser-cache

WORKDIR /app
# Source identity must not invalidate the large browser dependency layers.
ARG REVISION=unknown
LABEL org.opencontainers.image.revision=$REVISION
COPY . /app
RUN python shared/node-sdk/python/aishop_node/bundle.py shared/node-sdk
COPY --from=api /out/regd /usr/local/bin/regd

RUN set -eux; \
    # 从 Windows 工作区复制进来时统一去掉 CRLF，避免 /bin/bash^M 报错
    find /app/deploy -maxdepth 1 -type f \( -name '*.sh' -o -name '*.py' -o -name 'Caddyfile' \) \
        -exec sed -i 's/\r$//' {} + ; \
    chmod +x /app/deploy/entrypoint-go.sh /usr/local/bin/regd; \
    python /app/server/tools/export_schema.py; \
    chown -R rf:rf /app

ENV REGD_ADDR=0.0.0.0:8800 \
    REGD_APP_ROOT=/app \
    REGD_PYTHON=python3 \
    REG_FACTORY_DATA_DIR=/data \
    REG_FACTORY_ENV_FILE=/data/.env \
    REG_FACTORY_XVFB=1 \
    REG_FACTORY_BROWSER_HEADLESS=0 \
    REG_FACTORY_RUN_USER=rf

VOLUME ["/data"]
EXPOSE 8800

HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD curl -fsS "http://127.0.0.1:${REGD_ADDR##*:}/api/health" >/dev/null || exit 1

ENTRYPOINT ["/usr/bin/tini", "--", "/app/deploy/entrypoint-go.sh"]
