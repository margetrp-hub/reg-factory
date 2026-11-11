# 接口与对接

本仓有三种入口，部署及调用方必须先选对服务。Go面板路由以[api.go](../server/internal/api/api.go)为准，节点路由以[nodeserver](../server/internal/nodeserver/server.go)为准；Python本地接口是独立工具协议，不能把8799示例直接发到SG8800。

| 接口 | 认证 | 状态与副作用 |
| --- | --- | --- |
| Go regd面板，默认8800 | 登录Cookie `rf_sess`、Basic，或以面板密码作为Bearer / X-API-Key | 管理任务/配置/账号；资产列表GET只读，领取使用POST |
| Work节点监听 | 节点签名及Core业务授权，固定协议域与原操作身份 | 新生产、原结果核对及恢复；不是普通浏览器入口 |
| Python本地面板，默认8799 | 本地资产Key `REG_FACTORY_ASSET_API_KEY`，详见下文 | 部分资产GET会记录领取，不能作为只读健康探针 |

Core的 `X-Admin-API-Key` 和客户开发者Bearer属于Aishop平台，不是SG面板或节点凭据。Token、Cookie、密码、账号原文及签名材料只经受控通道传递，不出现在日志、工单或示例中。

## Go 面板 API

匿名可访问 `GET /api/health`、`GET /api/memory` 及 `POST /api/login`；IC公开取码另有Turnstile边界。生产关闭匿名管理，其余API要求认证。具体认证实现见[auth.go](../server/internal/auth/auth.go)；面板密码具有管理权限，不当作只读第三方Key分发。

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| POST / GET | `/api/login`、`/api/logout`（POST）；`/api/session`（GET） | 会话管理；登录体为username/password |
| GET | `/api/system`、`/api/scripts` | 运行信息及当前任务参数schema |
| POST / GET | `/api/tasks` | 受理任务 / 查询列表 |
| GET | `/api/tasks/{id}`、`/api/tasks/{id}/logs`、`/api/tasks/{id}/stream` | 原任务状态、历史日志及SSE |
| POST | `/api/tasks/{id}/stop`、`/api/tasks/stop-all` | 请求停止；回读实际终态，不把响应当外部副作用已撤销 |
| POST | `/api/tasks/{id}/production/{action}` | 原生产任务的允许控制动作；按当前阶段校验 |
| GET | `/api/logs`、`/api/logs/stream`、`/api/audit` | 日志及审计 |
| GET | `/api/accounts`、`/api/accounts/summary`、`/api/accounts/detail`、`/api/accounts/sources` | 去重账号、全量统计、详情及原来源 |
| POST | `/api/accounts/import`、`/api/accounts/convert`、`/api/accounts/change-password`、`/api/accounts/sync` | 账号资料操作；按各处理器校验输入及执行权限 |
| POST / GET | `/api/accounts/checks`；`/api/accounts/checks/{id}`（GET） | 创建/查询持久AT检查批次；不刷新RT |
| POST | `/api/accounts/liveness`、`/api/accounts/refresh` | 账号核验 / 显式凭据刷新 |
| GET / POST | `/api/accounts/refresh/{id}`（GET）；其 `/reconcile`、`/resolve`（POST） | 原刷新查询、核对及证据处理 |
| GET | `/api/assets`、`/api/assets/{kind}`、`/api/assets/tree`、`/api/assets/results`、`/api/assets/export` | 资产及受权限约束的导出 |
| POST | `/api/assets/{kind}/claim`、`/api/assets/claims/reset` | 领取及重置，改变领取事实 |
| GET / PUT | `/api/config`、`/api/config/raw` | 读取 / 保存原配置文件，保留未修改键 |
| GET | `/api/proxy/check` | 指定代理和目标的连通性探测，不修改已保存配置 |

### 任务受理与重试

账号页的“更新凭据”统一调用 `POST /api/accounts/refresh`，输入 `ref/platform/request_id/allow_relogin`。ChatGPT 的 `allow_relogin=false` 为纯RT刷新，`true` 为登录重新授权；不因RT失败自动切换登录。批量按账号独立受理，保留已受理任务；丢响应先查询 `request-<request_id>`，同编号不能切换方式。原 `/api/accounts/convert` 保留既有明确重新授权契约，前端不再把它命名为笼统“修复凭证”。已领取与原任务占用限制继续由服务端校验。

先从 `GET /api/scripts` 取得实际script及参数，再提交：

```http
POST /api/tasks
Authorization: Bearer <PANEL_PASSWORD>
Content-Type: application/json

{
  "request_id": "61c85973-8657-4719-a759-32c3881a48bf",
  "script": "<从任务schema选择的ID>",
  "args": {}
}
```

新受理返回202及 `id/status/title/args/slots`，随后查询 `GET /api/tasks/{id}`。自动化调用应提交小写UUID格式 `request_id`；虽然字段可省略，但省略后每次POST都可能新建任务。丢响应沿相同身份、script和规范化参数重发找回原任务；同ID换参数返回409，读取暂不可用返回503，内存保护拒绝新任务返回429。原已受理任务仍可查询，不能更换编号绕过保护。

普通任务为queued → running → succeeded/failed/stopped，重启中断保留interrupted及原结果；凭据保存后的收尾优先恢复本次成功工件。AT单条网络未知、历史刷新unknown及Work外部unknown按各自原任务处理，不统一降成失败或重新生产。详见[状态与恢复](architecture.md#状态与恢复)。检查批次与凭据刷新有各自输入及幂等校验，分别见[检查处理器](../server/internal/api/credential_checks.go)与[刷新处理器](../server/internal/api/account_refresh.go)，不能把普通任务请求体原样发到这些接口。

SSE使用 `Last-Event-ID` 补读持久事件，任务终态补齐后发送done。网络断流不取消服务端工作；配置保存响应未知时先回读，确认已落盘再清除草稿。

## Work 机器接口

节点地址由部署配置和Work注册表提供，不假定与面板8800同一监听。签名请求、授权材料、资源版本、预算和operation身份由Core/Work生成，接入方不要自行拼一份面板请求代替。

| 方法 | 路径 | 责任 |
| --- | --- | --- |
| GET | `/ready`、`/node/capabilities` | 实际就绪及能力，不等于平台商品可售 |
| POST | `/internal/work/v1/tasks` | 原签名授权接单 |
| GET | `/internal/work/v1/tasks/{task_id}` | 原任务状态，不创建新执行 |
| POST | `/internal/work/v1/tasks/{task_id}/result`、`/events`、`/artifact` | 按原授权读取回执、事件及工件，各路径独立验签 |
| POST | `/internal/work/v1/resolve`、`/internal/work/v1/stop` | 原结果查询及停止控制 |
| POST | `/internal/work/v1/production/admit` | `produce`，签名域 `work-production-admit-v1\n` |
| POST | `/internal/work/v1/production/resume` | `existing_result`，签名域 `work-production-existing-resume-v1\n` |
| POST | `/internal/work/v1/production/continue`、`/internal/work/v1/team/materials` | 原生产继续与授权物料交接，不作为重新下单入口 |
| POST | `/internal/work/v1/inventory/library`、`/internal/work/v1/inventory/reference` | 签名账号目录及原引用证明 |

生产动作均为 `production.team_json`，两种模式不能互换签名域。生产入口拒绝Cookie/Origin/Sec-Fetch-Site浏览器请求；密封资料、业务授权与回执密钥必须就绪。完整字段和校验见[生产接单](../server/internal/nodeserver/production.go)及主平台的 `docs/contracts/work-node-integration.md`，不能以面板密码替代节点签名。

unknown保留原任务、输入及外部证据，先查询/恢复已保存结果；Core决定库存、权益和终态账务。AT检查在节点执行，目录只读不隐式兑换、刷新或领取。

### Work 生产日志与安全停止

Work 节点统一规范由 Aishop Next 的 `docs/contracts/work-node-integration.md` 维护；SG 的 `POST /internal/work/v1/tasks/{task_id}/events` 复用其中 `account-node.events-request.v1 / account-node.events-response.v1`，签名域为 `account-node-work-events-v1\n`。该名称不代表只允许 Account 节点，禁止为 SG 另建不兼容的日志协议。

请求与原任务的 issuer、node_id、operation_id、task_id、attempt_id、fence、command_sha256、node_task_id 全量匹配；每次读使用新 request_id、120 秒内时间窗口和 page/page_size（最多 50）。响应 echo 范围、request_id、分页，返回 items/total/has_more，raw_output_available 固定 false。事件为 `{id,kind,state,code,created_at}`（phase/status）或 `{id,kind,text,stream,created_at}`（log）；id 按 kind 稳定、正数、追加不可变，created_at 带时区。text 在本节点先脱敏，Work 二次脱敏；禁止 token、Cookie、密码、验证码、完整邮箱、材料与原始上游响应进入日志。

`work_event_scopes` 在接单时固定原 scope 与本地任务映射，`work_event_reads` 持久防重放，重启不清除有效 nonce。生产任务本地编号为 `work-{operation_id}`，普通任务为 `work-{task_id}`；查询使用映射，不能假定二者相同。历史生产通过原签名 result/resolve 请求与完整 admission 摘要对照后恢复日志范围；无法证明原范围的历史普通任务返回 `WORK_EVENTS_SCOPE_UNAVAILABLE`，保留本地原记录，不根据日志请求伪造授权。

生产过程在实际边界记录账号选定、预检、邀请开始及返回、授权开始及返回、成品保存、只读核验、交接／投影、终态与耗时。阶段与结果先持久落盘，再回执；不能只写一条“子节点生产与成品回传”。Work 后台有界拉取，不依赖页面打开；重启／断线沿原任务补传，按 `(attempt_id,kind,id)` 去重，终态任务仍可读取，不重启原生产。

生产 stop 先持久记录 stop_requested，阻止开始下一次邀请或授权，等待已开始步骤自然返回并保存产物，不直接杀 Python 进程。停止未收尾时返回 `PRODUCTION_STOP_PENDING`（503），Work 重试原请求；已保存的有效成品仍核验、交接，暂时网络错误保留 running 并只读重试。明确没有副作用或已核清成员占用后才能签停止终态。已结束原销售订单不重新交付；迟到有效成品由 Core 原入库流程回库，SG 不能修改订单、退款或可售账务。

对接验收需覆盖正常阶段、邀请／授权途中停止、迟到输出、网络失败后原结果恢复、日志重复／分页／重启、错误 scope 与日志脱敏；节点事件不是库存／资金结算凭证。中文／emoji 日志遵循主平台文档中的 canonical 签名测试向量，不沿用原任务的 ASCII-only 限制。跨仓互通入口为 [`TestExecutionLogsInteropServer`](../server/internal/nodeserver/execution_events_test.go) 与 Work 的 `TestNodeEventsSGInterop`，使用回环地址和临时库。关闭 Work 派单或页面不停止已有任务补传，日志请求也不续期业务授权。

## Python 本地接口

以下仅适用于 `start.bat/start.sh` 启动的Python本地面板及其辅助服务，不是SG Go API。保留源工具的接口说明以免现有调用失去维护入口；本次SG部署没有对它们重新验收。

主 WebUI 提供资产领取接口，用于按顺序或指定下标读取邮箱与已注册平台凭据。默认地址为 `http://127.0.0.1:8799`。每个读取请求直接从本地尚未领取的资产中输出数据，不会先调用在线状态扫描。接口不修改原始邮箱、Cookie 或 Token 文件，只持久化领取标识。

控制台左侧打开“资产 API”，可以配置 API Key、选择平台和输出格式、生成 `curl` 命令、在线调用并重置领取记录；下面的接口也可供其他本地程序直接调用。

### 鉴权

未配置 `REG_FACTORY_ASSET_API_KEY` 时，接口只接受本机请求。配置后，请求必须携带其中一种请求头：

```text
X-API-Key: your-key
Authorization: Bearer your-key
```

不要把 WebUI 监听到公网；这些接口返回邮箱密码、refresh token、Cookie 或平台 token。

### 邮箱

```bash
# 领取下一个未领取邮箱
curl http://127.0.0.1:8799/api/assets/emails

# 领取当前未领取列表中的第 3 条
curl "http://127.0.0.1:8799/api/assets/emails?index=2"

# 返回原始四段文本
curl "http://127.0.0.1:8799/api/assets/emails?format=line"

# 只领取 iCloud 注册邮箱
curl "http://127.0.0.1:8799/api/assets/emails?format=json&email_provider=icloud"

# 只领取最近一次号池扫描中状态为正常的邮箱
curl "http://127.0.0.1:8799/api/assets/emails?normal_only=true"

# 按最近一次扫描状态领取；显式 status 会覆盖默认的正常筛选
curl "http://127.0.0.1:8799/api/assets/emails?status=normal"
curl "http://127.0.0.1:8799/api/assets/cookies/chatgpt?format=session&status=normal"
# 多个状态用逗号分隔
curl "http://127.0.0.1:8799/api/assets/emails?status=normal,error"
```

`format=json` 返回 `email`、`password`、`refresh_token`、`client_id`；`format=line` 返回原始 `----` 分隔文本；`format=four` 始终返回 `邮箱----密码----refresh_token----client_id` 四段。默认领取不读取扫描状态，但会永久排除已经被任意平台注册领取、尝试或成功使用的邮箱，防止同一 Outlook 邮箱再被单独售卖。设置 `normal_only=true` 或 `status=normal` 后只使用最近一次号池扫描缓存筛选，并在响应中返回 `verification`；`status` 支持 `normal`、`unlock`、`banned`、`expired`、`restricted`、`invalid`、`unknown`、`error`，多个状态用逗号分隔。显式 `status` 优先于默认正常筛选。领取请求本身仍不会联网检测。没有匹配状态且尚未领取的资产时返回 HTTP 409。

邮箱与平台资产响应都会包含 `email_provider`：`outlook`、`icloud`、`temporary` 或 `other`。可用 `email_provider` 查询参数按注册邮箱来源筛选。

### 平台 Cookie 与下游格式

```bash
# Claude 有效 Cookie 数组
curl "http://127.0.0.1:8799/api/assets/cookies/claude?format=raw"

# 指定 Claude 第 1 个账号，输出浏览器扩展标准 Cookie JSON
curl "http://127.0.0.1:8799/api/assets/cookies/claude?format=cookies&index=0"

# 浏览器 Cookie 请求头
curl "http://127.0.0.1:8799/api/assets/cookies/chatgpt?format=header&index=0"

# ChatGPT -> SUB2API 导入内容
curl "http://127.0.0.1:8799/api/assets/cookies/chatgpt?format=sub2api"

# ChatGPT -> 只领取 Outlook 注册账号
curl "http://127.0.0.1:8799/api/assets/cookies/chatgpt?format=sub2api&email_provider=outlook"

# ChatGPT -> Outlook 注册邮箱四段式
curl "http://127.0.0.1:8799/api/assets/cookies/chatgpt?format=email_four&email_provider=outlook"

# ChatGPT -> iCloud 注册邮箱
curl "http://127.0.0.1:8799/api/assets/cookies/chatgpt?format=email_four&email_provider=icloud"

# ChatGPT -> CPA codex 授权 JSON
curl "http://127.0.0.1:8799/api/assets/cookies/chatgpt?format=cpa"

# ChatGPT -> chatgpt2api account
curl "http://127.0.0.1:8799/api/assets/cookies/chatgpt?format=chatgpt2api"

# Grok -> SUB2API SSO 请求体
curl "http://127.0.0.1:8799/api/assets/cookies/grok?format=sub2api"

# Kiro Builder ID 账号凭据
curl "http://127.0.0.1:8799/api/assets/cookies/kiro?format=session"
```

支持的平台与格式：

| 平台 | 格式 |
|---|---|
| Claude | `cookies`、`raw`、`header` |
| ChatGPT | `cookies`、`raw`、`header`、`session`、`email_four`、`sub2api`、`cpa`、`chatgpt2api` |
| Grok | `cookies`、`raw`、`header`、`session`、`sub2api` |
| Kiro | `session` |

### 批量文件导出

```bash
curl -X POST http://127.0.0.1:8799/api/assets/export \
  -H "Content-Type: application/json" \
  -d '{"resource":"emails","format":"four","limit":100,"consume":true}' \
  -o emails.zip

curl -X POST http://127.0.0.1:8799/api/assets/export \
  -H "Content-Type: application/json" \
  -d '{"resource":"chatgpt","format":"sub2api","limit":100,"consume":true}' \
  -o chatgpt-sub2api.zip
```

批量接口默认带 `normal_only=true`，只导出最近一次扫描缓存中 `status=normal` 的账号；导出请求本身不会联网扫描。ZIP 中每个账号保存为独立文件并附带不含凭据的 `manifest.json`。`consume` 默认是 `true`：ZIP 生成成功后，源记录会移动到 `runtime/assets/exported/`，因此不会再出现在活动号池；文件仍可人工恢复。消费式批量导出默认带 `include_claimed=true`，因此可排空仍在活动目录、但已被旧领取账本标记过的正常记录。设置 `consume=false` 时不会移动源文件，此时 `include_claimed` 默认也是 `false`。

`cookies` 是浏览器扩展通用导入数组，包含 `domain`、`hostOnly`、`httpOnly`、`name`、`path`、`sameSite`、`secure`、`session`、`storeId`、`value`，持久 Cookie 额外包含 `expirationDate`。`raw` 保留注册脚本保存的原始字段，供旧调用兼容。

响应中的 `index` 是本次在未领取列表中的下标，`total` 是领取前可用总数，`remaining` 是领取后的剩余数量，`claim_recorded=true` 表示领取记录已持久化。省略 `index` 时选择第一条；指定 `index` 时从当前未领取列表中选择。两种方式都会记录领取。同一平台账号按邮箱或来源文件识别，切换 `raw`、`cookies`、`session`、`sub2api`、`cpa`、`chatgpt2api` 等格式也不会重复返回。除邮箱显式设置 `normal_only=true` 外，领取接口不读取上次扫描结论；所有领取请求都不会在请求时联网检测。

### ChatGPT 注册国家与 Plus 优惠

ChatGPT 注册可通过 `--country JP` 等两位 ISO 国家码约束出口。脚本会在浏览器启动前校验 Cloudflare `loc`，找不到匹配网络则停止；成功后在 session 和号池扫描结果中写入 `registration_country` 与 `network_node`。

首次打开 ChatGPT 登录页默认单次等待 30 秒、最多 3 次。若超时时登录文档已经提交，流程直接继续；否则在 `auto` Clash 模式下切换到下一个通过探测的节点、断开旧连接并新建标签页，避免在同一条卡死连接上重复等待。可用 `CHATGPT_GOTO_TIMEOUT_SECONDS` 和 `CHATGPT_GOTO_ATTEMPTS` 调整该边界。

扫描 ChatGPT 账号时会额外调用只读优惠资格接口，并在号池扫描结果中写入 `plus_trial`、`plus_trial_detail`、`plus_trial_evidence`。该检测不创建结账单、不领取优惠、不绑卡、不扣款；失败只标记为 `unknown`，不会改变账号的健康状态。

| `plus_trial` | 含义 |
|---|---|
| `eligible` | 兼容旧缓存的活动标记，不代表已确认 0 元，不能进入协议号池 |
| `zero_price` | 活动接口返回明确的应付 0 元、格式化 0 元价格或 100% 折扣 |
| `discount` | 活动接口确认有折扣但低于 100%，不是 0 元 |
| `ineligible` | 活动接口明确返回不符合、已领取或已过期 |
| `active` | 本地会话表明账号已有 Plus 或其他付费套餐 |
| `unknown` | 缺少 AT、网络失败或接口没有返回明确资格 |
| `disabled` | 已通过配置关闭资格检测 |

默认检测活动为 `plus-1-month-free`。可通过 `ASSET_SCAN_CHATGPT_PLUS_TRIAL=false` 关闭，或用 `ASSET_SCAN_CHATGPT_PLUS_CAMPAIGN` 修改活动标识。检测结果只用于标记，最终优惠仍以用户自行打开的官方结账页为准。

### Plus 协议提链

Plus 导入页把“授权导入”“渠道选择”“批量协议提链”和“批量协议支付”放在一个任务面板中。OAuth 导入成功后可从本地 session 读取当前 AT；也可选择“号池有资格账号”，直接载入资产扫描缓存中标记为 `zero_price` 且存在可用本地 session 的账号。`eligible`、`discount` 和 `unknown` 不会进入协议号池。无论来源如何，协议任务都会逐账号实时复检优惠资格，未再次命中的账号不会进入提链或支付。

渠道目录包括 PayPal、GoPay、GCash、GrabPay、UPI、iDEAL、PIX、Kakao Pay、BLIK、TWINT、Direct Card Checkout 和 MoMo。BLIK 的上游协议不支持批量，因此在批量界面中明确禁用。PayPal 与 GoPay 支持“批量协议支付”：PayPal 调用 `paypal_auto`，GoPay 在现有协议引擎生成 Midtrans 链接后从本地钱包池自动选择一号一付；其余渠道只生成支付链接或二维码。两种真实支付都必须勾选支付确认并通过二次确认弹窗。GoPay 批量任务开始前会检查可用钱包账号数，数量不足时不会开始部分支付。任务输入文件以本机权限保护，子进程结束即删除；最终报告不保存 AT、Cookie、PIN、钱包 token、卡片、地址或接码 URL。

协议执行复用本机的 GPT-Register-Tool 引擎。默认自动查找同级 `GPT-Register-Tool` 目录；发布包或其他目录可在 `.env` 中设置 `REG_FACTORY_PROTOCOL_PAYMENT_ROOT`。Plus 提链默认跟随 ChatGPT 的 Clash 出口；需要其他出口时，通过 `REG_FACTORY_PLUS_LINK_ROUTE`、`REG_FACTORY_PLUS_BIND_ROUTE` 或对应的显式 proxy override 配置。

### 号池状态扫描

扫描任务在 WebUI 后台运行，不阻塞其他 API，也不是默认领取的前置条件。支持的平台是 `outlook`、`chatgpt`、`claude`、`grok`、`kiro`。安全扫描模式按小批次并行同平台账号，`account_concurrency` 默认 4、上限 8；不同平台并行数由 `concurrency` 控制，默认 1、上限 2。WebUI 默认只做健康扫描，设置 `include_plus_trial=true` 才额外请求 ChatGPT Plus 资格接口。每个账号请求前仍随机等待 3–6 秒，默认复用 6 小时内的结果；出现 429 会暂停下一批，连续两次 403/挑战页或网络异常也会暂停，剩余记录标记为 `unknown`。扫描确认 `banned`、`expired` 或 `invalid` 时默认把源记录移动到 `runtime/assets/quarantine/`；隔离归档在线程中执行，不阻塞 WebUI。`restricted`、`error`、`unknown` 和 `unlock` 不会自动移动。

```bash
# 读取当前号池明细、上次结果和正在运行的扫描进度
curl http://127.0.0.1:8799/api/assets/scan

# 一键扫描全部号池
curl -X POST http://127.0.0.1:8799/api/assets/scan \
  -H "Content-Type: application/json" \
  -d '{"platforms":["outlook","chatgpt","claude","grok","kiro"],"concurrency":1,"account_concurrency":4,"quarantine_bad":true,"include_plus_trial":false,"timeout":15}'

# 只扫描 Outlook 邮箱
curl -X POST http://127.0.0.1:8799/api/assets/scan \
  -H "Content-Type: application/json" \
  -d '{"platforms":["outlook"],"concurrency":1}'
```

确需忽略 6 小时缓存时，可在 POST JSON 中增加 `"force":true`。不要在短时间内反复强制扫描。等待间隔和缓存时长可分别通过 `ASSET_SCAN_MIN_INTERVAL`、`ASSET_SCAN_MAX_INTERVAL`、`ASSET_SCAN_CACHE_SECONDS` 调整。

扫描状态：

| 状态 | 含义 |
|---|---|
| `normal` | 官方会话、OAuth 或邮箱访问验证正常 |
| `unlock` | Outlook 明确返回锁定、补充验证，或历史扫描确认需要解锁 |
| `banned` | 官方响应明确表示账号停用/封禁，或 Outlook 历史结果为 dead/abuse lock |
| `expired` | Cookie、session、SSO 或 refresh token 已过期/撤销 |
| `restricted` | HTTP 403、限流、Cloudflare 或出口风控，不能据此判定账号封禁 |
| `invalid` | 本地资产缺少平台关键凭据或文件结构无效 |
| `unknown` | 尚未扫描，或缺少足够证据确认状态 |
| `error` | 请求超时、网络失败或官方服务异常 |

GET 响应中的 `summary` 是全号池统计，`items` 是逐条结果，`scan.progress` 是当前任务进度，`safe_mode` 是最近一次扫描采用的限速和缓存参数。重复启动扫描会返回 HTTP 409。

结果保存在 `runtime/state/asset_pool_scan.json`，只包含账号标识、状态、判定依据、来源文件名和检测时间，不包含密码、refresh token、Cookie、access token、sessionKey 或 SSO。

### 状态与重置

```bash
curl http://127.0.0.1:8799/api/assets/summary

# 重置全部领取记录和兼容游标
curl -X POST http://127.0.0.1:8799/api/assets/cursors/reset \
  -H "Content-Type: application/json" -d '{"scope":"all"}'

# 只重置 ChatGPT 账号领取记录
curl -X POST http://127.0.0.1:8799/api/assets/cursors/reset \
  -H "Content-Type: application/json" -d '{"scope":"chatgpt"}'
```

领取账本保存在 `runtime/state/asset_api_claims.json`，只包含不可逆的 SHA-256 标识和平台范围，不保存邮箱或凭据。旧版兼容游标仍保存在 `runtime/state/asset_api_cursors.json`。重置不会修改 `emails.txt`、Cookie 或 Token 文件。

### Work 生产超期核验

`POST /internal/work/v1/production/readback` 由 Work 以独立域 `work-production-readback-v1\n` 签名，正文 `{payload:{assignment,envelope},signature}`。只接受原 assignment 和 Core 密封的 `team.work-production-readback.v1` 材料；不能用于新任务或继续邀请/OAuth。普通材料接口拒绝该 schema。

密封格式沿生产材料 AES-256-GCM，AAD 为 `schema,node_id,grant_id,context_sha256`；解密内容为原 context/action/parameters/workspace，新增 `renewal:{id,actor_id,reason,issued_at}`。原因支持中文；首次送达15分钟内、核验窗口最多1小时。母号凭据可更新，但 workspace、grant、原任务和原源账号不变。

先写入原停止事实，再在原任务锁内更新核验材料；执行未退场或锁忙返回503 pending，Work重试同一请求。重复授权幂等，较旧授权不能覆盖新授权。返回 `renewed:true` 仅表示已接收授权；实际成功/失败仍使用原签名 stop/resolve、产物交接及Core库存结算。已有输出优先只读验证，过期输出保留；不能另选账号、再次邀请、换取Token或重置调用次数。窗口过期后由Core原任务再次授权。

复用合同与异步日志字段见上文 Work 节点对接说明所指向的 `team-platform-next/docs/contracts/work-node-integration.md`。本地验证不代表远端节点已升级。

### GoPay 本地 API

主 WebUI 在 `/api/gopay/*` 提供 Plus 支付面板内置钱包池所需的本机接口。账号凭据、PIN、短信配置与支付任务状态保存在 `REG_FACTORY_DATA_DIR/runtime/gopay/`，接口响应不会返回 access token、refresh token、PIN 或代理口令。钱包池没有独立支付入口；正常支付应通过 `/api/chatgpt-plus/protocol-batch` 的 `method=gopay`、`operation=pay` 发起。

```bash
# 状态、账号和共享号码池
curl http://127.0.0.1:8799/api/gopay/status
curl http://127.0.0.1:8799/api/gopay/accounts
curl http://127.0.0.1:8799/api/gopay/phones

# 使用共享号码池启动单个注册任务
curl -X POST http://127.0.0.1:8799/api/gopay/register \
  -H "Content-Type: application/json" \
  -d '{"source":"custom","pin":"147258","country_code":"62"}'

# 等待 OTP 的注册任务可人工提交验证码
curl -X POST http://127.0.0.1:8799/api/gopay/register/jobs/JOB_ID/otp \
  -H "Content-Type: application/json" -d '{"code":"123456"}'
```

`POST /api/gopay/payments` 是协议 worker 使用的本机执行接口，JSON 必须明确包含 `"confirm_payment":true`；同一 Midtrans snap 和已绑定的 GoPay 账号会由协议引擎阻止重复使用。用户界面的唯一支付入口仍是 Plus 协议支付任务。
