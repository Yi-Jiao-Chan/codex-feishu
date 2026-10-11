# codex-feishu

独立 Go CLI：连接 Codex App 的共享远程 App Server 与飞书自建机器人。

M0 真实可行性联调已通过，包含 App 双向输入、排队/引导、fork/worktree/子代理分类，以及飞书卡片、reaction、引用回复和 @ 验证。当前整合 M1–M3，正式服务、配置向导和用户服务管理已有开发实现，完整一期仍待真实部署验收，不是一期发布版。

2026-10-12：用户将 fork 跟进暂缓，发现器保留 TODO 并记录 `fork_deferred`；普通 App 会话和注册工作树继续实现。M1 已有持久基线、发现/目录校验和建群意图基础，完整同步组合仍待完成，见 [M1 进展](docs/research/m1-discovery-foundation-2026-10-12.md)。

最新开发边界见[配置、服务管理与待处理提醒进展](docs/research/m3-setup-and-request-reminders-2026-10-12.md)和[管理通道、状态核验与离线取消](docs/research/m3-control-and-offline-cancel-2026-10-12.md)。开发版本可预览配置，正式 apply / 服务安装和启动保留发布门槛；status 对新版实例查询真实连接，旧 M0 退回只读快照和 pending。

- [开发手册](docs/codex-feishu/README.md)
- [Agent 配置指南](docs/codex-feishu/AGENT-SETUP.md)
- [实施与验收](docs/codex-feishu/11-integration-and-acceptance.md)
- [M0 联调进展](docs/research/m0-live-integration-2026-10-10.md)
- [卡片交互参考](docs/design/cards-preview.html)（早期 Web 原型；新版以原生卡片与 08 手册为准）

文档已从邮件服务仓库迁移。CLI 回显与二期飞书创建暂不实现。
