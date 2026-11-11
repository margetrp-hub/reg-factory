# reg-factory

reg-factory 是独立账号生产与凭据管理节点。当前 SG 服务由 Go `regd` 管理任务、账号投影、配置和日志，Next.js 静态页面内嵌到程序，Python worker 执行平台流程。Core 负责平台商品、库存、订单、权益和资金；SG 只接受明确授权并返回证据，不建第二份平台业务账。

本仓维护在 [margetrp-hub/reg-factory](https://github.com/margetrp-hub/reg-factory)。源码版本见[VERSION](VERSION)，实际上线身份、验证范围与恢复材料只在[当前验收](docs/acceptance.md)维护，不能用源码版本推断服务器已升级。

## 选择正确入口

| 使用场景 | 入口与默认端口 | 文档 |
| --- | --- | --- |
| SG/服务器控制台 | Go regd，宿主回环8800；单个panel容器，按需启动Python worker | [构建与部署](deploy/README-GO.md)、[SG环境差异](deploy/README-OCI-SG.md) |
| Python本地工具 | `start.bat` / `start.sh` 启动 `webui.server:app`，默认8799 | [本地教程](docs/getting-started.md)、[CLI](docs/cli.md) |
| Aishop Work接入 | 节点监听及签名协议，地址来自私有部署配置 | [接口说明](docs/api.md#work-机器接口) |
| 独立K12 | 自有服务、端口及数据目录，不代替SG控制面 | [K12](k12/README.md) |

Python本地入口仍在源码中，不宣称已删除；它不是SG生产面板。8799与8800的鉴权、路由和领取语义不同，调用前先辨认服务。不要为同一生产数据目录启动第二个控制面。

## 日常操作

- 账号页导入/查看资料，按规范化身份计数，原始来源可展开；已领取、已生产与待核验分别显示。
- AT测试只读当前AT，不交换RT、不推进凭据保存时间；RT刷新和重新授权是显式操作。已保存结果优先自动恢复，未知外部执行沿原任务核对。
- 任务受理后由服务端持久保存，关闭页面不取消任务；运行页提供进度、日志、失败原因及原任务恢复操作。
- 系统配置按类别与对象编辑，共用原配置文件；切换查看不改变启用供应商，跨类草稿一起保存，失败保留草稿。
- 新生产与已有成品恢复是两种模式。生产选账号时由节点测活；Core经Work读目录并按自身授权/库存事实筛选，不能将节点可用数当商城库存。

完整状态机和数据责任见[架构](docs/architecture.md)，字段含义见[配置](docs/configuration.md)，操作失败见[排错](docs/troubleshooting.md)。

## 开发与验证

在隔离数据及配置下执行相关测试；真实账号、上游费用和生产数据不用于默认回归。

```sh
# 仓库根：Python执行层
python -m unittest discover -s tests
# server目录：Go控制面与节点
go test ./...
# 仓库根：前端
pnpm --dir server/web install --frozen-lockfile
pnpm --dir server/web build
```

Go编译的前端复制与schema生成顺序见部署文档。构建成功不等于真实账号操作或已部署，实际执行范围写入验收页。

## 文档责任

| 主题 | 唯一维护位置 |
| --- | --- |
| 当前运行、证据、未完成及恢复位置 | [docs/acceptance.md](docs/acceptance.md) |
| owner、状态机、账号/凭据/任务关系 | [docs/architecture.md](docs/architecture.md) |
| 配置操作、优先级及生效方式 | [docs/configuration.md](docs/configuration.md) |
| Go面板、Work机器及Python本地协议 | [docs/api.md](docs/api.md) |
| 构建、发布与程序恢复 | [deploy/README-GO.md](deploy/README-GO.md) |
| 本地脚本、辅助工具 | [CLI](docs/cli.md)、[工具索引](tools/README.md) |
| 最近发行变化 | [CHANGELOG.md](CHANGELOG.md) |

架构、配置和部署文档不追加发布流水；更新时替换失效口径。旧测试计数、候选和清理过程从Git历史及私有回执追溯，失败/未知与未完成责任先收进现役owner再退役。版本限定的 `.github/release-notes/` 只解释对应发行，不充当当前手册。

## 来源与运行数据

本项目基于 [tiantianGPU/reg-factory](https://github.com/tiantianGPU/reg-factory) 持续整合；上游便携包及教程不代表本仓SG发行。第三方代码保留各目录的许可证与来源记录，例如[Plus适配来源](vendor/chatgpt_plus/UPSTREAM.md)、[GoPay来源](vendor/gopay_engine/UPSTREAM.md)。

仅用于经授权的操作。密钥、账号、Cookie、Token、日志及数据卷与源码分离，不提交到仓库。SG独立SQLite任务/索引库不是Aishop Next七组件的运行库；主系统PostgreSQL-only规则不要求删除SG数据。当前凭据及结果保留/清理责任见架构文档。
