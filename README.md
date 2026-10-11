# codex-feishu

独立 Go CLI：连接 Codex App 的共享远程 App Server 与飞书自建机器人。

M0 真实可行性联调已通过，包含 App 双向输入、排队/引导、fork/worktree/子代理分类，以及飞书卡片、reaction、引用回复和 @ 验证。当前进入 M1 开发，尚未交付自动建群与完整一期，不是一期发布版。

- [开发手册](docs/codex-feishu/README.md)
- [Agent 配置指南](docs/codex-feishu/AGENT-SETUP.md)
- [实施与验收](docs/codex-feishu/11-integration-and-acceptance.md)
- [M0 联调进展](docs/research/m0-live-integration-2026-10-10.md)
- [卡片交互参考](docs/design/cards-preview.html)（早期 Web 原型；新版以原生卡片与 08 手册为准）

文档已从邮件服务仓库迁移。CLI 回显与二期飞书创建暂不实现。
