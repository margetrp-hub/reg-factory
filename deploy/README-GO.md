# reg-factory Go 控制面（Next.js + Gin + SQLite）

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
| 日志 | 内存环形 5000 行 | 全量落库 + SSE 实时 + 历史 tail |
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
| GET | `/api/tasks/{id}/logs?tail=N` | 历史日志 |
| GET | `/api/tasks/{id}/stream` | SSE 实时日志（`event: log/done`） |
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

## 4. 构建与部署

### 4.1 本地

```bash
# 1) 前端
cd server/web && pnpm install && pnpm build
# 2) 复制产物
cp -r out/* ../webdist/
# 3) 导 schema（Python 侧）
python server/tools/export_schema.py
# 4) 编译（前端已 embed）
cd server && go build -o regd ./cmd/regd
# 5) 运行
REGD_ADDR=127.0.0.1:8800 REG_FACTORY_DATA_DIR=/data \
REGD_APP_ROOT=<repo> REGD_PYTHON=python3 \
REG_FACTORY_PANEL_PASSWORD=<强口令> ./regd
```

### 4.2 容器（推荐）

镜像基于已有的 `reg-factory:server`（Python + Chromium + Xvfb），只加一个 25MB 的
Go 二进制，所以几乎不增加体积。

```bash
cd deploy
# 构建并启动统一控制面（Go + 内置 Python worker）
sudo docker compose -f docker-compose.yml -f docker-compose.sg.yml up -d --build panel
```

生产只运行一个 `reg-factory` 控制面容器：Go 负责状态、界面和调度，镜像内复用
Python worker；共用 `/data` 与 `/data/.env`，SQLite 使用 `/data/regd.db`。

### 4.3 切换

1. 用 SSH 隧道或临时端口验证 8800 一切正常；
2. Nginx 把 `reg.yhoo.lol` 的 upstream 从 `127.0.0.1:8799` 改成 `127.0.0.1:8800`；
3. 确认 `/api/health`、`/ready` 和 `/node/capabilities` 后继续使用同一容器；
4. 需要回滚：恢复旧镜像标签和源码备份，再执行同一个 `panel` 服务启动命令。

## 5. 运维

```bash
# 状态
curl -s http://127.0.0.1:8800/api/health
curl -s http://127.0.0.1:8800/api/memory

# 日志（容器）
sudo docker logs -f reg-factory --tail 100

# 数据库（SQLite，可随时拷走）
sudo docker exec reg-factory sqlite3 /data/regd.db \
  "select id,script,status,exit_code,created_at from tasks order by created_at desc limit 10;"

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
* `scripts.schema.json` 由 `schema/scripts.py` 生成，新增任务需要重新导出并重跑构建；
* 单实例单进程（SQLite + 内存订阅表），不要开多副本；需要多机请换 Postgres + Redis Stream；
* 面板只绑 `127.0.0.1`，公网入口必须走 Nginx/Caddy 反代（与旧面板一致）；
* `--single-process` 等 Chromium 参数在容器里会崩，内存优化见 `README-OCI-SG.md` 第 10 节。
