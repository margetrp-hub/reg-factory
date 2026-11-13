# 配置说明

本页维护配置操作与生效规则。字段目录来自 `schema/scripts.py`，当前部署与读写验证只在[验收](acceptance.md)维护。

## 统一配置中心

配置按两层维护：能力连接保存供应商地址、密钥与连接参数；流程使用只保存对已有能力的选择。浏览器与邮箱继续复用原供应商实现，生产/修复可选择公共网络出口，Claude 视觉识别可直接选择公共视觉或投票网关。节点身份、业务授权、支付资料和不同协议的供应商保持各自归属。

共享配置状态机：读取当前连接和选择 → 编辑草稿 → 校验并原子保存 → 回读确认 → 新任务解析选择并冻结连接 → 原执行器运行及记录结果。校验失败不改原文件，保存响应未知由原页重新读取/重试；切换选择保留未选连接的资料。选中的能力未配置时明确失败，由管理员补齐连接后从原入口重试；不静默直连、不换供应商。已有运行任务不重新解析连接，也不重新执行已完成的外部动作。

验收要求：共用连接变更后，新建本机和 Work 任务均消费同一连接；正在执行的任务保留原连接；直连、专用连接、共享连接切换及未配置恢复可达；原始编辑和页面保存沿同一验证边界；未选字段、自定义字段和跨类别草稿保留。

`/settings/` 是公共配置的编辑入口，按“运行与资源、网络与代理、浏览器、邮箱与取码、短信接码、图形验证、平台规则、导入与对接、其他配置”选择能力。供给中心只提供入口，原物料配置链接转到同一中心；邮箱池的设置页保留当前生效信息并进入同一编辑页，不再内嵌第二份公共配置。节点身份仍由原节点配置管理。

| 能力 | 模式及参数归属 |
| --- | --- |
| 浏览器 | `BROWSER_ENGINE` 选择Patchright或Camoufox；Patchright下继续由 `FINGERPRINT_BROWSER` 选择原服务。Firefox默认自动发现内核路径 |
| 网络出口 | `PROXY_MODE` 与六个平台覆盖选择注册出口；`REG_FACTORY_PRODUCTION_PROXY_SOURCE` 为生产/修复选择共享住宅代理、共享Clash、直连或专用出口。连接地址只维护一份 |
| 邮箱来源 | ChatGPT、临时邮箱及Outlook辅助邮箱按原选择字段配置；同一供应商被多处使用时只显示一份连接，收码等待与邮箱池共用 |
| 短信接码 | 选择要配置的供应商，公共等待/重试在同一视图中保留；实际供应商在发起任务时选择，不新增全局启用开关 |
| 图形验证 | 按工具查看原参数，多个工具可并存；`CLAUDE_VISION_SOURCE` 选择公共视觉、公共投票或专用连接，模型按流程选择。默认auto保留原自动选择 |

模式关系也由 `schema/scripts.py` 导出，页面不再复制一套键或默认值。浏览器、代理和邮箱的模式字段修改后需要保存；未选模式的原值和草稿均保留，“查看全部模式配置”与全局搜索可找到全部字段。自定义键保留在原配置里，不因切换方式或升级被清空。并存的下游对接和平台规则保持按对象编辑，不能将多个可同时运行的服务错误改成互斥开关。

管理员编辑流程：读取 → 选择能力/模式 → 编辑草稿 → 保存全部修改 → 回读确认。跨类别及模式切换保留全部草稿，页面说明保存涵盖哪些类别；全局搜索可找到任何原参数并显示归属。读取或保存失败保留旧值和草稿，可原位重试；保存响应丢失时重新读取对照，确认落盘后才清除对应草稿。离页取消读取，保存请求已受理则仍由原接口完成；未设置与默认值分开显示，不自动写默认值。离页沿用未保存提醒；秘密字段继续默认隐藏。

配置类别只改变组织方式，不删除原字段或自定义键。同一键的值、类型、默认值、只读及重启语义来自唯一schema；重复定义在该处修正，不在页面覆盖。节点身份保留原专用入口。分类选择同时复用供给与邮箱设置，不复制另一套配置。

## 前置条件

### 内置邮箱池：注册与登录授权

邮箱登录统一在“邮箱池 → 收件来源”维护：选择 IMAP、POP3 或 Microsoft Graph。IMAP/POP3 使用邮件服务器、端口和应用专用密码；Graph 使用该 Microsoft 邮箱的 Client ID 与 Refresh Token。保存后自动应用并显示收信结果。重新登录同一邮箱时编辑原来源，密码、Client ID、RT 留空保留原值；Graph 成功轮换的新 RT 立即保存，后续连接读取最新版本。换邮箱、服务器、协议或文件夹时新建来源并调整地址绑定，原邮件保留。

IMAP 推送/轮询、POP3 TLS 轮询和 Graph 轮询共享地址索引、验证码筛选、状态与保留策略。POP3 不删除上游邮件。Graph 401 只使当前 AT 缓存失效，下一轮使用最新 RT 重新授权取信；明确授权失效须在来源中更新登录资料。连接失败只影响该来源，不阻塞其它邮箱。

“邮箱池 → 地址”导入实际用于注册或授权的地址并绑定来源。收信母邮箱不自动作为注册账号；从邮件发现的地址只作索引，经管理员导入确认后才能进入注册分配。重复导入保留原来源、暂停状态和使用记录，变更来源仍通过地址设置。注册使用已启用、来源可用且没有使用记录的地址；取到地址即登记占用，失败后沿原邮箱重新授权，避免再次注册同一个身份。

- ChatGPT 注册：任务的邮箱来源选“内置邮箱池”，或在配置中心设置 `CHATGPT_EMAIL_PROVIDER=icmail`。
- Claude 注册：任务的 provider 选“内置邮箱池”；Grok 使用 `TEMP_EMAIL_PROVIDER=icmail` 并启用原临时邮箱方式；Kiro 选择临时邮箱方式并将 provider 设为 `icmail`。这些入口共用同一个地址分配和取信实现。
- ChatGPT 重新授权：保存的 `mailbox_provider=icmail` 优先；没有明确邮箱来源及专用邮箱凭据时，可以沿用 `CHATGPT_EMAIL_PROVIDER=icmail`。始终按原账号邮箱绑定，绝不换一个地址取码。注册结果、OAuth导出和后续重新授权保留该标记。
- RT刷新不读邮箱，密码＋2FA的协议授权仍保留。暂停的地址、停用的来源、读取失败和等待超时会反馈原任务；修正来源后从原入口重新授权，不切换到外部邮箱供应商。

只有本次发起后新索引、发送时间符合要求且属于原地址/来源的邮件可用于取码，已使用验证码不重复提交。执行端只访问本节点的认证接口，不直接打开邮箱数据库。自动巡检仍默认关闭。

### 页面与执行配置一致性

可编辑参数以页面保存值为准，未保存时继承部署初始值，最后使用默认值。显式清空不会重新启用部署中的旧密钥或代理。表单直接展示实际可编辑值；秘密只显示配置状态。配置优先级、技术键名和实现说明保留在源码与本文，不在页面逐字段折叠展示。新任务在启动时取得配置快照，运行中任务继续完成原动作。并发、内存保护与日志读取参数在下一次调度／读取时生效。固定目录、监听、数据库连接和首次预置资料不进入配置表单；收件来源及节点连接保留直接入口。

保存状态机：草稿 → 校验 → 保留恢复所需旧密钥／连接 → 原子落盘 → 回读确认。校验或写入失败保留原配置与页面草稿；响应丢失时沿原保存请求重试或回读核对，不能把网络错误当成已生效。只允许在参数实际使用边界切换，不重新执行已有付款、注册或凭据刷新。中央资产结果在换密钥后仍可读取、补传并按匹配 ACK 清理，切换生产中心时原任务继续归原中心。配置保存与远端接通分别反馈，不把本地保存冒充远端验证成功。

验收范围：普通任务的模式／供应商／代理保存覆盖部署初始值；清空及非法配置失败；控制面参数热生效；节点配置集中展示、密钥生成；原结果换密钥／重启后补传；配置响应丢失后回读恢复。生产验收事实另记于验收页。

`MAIL_IMAP_*` 只在空来源库首次启动时预置来源，不覆盖已经由页面管理的登录资料。运行参数使用唯一的 `MAIL_*` 命名；历史 `ICMAIL_*` 由 `deploy/upgrade-mail-config.py` 一次性升级，不在运行中兼容读取。升级须处理部署 env_file 和数据卷 `.env`，工具默认只预览键名，`--apply` 保留原文件备份，不输出密钥。

临时邮箱 API 共用 `schema/mail_settings.py` 目录：内置邮箱池、MoeMail、YYDS、GPTMail、CFMail、外部 iCloud API、Remail、自定义 API。`icmail` 是已有资料中内置池的 provider 值，外部 iCloud API 使用 `icloud`，两者不互换。每个平台只展示已接通的模式；ChatGPT 保留 Outlook 账号池、内置池、iCloud API、Remail，GitHub 保留既有邮箱流程。错误配置不会自动改成另一供应商。只有明确配置多个供应商时按顺序尝试；取信始终固定创建时的供应商和邮箱。

Outlook Graph 邮箱与 Claude 登录链接的读取共用相同的失败语义：授权被拒绝、读取失败或响应无法解析会立即回到原任务，保留具体原因；只有成功读到空列表才继续等待邮件。临时连接重试由收信层完成，耗尽后不再当作未收到验证码重复等待。验证码与完整登录链接不写入这段任务日志。

邮箱状态分开保存：手工暂停/阻止、收信健康、邮箱母号停用、目标平台限制。六个平台的限制互不覆盖；来源重新登录成功清除旧授权失败，不解除手工暂停或平台封禁。收信故障不再伪装成空收件箱拖到验证码超时。临时服务的 GET/HEAD 瞬时错误有界重试；创建邮箱 POST 丢失响应不会自动重放。

### 周期巡检（默认关闭）

入口为“配置中心 → 运行与资源 → 周期巡检”。`REG_FACTORY_LIVENESS_ENABLED=0` 保持关闭；`1` 才启用，每15秒重读保存配置，无需重启。默认周期 `6h`，可设 `12h` 或 `24h`；开启、改变周期或重启服务后等待完整周期，不立即首扫、不补跑停用期间的任务。

ChatGPT 按当前账号身份去重，读取执行时最新凭据，仅测试AT，不自动刷新RT或重新授权。任务与逐项结果在原检查记录/运行页查看，未知和未执行项可手动重测。空库不创建虚假的成功任务，正在刷新或检查的账号不重复排队。关闭后未执行的周期AT项停止，已返回结果保留；手动检查和修复成功后的自动AT核验不受影响。

AT检查固定单并发、每项间隔300ms、总时限30秒。`REG_FACTORY_LIVENESS_ACCOUNT_CONCURRENCY` 仅作用于其他平台原扫描器，和修复凭据的执行并发分别管理。历史 `REG_FACTORY_LIVENESS_TIMEOUT` 只保留原值，不控制当前AT检查。现役保存的平台和周期不会因升级被重写。

### 指纹浏览器

支持以下浏览器类型；外部客户端模式需要保持客户端运行：

- 内置 Chromium：`FINGERPRINT_BROWSER=bundled`，安装程序会配置浏览器路径。
- 普通 Chrome/Chromium：`FINGERPRINT_BROWSER=custom`，通过 `CUSTOM_BROWSER_PATH` 指定可执行文件；留空时会尝试查找系统 Chrome。
- [BitBrowser 官方下载页](https://www.bitbrowser.cn/download)：默认 API 为 `http://127.0.0.1:54345`。
- CloakBrowser：`FINGERPRINT_BROWSER=cloak`，在同一视图设置无头运行、行为模拟、语言、时区与独立环境参数。
- RoxyBrowser：`FINGERPRINT_BROWSER=roxy`，通过 `ROXY_API_BASE`、`ROXY_API_TOKEN` 与 `ROXY_WORKSPACE_ID` 配置原连接及工作区。
- AdsPower：默认 API 为 `http://127.0.0.1:50325`，启用鉴权时还需 API Key。
- 其他指纹浏览器：`FINGERPRINT_BROWSER=custom_api` 并设置 `CUSTOM_BROWSER_API`。默认 `CUSTOM_BROWSER_API_MODE=auto` 保持 BitBrowser 兼容；对接自己的 REST API 时改为 `generic`，在 WebUI 填写 API Key、鉴权请求头以及创建/列表/启动/关闭/删除/更新路径即可。启动接口返回 `ws`、`cdp`、`endpoint` 或 `debugPort` 任一字段即可。

在配置中心或 `.env` 中用 `FINGERPRINT_BROWSER=bitbrowser|cloak|roxy|bundled|custom|adspower|custom_api` 切换。默认使用 BitBrowser，网页流程沿用各方式的原自动化接口。

`generic` 模式的最小约定：创建接口接收 `{name, remark, fingerprint, proxy}` 并返回 profile ID；列表接口返回数组或 `data.items`/`data.profiles`；启动、关闭、删除、更新接口接收配置的 `CUSTOM_BROWSER_API_ID_FIELD`（默认 `id`）。路径支持 `{id}` 和 `{profile_id}` 占位符，方法可分别配置为 GET/POST/DELETE。

### 网络出口

在“配置中心 → 网络与代理”的“流程使用”选择各用途使用的能力，在“能力连接与参数”维护唯一连接。生产、重新授权、RT刷新和AT核验均可引用同一Clash或住宅代理；选择专用出口才填写 `REG_FACTORY_PRODUCTION_PROXY`。升级保留原专用出口值和选择，未配置该新选择时仍按原专用出口（空值直连）执行。共享住宅代理读取当前池索引但不轮换，Go在每个执行进程启动时固定实际地址，浏览器和协议步骤共用该地址。独立CLI从自身进程配置解析同一选择。注册出口支持三种互斥模式：

| 模式 | `PROXY_MODE` | 行为 |
|---|---|---|
| 自动轮换 | `clash_auto` | 从 Clash 代理组选择可响应节点，注册流程可按失败或批次继续换节点 |
| 固定节点 | `clash_fixed` | 始终强制使用 `CLASH_FIXED_NODE`，脚本传入其他节点也不会覆盖 |
| 动态住宅 IP | `residential` | 使用单个住宅代理或持久轮换代理池，可调用供应商换 IP 接口 |

六个平台可覆盖全局模式：`OUTLOOK_PROXY_MODE`、`CLAUDE_PROXY_MODE`、`CHATGPT_PROXY_MODE`、`GROK_PROXY_MODE`、`KIRO_PROXY_MODE`、`GITHUB_PROXY_MODE`。值为 `inherit`、`clash_auto`、`clash_fixed` 或 `residential`。例如：

```env
PROXY_MODE=clash_auto
OUTLOOK_PROXY_MODE=clash_auto
CLAUDE_PROXY_MODE=residential
CHATGPT_PROXY_MODE=residential
GROK_PROXY_MODE=residential
GITHUB_PROXY_MODE=residential
REG_FACTORY_PROXY=http://user:pass@host:port
```

Claude 的登录、验证码和 magic-link 验证必须保持同一个出口 IP。住宅模式请在代理供应商控制台生成 Sticky/粘性会话 endpoint；按请求轮换 IP 的 endpoint 会在 Claude 任务启动时被检测并拒绝。多个 Sticky endpoint 可写入 `REG_FACTORY_PROXY_POOL`，提交邮箱前被风控时程序会先轮换代理池，再创建新的浏览器 profile。只有在确认供应商通过其他机制保持会话时，才设置 `CLAUDE_ALLOW_ROTATING_PROXY=true` 跳过检测。

编排器会为每个平台创建独立子进程环境；住宅代理认证直接写入该平台新建的浏览器 profile。“检测出口”可用指定代理和目标站点验证连接，不修改已保存出口或轮换正在执行的会话。

Clash 模式先安装 [Clash Verge 2.5.2 Windows x64](https://github.com/clash-verge-rev/clash-verge-rev/releases/download/v2.5.2/Clash.Verge_2.5.2_x64-setup.exe)。在 Clash Verge 的“设置”中进入 Clash 设置或内核设置，找到“外部控制器”或 External Controller，按下面顺序配置：

1. 启用 External Controller，并填仅本机监听地址，例如 `127.0.0.1:9097`。面板的控制器地址填写为 `http://127.0.0.1:9097`。mihomo 常见默认端口是 `9090`。
2. 在同一页设置 Secret、Controller Secret 或 API Secret。将完全相同的值写入 `CLASH_SECRET`。未设置密码时，`CLASH_SECRET` 也必须留空。
3. 找到 mixed-port 或混合端口，例如 `7897`，写入 `CLASH_PROXY=http://127.0.0.1:7897`。
4. 保存后应用设置或重载内核，在系统配置的“网络与代理”中保存对应字段，再用“检测出口”核对代理连接。外部控制器只应监听 `127.0.0.1`，不要公开到网络。

默认值：

```env
CLASH_API=http://127.0.0.1:9097
CLASH_PROXY=http://127.0.0.1:7897
CLASH_GROUP=GLOBAL
CLASH_SECRET=
```

固定节点模式额外配置：

```env
PROXY_MODE=clash_fixed
CLASH_FIXED_NODE=美国 01
```

Plus Codex 导入只处理已经开通的账号，不会提取优惠链、绑卡或发起支付。每个账号登录后都会进入手机号接码验证阶段，验证成功后才继续 Codex OAuth 和 SUB2API 导入。WebUI 的 Plus 导入页支持批量账号，并可选择接码平台、换号次数、等待时间、并发数、ChatGPT 节点和 SUB2API 分组。

账号行支持 Outlook、iCloud、ChatGPT session token 和完整 Codex OAuth JSON。Outlook RT 与 client_id 顺序均可，也兼容 Hotmail/Live/MSN、两段/三段记录、Tab、逗号、竖线、分号及 `email:password`：
```text
email@outlook.com----password----refresh_token----client_id
email@outlook.jp----password----client_id----refresh_token
email@hotmail.com----password----refresh_token
email@live.jp:password
email@icloud.com
__Secure-next-auth.session-token=...
{"email":"...","access_token":"...","refresh_token":"...","plan_type":"plus","codex_phone_status":"verified"}
```
Outlook Graph RT/client_id 可用时优先通过 Graph 读取 OpenAI 邮箱验证码，否则使用账号密码在浏览器登录 Outlook 取码。iCloud 地址使用已配置的 `ICLOUD_MAIL_*` API。原始 ChatGPT session cookie 会先验证登录态，再走手机号验证和 Codex OAuth；普通短期 access token 不能直接换出 refresh token，会明确拒绝。完整 OAuth JSON 仅在自身带 `codex_phone_status=verified` 时允许直接导入。

住宅代理支持 `http`、`https`、`socks4` 和 `socks5`；BitBrowser 窗口支持 `http`、`https` 和 `socks5`。代理池优先于单个代理；`.env` 中用逗号分隔，WebUI 中可每行填写一个：

```env
PROXY_MODE=residential
REG_FACTORY_PROXY=http://user:pass@host:port
REG_FACTORY_PROXY_POOL=http://user:pass@host-a:8000,http://user:pass@host-b:8000
REG_FACTORY_PROXY_ROTATE_URL=https://provider.example/rotate
REG_FACTORY_PROXY_ROTATE_METHOD=GET
```

`REG_FACTORY_PROXY_ROTATE_URL` 可留空。点击“立即轮换”时，程序会推进代理池索引，并在配置后调用供应商接口。当前池索引保存在 `runtime/state/residential_proxy_index.txt`。

新建浏览器 profile 时会写入当前住宅代理的地址、端口、用户名和密码。代理池轮换后，下一个新建窗口使用新的代理；已经打开的注册窗口不会被强制改 IP，以免破坏当前登录会话。

旧变量 `RESIDENTIAL_PROXY`、`DIRECT_PROXY` 和 `RESIDENTIAL_PROXY_POOL` 仍兼容；新配置应使用 `REG_FACTORY_*`。

### Python 与 Node.js

- Python 3.10+ 是主流程必需依赖。
- Codex K12 新适配器由 Python/FastAPI 提供独立服务；源码运行不额外要求 Node.js。
- Gmail Android 流程还需要 BlueStacks、ADB、Appium 2.x 和 UiAutomator2 driver。

## 创建配置

Windows：

```powershell
Copy-Item .env.example .env
```

macOS / Linux：

```bash
cp .env.example .env
```

Go 面板保存的可编辑配置覆盖部署初始值，新任务使用保存后的快照，不需要重启主服务。独立 Python CLI 的进程环境变量优先于 `.env`；部署固定信息保持部署入口管理。

本地邮箱/Cookie 读取接口默认只允许回环地址调用。需要由其他本机服务统一携带密钥时，设置 `REG_FACTORY_ASSET_API_KEY`，并使用 `X-API-Key` 或 Bearer Token。ChatGPT 健康扫描默认通过 `ASSET_SCAN_CHATGPT_PLUS_TRIAL=true` 标注 Plus 免费试用或明确 0 元优惠，活动标识由 `ASSET_SCAN_CHATGPT_PLUS_CAMPAIGN` 控制；完整接口见 [本地资产 API](api.md)。

## 配置分组

`.env.example` 是全部配置项和默认值的唯一完整清单。通常只需要填写当前流程涉及的分组。

| 分组 | 常用变量 | 使用场景 |
|---|---|---|
| 浏览器 | `FINGERPRINT_BROWSER`、`CUSTOM_BROWSER_*`、`BITBROWSER_API`、`ADSPOWER_*` | 浏览器注册流程 |
| 网络出口 | `PROXY_MODE`、`CLASH_*`、`REG_FACTORY_PROXY*` | Clash 节点或住宅代理 |
| Claude 验证 | `CLAUDE_VISION_*`、`CLAUDE_HCAPTCHA_*` | Claude 图形验证 |
| 通用视觉 | `VISION_*`、`VOTE_*`、`IMAGE_EDIT_*` | 多模型视觉投票 |
| ChatGPT iCloud 邮箱 | `CHATGPT_EMAIL_PROVIDER`、`ICLOUD_MAIL_*` | ChatGPT 不使用 Outlook 池时 |
| 临时邮箱 | `YYDS_API_KEY` 等 provider 配置 | Claude/Grok 不使用 Outlook 池时 |
| 接码 | `SMSMAN_*`、`SMS_API_NAME`、`SMS_TOKEN`、`HERO_SMS_*` | 手机验证；firefox.fun 使用 APIName 标识账号，token + 项目 ID 调用接口 |
| SUB2API | `SUB2API_*` | Codex / Grok 下游导入 |
| CPA | `CPA_URL`、`CPA_MGMT_KEY`、`CODEX_AUTH_URL_SOURCE` | Codex 授权地址与凭据导入 |
| chatgpt2api | `CHATGPT2API_URL`、`CHATGPT2API_KEY` | 普通 ChatGPT 网页号导入 |

密钥必须留在 `.env` 或进程环境变量中。不要把真实值写进 `.env.example`、README、测试和截图。

ChatGPT 使用 iCloud 邮箱时，将 `CHATGPT_EMAIL_PROVIDER=icloud`，并填写 `ICLOUD_MAIL_API_KEY`。默认接口地址为 `https://mail.no-replyca.xyz`；`email.manageh.shop` 仅是接口文档站。`ICLOUD_MAIL_TYPE=icloud-code`、`ICLOUD_MAIL_SERVICE=openai` 用于申请 ChatGPT 接码邮箱；需要普通 iCloud 子邮箱时改用 `ICLOUD_MAIL_TYPE=icloud`，对应 `/api/user/email?type=icloud&apikey=...`。程序随后轮询 `/api/user/mail` 获取验证码。`ICLOUD_MAIL_API_BASE` 也兼容直接填写完整的 `/api/user/email?...` 地址。
创建 iCloud 邮箱时程序会自动请求 `share=1`，并将返回的 `/api/share/{share_token}` 保存到 ChatGPT session JSON 的 `mail_api_url` 字段。该链接免 API Key，可直接查看邮箱 HTML，适合后续手动登录取码；分享链接等同于邮箱读取权限，请按敏感凭据保管。

> **重要：提取 Graph RT 必须配置可接收验证码的辅助邮箱。**
>
> Outlook Graph 提取遇到 Microsoft `proofs/Add` 安全信息页时，默认使用 `OUTLOOK_GRAPH_RECOVERY_PROVIDER=yyds` 创建辅助临时邮箱，提交地址后轮询验证码并完成绑定。要用自定义邮箱则设 `OUTLOOK_GRAPH_RECOVERY_PROVIDER=custom`，并沿用 `CUSTOM_MAIL_*` 配置；也可以设置 `OUTLOOK_GRAPH_RECOVERY_PROVIDER=outlook` 使用自有 Outlook 辅助邮箱。自有邮箱格式为 `email@outlook.com----password----refresh_token----client_id`，程序会先通过 Graph API 验证 refresh token，再从 Inbox/Junk 轮询 Microsoft 安全码。也可写成 `yyds,outlook` 按顺序做故障转移。设为 `OUTLOOK_GRAPH_RECOVERY_EMAIL=false` 可恢复旧的跳过行为，但不保证能提取 RT。

## 资产移交

节点管理提供三个独立表格页面：“节点现有资料”`/nodes/sharing/`、“移交记录”`/nodes/sharing/history/`、“收到的资料”`/nodes/sharing/received/`。现有资料按全部、账号、邮箱筛选，支持逐项勾选、全选本页和选择筛选下全部；改变筛选清除选择，选择本身不产生移交。移交记录与接收记录独立读取，不依赖原来源可用。

所有业务字段可选：账号/邮箱资料与 JSON 独立选择，两者均不选也可移交空资料；补充字段和 JSON 不按执行任务格式校验。执行具体任务时才校验必要输入。秘密不出现在预览、列表和日志中。批量操作逐项保存原请求、展示提交结果，一项失败不阻断其他项；重试只处理未完成项，确认未知时保持原请求。可停止后续提交并继续未提交项；已提交项继续按服务端原记录收敛。明确拒收从移交记录修改后重提。

确认移交前持久保存原密封包，中央确认后撤出原私有账号、邮箱及文件来源。丢确认只重传原包，撤出失败仅重试撤出；明确拒收可修改后提交新请求。成功后仅留审计与回执，不再更新中央资料、报告该资料使用历史或决定再次投入。重新扫描和启用不能恢复已移交来源。

未移交来源仍可被中央实时查询，查询不自动入库；清单只报告技术可用性，业务使用、占用和再投入由生产中心决定。中央通过 `asset.restore` 直接发送资料时，本节点独立加密保存并幂等回传，不创建私有池条目、不调用平台、不刷新凭据。后续任务按中央授权读取指定版本。

本地隔离 PostgreSQL、合成签名服务及浏览器用于验收，真实凭据和线上节点不因运行这些检查而变更。

## 连接与执行检查

Work 授权、TEAM 生产和本机凭据修复共同读取 `REG_FACTORY_PRODUCTION_PROXY_SOURCE`，选择公共住宅代理、公共Clash、直连或专用出口。授权页面、协议登录与Token交换使用启动时固定的出口；后续独立核验读取核验时的选择。公共连接只维护一次，注册流程仍保留各平台的选择和轮换策略。

登录校验请求遇到 TLS 握手、连接中断、超时或临时服务错误时，最多执行3次，重试间隔为1秒、3秒。日志显示当前次数与安全原因，不输出校验材料。证书验证失败和明确拒绝不自动重试。重试耗尽且尚未交换Token时，任务回传失败，主系统结束本次处理并保留正常的重新发起入口；已交换Token而结果未确认时继续沿原回执核对，不自动重新登录。

推荐从 WebUI 的“环境配置”页面执行测试。命令行也可检查 Clash：

```bash
python -m common.proxy_switch list
python -m common.proxy_switch current
python -m common.proxy_switch rotate
python _clash_verge.py ping
```

配置问题的常见表现和处理方式见 [常见问题](troubleshooting.md)。
