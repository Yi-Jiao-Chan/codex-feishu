# 飞书自建机器人 M0 最小权限核验

核验日期：2026-10-10。运行 SDK 固定 `github.com/larksuite/oapi-sdk-go/v3 v3.12.0`；权限以本次读取的飞书官方 API/事件文档为准，不从 SDK 方法存在与否推导权限。范围为企业自建应用、机器人能力、`tenant_access_token`、用户 `open_id` 与已有 `chat_id`。本调研没有读取凭据、调用业务 API、发送消息或操作后台。

## 推荐清单与唯一未闭合边界

私聊输入、用户群 @ 输入、创建群及指定初始 owner/member、查群/成员/机器人在群、发送/引用/更新、写 reaction，可以用以下 **7 项**组成新部署的最小推荐配置：

```json
[
  "im:message.p2p_msg:readonly",
  "im:message.group_at_msg:readonly",
  "im:chat:create",
  "im:chat:read",
  "im:chat.members:read",
  "im:message:send_as_bot",
  "im:message.reactions:write_only"
]
```

现有 M0 启动与 doctor 始终通过 `internal/feishu/gateway.go` 的 `Bot` 调用 legacy **`GET /open-apis/bot/v3/info`**（入口事实由主代理提供）。这个必调能力的官方权限要求明确为“无”，所以 **7 项基线不因机器人身份查询而缺少 scope**，不需追加 `application:bot.basic_info:read`。它仍要求有效 `tenant_access_token`、机器人能力启用并发布、接口可达；7 项权限本身不是这些启动条件已满足的证明。doctor 若同时执行下面的消息读回检查，则还要单独检查读回能力。[获取机器人信息：准确 legacy endpoint](https://open.feishu.cn/document/client-docs/bot-v3/obtain-bot-info)

如果保留 `GET /im/v1/messages/:message_id` 读回校验，还需推荐 `im:message:readonly`，成为 **8 项**。这项明确覆盖文档中的单聊消息读取；**自身发送的群消息读回不能仅凭当前文档证明上述 8 项充分**。官方 GET 文档要求群消息额外具有 `im:message.group_msg`，却没有描述“机器人只读自己消息”的豁免。主代理提供的实际结果是：现授权集合没有该全群消息权限，自身消息仍读回成功。这是已观察到的能力边界，尚不是单 scope 独立证明。[获取指定消息的内容](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message/get)

M0 不因此申请 `im:message.group_msg`、`im:message.group_msg:readonly` 或其 include_bot 变体。群内自身消息读回在 compat 中保留独立的实测状态；生产发送/更新的成功响应本身不依赖读回。若新部署读回失败，标记该校验能力受限，不自动扩大授权。

## 可转换为 compat 的能力表

“替代权限”各项是 **OR**；除明确写出的组合条件外，不应把它们全部列成必需。最小推荐列是新部署选择，替代列用于识别已有应用具备的能力。

| capability | 官方接口 / 事件 | 最小推荐 scope ID | 文档认可的替代 / 组合条件 | 运行条件与验证边界 |
| --- | --- | --- | --- | --- |
| `bot_identity` | [`GET /open-apis/bot/v3/info`](https://open.feishu.cn/document/client-docs/bot-v3/obtain-bot-info) | **无独立 scope** | 无；`application:bot.basic_info:read` 不是该准确 endpoint 的要求 | **现有启动与 doctor 必调**；有效 tenant token、启用机器人并发布。读根级 `bot.open_id`，不是 `data.bot`。不可与其他应用信息或机器人信息接口混淆。 |
| `receive_p2p_user` | [`im.message.receive_v1`](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message/events/receive) 的私聊分支 | `im:message.p2p_msg:readonly` | 历史权限 `im:message.p2p_msg` | 开启机器人、订阅“接收消息 v2.0”；事件订阅权限表列任一项，不代表一个群权限能同时接收私聊。 |
| `receive_group_user_at` | [同一接收消息事件](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message/events/receive) 的群 @ 分支 | `im:message.group_at_msg:readonly` | 历史 `im:message.group_at_msg`；若明确需要其他机器人 @，选择 `im:message.group_at_msg.include_bot:readonly` | M0 人类输入选择用户 @ 权限；无需 include_bot。接收事件后按 `sender_type` 过滤，按 `message_id` 去重。 |
| `create_chat_with_initial_owner_members` | [`POST /im/v1/chats`](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/chat/create) | `im:chat:create` | `im:chat` | 创建请求自带 `owner_id`、`user_id_list`；文档没有为初始邀请追加成员写 scope。使用 `user_id_type=open_id`。 |
| `get_chat` | [`GET /im/v1/chats/:chat_id`](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/chat/get) | `im:chat:read` | `im:chat:readonly` 或 `im:chat` | 不在群内只返回有限基本信息；内部群要求同租户。`im:chat.members:read` 不是此接口的替代权限。 |
| `get_chat_human_members` | [`GET /im/v1/chats/:chat_id/members`](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/chat-members/get) | `im:chat.members:read` | `im:chat:readonly`、`im:chat`、历史 `im:chat.group_info:readonly` 中任一 | 操作者必须在群内；分页必须遍历；结果过滤所有机器人，不能据此判断本机器人是否入群。 |
| `bot_is_in_chat` | [`GET /im/v1/chats/:chat_id/members/is_in_chat`](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/chat-members/is_in_chat) | `im:chat.members:read` | 同上一行 | 检查的是 token 对应主体；机器人使用 tenant token，无需传自己的成员 ID。文档未列 `im:chat.members:bot_access` 为替代。 |
| `send_message` | [`POST /im/v1/messages`](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message/create) | `im:message:send_as_bot` | `im:message` 或历史 `im:message:send` | 私聊用户在应用可用范围内；群内机器人已入群并能发言。卡片直接使用消息 `interactive` 内容。 |
| `reply_message` | [`POST /im/v1/messages/:message_id/reply`](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message/reply) | `im:message:send_as_bot` | 同发送 | 引用回复已收到的消息无需增加读全群消息权限；目标 ID 来自接收事件或发送响应。 |
| `patch_shared_card` | [`PATCH /im/v1/messages/:message_id`](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message/patch) | `im:message:send_as_bot` | `im:message:update` 或 `im:message` | 与发送卡片的身份一致，更新前后 `config.update_multi=true`，未撤回且不超过 14 天，单条消息 5 QPS。无需同时申请 `im:message:update`。 |
| `get_own_p2p_message` | [`GET /im/v1/messages/:message_id`](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message/get) | `im:message:readonly`，仅保留读回时增加 | 正式权限表替代为 `im:message` | `send_as_bot` 没有列为读权限替代。文档注意事项额外出现 `im:message.history:readonly`，但与主权限表不一致；不作为本清单推荐或已确认替代。 |
| `get_own_group_message` | [同一 GET 消息接口](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message/get) | 暂无可由文档独立确认的无全群读取最小组合 | 文档通用规则为 `im:message:readonly` 或 `im:message`，再加 `im:message.group_msg` | 当前应用自身消息读回已成功；M0 不申请全群读取。该项 compat 应允许 `observed_pass / documentation_gap`，不能把通用群历史读取宣布可用。 |
| `create_delete_own_reaction` | [`POST .../messages/:message_id/reactions`](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message-reaction/create)；[`DELETE .../reactions/:reaction_id`](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message-reaction/delete) | `im:message.reactions:write_only` | `im:message` | 仅删除自己添加且仍存在的 reaction；保存创建响应的 `reaction_id` 即可切换状态，不要求 reaction 读权限或事件订阅。 |
| `card_callback` | [`card.action.trigger`](https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-callback-communication) | **无独立 scope** | 无 | 开发者后台订阅新版卡片回传交互，卡片内有回传组件/交互，注册 SDK handler；长连接或 Webhook 均官方支持。3 秒内响应。[操作步骤](https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/handle-card-callbacks) |

## owner/member、身份与卡片的范围说明

建群 `owner_id` 留空时，建群机器人为群主；指定用户时可以用查询参数 `set_bot_manager=true` 同时设机器人管理员。`user_id_list` 是创建时邀请成员，最多 50 人；创建机器人自动入群，不需要在 `bot_id_list` 再填写自己。这些属于创建 API 的参数能力。后续另调成员拉入/移除、群配置更新，是另外的接口与权限需求，本清单不将其混进初始建群。[创建群](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/chat/create)

owner、member、事件发送者以及卡片点击者统一用 `open_id`。仅为了获得 `user_id` 才需 `contact:user.employee_id:readonly`；它是敏感字段权限，不是机器人收消息、指定已知 open_id 为群主、初始邀请成员或处理卡片点击的前提。无需检索通讯录来确定已收到消息的发起人。[创建群](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/chat/create)、[卡片回传交互](https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-callback-communication)

本 M0 使用 `interactive` 原生 JSON 消息卡片及消息 PATCH；卡片回调权限要求为“暂无”。不能因为使用卡片就把 `cardkit:card:read/write` 加入必需清单。机器人身份查询是现有入口的必调能力，已经在表中单独核验：legacy `/bot/v3/info` 无独立 scope，不能从其他同名信息 API 或当前已授权 `application:bot.basic_info:read` 反推它必需。上传附件 `im:resource`、反应查询 `im:message.reactions:read` 只有调用对应额外 API 时才另立能力项；不能由本表推导它们必需。已授权权限也不建议在本次任务中自动撤销。[获取机器人信息](https://open.feishu.cn/document/client-docs/bot-v3/obtain-bot-info)

## 当前授权与真实验证怎样入 compat

主代理只读提供的当前相关授权为：

```text
application:bot.basic_info:read
im:chat.members:bot_access
im:chat.members:read
im:chat:create
im:chat:read
im:chat:update
im:message.group_at_msg.include_bot:readonly
im:message.group_at_msg:readonly
im:message.p2p_msg:readonly
im:message.reactions:read
im:message.reactions:write_only
im:message:readonly
im:message:send_as_bot
im:message:update
im:resource
cardkit:card:read
cardkit:card:write
```

这不是项目必需列表。发送、回复、PATCH、自己消息读回、建群，以及 `im:chat.members:read` 发布后的成员列表、reaction create/delete，均由主代理报告真实调用成功。本研究没有重跑。多权限同时存在时，接口成功只能证明当前配置可用，不能证明命中了某一个 scope：例如当前 `send_as_bot` 与 `message:update` 同时存在，PATCH 成功不能独立证明两者的必要性；最小替代关系依据官方文档。

compat 宜分别记录 `documented_permission_satisfied`、`runtime_observed`、`verification_pending`，并把 `event_subscribed` / `callback_subscribed` / `bot_enabled` / `availability` 作为非 scope 条件。替代检查做 OR；M0 默认检查 7 项推荐权限对应的能力，并把 **无 scope 但必调用的 `bot_identity`** 列为启动/doctor 检查，再加独立可选消息读回，不把当前授权列表直接当作 `required_scopes`。`bot_identity` 的权限满足不代表 token、能力发布或运行调用成功。

仍需主代理或真实客户端完成的验收：

- 人类私聊输入、群 @ 输入（含手机端）与非 @ 群消息不进入桥接；后台订阅与范围必须实际匹配。已具 include_bot 时还要确认用户/机器人过滤。
- `is_in_chat` 对已入群和未入群分别返回正确状态；不要复用过滤机器人后的成员列表作判断。
- 人类 owner、初始 member 的返回和 GET 群 owner 是否符合预期；已有建群成功不替代 owner/member 分支验收。
- 原生 Card2 的按钮经 `card.action.trigger`、当前 SDK 长连接链路往返且 3 秒内响应；本次未操作后台、未声称实际点击验证。
- 新部署的 legacy `bot_identity` 运行检查：响应 `code=0`、根级 `bot.open_id` 非空；当前存在 `application:bot.basic_info:read` 不影响官方“无独立 scope”的结论，但现有授权集合的成功调用不能算未授该权限的独立运行试验。
- 若需要证明 8 项足够用于自身群消息 GET，需使用独立最小权限测试应用或官方对该豁免的说明；不要为证明而修改当前生产应用授权。不读取其他人的群消息，不扩成历史消息测试。

## 来源留档

以上 API、事件与回调 URL 均为飞书开放平台一手来源。2026-10-10 读取了对应 `.md` 官方导出（在 URL 后加 `.md`），暂存于 `/tmp/feishu-m0-permissions-research/`；飞书在线文档不提供可固定的源码 commit，SDK 版本不会冻结服务端权限规则。机器人身份的[旧版地址](https://open.feishu.cn/document/ukTMukTMukTM/uAjMxEjLwITMx4CMyETM)与语义地址导出完全相同，均明确列出 legacy endpoint、GET、权限“无”。关键边界的导出 SHA-256：

| 官方导出 | SHA-256 |
| --- | --- |
| `bot_info.md`（`bot_info_legacy.md` 内容相同） | `1a93c97dfc5b81cdd72b8ae11890db8c422145a881407586bacd36bf83e332cd` |
| `message_events_receive.md` | `7a76be9631c20f1e0a0a7b3ecc43b9ec968b8230550088e82f25ff7d8be9b8d2` |
| `message_get.md` | `75621cb4e1c4a874b7dd0ab8558a7ee9a1b2768524215f488d6f6e3ff2cd0198` |
| `callback-0.md`（卡片回传交互） | `fbf624330f526bb7e90844e898bf573f5323d8b74756ddadfd848cb2b4c7afba` |
