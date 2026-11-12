# 配置说明

本页维护配置操作与生效规则。字段目录来自 `schema/scripts.py`，当前部署与读写验证只在[验收](acceptance.md)维护。

## 统一配置中心

`/settings/` 是公共配置的编辑入口，按“运行与资源、网络与代理、浏览器、邮箱与取码、短信接码、图形验证、平台规则、导入与对接、其他配置”选择能力。供给中心只提供入口，原物料配置链接转到同一中心；邮箱池的设置页保留当前生效信息并进入同一编辑页，不再内嵌第二份公共配置。节点身份仍由原节点配置管理。

| 能力 | 模式及参数归属 |
| --- | --- |
| 浏览器 | `FINGERPRINT_BROWSER` 选择实际方式；通用参数与该方式的连接配置一起显示 |
| 网络出口 | `PROXY_MODE` 与六个平台覆盖决定展示的Clash/住宅配置；生产与凭据修复出口保持独立作用范围 |
| 邮箱来源 | ChatGPT、临时邮箱及Outlook辅助邮箱按原选择字段配置；同一供应商被多处使用时只显示一份连接，收码等待与邮箱池共用 |
| 短信接码 | 选择要配置的供应商，公共等待/重试在同一视图中保留；实际供应商在发起任务时选择，不新增全局启用开关 |
| 图形验证 | 按工具查看原参数，多个工具可并存；切换查看不替它们启用、停用或改变回退顺序 |

模式关系也由 `schema/scripts.py` 导出，页面不再复制一套键或默认值。浏览器、代理和邮箱的模式字段修改后需要保存；未选模式的原值和草稿均保留，“查看全部模式配置”与全局搜索可找到全部字段。自定义键保留在原配置里，不因切换方式或升级被清空。并存的下游对接和平台规则保持按对象编辑，不能将多个可同时运行的服务错误改成互斥开关。

管理员编辑流程：读取 → 选择能力/模式 → 编辑草稿 → 保存全部修改 → 回读确认。跨类别及模式切换保留全部草稿，页面说明保存涵盖哪些类别；全局搜索可找到任何原参数并显示归属。读取或保存失败保留旧值和草稿，可原位重试；保存响应丢失时重新读取对照，确认落盘后才清除对应草稿。离页取消读取，保存请求已受理则仍由原接口完成；未设置与默认值分开显示，不自动写默认值。离页沿用未保存提醒；秘密字段继续默认隐藏。

配置类别只改变组织方式，不删除原字段或自定义键。同一键的值、类型、默认值、只读及重启语义来自唯一schema；重复定义在该处修正，不在页面覆盖。节点身份保留原专用入口。分类选择同时复用供给与邮箱设置，不复制另一套配置。

## 前置条件

### 内置邮箱池：注册与登录授权

邮箱登录统一在“邮箱池 → 收件来源”维护：添加 IMAP 服务器、邮箱及应用授权码，保存后查看同步状态。重新登录同一个邮箱时编辑原来源；换成另一个收信邮箱时新增来源，确认转发关系后再调整地址绑定，不能把旧地址自动转给新账号。密码仅保存在节点的数据目录，不重复写入任务参数或配置文件。

“邮箱池 → 地址”导入实际用于注册或授权的地址并绑定来源。收信母邮箱不自动作为注册账号；从邮件发现的地址只作索引，经管理员导入确认后才能进入注册分配。重复导入保留原来源、暂停状态和使用记录，变更来源仍通过地址设置。注册使用已启用、来源可用且没有使用记录的地址；取到地址即登记占用，失败后沿原邮箱重新授权，避免再次注册同一个身份。

- ChatGPT 注册：任务的邮箱来源选“内置邮箱池”，或在配置中心设置 `CHATGPT_EMAIL_PROVIDER=icmail`。
- Claude 注册：任务的 provider 选“内置邮箱池”；Grok 使用 `TEMP_EMAIL_PROVIDER=icmail` 并启用原临时邮箱方式；Kiro 选择临时邮箱方式并将 provider 设为 `icmail`。这些入口共用同一个地址分配和取信实现。
- ChatGPT 重新授权：保存的 `mailbox_provider=icmail` 优先；没有明确邮箱来源及专用邮箱凭据时，可以沿用 `CHATGPT_EMAIL_PROVIDER=icmail`。始终按原账号邮箱绑定，绝不换一个地址取码。注册结果、OAuth导出和后续重新授权保留该标记。
- RT刷新不读邮箱，密码＋2FA的协议授权仍保留。暂停的地址、停用的来源、读取失败和等待超时会反馈原任务；修正来源后从原入口重新授权，不切换到外部邮箱供应商。

只有本次发起后新索引、发送时间符合要求且属于原地址/来源的邮件可用于取码，已使用验证码不重复提交。执行端只访问本节点的认证接口，不直接打开邮箱数据库。自动巡检仍默认关闭。

### 页面与执行配置一致性

每项保留文件中的原值，另显示配置来源：显式部署环境优先，其次保存文件，最后为默认值；部署环境中的秘密只显示是否已配置。配置来源不代表任务已成功调用供应商。邮箱来源、YYDS地址及iCloud地址/类型/服务共用 `schema/mail_settings.py` 的默认值，标注“留空使用默认”的字段清空后回默认；密码、API密钥和可选自定义地址清空后保持为空。

`ICMAIL_IMAP_*` / `MAIL_IMAP_*` 只在空来源库首次启动时预置来源，不覆盖已经由页面管理的登录资料。配置页明确标注这一点并链接当前收件来源；其它IC运行参数在重启后读取。原已保存配置、未选模式和自定义键不因升级清除。

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

在“配置中心 → 网络与代理”设置全局出口与平台覆盖；所选Clash或住宅代理参数在同一视图中展示，生产与凭据修复出口保留独立配置。注册出口支持三种互斥模式：

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

真实进程环境变量优先于 `.env`。WebUI 保存配置后，新任务立即使用新值，不需要重启主服务。

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

## 连通性检查

Work 授权与 TEAM 生产使用专用 `REG_FACTORY_PRODUCTION_PROXY`，为空时直连；授权页面、协议登录、Token 交换和生产核验使用同一出口。注册任务继续使用原平台/全局代理配置。不要用注册住宅代理的切换来修复生产任务。

登录校验请求遇到 TLS 握手、连接中断、超时或临时服务错误时，最多执行3次，重试间隔为1秒、3秒。日志显示当前次数与安全原因，不输出校验材料。证书验证失败和明确拒绝不自动重试。重试耗尽且尚未交换Token时，任务回传失败，主系统结束本次处理并保留正常的重新发起入口；已交换Token而结果未确认时继续沿原回执核对，不自动重新登录。

推荐从 WebUI 的“环境配置”页面执行测试。命令行也可检查 Clash：

```bash
python -m common.proxy_switch list
python -m common.proxy_switch current
python -m common.proxy_switch rotate
python _clash_verge.py ping
```

配置问题的常见表现和处理方式见 [常见问题](troubleshooting.md)。
