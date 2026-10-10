# 00 公共契约与工程布局

适用：两期全部模块。这里定义桥的领域接口；Codex 和飞书的 wire 字段仍以固定版本的生成协议及官方 SDK 为准。

## 工程边界

建议新建独立仓库 `codex-feishu`，复制本手册到该仓库 docs。当前工作区是文档承载位置，不修改邮件服务的 go.mod、数据库、RPC 或运行配置。

```text
cmd/codex-feishu/             CLI 入口
internal/config/             配置解析与校验
internal/domain/             最少量共享 DTO、标识和状态
internal/codexclient/         Unix WebSocket JSON-RPC 与协议适配
internal/store/              SQLite、迁移、收据与 outbox
internal/feishu/              官方 SDK、原始事件和出站 API
internal/discovery/           候选会话与补偿扫描
internal/bindings/            群 / 话题创建意图与映射
internal/sync/                公共内容镜像与历史补齐
internal/submissions/         原生队列、引导和核验
internal/rules/               目录与通知策略
internal/cards/               卡片视图模型、JSON 和动作令牌
internal/creation/            二期创建草稿
internal/setup/               配置计划、应用、配对、服务安装
internal/app/                 组合根与生命周期
compat/                      验收版本和权限能力清单
testdata/                    脱敏协议录制、卡片及故障样例
docs/                        手册、配置指南和版本证据
```

Go 版本按所选 SDK 的最低要求确定并固定；SQLite 驱动优先纯 Go，Unix WebSocket 使用能注入 Unix dialer 的维护中 Go 库。M0 记录具体依赖版本和构建结果，再提交 go.mod / go.sum。正常继承 GOCACHE、GOMODCACHE，不在测试脚本中覆盖或清理它们。

## 标识与归属

| 标识 | 唯一范围及用途 |
| --- | --- |
| host_id | 首次配置生成并持久保存的部署 ID；不随机器改名或重启变化 |
| thread_id | 与 host_id 合成 ThreadKey；必须是 App 的同一会话 ID |
| turn_id / item_id | 原生轮次 / 内容标识；不能用文字或时间戳代替 |
| feishu message_id | 入站消息去重；与 app_id 一起唯一 |
| queued_submission_id | 原生 queue/add 返回 queuedSubmission.id；后续删除按此值定位 |
| receipt_id | 桥内部输入收据 ID；持久关联一条原始飞书输入 |
| client_user_message_id | 为收据生成的稳定相关 ID，对应历史 userMessage.clientId；**不是 Codex 的去重键** |
| action_token / action_revision | 服务器生成的卡片动作授权与版本；防重复和旧状态操作 |
| draft_id / create_intent_id | 创建流程的持久标识；重试从已完成步骤继续 |

ThreadKey = `(host_id, thread_id)`。FollowTarget 只有两种：`group(chat_id)` 或 `topic(chat_id, root_message_id, thread_id_if_available)`。单个有效绑定只持有一种目标；换绑须明确操作并留记录，不自动把话题降级为主群。

操作者 ID 和通知目标统一使用该飞书应用下的 **open_id**。operator 可提交任务与管理目录规则；notify target 不自动获得执行权限。每个接收用户可以控制自己的通知策略。

## 模块交接数据

| DTO | 必需内容 |
| --- | --- |
| ThreadCandidate | ThreadKey、cwd、可核验的项目根、projectId、父线程、持久/临时、来源元数据、首条人类 item |
| NativeEvent | 连接代次、method、ThreadKey、turn/item 标识、原始 payload；不把连接内顺序当持久日志序号 |
| InboundMessage | app_id、event_id、message_id、sender_open_id、chat_id、chat_type、root/thread ID、mention 列表、正文、时间 |
| SubmissionReceipt | 收据 ID、目标会话、原飞书 ID、相关 ID、输入内容、native queue / turn / item、阶段和核验证据 |
| MirrorDocument | 角色、正文、公开进度、来源、生成/完成状态、对应 item、目标位置；不含隐藏 reasoning |
| CardAction | 操作者、消息 ID、绑定 ID、服务器令牌、版本；客户端提交的目录/会话不能直接成为执行目标 |
| MentionDecision | 当前时刻可提醒的目标 open_id 列表、被抑制原因、策略版本 |

## 最小接口约定

以下为语义接口，开发时转换为少量 Go interface；不要为了每个 DTO 都造框架。

```text
CodexClient:
  DiscoverEndpoint -> endpoint + actual_server_version
  Connect / Initialize / SubscribeEvents
  AttachExisting(thread)              // resume，无会话参数覆盖
  ReadMetadata / ListThreads / PageTurns / PageItems
  ListQueue / AddQueue / DeleteQueue / Steer(expectedTurnId)
  Interrupt / StartQueue
  StartThread / ListProjects         // 二期

Store:
  Transaction / ClaimInbound / ClaimAction
  GetBinding / SaveCreateIntent / SaveStepResult
  SaveReceipt / SaveAttempt / ScheduleReconcile
  UpsertMirror / EnqueueOutbound / AdvanceHistoryWatermark

FeishuGateway:
  ReceiveMessages / ReceiveActions
  CreateGroup / VerifyMembership
  SendOrReply(target, document, stable_delivery_id)
  UpdateMessage / ReadOwnDelivery

Rules:
  CanAutoBind(candidate)
  CanOperate(sender, binding, action)
  DecideMentions(targets, event_kind, send_time)
```

跨模块事务由上层用例组合；不能在 SQLite 事务中等待远程 RPC。单会话 actor 只序列化本桥的写操作，不能阻止 App 在另一连接上同时操作。

## 返回结果分类

每个会改变远程状态的调用返回三类结果：`confirmed`（已知成功及标识）、`rejected`（明确未接受）、`unknown`（可能接受，响应丢失）。连接超时不等于 rejected。unknown 必须进入持久核验，不能由通用重试器重发。

卡片状态另有 `received`、`queued`、`executing`、`steering`、`confirmed`、`reconciling`、`failed`、`removed`。队列 accepted 不等于已执行；deleted=false 不等于“已成功追加”。

## 本模块交付与验收

1. 独立项目可构建；没有邮件服务依赖或全局 Codex 配置变更。
2. 一份固定版本的领域契约、协议能力清单和依赖锁定记录。
3. 其他模块能按上述标识关联同一输入、线程及卡片；unknown 不会被错误重试。
4. 新增接口只覆盖当前两期用例；CLI 回显、多主机调度、Web 管理界面不进入首版。
