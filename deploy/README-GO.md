# reg-factory Go 控制面部署

SG独立系统由Go regd + 内嵌Next.js页面 + Python worker组成，默认宿主回环8800。生产、账号资料、邮箱池、日志与独立CLI可在没有Work配置时使用；节点签名接口仅负责Work接入。旧Python 8799面板已退役。实际提交、镜像与恢复位置只在[当前验收](../docs/acceptance.md)登记；本页提供操作流程。

| 目录 | 责任 |
| --- | --- |
| `server/cmd/regd`、`server/internal/api` | 进程装配与面板接口 |
| `server/internal/auth`、`settings` | 面板认证与配置 |
| `server/internal/store`、`runner`、`assetstore` | 本机PostgreSQL任务/日志、当前资料、Go凭据发布恢复及受控Python进程 |
| `server/internal/nodeserver` | Work签名接单、原结果与恢复 |
| `server/internal/centralasset` | Go中央资产执行宿主、旧工件读取、加密结果及有界补传 |
| `server/web`、`server/webdist` | Next.js源码及构建时嵌入的静态产物 |
| `schema/scripts.py`、`server/tools/export_schema.py` | 唯一任务/配置目录及生成工具 |

API、鉴权和请求重试统一见[接口说明](../docs/api.md)。Cookie/Basic/Bearer属于面板认证；Work使用节点签名，不能互换。历史日志与SSE同读持久事件，重连带原事件ID，终态补齐日志后结束。

## 构建与部署

### 本地构建

```bash
# 在仓库根目录执行
# 1) 前端
pnpm --dir server/web install --frozen-lockfile
pnpm --dir server/web build
# 2) 复制产物
mkdir -p server/webdist
cp -r server/web/out/. server/webdist/
# 3) 导 schema（Python 侧）
python server/tools/export_schema.py
# 4) 编译（前端已 embed）
(cd server && go build -o regd ./cmd/regd)
# 5) 运行
REGD_ADDR=127.0.0.1:8800 REG_FACTORY_DATA_DIR=/data \
REGD_APP_ROOT="$PWD" REGD_PYTHON=python3 \
REG_FACTORY_PANEL_PASSWORD='<强口令>' ./server/regd
```

### 容器部署

唯一构建入口是 `deploy/Dockerfile`。它从 Node、Go、Python 基础镜像分别构建前端、
regd 和 worker 运行时，不依赖本机已有的项目镜像。Python、Chromium、Xvfb 与原服务器
运行依赖保持同一套安装方式，前端静态资源内嵌在 regd 中，不需要独立前端服务。
构建会从 `schema/scripts.py` 重新导出任务定义；前端或 Go 编译失败会直接终止构建。

```bash
cd deploy
# 首次部署：复制后设置 REG_FACTORY_PANEL_PASSWORD
cp .env.server.example .env
# 建议 REG_FACTORY_IMAGE=reg-factory:unified
docker compose build panel
docker compose up -d --no-build panel
curl -fsS http://127.0.0.1:8800/api/health
```

默认只运行一个 `panel` 服务、一个 `reg-factory` 容器。Go 负责界面、状态和调度，
中央资产领取/回库和AT检查由Go执行，平台登录/RT等步骤按需启动镜像内的 Python worker；共用 `/data` 与 `/data/.env`。SG的 `REGD_DB`、`MAIL_DB` 使用所在主机的PostgreSQL DSN，沿用现役数据库和账号，不跨机写库。首次启动幂等建表，不依赖容器外手写兼容函数；测试工具仍可使用隔离SQLite。
启动入口仍负责数据目录初始化、资产 Key 首次生成、Xvfb 和降权，不改变业务或节点密钥协议。

镜像同时固定Patchright、Playwright、Camoufox和Ruyipage Python包。构建时安装Chromium及Firefox，Firefox缓存位于 `/opt/browser-cache`，可由非root运行用户访问。`BROWSER_ENGINE=patchright` 保留原浏览器供应商选择；`camoufox` 使用独立Firefox上下文。Ruyipage是不同API的工具入口，不能直接替换现有Playwright任务。自动巡检保持显式配置的关闭状态。

升级前保存现役容器、实际二进制摘要、Compose、配置摘要、PostgreSQL一致性备份与文件卷备份。当前SG曾在容器层替换程序，原镜像标签不能单独代表恢复程序；保留原容器供恢复。回退仅恢复兼容程序，不覆盖升级后的业务数据。切换前确认无执行中任务，切换后核对认证、账号/任务数量、旧任务SSE、邮箱池、新增字段及浏览器启动。

默认 bridge 网络中，容器监听 `0.0.0.0:8800`，宿主发布 `127.0.0.1:8800`。
`RF_PUBLISH` 只填宿主地址和端口（例如 `127.0.0.1:8800` 或 `0.0.0.0:8800`），
容器目标端口由 Compose 固定为 8800。健康检查统一使用 `/api/health`。

SG 使用同一个构建入口和服务，追加主机网络与资源限制：

```bash
# 在 deploy/.env 与 /data/.env 保留现有节点身份、平台与密钥
docker compose -f docker-compose.yml -f docker-compose.sg.yml config --quiet
docker compose -f docker-compose.yml -f docker-compose.sg.yml build panel
docker compose -f docker-compose.yml -f docker-compose.sg.yml up -d --no-build panel
curl -fsS http://127.0.0.1:8800/api/health
```

SG 的 `REGD_ADDR=127.0.0.1:8800` 只监听主机回环，Nginx 上游保持 8800。
override 用 `ports: !reset []` 移除基础配置的端口发布；普通 `ports: []` 会保留继承的映射。
使用支持 `!reset` 的 Docker Compose V2 或更新版本，旧版 `docker-compose` 不适用。

### 切换与恢复

1. 保留当前镜像和 `/data` 的一致性备份，确认没有需要继续处理的运行中任务；
2. 构建统一镜像，以同一个 `panel` 服务切换；不要再另起一套控制面；
3. 确认反代上游指向 8800，验证 `/api/health`、登录和真实任务日志，启用节点时再验证 `/ready` 与 `/node/capabilities`；已有 `.env` 若覆盖了 `RF_PUBLISH`，同时核对宿主实际端口；
4. 需要回退时使用已验证兼容的原镜像和原运行配置重建 `panel`，保留最新业务数据。数据一致性备份用于独立灾难恢复，不能在业务继续推进后用旧快照覆盖当前凭据、任务或领取事实。

## 运维

```bash
# 状态
curl -s http://127.0.0.1:8800/api/health
curl -s http://127.0.0.1:8800/api/memory

# 日志（容器）
sudo docker logs -f reg-factory --tail 100

# 主SG PostgreSQL只读查询（其他部署使用各自的数据库容器与库名）
sudo docker exec reg-pg psql -U postgres -d regfactory -X -c \
  'SELECT id,script,status,exit_code,created_at FROM tasks ORDER BY created_at DESC LIMIT 10'

# 备份：PostgreSQL一致性备份与/data文件卷必须一并保留
sudo ./sg-maintenance.sh
```

数据库表：

| 表 | 用途 |
|---|---|
| `tasks` | 任务状态机（queued/running/succeeded/failed/stopped/interrupted） |
| `events` | 每行日志（按 task_id 索引） |
| `claims` | 资产领取账本（kind + ref 唯一） |
| `audit` | 登录/任务/配置/资产操作审计 |

## 运行约束

* 中央资产执行宿主与AT检查为Go，平台执行器仍是Python，`/app`里的脚本需要持久目录及SDK导入路径（entrypoint已设）；
* `scripts.schema.json` 由 `schema/scripts.py` 生成；容器构建会自动重新导出，本地直接运行需手动导出；
* 单实例单进程（生产使用PostgreSQL持久化日志与任务状态），不要对同一数据目录启动多个控制面副本；
* 默认宿主端口和 SG 监听地址都只绑 `127.0.0.1`，公网入口走 Nginx/Caddy 反代；
* 不默认启用 `--single-process` / `--in-process-gpu`；SG资源限制及测量要求见[环境差异](README-OCI-SG.md)。

## 生产与凭据恢复

节点支持 `production.team_json` 的 `produce` 和 `existing_result`：前者按Core原授权生产，后者核验同一已保存结果，不重新登录或生产。前置配置、签名域与入口见[机器接口](../docs/api.md#work-机器接口)，Core继续拥有库存审批、订单和账务。

账号执行、修复、AT检查和常规下载解析最近成功保存的完整凭据；原来源引用继续可用。当前凭据、原任务不可变回执、未结案恢复工件是不同责任，不能宣称所有历史Token副本永久保留。清理和失败恢复遵守[账号当前凭据](../docs/architecture.md#账号当前凭据)，不删除仍有Work核对责任的结果。
