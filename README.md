# codex-feishu

独立 Go CLI：连接 Codex App 的共享远程 App Server 与飞书自建机器人。

M0 真实可行性联调已通过，包含 App 双向输入、排队/引导、fork/worktree/子代理分类，以及飞书卡片、reaction、引用回复和 @ 验证。当前整合 M1–M3，正式服务、配置向导和用户服务管理已有开发实现，完整一期仍待真实部署验收，不是一期发布版。

2026-10-12：用户将 fork 跟进暂缓，发现器保留 TODO 并记录 `fork_deferred`；普通 App 会话和注册工作树继续实现。M1 已有持久基线、发现/目录校验和建群意图基础，完整同步组合仍待完成，见 [M1 进展](docs/research/m1-discovery-foundation-2026-10-12.md)。

最新开发边界见[配置、服务管理与待处理提醒进展](docs/research/m3-setup-and-request-reminders-2026-10-12.md)和[管理通道、状态核验与离线取消](docs/research/m3-control-and-offline-cancel-2026-10-12.md)。开发版本可预览配置，正式 apply / 服务安装和启动保留发布门槛；status 对新版实例查询真实连接，旧 M0 退回只读快照和 pending。

只读 doctor 已检查配置、实际 daemon、机器人与 tenant 授权；启动后用 `--require-running` 核验原实例，不额外监听消息。version 内嵌权限能力清单，授权满足与实际行为分开，见[诊断与权限清单](docs/research/m3-doctor-and-permissions-2026-10-12.md)。

- [开发手册](docs/codex-feishu/README.md)
- [Agent 配置指南](docs/codex-feishu/AGENT-SETUP.md)
- [实施与验收](docs/codex-feishu/11-integration-and-acceptance.md)
- [M0 联调进展](docs/research/m0-live-integration-2026-10-10.md)
- [卡片交互参考](docs/design/cards-preview.html)（早期 Web 原型；新版以原生卡片与 08 手册为准）

文档已从邮件服务仓库迁移。CLI 回显与二期飞书创建暂不实现。

私聊配对已有自动测试通过的开发实现：新部署使用受锁保护的临时接收器，已有实例复用同用户管理通道，不写配置或重置 owner。真实配对 UI 尚待验收，见[配对与 Agent 配置指引进展](docs/research/m3-pairing-2026-10-12.md)。

默认 verify 已改为只读诊断，不再升级旧数据库；正式受控验收仍 pending，版本能力单独标识。见[验证命令与数据库保护](docs/research/m3-verify-safety-2026-10-12.md)。

已有通知规则的配置更新已通过本地故障恢复测试：保留关闭 @ / snooze，只修改本人明确选定的免打扰与时区；中断可重跑原计划，未完成事务阻止服务启动。当前 M0 配置和数据库没有迁移或切换。见[通知配置事务进展](docs/research/m3-policy-apply-2026-10-12.md)。

正式消息 outbox、reaction、自动建群和命令确认回复已接入持久限流等待，重启后按最新状态恢复；正文发送前重新核验 @ / 免打扰。App 待处理提醒已接入限流与只读核验，镜像和命令确认也支持发送前标记核验；真实服务部署与读回/UI 验收仍待完成。见[限流投递进展](docs/research/m3-rate-limit-delivery-2026-10-12.md)。

旧 M0 接管已有隔离测试通过的历史映射和原子存储基础：复用已有群及消息 ID，历史卡片只更新原卡；普通启动拒绝未接管的 schema 1。迁移计划 CLI、WAL 一致备份、共享计划锁和中断恢复已通过隔离测试；开发版仍拒绝真实 apply，服务切换和 UI 验收待完成，见[M0 接管进展](docs/research/m3-legacy-adoption-2026-10-12.md)。

开发验收包入口为 `go run ./cmd/dev-package --output /absolute/private/dist --json`。包只含本次构建、校验和、兼容 / 权限清单、自包含 Agent 指南、配置和验收模板，不包含本机密钥、真实配置、数据库或研究日志。必须先提交并保持 Git 干净；输出是 development 包，保留未发布边界，不安装、启动或替换服务。见[CLI 与配置手册](docs/codex-feishu/10-cli-and-setup.md)。

管理实例的本人测试卡组件已接入现有接收器：显式 card 授权与固定 request ID，原子领取和私有记录，重复 / unknown 不重发，回调与私聊探针不进入 Codex 输入。真实部署、受控群 / 线程及完整 P1 仍 pending。见[管理测试卡进展](docs/research/m3-managed-card-verification-2026-10-12.md)。

管理测试群组件已接入：固定本人和机器人，持久领取后一次创建，成员 GET 与群 @ 验证记录在原接收器内；不绑定线程、不发测试群消息。卡 / 群标志独立授权，查询不生成缺失的组件，完整 P1 和受控线程仍 pending。见[管理测试群进展](docs/research/m3-managed-group-verification-2026-10-12.md)。

隔离原生创建适配器已有受限制的开发入口与本地协议测试，固定本机模拟模型和只读策略，拒绝出生证明不符或缺失。管理线程编排与真实重验仍 pending，未开放完整 verify。见[隔离创建适配器进展](docs/research/m3-native-verification-adapter-2026-10-12.md)。

内部隔离创建步骤已补齐不可变请求日志、单次领取及随机连接身份，unknown 与重连不重新创建。绑定、输入、远端清理和真实 UI 仍待完成，完整 verify 未开放；见[创建日志进展](docs/research/m3-native-verification-journal-2026-10-12.md)。

受控验收的私有目录和固定本机模拟模型已接入内部创建步骤，包含时序控制、五分钟资源期限、取消与重启保护。开发管理通道 / CLI 已提供显式 thread 创建组件，仍不启动 turn、绑定群、提交输入或清理远端；完整验收待完成，见[本地资源进展](docs/research/m3-native-verification-resources-2026-10-12.md)和[管理创建组件](docs/research/m3-managed-thread-creation-2026-10-12.md)。

验收群与线程的内部绑定基础已补齐：同一请求的不可变证据、最新群成员核验、原连接上的无覆盖附着和本地原子登记。当前尚未接入运行时授权、激活或 CLI 绑定入口，不提交输入或投递消息；完整验收仍 pending。见[绑定基础进展](docs/research/m3-verification-binding-foundation-2026-10-12.md)。

专属操作授权、Runtime 激活和控制器逐次原生写入检查已有隔离实现；标记失效不退回普通授权，资源 / 原连接关闭或原窗口到期取消操作。完整飞书投递上下文与 CLI 绑定入口仍待接入，一期未发布。见[操作授权进展](docs/research/m3-verification-operation-guards-2026-10-12.md)。
