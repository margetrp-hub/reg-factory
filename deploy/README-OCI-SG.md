# OCI / SG 部署补充

当前构建、运行、API 和升级入口统一见 [README-GO.md](README-GO.md)。
本文件只保留 OCI / SG 的环境差异与资源约束，不再维护另一套面板部署步骤。
当前仓库使用 `deploy/Dockerfile` 构建一个 `regd + Python worker` 镜像，
Compose 只有 `panel` 服务，默认监听 `127.0.0.1:8800`，健康端点是 `/api/health`。
这些是仓库配置事实，不代表某台主机已经完成切换；上线前仍需核对现场容器、镜像与反代。

## OCI 新实例

`deploy-oci-sg.sh` 用于新实例的通用 bridge 部署：安装 Docker、生成部署配置、
构建并启动统一镜像，再执行健康检查和 `smoke-test.sh`。健康超时或冒烟失败会退出非零。
已有 SG 主机应使用主指南中的 `docker-compose.yml + docker-compose.sg.yml` 组合，
不要拿新实例初始化脚本覆盖现有部署。

默认宿主仅发布 `127.0.0.1:8800`。OCI 安全列表与主机防火墙需允许反代使用的 80/443；
面板通过 SSH 隧道或 HTTPS 反代访问。`RF_PUBLISH` 只包含宿主地址和端口，
例如 `127.0.0.1:8800`；容器目标端口由 Compose 固定为 8800。

镜像支持 `linux/amd64` 与 `linux/arm64`，运行依赖沿用 Playwright Chromium、
Xvfb 和 Python worker。浏览器任务需要足够内存，先按实际任务峰值设置并发。
容器不提供 BlueStacks / Appium / ADB 或本机指纹浏览器客户端。

## SG override

| 配置 | 当前仓库值 | 用途 |
|---|---|---|
| 网络 | `network_mode: host` | 访问仅监听宿主回环的现有服务 |
| 监听 | `REGD_ADDR=127.0.0.1:8800` | host 网络下限定回环入口 |
| 端口映射 | `ports: !reset []` | 清除基础 Compose 继承的发布端口 |
| 内存 | 上限 `8g`、reservation `5g`、swap 上限 `8g` | 限定浏览器任务资源 |
| CPU / PID | `3.0` / `4096` | 与其他业务共享宿主资源 |
| 持久化 | `reg-factory_rf-data:/data` | 同一份 SQLite、配置、资产和日志 |

节点身份、平台和密钥保留现有值；override 不生成或映射另一套密钥。
`deploy/.env` 经 `env_file` 注入进程，regd 再读取 `/data/.env`，已有进程环境值优先。

SG 的 HTTP 与 HTTPS 安装脚本都管理同一个 `/etc/nginx/reg-factory.conf`，
上游统一为 `127.0.0.1:8800`。`sg-install-nginx.sh` 安装 HTTP 入口，
`sg-enable-https.sh` 在证书就绪后替换同一站点；写入前保留原配置，`nginx -t` 失败时恢复。
SG 模板依赖宿主已有的 `la2-migrated/snippets`，通用 Nginx 主机请使用对应通用模板。
SSE 日志路径 `/api/logs/stream` 与 `/api/tasks/<id>/stream` 需要关闭反代缓冲。

`smoke-test.sh` 检查 regd 健康、静态登录页、匿名 API 拒绝、JSON 登录、Cookie 会话和登出，
并在容器可访问时检查 Chromium 和 Python 依赖。`sg-maintenance.sh` 的健康与内存查询也
使用 `/api/health`、`/api/memory`。数据备份仍包含敏感配置与账号资产，应按现有权限保存。

## 资源核对

浏览器容量按当前镜像和实际任务测量，使用 `/api/memory`、容器资源统计及任务日志记录峰值后确定并发。旧单页内存样本和清理数字不作为当前容量承诺，历史回执从Git及私有运维证据追溯。

`--single-process` / `--in-process-gpu` 曾导致容器浏览器启动失败，不作为默认优化参数。任务、日志和恢复仍由同一regd控制面持久管理，不能另启Python面板共用生产数据目录。当前运行及恢复材料见[验收](../docs/acceptance.md)。
