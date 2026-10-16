# Codex × 飞书模块手册

本目录描述模块接口、行为约束和验收要求。安装从 [AGENT-SETUP](AGENT-SETUP.md) 开始，开发与提交按 [CONTRIBUTING](../../CONTRIBUTING.md) 执行；其他方案、调研和实施记录从 [文档总览](../README.md) 查找。

## 阅读与证据边界

开发先读 00 公共契约，再读本次涉及的模块；跨模块变更同时核对依赖模块。M0 的真实 App / 飞书可行性联调已通过，完整一期发布仍以 [11 验收要求](11-integration-and-acceptance.md) 和 [12 集中清单](12-live-acceptance-handoff.md) 为准。

模块中的日期化进展只描述当时版本，不能据此推导新版本或其他客户端已通过。实际命令和能力结合当前代码、version 输出和 compat 清单核对；按日期记录的实施过程统一见 [records](../records/README.md)。`pending` 不因代码合并、mock 或平台 API 返回成功而提升为 `pass`。

## 模块索引

| 编号 | 手册 | 主要代码与职责 |
| --- | --- | --- |
| 00 | [公共契约与工程布局](00-contracts-and-layout.md) | 标识、接口、unknown 语义和跨模块边界 |
| 01 | [共享 Codex 客户端](01-codex-client.md) | internal/codexclient、compat：共享 daemon、协议与原生队列 |
| 02 | [存储与恢复](02-store-and-recovery.md) | internal/store：迁移、收据、attempt 与 outbox |
| 03 | [飞书网关](03-feishu-gateway.md) | internal/feishu：消息、权限、长连接与群 / 话题 API |
| 04 | [发现与绑定](04-discovery-and-bindings.md) | internal/discovery、internal/bindings：基线、创建意图与目标映射 |
| 05 | [消息同步](05-message-sync.md) | internal/sync：公开历史、附件展示与投递 |
| 06 | [输入队列与引导](06-queue-and-steering.md) | internal/submissions：排队、显式 steer 与恢复 |
| 07 | [规则与通知](07-rules-and-notifications.md) | internal/rules：目录、mute 与 @ 策略 |
| 08 | [卡片与动作](08-cards-and-actions.md) | internal/cards：视图、操作令牌、回调和反馈 |
| 09 | [飞书创建任务](09-feishu-task-creation.md) | internal/creation：目录、工作树、群 / 话题与创建恢复 |
| 10 | [CLI、配置与安装](10-cli-and-setup.md) | cmd/codex-feishu、config、setup、userservice：配置与服务生命周期 |
| 11 | [集成与验收](11-integration-and-acceptance.md) | 实施顺序、隔离边界、完整发布门槛 |
| 12 | [集中验收清单](12-live-acceptance-handoff.md) | 当前构建、真实客户端确认与剩余项 |
| 13 | [审批与用户问答](13-feishu-approvals-and-questions.md) | internal/interactions：原生请求、飞书处理与实际结果 |
| 14 | [本地文件网页预览](14-file-preview.md) | internal/filepreview：受控文件登记、口令和浏览器展示 |

## 使用指南与样例

- [Agent 一键配置指南](AGENT-SETUP.md)：检查实际能力、身份、目录和提醒设置，生成配置计划并按授权应用。
- [配置与消息样例](examples/README.md)：正式配置、向导输入、卡片、用户服务和验收报告模板。
- [CLI 参考](10-cli-and-setup.md)：命令、副作用、退出码与候选安装边界。

`AGENT-SETUP.md` 与交付用 examples 文件被打包器明确引用；文档重组不改变这些交付路径。

## 重点功能的设计与记录

| 功能 | 方案 / 依据 | 实施记录 |
| --- | --- | --- |
| 双向同步 | [已确认实施方案](../plans/codex-feishu-implementation-proposal-2026-10-10.md) | [M0 联调](../records/m0-live-integration-2026-10-10.md)、[正式闭环](../records/m3-managed-roundtrip-2026-10-12.md) |
| 图片与文件输入 | [附件方案](../plans/feishu-inbound-attachments-plan-2026-10-15.md)、[入站资源核验](../research/feishu-inbound-resources-2026-10-15.md) | [附件实施](../records/feishu-inbound-attachments-implementation-2026-10-15.md) |
| 飞书创建与工作树 | [任务创建方案](../plans/feishu-create-codex-session-plan-2026-10-15.md)、[目录设计](../design/feishu-create-directory-ui-redesign-2026-10-15.md) | [创建实施与验收](../records/feishu-create-codex-session-implementation-2026-10-15.md) |
| 网页预览 | [浏览器链接方案](../research/feishu-local-file-browser-links-options-2026-10-15.md) | [预览实施](../records/feishu-local-file-preview-implementation-2026-10-15.md) |

## 跨模块约定

- 复用共享服务和原会话，普通输入 queue/add，显式引导才 steer；标识与返回分类以 00 为准。
- 远程变更先记账，unknown 先核验；不要在 SQLite 事务中等待网络。
- 目录与工作树校验复用 07 的规范化规则；卡片不绕过后端授权。
- 真实客户端验收、原生协议实验、平台 API 核验和本地测试分开记录；新模块不以旧模块的验证替代自身证据。
