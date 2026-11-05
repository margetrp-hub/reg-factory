# syntax=docker/dockerfile:1.7
#
# reg-factory Go 控制面镜像（arm64 / amd64）
#
#   阶段 1  web : Next.js 静态导出
#   阶段 2  api  : Go 编译，把 webdist 用 go:embed 打进单二进制
#   阶段 3  final: 复用 reg-factory:server 的 Python + Chromium + Xvfb 运行时
#
# 构建前先有基础镜像（仓库 deploy/Dockerfile 构建出的 reg-factory:server），
# 否则先跑：docker compose build panel
#
#   docker build -f deploy/Dockerfile.go -t reg-factory:go .
#
ARG BASE_IMAGE=reg-factory:server
ARG GO_IMAGE=golang:1.26-bookworm
ARG NODE_IMAGE=node:22-bookworm-slim

# ---------------------------------------------------------------- 前端
FROM ${NODE_IMAGE} AS web
WORKDIR /web
ENV NEXT_TELEMETRY_DISABLED=1 CI=1
RUN corepack enable && corepack prepare pnpm@10.18.2 --activate
COPY server/web/package.json server/web/pnpm-lock.yaml* ./
# 新版 pnpm 的 minimumReleaseAge 策略会拒绝刚发布的依赖（如 tslib），
# 这里显式关闭，保证锁文件可用、构建可复现。
RUN pnpm install --frozen-lockfile --config.minimumReleaseAge=0 \
 || pnpm install --config.minimumReleaseAge=0
COPY server/web/ ./
RUN pnpm build

# ---------------------------------------------------------------- Go
FROM ${GO_IMAGE} AS api
WORKDIR /src
ENV CGO_ENABLED=0 GOPROXY=https://goproxy.cn,direct
COPY server/go.mod server/go.sum* ./
RUN go mod download
COPY server/ ./
COPY --from=web /web/out ./webdist
RUN go build -trimpath -ldflags="-s -w" -o /out/regd ./cmd/regd \
 && /out/regd --version 2>/dev/null || true

# ---------------------------------------------------------------- 运行时
FROM ${BASE_IMAGE}

# regd 二进制指向 built 产物
COPY --from=api /out/regd /usr/local/bin/regd
# 业务代码必须与源码一致：base 镜像是旧快照，新增的 Python 脚本（如 tools/asset_ops.py）
# 不会自动进镜像，这里用当前构建上下文覆盖 /app（.dockerignore 已排除运行数据）
COPY . /app
# 退役目录清理：COPY 只覆盖不删除，base 镜像里残留的旧面板要显式移除
RUN rm -rf /app/webui /app/scripts
COPY deploy/entrypoint-go.sh /app/deploy/entrypoint-go.sh
COPY deploy/scripts.schema.json /app/deploy/scripts.schema.json

RUN set -eux; \
    sed -i 's/\r$//' /app/deploy/entrypoint-go.sh; \
    chmod +x /app/deploy/entrypoint-go.sh /usr/local/bin/regd; \
    chown -R rf:rf /app 2>/dev/null || true

ENV REGD_ADDR=127.0.0.1:8800 \
    REGD_APP_ROOT=/app \
    REGD_DB=/data/regd.db \
    REG_FACTORY_DATA_DIR=/data \
    REG_FACTORY_ENV_FILE=/data/.env \
    REG_FACTORY_XVFB=1 \
    REG_FACTORY_BROWSER_HEADLESS=0 \
    REG_FACTORY_RUN_USER=rf

VOLUME ["/data"]
EXPOSE 8800

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD curl -fsS "http://${REGD_ADDR}/api/health" >/dev/null || exit 1

ENTRYPOINT ["/usr/bin/tini", "--", "/app/deploy/entrypoint-go.sh"]
