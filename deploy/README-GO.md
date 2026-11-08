# reg-factory 统一控制面（Next.js + regd + Python worker）

把原来的 Python FastAPI 面板换成 **Go 控制面 + Next.js 前端**，执行层仍是仓库里
成熟的 Python worker（`register*.py` / `tools/*.py`）。理由很直接：

* 注册流程深度依赖 Playwright Python、`curl_cffi`、`tls-client`、以及几十个平台适配
  文件（60 万行），重写成 Go 的收益是负的；
* 真正拖慢运维的是**控制面**：单进程内存态任务、重启丢队列、13 万行原生 JS、
  没有持久化日志/审计、面板和业务耦合在一个进程里。

所以这次改造把边界划成：**Go 管状态，Python 管执行，Next.js 管界面。**

```
┌──────────────────────────┐
│ Next.js (static export)  │  ← 构建产物 go:embed 进二进制
│ 概览/任务/资产/配置/审计 │
└───────────┬──────────────┘
            │ fetch / SSE（同源 /api）
┌───────────▼──────────────┐
│ regd (Go 1.26 + Gin)     │
│ 鉴权 · 队列 · 并发 · 闸门 │
│ SQLite: tasks/events/    │
│         claims/audit     │
└───────────┬──────────────┘
            │ exec（独立进程组）
┌───────────▼──────────────┐
│ python3 -u /app/xxx.py   │  ← 原仓库脚本，零改动
│ Playwright + Chromium     │
└──────────────────────────┘
```

## 1. 目录

```
server/
├─ go.mod / go.sum
├─ cmd/regd/main.go          # 启动、路由装配、静态托管
├─ embed.go                  # go:embed webdist
├─ webdist/                  # 前端产物（构建时从 web/out 复制）
├─ internal/
│  ├─ auth/                  # Cookie/Basic/Bearer + 登录限流
│  ├─ settings/              # 环境变量 + .env 读写
│  ├─ store/                 # SQLite：tasks / events / claims / audit
│  ├─ runner/                # 队列、进程、日志流、内存闸门
│  └─ api/                   # Gin handlers
├─ tools/export_schema.py    # 把 schema/scripts.py 导成 scripts.schema.json
└─ web/                      # Next.js 15 App Router + Tailwind v4
   ├─ app/(panel)/…          # 概览、任务、资产、配置、审计
   ├─ app/login/             # 登录页
   ├─ components/            # Shell、TaskForm、LogView、UI 原子
   └─ lib/api.ts             # 类型化 API 客户端
```

## 2. 相对旧面板的变化

| 维度 | Python FastAPI 面板 | Go 控制面 |
|---|---|---|
| 任务状态 | 进程内 `RUNS` dict，重启即丢 | **SQLite 持久化**，重启把 running 标 interrupted |
| 队列 | 无，直接 spawn | **真正的队列**：queued → dispatcher → running，受并发与内存双闸门 |
| 日志 | 内存环形 5000 行 | SQLite events 同时提供全量历史与 SSE 实时日志 |
| 并发控制 | 无（只靠任务自身参数） | `REG_FACTORY_MAX_CONCURRENCY` + cgroup 内存水位（默认 85%） |
| 鉴权 | 无（靠绑回环） | Cookie 会话 / Basic / Bearer / X-API-Key，登录失败限流 |
| 审计 | 无 | 登录、建任务、停任务、改配置、领资产全部落库 |
| 资产 | 只读文件扫描 | 文件扫描 + **领取账本**（同一条不重复发放） |
| 前端 | 13 万行原生 JS | Next.js 15 + React 19 + Tailwind v4，7 个页面 |
| 部署 | uvicorn 单进程 | 单个 Go 二进制（前端内嵌）+ Python 运行时 |
| 内存 | Python 面板 ~90MB | Go 面板 **~7MB**（实测） |

执行层完全复用：`/api/scripts` 的 schema 由 `schema/scripts.py` 导出，
参数拼命令行等价于原 `_build_cmd`，任务仍在 `/data` 下读写。

## 3. API

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/health` | 免认证健康检查 |
| GET | `/api/memory` | cgroup 水位（免认证） |
| POST | `/api/login` · `/api/logout` · GET `/api/session` | 会话 |
| GET | `/api/system` | 版本、并发、内存、磁盘、任务计数 |
| GET | `/api/scripts` | 任务 schema（16 个任务） |
| POST | `/api/tasks` | 建任务 `{script, args}`；接受 `--flag` 或裸名 |
| GET | `/api/tasks` · `/api/tasks/{id}` | 列表/详情 |
| POST | `/api/tasks/{id}/stop` · `/api/tasks/stop-all` | 停止 |
| GET | `/api/logs` · `/api/logs/stream` | 全部日志：服务诊断与所有任务，历史 / SSE |
| GET | `/api/tasks/{id}/logs?tail=N` | 任务历史日志；`?download=1` 下载完整日志 |
| GET | `/api/tasks/{id}/stream` | SSE 日志，支持 `Last-Event-ID` 无重复补读；终态立即 `done` |
| GET | `/api/assets` · `/api/assets/{kind}` | 资产概览/列表 |
| POST | `/api/assets/{kind}/claim` | 领取一条（记账） |
| POST | `/api/assets/claims/reset?kind=` | 重置领取 |
| GET/PUT | `/api/config` · `/api/config/raw` | `.env` 读写（KEY 校验 + 敏感打码） |
| GET | `/api/audit` | 审计 |
| GET | `/api/proxy/check?proxy=&url=` | 代理出口探测 |

脚本调用示例：

```bash
curl -s -c jar -H 'content-type: application/json' \
  -d '{"username":"admin","password":"<面板密码>"}' http://127.0.0.1:8800/api/login

curl -s -b jar -H 'content-type: application/json' \
  -d '{"script":"register_claude","args":{"count":1,"protocol":"http"}}' \
  http://127.0.0.1:8800/api/tasks

curl -N -b jar http://127.0.0.1:8800/api/tasks/<id>/stream
```

历史查询与实时流都直接读取 SQLite `events`，没有另一份内存订阅日志。
断线后客户端带 `Last-Event-ID` 继续读取，服务重启后仍可从持久游标恢复；
已结束任务补齐日志后立即发送 `done`，不会等待新的内存事件。

## 4. 构建与部署

### 4.1 本地

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

### 4.2 容器（推荐）

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
按需启动镜像内的 Python worker；共用 `/data` 与 `/data/.env`，SQLite 使用 `/data/regd.db`。
启动入口仍负责数据目录初始化、资产 Key 首次生成、Xvfb 和降权，不改变业务或节点密钥协议。

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

### 4.3 切换

1. 保留当前镜像和 `/data` 的一致性备份，确认没有需要继续处理的运行中任务；
2. 构建统一镜像，以同一个 `panel` 服务切换；不要再另起一套控制面；
3. 确认反代上游指向 8800，验证 `/api/health`、登录和真实任务日志，启用节点时再验证 `/ready` 与 `/node/capabilities`；已有 `.env` 若覆盖了 `RF_PUBLISH`，同时核对宿主实际端口；
4. 需要回滚时恢复对应镜像与必要的数据备份，再使用相同网络配置重建 `panel` 服务。

## 5. 运维

```bash
# 状态
curl -s http://127.0.0.1:8800/api/health
curl -s http://127.0.0.1:8800/api/memory

# 日志（容器）
sudo docker logs -f reg-factory --tail 100

# 数据库（Python 运行时已内置 sqlite3）
sudo docker exec reg-factory python3 -c \
  'import sqlite3; print(sqlite3.connect("file:/data/regd.db?mode=ro", uri=True).execute("select id,script,status,exit_code,created_at from tasks order by created_at desc limit 10").fetchall())'

# 备份：/data 卷里含 regd.db、.env、号池、Cookie、Token
sudo ./sg-maintenance.sh
```

数据库表：

| 表 | 用途 |
|---|---|
| `tasks` | 任务状态机（queued/running/succeeded/failed/stopped/interrupted） |
| `events` | 每行日志（按 task_id 索引） |
| `claims` | 资产领取账本（kind + ref 唯一） |
| `audit` | 登录/任务/配置/资产操作审计 |

## 6. 已知边界

* 执行层仍是 Python，`/app` 里的脚本需要 `REG_FACTORY_DATA_DIR`、`PYTHONPATH=/app`（entrypoint 已设）；
* `scripts.schema.json` 由 `schema/scripts.py` 生成；容器构建会自动重新导出，本地直接运行需手动导出；
* 单实例单进程（SQLite 持久化日志与任务状态），不要对同一数据目录启动多个控制面副本；
* 默认宿主端口和 SG 监听地址都只绑 `127.0.0.1`，公网入口走 Nginx/Caddy 反代；
* `--single-process` 等 Chromium 参数在容器里会崩，历史内存测量与 SG 限制见 `README-OCI-SG.md`。
# TEAM JSON 接收、结果版本与执行证据

Work 可直接向本节点派发 `production.team_json`，无需经过另一个 Account 服务。
本版只声明 `existing_result` 模式：读取已有成品、冻结来源摘要、保留加密原文件、
只读核验并签名回传；不自动刷新、重新登录或重复生产。`produce` 执行器尚未接入。
未知核验结果由 Work 查询同一任务；明确拒绝保留原因，取消不删除已保存结果。
Core 承担商品验收、库存和订单账务，本节点不直接修改这些业务事实。

“资产 → 结果版本与导出”按邮箱或引用搜索完整保留结果，再选择摘要导出。
每次生产/授权结果先保存到 `asset-results/`，再更新当前文件；刷新还保留
`oauth-results/` 原响应和写回清单。当前文件写入失败时仍能搜索和导出保留结果。
不会从不同会话拼接 AT/RT，歧义必须明确选择；旧目录扫描不覆盖已存在的新结果。
这些目录属于私有数据卷，不能提交 Git 或打入镜像。

“运行”显示持久化步骤、时间、耗时、错误码和原任务处理提示，可筛选、搜索和下载。
SSE 断线按事件 ID 补读，未知状态保持连接；日志在写入和历史读取时脱敏。
普通脚本执行器不会领取、停止或在重启时重跑生产节点任务。
