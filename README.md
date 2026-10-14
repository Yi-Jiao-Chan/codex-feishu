# codex-feishu

独立 Go CLI：连接 Codex App 的共享远程 App Server 与飞书自建机器人。

M0 真实可行性联调已通过。一期候选 `serve` 已实际部署，用户确认了自动建群、双向回显、默认排队与追加引导、停止/继续、目录 mute、@ 免打扰、手机布局和重启后跟进。独立配置 Agent 已验证首次安装、重复配置和卸载保留数据；私聊配对也已通过。完整一期矩阵仍有待验收项，当前是 development 版本，`phase1_released=false`。构建身份及剩余项见[集中验收记录](docs/codex-feishu/12-live-acceptance-handoff.md)。

## 一期开发实现

- 发现 App 主会话与注册工作树，首条实际输入后创建专属群；首装历史基线、草稿和子代理排除。fork 按用户要求暂缓，发现器保留 TODO。
- 镜像 Codex 人类输入与公开 AI 回复；每轮更新一张过程卡，最终结果独立发送。飞书输入不重复镜像，过程、结果和操作卡引用原消息。
- 普通飞书输入走原生 queue/add；只有点击追加引导才 steer。支持移除、停止、明确恢复与原消息 reaction。
- 精确/递归目录 mute、注册工作树规则，以及 @ 的 off/on/snooze/until/跨午夜 quiet；正文继续同步，休息时间不积压提醒。
- SQLite schema 25 保存绑定、收据、镜像、操作尝试、通知规则及原生问答实际结果；限流等待可恢复，未知结果保守读回，不盲重试。
- 配置预览/原子应用、私聊配对、只读 doctor/status、用户服务管理，以及受控卡/群/线程/绑定组件。管理组件复用原接收器和原生连接，不自动提交测试输入。
- 飞书卡片处理原生审批与问答，完成后展示实际回答。命令审批（允许本次）和同步问答已通过真实 App / 飞书验收并在本机常用候选开启；文件、网络、额外权限与异步问答仍待联调。新配置默认关闭，显式按类型启用，见[扩展验收记录](docs/codex-feishu/13-feishu-approvals-and-questions.md)。

以上同时有隔离测试、原生协议证据和部分真实 App / 飞书确认；全部一期矩阵尚未通过。默认 `verify` 与受控组件 observed 仍保留完整 P1 pending；显式 `--acceptance-report` 可只读核验已有的完整案例报告、人工确认、证据摘要与当前构建/部署，不能以 mock 或诊断就绪替代真实 UI，也不自动发布。

## 配置与交付

- [Agent 一键配置指南](docs/codex-feishu/AGENT-SETUP.md)：独立提供给配置 Agent 即可；按实际能力检查、身份、目录与提醒设置执行。
- [开发手册](docs/codex-feishu/README.md)：模块接口与开发约束。
- [CLI 与安装契约](docs/codex-feishu/10-cli-and-setup.md)：命令、事务、恢复和退出码。
- [一期验收矩阵](docs/codex-feishu/11-integration-and-acceptance.md)：P1-01 至 P1-12 全部通过后才正式发布。
- [M0 真实联调证据](docs/research/m0-live-integration-2026-10-10.md)、[正式服务消息闭环隔离证据](docs/research/m3-managed-roundtrip-2026-10-12.md)。

正式 apply/install/start 保留发布门槛。显式 `--candidate-test` 可用于发布前安装联调，要求明确的配置/数据库和支持该能力的二进制；候选服务绑定二进制摘要，拒绝替换活动 unit 和迁移既有数据库。旧 M0/schema 1 接管已有隔离测试，但真实迁移和服务切换尚未执行。

本机已验证的共享 daemon/CLI 版本为 **0.162.0、0.162.1**，当前扩展联调使用 0.162.1；实际目标机器仍须 doctor 核验，不自动创建新 app-server 或改变原会话权限。开发包入口为 `go run ./cmd/dev-package --output /absolute/private/dist --json`，要求已提交且 Git 干净；包含二进制、校验和、兼容/权限清单、自包含指南和验收模板，不含本机凭据、数据库或真实对话。打包不安装或启动服务。

CLI 界面的外部输入回显暂不实现；飞书内创建任务、目录卡和群/话题选择属于二期。
