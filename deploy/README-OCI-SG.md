# reg-factory 服务器版部署手册（甲骨文云 OCI · 新加坡）

把原仓库（桌面/Windows 便携版）改造成 **Linux 服务器常驻服务**：容器化、内置浏览器、
面板登录鉴权、数据单卷持久化、HTTPS 反代就绪。

---

## 1. 改了哪些地方

原项目是“本机控制台”：`start.sh` 只绑 `127.0.0.1`，靠本机 BitBrowser / Clash Verge，
Windows 侧还有 PowerShell、便携 EXE、BlueStacks 等假设。服务器版做了这些事：

| 项 | 原状态 | 服务器版 |
|---|---|---|
| 监听 | `127.0.0.1:8799` | 容器内 `0.0.0.0:8799`，宿主默认只绑 `127.0.0.1`，公网走反代 |
| 鉴权 | 无（靠本机回环保护） | `deploy/gate.py`：登录页 + Cookie 会话 + Basic + Bearer/X-API-Key，按 IP 限流 |
| 浏览器 | 默认 BitBrowser 本地 API | 默认 `bundled`：镜像内置 Playwright Chromium，Xvfb 有头运行 |
| 网络出口 | Clash Verge（`127.0.0.1:7897/9097`） | 默认 `PROXY_MODE=residential`，填住宅代理池即可，无 Clash 依赖 |
| 依赖 | 含 Appium / Selenium / BlueStacks | `deploy/requirements-server.txt` 精简，去掉安卓与桌面模块 |
| 数据 | 仓库目录 + `%LOCALAPPDATA%` | 全部落 `/data` 单卷（docker volume `rf-data`） |
| 进程 | `start.bat` / EXE | `tini` + entrypoint 降权到 `rf`，healthcheck 探活 |
| 资产 API | 未配 key 时仅本机 | 首次启动自动生成 `REG_FACTORY_ASSET_API_KEY`，写到 `/data/runtime/asset-api-key.txt` |

代码级改动只有一处（保持桌面行为不变）：
`common/bundled_browser_helper.py` 支持 `REG_FACTORY_BROWSER_HEADLESS` 与
`REG_FACTORY_BROWSER_ARGS`（容器里需要 `--no-sandbox`，否则非 root 起 Chromium 会失败）。

新增文件全部在 `deploy/`，不动原有业务逻辑。

---

## 2. 实例规格（重要）

| 场景 | 建议 |
|---|---|
| 面板看数据 / 1 并发 | A1.Flex **1C6G** 或 E2.1.Micro（1G）**不推荐** |
| 常规注册（2-3 并发 Chromium） | A1.Flex **2C8G** 起步 |
| 6-8 并发 | A1.Flex **4C24G** |

每个 Chromium 进程实测 300-700MB（有头 + 页面资源），外加面板 Python 进程 ~300MB。
`E2.1.Micro`（1GB）会在第一个浏览器启动时 OOM，`dmesg` 里能看到 oom-killer。
OCI 免费额度里 **A1.Flex 4C24G** 是最合适的选择（ARM64，镜像支持）。

架构：`linux/amd64` 与 `linux/arm64` 都支持（A1 是 ARM，Playwright 官方提供 arm64 Chromium）。

---

## 3. 部署（三步）

### 3.1 准备实例

- 镜像：Ubuntu 22.04 / 24.04（推荐）或 Oracle Linux 8/9
- 公网 IP：Reserved public IP 建议绑定，避免重启换 IP
- 安全列表（Security List / NSG）放行 ingress：
  - `TCP 80` ← `0.0.0.0/0`
  - `TCP 443` ← `0.0.0.0/0`
  - 想直接开 8799 调试：`TCP 8799` ← 你的出口 IP（别开 0.0.0.0/0）
- 出站：默认全通即可（注册流程要访问 outlook/claude/openai 等）

### 3.2 上传并一键部署

```bash
# 本机
scp -i <ssh-key> -r reg-factory ubuntu@<实例IP>:~/

# 服务器
ssh -i <ssh-key> ubuntu@<实例IP>
cd ~/reg-factory/deploy
chmod +x *.sh
sudo ./deploy-oci-sg.sh
```

脚本会：装 Docker + compose 插件 → 放行本机 iptables/firewalld 的 80/443 →
生成 `deploy/.env`（随机面板密码）→ 构建镜像 → 启动 → 健康检查 → 冒烟测试 → 打印摘要。

首次构建 5-15 分钟（下载 Playwright Chromium 约 150MB）。

### 3.3 访问

默认 `RF_PUBLISH=127.0.0.1:8799`，即只有服务器本机能访问。两种打开方式：

```bash
# A. 临时隧道（推荐先这样验证）
ssh -i <ssh-key> -N -L 8799:127.0.0.1:8799 ubuntu@<实例IP>
# 浏览器打开 http://127.0.0.1:8799，用 deploy/.env 里的账号密码登录
```

```bash
# B. 直接公网 HTTP（仅测试用）
sudo sed -i 's/^RF_PUBLISH=.*/RF_PUBLISH=8799:8799/' deploy/.env
sudo docker compose up -d
# 打开 http://<实例IP>:8799
```

### 3.4 上 HTTPS（正式用）

1. 域名 A 记录指向实例公网 IP
2. 编辑 `deploy/Caddyfile`，把 `panel.example.com` 换成你的域名、`you@example.com` 换成邮箱
3. 取消 `deploy/docker-compose.yml` 里 `caddy:` 段与 `caddy-data/caddy-config` 卷的注释
4. 确认 `.env` 里 `RF_PUBLISH=127.0.0.1:8799`（不要再暴露 8799）
5. `sudo docker compose up -d`

Caddy 自动签发 Let's Encrypt 证书。`/api/logs/<run_id>` 是 SSE，Caddyfile 已经
`flush_interval -1`，Nginx 等价配置要加 `proxy_buffering off;`。

### 3.5 不想在服务器上构建（可选）

甲骨文小规格实例构建时会吃 ~2GB 内存，且从 Docker Hub 拉包慢。可以在本地/CI
构建好再推上去：

```bash
# 本机（架构必须和服务器一致：A1 是 arm64，E2 是 amd64）
docker buildx build --platform linux/arm64 -f deploy/Dockerfile -t reg-factory:server .
docker save reg-factory:server | gzip -9 > reg-factory-server-arm64.tgz

scp reg-factory-server-arm64.tgz ubuntu@<实例IP>:~/
ssh ubuntu@<实例IP>
gunzip -c reg-factory-server-arm64.tgz | docker load
cd ~/reg-factory/deploy && sudo ./deploy-oci-sg.sh        # 会命中已有镜像层，只做启动
```

`deploy-oci-sg.sh` 里的 `docker compose build` 对已存在的同 tag 镜像会重新校验
Dockerfile 缓存，通常几秒完成。

---

## 4. 首次使用配置

登录面板后去 **环境配置 / 网络出口**，按服务器场景设置（对应 `/data/.env`）：

1. **网络出口**：`PROXY_MODE=residential`，`REG_FACTORY_PROXY_POOL` 填住宅代理
   （`http://user:pass@host:port` 逗号分隔）。没有住宅代理时，账号注册会被目标站
   按数据中心 IP 拦，新加坡机房 IP 直接在 Cloudflare 判定里就是高风险。
2. **并发**：`REG_FACTORY_MAX_CONCURRENCY` 从 2-3 开始；每个并发占一个浏览器进程。
3. **浏览器**：保持 `FINGERPRINT_BROWSER=bundled`（镜像内置 Chromium）。
   想换 Cloak 指纹内核：`INSTALL_CLOAK=1` 重新构建 + 填 `CLOAK_LICENSE_KEY`，
   再把 `FINGERPRINT_BROWSER` 设成 `cloak`。
4. **邮箱/接码**：按你实际的服务填 `.env`（YYDS、iCloud、sms-man 等）。
5. **资产 API**：给下游程序用，必须带 key：
   ```bash
   KEY=$(sudo docker exec reg-factory cat /data/runtime/asset-api-key.txt)
   curl -H "X-API-Key: $KEY" "http://127.0.0.1:8799/api/assets/emails?format=json"
   ```

`K12_AUTO_START=0` 是刻意的：K12 面板需要 `codex_k12` 的 Node 构建产物，服务器版
默认不构建。需要时在容器里 `cd codex_k12 && npm install && npm run build`（或自己
做一个带 Node 的镜像层），再把 `K12_AUTO_START` 设回 1。

---

## 5. 运维

```bash
cd ~/reg-factory/deploy

docker compose ps
docker compose logs -f --tail 200 panel
docker exec -it reg-factory bash            # 进容器
docker exec reg-factory ls /data            # 看数据卷

# 冒烟测试（鉴权、面板接口、Chromium、资产 key）
bash smoke-test.sh

# 备份（只备 /data，含 .env、号池、Cookie、Token、日志）
docker run --rm -v rf-data:/data -v "$PWD":/backup alpine \
  tar czf /backup/rf-data-$(date +%F).tgz -C /data .

# 升级到上游新版本
cd ~/reg-factory && git pull
cd deploy && docker compose build && docker compose up -d
```

数据卷布局：

```
/data/.env                    # 业务配置（面板可编辑）
/data/runtime/asset-api-key.txt
/data/runtime/logs/           # 面板日志
/data/cookies/ tokens/        # 资产
/data/browser-profiles/       # bundled 浏览器的 profile
/data/emails.txt outlook_accounts/ _outlook_pool/
```

面板密码在 `deploy/.env`（不进 `/data`，不会被面板的“环境配置”页读到）。
改了密码：`sudo docker compose up -d` 重启即可，旧会话全部失效。

---

## 6. 排错

| 现象 | 原因 / 处理 |
|---|---|
| 容器 `unhealthy`，日志 `缺少 REG_FACTORY_PANEL_PASSWORD` | `deploy/.env` 没写密码，补上后 `docker compose up -d` |
| 打开页面 502 | 容器没起来：`docker compose logs panel`；或 Caddy 反代端口不是 8799 |
| 浏览器任务报 `Chrome/Chromium is unavailable` | `REG_FACTORY_BROWSER_PATH` 被改动；默认 `/usr/local/bin/chromium`，`docker exec reg-factory ls -l /usr/local/bin/chromium` |
| 任务报 `bundled browser exited before CDP ready` | 内存不足或 `/dev/shm` 太小；确认 compose 的 `shm_size: 1gb` 和实例内存 |
| 浏览器起来但页面空白/超时 | 出口代理不通；面板“网络出口 → 测试”看真实出口 IP |
| Cloudflare 无限验证 | 机房 IP + 无头被识别：换成住宅代理、保持 `REG_FACTORY_BROWSER_HEADLESS=0`（Xvfb 有头） |
| 日志页不刷新 | 反代缓冲了 SSE：Caddy `flush_interval -1`、Nginx `proxy_buffering off` |
| 资产 API 403 | 请求没带 `X-API-Key`，或 key 与 `/data/runtime/asset-api-key.txt` 不一致 |
| 时间显示不对 | compose 已设 `TZ=Asia/Singapore`，容器内 `date` 核对 |
| OOM | `dmesg -T | grep -i oom`；降并发或升规格 |

---

## 7. 安全清单

- [ ] `REG_FACTORY_PANEL_PASSWORD` ≥ 16 位随机（脚本已自动生成）
- [ ] 公网只开 80/443；8799 只绑 127.0.0.1
- [ ] 已配置 HTTPS（Cookie 在 HTTPS 下自动带 `Secure`）
- [ ] 资产 API Key 已生成，只发给可信下游
- [ ] `/data` 备份加密存放（里面有 Cookie、Token、账号密码）
- [ ] 不用 `REG_FACTORY_ALLOW_ANONYMOUS=1` 对公网
- [ ] 定期 `docker compose pull/build` 更新基础镜像

---

## 8. 已知边界

以下模块依赖桌面/本地环境，容器里不工作（不影响主流程）：

- `gmail_android/`：需要 BlueStacks + Appium + ADB，服务器无安卓模拟器
- `FINGERPRINT_BROWSER=bitbrowser/adspower/roxy`：这些是“连本机客户端 API”，
  容器里没有对应进程；服务器请用 `bundled` 或 `cloak`
- `CLASH_*`：容器里没有 Clash Verge，`PROXY_MODE` 请用 `residential`
- `codex_k12/`：需要 Node 构建产物，默认不启动
- 面板“一键更新”按钮：服务器版请用 `git pull + docker compose build` 升级，
  容器内自更新会丢改动
- Windows PowerShell 相关（残留任务清理、便携包启动）在 Linux 上自动跳过

---

## 9. SG 生产机落地（140.245.54.144 / sgp / aarch64）

已按主机现状部署在 `/opt/reg-factory`，容器 `reg-factory`，面板只绑 `127.0.0.1:8799`。
完整回执与截图证据见 `lab/ops/reg-factory-sg-20261031/`。

### 9.1 与通用部署的差异（`docker-compose.sg.yml`）

| 项 | 值 | 原因 |
|---|---|---|
| 网络 | `network_mode: host` | 现役 sub2api(18082)/cpa(18317)/new-api(3008)/grok2api(18081) 只监听宿主回环，bridge + host.docker.internal 实测不通 |
| 面板绑定 | `REG_FACTORY_HOST=127.0.0.1` | host 网络下没有 Docker 端口隔离，必须显式收口 |
| 内存 | `8g`（无 swap） | 主机 4C/24G 与 33 个现役容器共用；单 Chromium 峰值 0.5-1G |
| CPU | `2.5` | 给现役业务留 1.5 核 |
| pids | `4096` | Chromium 多进程需要 |

启动命令：

```bash
cd /opt/reg-factory/deploy
sudo docker compose -f docker-compose.yml -f docker-compose.sg.yml up -d
```

### 9.2 SG 出站实测（2026-10-31）

| 目标 | curl | 有头 Chromium |
|---|---|---|
| claude.ai | 403 | 200 登录页完整渲染（hCaptcha invisible） |
| chatgpt.com | 403 | 200 首页完整渲染 |
| grok.com | 403 | 403 Cloudflare Turnstile，未通过 |
| login.live.com | 200 | 200 |

即：curl 的 403 是 TLS 指纹层，有头 Chromium 能过；Grok 需要 Turnstile。
上任务前先跑 `./proxy-check.sh` 判断当前出口是否够用。

### 9.3 访问

```bash
# 隧道（立即可用）
ssh -i <key> -N -L 8799:127.0.0.1:8799 ubuntu@140.245.54.144
# → http://127.0.0.1:8799

# 公网（先加 DNS：reg.ohlao.cfd A 140.245.54.144）
sudo ./sg-install-nginx.sh reg.ohlao.cfd   # 阶段 1：仅 80 反代
sudo ./sg-enable-https.sh  reg.ohlao.cfd admin@ohlao.cfd   # 阶段 2：签发证书切 443
```

两个脚本都会先备份 `nginx.conf`、`nginx -t` 校验、失败自动回滚，不影响同机其它站点。

### 9.4 维护

```bash
sudo ./sg-maintenance.sh --dry-run   # 只看不动
sudo ./sg-maintenance.sh             # 清理 3 天前 profile + 备份 /data（保留 7 份）
systemctl list-timers reg-factory-maintenance.timer   # 每日 12:32 SG 自动执行
```

### 9.5 对接现役服务

`/data/.env` 已预置（host 网络下 127.0.0.1 即宿主回环，已实测可达）：

```text
SUB2API_URL=http://127.0.0.1:18082   # sub2api-ol-test
SUB2API_EMAIL= / SUB2API_PASSWORD=   # 待填
SUB2API_GROUP=codex
CPA_URL=http://127.0.0.1:18317       # cpa-manager-plus
CPA_MGMT_KEY=                        # 待填
```

填好凭据后，注册产出的 token 可直接导入现有 SUB2API / CPA，无需手工搬运。

---

## 10. 浏览器内存：实测与治理

### 10.1 实测基线（SG，arm64，2026-10-31）

同一台机、同一镜像、Xvfb 有头、Playwright Chromium 1248，`ps` 汇总所有 chrome 进程 RSS：

| 场景 | 启动空载 | 打开页面后 | 进程数 |
|---|---|---|---|
| 默认参数 + claude.ai | 714MB | **1361MB** | 12 |
| 默认参数 + 3 个标签（claude/chatgpt/outlook） | 913MB | 2089MB | 16 |
| 优化参数 + claude.ai | **634MB** | **1302MB** | 9 |
| 优化参数 + chatgpt.com | 835MB | **987MB** | 10 |
| 无头（headless）同参数 | — | 与有头基本一致（±20MB） | — |

关键结论：

1. **有头 vs 无头内存几乎无差别**（2089 vs 2102MB），省内存不能靠切 headless，
   而 SG 实测有头才能过 Claude/ChatGPT 的 CF 首屏 —— 保持有头。
2. **页面才是大头**：Claude 单页 +600MB（重 SPA），ChatGPT 单页 +150MB。
3. **参数优化只省 4-5%**（空载 -80MB，页面后 -60MB），别指望靠 flags 解决。
4. **关闭即归零**：context close 后 RSS 回到 0，无泄漏（`after-close=0MB/0p`）。
5. `--single-process` / `--in-process-gpu` 在容器 + Xvfb 下**直接启动失败**
   （SIGTRAP / ANGLE 崩溃），不要用。
6. `--no-zygote` 单独用只减进程数不减内存（714→743MB），无收益已移除。

### 10.2 已落地的治理（四层）

**第一层：参数（已写进 `/data/.env`）**

```text
REG_FACTORY_BROWSER_ARGS=--no-sandbox --disable-dev-shm-usage --disable-gpu
  --js-flags=--max-old-space-size=256 --renderer-process-limit=2
  --disable-background-networking --disable-breakpad --disable-component-update
  --disable-default-apps --disable-sync --no-first-run --no-default-browser-check
  --disable-extensions
```

`--renderer-process-limit=2` 是防膨胀的关键：单个实例开多标签时不会每标签一个渲染进程。

**第二层：内存闸门（`deploy/memory_guard.py`，已启用）**

- 读容器自己的 `/sys/fs/cgroup/memory.current|max`
- 只拦 `POST /api/run` 和 `POST /api/authorize-outlook`（会拉起浏览器的入口）
- 超过 `REG_FACTORY_MEMORY_GUARD_PCT`（默认 85%）返回 **429**，说明当前水位与建议
- 停止类接口永远放行；`/__rf/memory` 免认证供运维查看：

```bash
curl -s http://127.0.0.1:8799/__rf/memory
# {"ok":true,"current_mb":469,"max_mb":8192,"used_pct":5.7,...}
```

**第三层：软限制与回收**

- `mem_limit 8g` + `memswap_limit 8g`（禁 swap，避免整机卡死）
- `mem_reservation 5g`（软限制，内核优先回收页缓存而不是 OOM kill）
- 维护脚本每次运行先清理**孤儿 Chromium**（父任务被 SIGKILL 后会被 init 收养，
  单进程可占 600MB+）

**第四层：调度策略（最有效）**

按实测峰值算并发预算（8GB 上限，预留面板 0.4GB）：

| 平台组合 | 单实例峰值 | 建议 `REG_FACTORY_MAX_CONCURRENCY` |
|---|---|---|
| 纯 Claude 浏览器流程 | 1.30-1.45GB | 3（安全）→ 4（紧张） |
| 纯 ChatGPT 流程 | 0.99GB | 5-6 |
| 纯 Outlook 注册 | ~0.9GB | 5-6 |
| 混合（推荐保守） | 取最高 | **3** |

> 另有一条 0 浏览器路径：Claude 支持 HTTP 协议注册
> `python register.py --protocol http --provider yyds`（面板里选「Claude 协议版」），
> 完全不启动 Chromium，内存占用基本只有进程本身。批量跑 Claude 时优先用它。

### 10.3 监控与排障

```bash
# 当前水位
curl -s http://127.0.0.1:8799/__rf/memory
# 容器内浏览器进程
sudo docker exec reg-factory ps -eo rss,etime,args | grep -c chrome
# 是否发生过 OOM
sudo docker inspect reg-factory --format '{{.State.OOMKilled}}'
sudo dmesg -T | grep -i -m5 'oom'

# 临时放宽/收紧闸门（改完重启容器生效）
sudo docker exec reg-factory sh -c 'grep MEMORY_GUARD /data/.env'
# 或在 deploy/.env 里加 REG_FACTORY_MEMORY_GUARD_PCT=90
```

如果频繁出现 `[guard] reject POST /api/run`：说明并发定太高或任务未及时释放，
先把 `REG_FACTORY_MAX_CONCURRENCY` 降到 2，或改走 Claude 协议版。
