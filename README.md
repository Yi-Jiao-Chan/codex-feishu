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

正式消息 outbox、reaction 和自动建群已接入持久限流等待，重启后按最新状态恢复；正文发送前重新核验 @ / 免打扰。真实服务部署、命令回复等其他网关操作的恢复仍待完成。见[限流投递进展](docs/research/m3-rate-limit-delivery-2026-10-12.md)。

旧 M0 接管已有隔离测试通过的历史映射和原子存储基础：复用已有群及消息 ID，历史卡片只更新原卡；普通启动拒绝未接管的 schema 1。迁移计划 CLI、WAL 一致备份、共享计划锁和中断恢复已通过隔离测试；开发版仍拒绝真实 apply，服务切换和 UI 验收待完成，见[M0 接管进展](docs/research/m3-legacy-adoption-2026-10-12.md)。
