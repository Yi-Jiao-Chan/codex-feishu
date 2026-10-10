# 03 飞书网关

负责 `internal/feishu/`、`compat/feishu-permissions.json`、脱敏事件样例。依赖 00、02；封装平台行为，业务模块不直接操作 SDK。

## 接入与发布条件

使用飞书企业自建应用，启用机器人，通过官方 Go SDK 长连接接收事件，不要求公网回调服务。当前 M0 锁定 Go SDK v3.12.0，使用 ws + event dispatcher + core.Request，以避免 Channel 发送降级和不明确的重试；该版本已有 Channel，但本项目不使用其出站默认分支。SDK 版本升级须重新核验生命周期和发送语义。[官方 SDK Channel 文档](https://github.com/larksuite/oapi-sdk-go/blob/v3_main/doc/channel.zh.md)

订阅 `im.message.receive_v1` 和 `card.action.trigger`；接收范围须包含操作者与测试群。SDK 负责长连接协议与事件解析，桥负责权限、去重、路由和业务确认。Channel 启动/停止生命周期按固定版本文档执行，不能停止后再次 Start 同一个终态 ws.Client。

准确权限 ID、替代权限的 OR 关系及当前实测边界见[权限研究](../research/feishu-m0-permissions-2026-10-10.md)。现有应用有多项同时授权，请求成功不证明每项都必需；自身群消息读回与官方通用群消息权限说明存在差异，保留独立 capability 状态，不能据此申请全群读取。

发布包必须携带权限能力清单：API、具体权限 ID、官方来源、已验收 SDK 版本、检查方式。下表是必须满足的能力；不要把文档里的中文名称冒充精确权限 ID。M0 用当时平台控制台/官方接口文档落实这些 ID，setup 按发布清单展示申请项。

| 必需能力 | 使用位置 | 如何验收 |
| --- | --- | --- |
| 机器人私聊收消息 | 配对、控制命令、二期创建 | 本人私聊收到 message event |
| 群内 @ 机器人消息 | 同群跟进 | @ 有事件，不 @ 不触发任务 |
| 卡片操作回调 | 引导、移除、停止、设置 | 点击真实卡片收到回调 |
| 以机器人发送 / 回复 / 更新消息 | 镜像与状态 | 收到消息，原卡可更新 |
| 创建群聊及管理所需成员 | 自动建群 | 建一个本人和机器人的测试群 |
| 读取群、成员及必要的自身投递记录 | 重建绑定、超时核验、@ 检查 | 能读回已建群 / 目标成员 / 自己的消息；成员最小 `im:chat.members:read` 已发布并实际通过 |
| 原消息状态 reaction | 接收 / 排队 / 执行 / 完成反馈 | 添加、替换并仅删除本机器人 reaction；`im:message.reactions:write_only` |
| 话题回复 | 二期 | 原始 root/thread 信息保留，回复留在话题 |

控制台配置、版本发布和组织审批不能通过“拿到 tenant token”替代。doctor 检查不出来的权限标 pending，真实 probe 才能将其变为 pass。不要为了接收 @ 消息申请读取所有群消息，除非平台当前权限模型确有需要并在发布清单解释。

## 入站归一化

保留原始 message_id、sender.open_id、chat_id、chat_type、mentions、root_id、thread_id、message_type、正文和时间。Channel 归一化没有保留的字段从 RawEvent 提取。

先排除机器人自己、其他应用消息、已知镜像投递，再按用途核验身份：任务输入/目录管理需 operator；已配置通知接收者可用 /notify 管自己的规则；临时配对只接受有效私聊口令。不能用统一的 operator 拦截误禁接收者关闭自己的 @。群消息要求结构化 mentions 含本机器人 open_id，不能靠正文字符串“@机器人”判断。只移除实际 @ 本机器人的 mention，保留用户正文、换行和代码。首版文本及可提取文字的富文本必需；其他附件提示暂未支持，不把文件名当完整输入。

普通消息默认路由到当前有效绑定；命令由命令解析器处理。Channel allowlist 随有效绑定更新；二期入口群/工作群来自 creation.workgroup_chat_ids。未绑定入口群的普通 @ 仅在二期 creation.enabled 时进入草稿。未启用二期时回一条明确的使用说明，不创建会话。

## 出站接口与幂等

发送类型分为 send、reply、update。每次记录稳定 delivery_id 与平台支持的实际 UUID/去重字段；M0 核验字段、有效期及重试行为。群创建同样核验接口去重能力。无法证明幂等且响应丢失的请求进入 unknown，不能沿用普通 HTTP 重试器。

Card JSON 2.0 用原生 API 发送与更新，保留 message/card ID。每 turn 一张过程卡，同一卡最短 2 秒 PATCH；最终结果独立发送。不要每个 agentMessage 或增量发一条新消息。reaction 是辅助反馈，其失败不能阻断主链路或触发输入重试。发送和更新限流各自按平台错误与 x-ogw-ratelimit-reset 等实际信号调节。原消息引用使用 POST /im/v1/messages/:message_id/reply，持久父消息 ID 与回复 ID；普通群 reply_in_thread=false，不把 reply 失败降级成普通 send。

@ 必须由桥根据 07 的策略生成结构化 mention；用户/AI 正文中的类似 mention 标签作为普通内容显示，不能绕过免打扰。目标须已在群内；不可达成员在状态中明确标注，不退回 @所有人。

**提醒能否由更新卡片触发须实测。** 首版可靠默认：流式卡片不 @；最终事件需要提醒时，发送一条包含 @ 和原回复定位的简短结果通知，和正文卡分开。若最终正文第一次发出时已完成，可在该发送中提醒，无需额外通知。持久 delivery key 保证同一最终事件最多安排一次提醒，补齐旧历史不补 @。

## 话题隔离

FollowTarget 为 topic 时，所有镜像、排队卡、通知、错误和操作结果都使用明确的 reply/root/thread 参数。单聊没有当前群话题；由二期选已授权工作群。

原根消息被撤回、目标话题失效或平台拒绝回复时，把绑定标记需要处理并通知操作者。**不得使用 SDK 将 reply 失败自动退回普通群消息的默认分支**。必要时直接调用原始 reply API，显式禁止该降级。[群管理](https://open.feishu.cn/solutions/detail/groups?lang=zh-CN)、[消息回复 API](https://open.feishu.cn/document/server-docs/im-v1/message/reply)

## 回调处理

3 秒内完成身份/令牌校验、去重领取和短事务保存，返回“已收到，正在处理”；Codex/建群 API 异步执行。数据库失败时返回明确失败，不启动后续操作。完成后通过 outbox 更新同一卡片。

日志记录 error code、request ID 和动作 ID，不输出 app_secret、tenant token、配对码、完整对话正文。诊断可包含脱敏目标及事件形状。[飞书卡片回调](https://open.feishu.cn/document/feishu-cards/card-callback-communication)

## 验收

1. 本人私聊、群 @、卡片回调全部能收到；自发镜像不反向提交。
2. 重复事件由 02 去重；飞书限流不会丢失最终正文。
3. @ 本人确实触发提醒；关闭 / quiet 后不再含 mention；补齐旧回复不提醒。
4. 真实发送超时、创建群超时、更新被限流各走对应核验/重试路径。
5. 二期根消息失效时，没有一条话题内容落入主群。
