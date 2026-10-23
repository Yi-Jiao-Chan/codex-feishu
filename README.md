# codex-feishu

独立 Go CLI，将 Codex App 的共享远程 App Server 与飞书自建机器人连接起来，在 App 和飞书中继续同一个会话。

## 主要能力

| 能力 | 行为与说明 |
| --- | --- |
| 会话跟进 | 自动发现符合条件的 App 主会话和注册工作树，首条输入后建专属群；支持群聊与话题目标 |
| 群名与置顶 | 专属群跟随 App 标题；进入已确认的 Pinned 分区加 `📌`，取消后移除；开发实现，飞书 UI 验收待完成 |
| 双向同步 | 同步人类输入与公开 AI 回复；每轮更新过程卡，最终结果单独发送 |
| 队列与控制 | 普通消息进入原生队列；停止后发送新消息可继续，保留显式引导、移除、停止和继续入口，原消息 reaction 反馈状态 |
| 图片与附件 | 飞书图片使用原生 localImage，文件通过安全的本地路径引用读取，支持附件合批与恢复 |
| 飞书内创建 | `/new` 可携带图片、文件，选择远程目录与跟进位置；Git 仓库可选新工作树与基准分支，使用已提交内容 |
| 规则与提醒 | 精确或递归目录 mute、@ 开关、延后和免打扰；正文继续同步 |
| 审批与问答 | 同步与异步问答默认可在飞书回答；审批按类型开启，保留原生会话策略与实际回答证据 |
| 本地文件预览 | 可选口令保护的网页预览，支持 Markdown 阅读、代码高亮与行号、HTML 可视化 |

能力开关、协议边界和待验收项见[模块手册](docs/codex-feishu/README.md)。新配置默认开启同步与异步问答；已有的显式关闭和类型清单继续生效。图片附件、任务创建、审批和网页预览分别显式启用。

## 当前状态

当前为 development 候选，`phase1_released=false`。M0 真实 App / 飞书联调已通过，后续能力已有自动测试、协议核验和部分客户端确认；完整一期验收尚未全部完成。当前验收结论见[集中验收清单](docs/codex-feishu/12-live-acceptance-handoff.md)，分功能证据见[实施与验证记录](docs/records/README.md)。

仓库兼容清单已核验共享 daemon / CLI 0.162.0、0.162.1；目标环境仍须检查实际版本。普通输入复用原生 queue/add，不新建替代 app-server，不修改全局 Codex 配置。CLI 的外部输入回显和 fork 跟进不在当前交付范围内。

## 从源码开始

需要 [go.mod](go.mod) 要求的 Go 版本（当前 Go 1.25.6）和 Git。桥与 Codex App 连接的共享服务应运行在同一远程系统用户下；用户服务安装使用 Linux systemd。

```sh
git clone https://github.com/Yi-Jiao-Chan/codex-feishu.git
cd codex-feishu
go build -o bin/codex-feishu ./cmd/codex-feishu
./bin/codex-feishu version --json
```

准备飞书自建应用、权限、私有凭据和目录范围后，按[安装与配置指南](docs/codex-feishu/AGENT-SETUP.md)执行计划和核验。命令及副作用见 [CLI 参考](docs/codex-feishu/10-cli-and-setup.md)。正式安装仍受发布门槛约束；候选安装须明确使用 `--candidate-test`，不因此视为正式发布。

## 文档入口

| 目标 | 入口 |
| --- | --- |
| 找文档、了解存放规则 | [文档总览](docs/README.md) |
| 配置与使用 | [Agent 安装指南](docs/codex-feishu/AGENT-SETUP.md)、[CLI 参考](docs/codex-feishu/10-cli-and-setup.md)、[配置样例](docs/codex-feishu/examples/README.md) |
| 理解模块和接口 | [模块手册](docs/codex-feishu/README.md)、[公共契约](docs/codex-feishu/00-contracts-and-layout.md) |
| 开发、验证与提交代码 | [贡献指南](CONTRIBUTING.md)、[Agent 工作约束](AGENTS.md) |
| 查看方案与依据 | [技术方案](docs/plans/README.md)、[接口与交互调研](docs/research/README.md)、[设计预览](docs/design/README.md) |
| 判断验收和发布状态 | [集中验收清单](docs/codex-feishu/12-live-acceptance-handoff.md)、[发布门槛](docs/codex-feishu/11-integration-and-acceptance.md)、[实施记录](docs/records/README.md) |

## 贡献代码

从 `origin/main` 创建 `codex/` 主题分支，按范围验证，使用 `feat`、`fix`、`docs` 等类型提交，并通过 PR 合入 main。具体命令、提交内容、PR 模板及合并要求统一维护在 [CONTRIBUTING.md](CONTRIBUTING.md)。

开发包入口为 `go run ./cmd/dev-package --output /absolute/private/dist --json`，要求已提交且工作区干净。开发包只包含明确列入清单的二进制、指南、样例和兼容元数据，不含凭据、数据库、日志或真实对话；打包不会安装或启动服务。
