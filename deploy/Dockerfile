# syntax=docker/dockerfile:1.7
#
# reg-factory 服务器镜像（linux/amd64 + linux/arm64，甲骨文 A1 免费 ARM 可用）
#
# 设计要点：
#   * 基础镜像 Debian bookworm slim，Playwright 官方支持，依赖库齐全
#   * Chromium 由 playwright install 提供，做成 /usr/local/bin/chromium 供项目
#     的 bundled 浏览器适配器发现（common/bundled_browser.py: find_browser_path）
#   * 同时装 Xvfb：有头 Chromium 在 Cloudflare / Arkose 面前明显更耐打
#   * 容器以非 root 用户 rf 跑浏览器；entrypoint 以 root 做目录 chown 后降权
#   * 数据全部落 /data（单卷即可备份/迁移）

ARG PYTHON_IMAGE=python:3.11-slim-bookworm
FROM ${PYTHON_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG INSTALL_CLOAK=0
ARG APT_MIRROR=""
ARG PIP_INDEX_URL_ARG=""

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    PLAYWRIGHT_SKIP_BROWSER_GC=1 \
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
        ca-certificates curl tini util-linux \
        xvfb x11-utils xauth \
        tzdata procps \
        fonts-liberation2 fonts-noto-color-emoji fonts-noto-cjk \
        libasound2 libatk-bridge2.0-0 libatk1.0-0 libatspi2.0-0 libcairo2 \
        libcups2 libdbus-1-3 libdrm2 libgbm1 libglib2.0-0 libnspr4 libnss3 \
        libpango-1.0-0 libx11-6 libxcb1 libxcomposite1 libxdamage1 libxext6 \
        libxfixes3 libxkbcommon0 libxrandr2; \
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
    python -m playwright install --with-deps chromium; \
    CHROME="$(python -c 'from playwright.sync_api import sync_playwright as s; p=s().start(); print(p.chromium.executable_path); p.stop()')"; \
    test -x "${CHROME}"; \
    ln -sf "${CHROME}" /usr/local/bin/chromium; \
    chmod -R a+rX /ms-playwright; \
    python -c 'import os; print("[build] arch:", os.uname().machine)'

# ---------------------------------------------------------------- 运行用户
RUN set -eux; \
    groupadd -g 10001 rf; \
    useradd -m -d /home/rf -u 10001 -g 10001 -s /bin/bash rf; \
    mkdir -p /data /app; \
    chown -R rf:rf /data

WORKDIR /app
COPY . /app

RUN set -eux; \
    # 从 Windows 工作区复制进来时统一去掉 CRLF，避免 /bin/bash^M 报错
    find /app/deploy -maxdepth 1 -type f \( -name '*.sh' -o -name '*.py' -o -name 'Caddyfile' \) \
        -exec sed -i 's/\r$//' {} + ; \
    chmod +x /app/deploy/entrypoint.sh /app/deploy/deploy-oci-sg.sh /app/deploy/smoke-test.sh 2>/dev/null || true; \
    chown -R rf:rf /app 2>/dev/null || true; \
    python -m compileall -q /app/deploy /app/webui /app/common >/dev/null || true

ENV REG_FACTORY_DATA_DIR=/data \
    REG_FACTORY_ENV_FILE=/data/.env \
    REG_FACTORY_HOST=0.0.0.0 \
    REG_FACTORY_PORT=8799 \
    REG_FACTORY_XVFB=1 \
    REG_FACTORY_BROWSER_HEADLESS=0 \
    REG_FACTORY_RUN_USER=rf

VOLUME ["/data"]
EXPOSE 8799

HEALTHCHECK --interval=30s --timeout=5s --start-period=40s --retries=3 \
    CMD curl -fsS "http://127.0.0.1:${REG_FACTORY_PORT}/__rf/health" >/dev/null || exit 1

ENTRYPOINT ["/usr/bin/tini", "--", "/app/deploy/entrypoint.sh"]
CMD ["python", "-m", "deploy.serve"]
