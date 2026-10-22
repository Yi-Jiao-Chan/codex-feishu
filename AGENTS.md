# AGENTS.md

本文件适用于整个仓库，供代码 Agent 执行任务时读取。项目介绍见 [README](README.md)，开发与提交步骤见 [CONTRIBUTING](CONTRIBUTING.md)，文档分类见 [docs/README](docs/README.md)。文件名统一使用 `AGENTS.md`。

## 开始工作

1. 先检查 `git status --short --branch`，保留其他开发者的修改，确认本次任务范围。
2. 阅读 [贡献指南](CONTRIBUTING.md)、[模块索引](docs/codex-feishu/README.md) 和当前模块手册；涉及历史方案时再按需阅读 plans、research、records。
3. 采用满足需求的最小改动，复用现有接口；不要顺带重构无关模块或修改运行环境。

## 实现约束

- 复用现有 daemon 和原线程；不新起 app-server / exec resume，不修改全局 Codex 配置或原会话策略。
- 普通输入使用原生 queue/add；只有用户点击追加引导，或异步问题卡的“提交回答并追加”，才 steer。`clientUserMessageId` 不是原生去重键。
- 远程状态变更在发出前持久记录；结果 unknown 时先核验，不盲重试或创建替代资源。
- 凭据、真实对话正文、数据库及运行日志不得进入 Git、普通日志或诊断输出。样例使用合成身份和内容。
- Go build/test 继承 `GOCACHE`、`GOMODCACHE`，不覆盖或清理，除非用户明确要求隔离缓存。
- M0 真实 App 与飞书联调是继续 M1–M3 的前提；协议模拟和平台 API 读回不替代客户端 UI 验收。

## 文档与交付

- 用户行为、配置、协议或持久化规则变化时，同步修改对应模块手册；新增方案、研究、实施记录按 [文档归档规则](docs/README.md#维护规则) 放置。
- 验证按 [CONTRIBUTING](CONTRIBUTING.md#验证要求) 执行。只改文档时检查本地链接和 diff；真实探针仅在明确授权的隔离资源中运行。
- 提交与 PR 使用 [提交规范](CONTRIBUTING.md#提交与-pr)，不混入其他任务的修改。发布、合并和部署各自按用户授权范围执行。
- 交付说明变更、验证结果与未完成项。发布逐项核验一期矩阵，`pending` 不得写成 `pass`；合并代码不会自动提升 `phase1_released`。
