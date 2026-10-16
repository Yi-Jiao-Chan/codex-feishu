# 文档总览

从项目介绍和本地构建开始，阅读根目录 [README](../README.md)；准备改代码，先看 [CONTRIBUTING](../CONTRIBUTING.md) 与 [AGENTS](../AGENTS.md)。本目录负责模块契约、技术方案、研究依据及验证记录。

## 按目的阅读

| 目的 | 推荐顺序 |
| --- | --- |
| 安装或重新配置 | [配置指南](codex-feishu/AGENT-SETUP.md) → [CLI 参考](codex-feishu/10-cli-and-setup.md) → [配置样例](codex-feishu/examples/README.md) |
| 修改已有模块 | [公共契约](codex-feishu/00-contracts-and-layout.md) → [模块手册](codex-feishu/README.md) → 对应方案和记录 |
| 开发飞书内创建与工作树 | [09 任务创建](codex-feishu/09-feishu-task-creation.md) → [创建方案](plans/feishu-create-codex-session-plan-2026-10-15.md) → [实施记录](records/feishu-create-codex-session-implementation-2026-10-15.md) |
| 开发图片与文件输入 | [05 消息同步](codex-feishu/05-message-sync.md) → [附件方案](plans/feishu-inbound-attachments-plan-2026-10-15.md) → [实施记录](records/feishu-inbound-attachments-implementation-2026-10-15.md) |
| 判断能否发布 | [集成与验收](codex-feishu/11-integration-and-acceptance.md) → [集中验收清单](codex-feishu/12-live-acceptance-handoff.md) → 对应原始证据 |

## 目录职责

```text
docs/
├── README.md                 文档入口、分类与维护规则
├── codex-feishu/             00–14 模块手册、安装指南
│   └── examples/             配置、卡片和验收报告模板
├── plans/                    技术方案与范围决策
├── research/                 版本化接口、协议和交互调研
├── records/                  按日期保存的实施、联调和验证记录
│   ├── evidence/             已脱敏的结构化证据
│   └── probes/               历史探针及其结果
└── design/                   交互设计与静态预览
```

| 分类 | 内容与索引 |
| --- | --- |
| [模块手册](codex-feishu/README.md) | 按模块定位接口、行为约束、恢复和验收要求；安装包依赖的指南和 examples 保留稳定路径 |
| [技术方案](plans/README.md) | 目标、范围、约定与实现设计；描述方案状态，不代替实施结论 |
| [调研](research/README.md) | 记录官方来源、核验版本、能力限制与判断依据 |
| [实施与验证记录](records/README.md) | 按日期保留当时的实现、构建、检查和人工反馈；记录不是当前操作手册 |
| [设计与预览](design/README.md) | 交互设计说明及随仓库保存的 HTML 预览 |

## 维护规则

1. **入口各有职责。** 根 README 说明项目和上手方式；AGENTS 约束 Agent；CONTRIBUTING 规定开发、验证和提交；本文负责文档导航。详细接口只在对应模块维护。
2. **先查已有位置。** 模块规则变更直接更新原手册；新方案放 plans，能力核验放 research，实施和测试结果放 records，静态设计放 design。新文档同时补分类索引。
3. **文件名可预测。** 模块手册保留现有 `00–14` 编号；新增主题使用小写英文短横线命名，日期记录用 `topic-YYYY-MM-DD.md`，版本研究可带版本号。入口统一为 `README.md`，Agent 指令统一为根 `AGENTS.md`。
4. **标明文档性质。** 新方案、调研和记录开头写日期、适用范围、状态及对应模块；有结论时附来源或证据边界。实施状态使用明确的 planned / implemented / verified / pending，避免仅写“已完成”。
5. **保留历史语境。** records 中的旧版本、步骤和未完成项保留当时事实；后续更正附日期说明。当前实现以代码、兼容清单和模块约束共同核对，当前客户端验收以集中清单及对应人工证据为准。
6. **链接随迁移更新。** 使用仓库相对 Markdown 链接和可用锚点。移动时同时更新入站引用、出站引用和目录索引；不要提交仅在个人机器存在的绝对路径链接。安装包固定路径变化还要更新打包清单及测试。
7. **只保留可公开的仓库材料。** 样例使用合成数据，证据先脱敏；凭据、真实正文、数据库和原始运行日志保存在仓库外。探针脚本不是普通测试，不因文档检查而运行。
8. **验证后交付。** 执行 `python3 scripts/check_docs.py` 和 `git diff --check`。代码或交付路径变化按 [贡献指南](../CONTRIBUTING.md#验证要求)追加检查；平台 API、mock 与真实 UI 的验证结果分别记录。

## 本次目录调整

原 `docs/research/` 中的技术方案迁入 plans，实施与阶段验证迁入 records，结构化证据和探针集中到 records 的对应子目录；目录选择设计迁入 design。文件主题、日期和证据内容保留，链接随新位置调整。

`docs/codex-feishu/` 保持为稳定模块入口，其中的 `AGENT-SETUP.md` 和 `examples/` 继续被开发包白名单引用。后续新增文档遵循上述分类，不再把实施进展堆入 research。
