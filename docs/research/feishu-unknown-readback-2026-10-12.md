# 飞书 unknown 请求的只读核验边界

日期：2026-10-12。范围：已冻结的 send / reply / PATCH、建群、reaction add / delete，在超时或远端成功但本地确认事务丢失后，如何用官方 GET/readback 证明原 attempt。未调用真实 API、读取密钥、运行 M0 或修改实现。依据当前飞书官方文档及项目锁定的 Go SDK v3.12.0；实现与存储契约由主任务另行检查。

## 结论与实现边界

**当前平台读接口能证明资源身份及当前状态，普遍不能返回原写请求的 UUID 或操作版本。不要将“内容、时间、群名相同”升级为“原 attempt 已接受”。** 本报告中的“关联证据”指原响应或其他可信记录将远端 ID 与该不可变 attempt 明确关联；操作者仅给一个候选 ID，仍须区分人工认领和平台证明。[GET 消息](https://open.feishu.cn/document/server-docs/im-v1/message/get.md)、[GET 群信息](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/chat/get.md)、[GET reaction](https://open.feishu.cn/document/server-docs/im-v1/message-reaction/list.md)

| 原操作 | 可自动读取核对 | 原 attempt accepted 的边界 |
| --- | --- | --- |
| send / reply，已可信关联 message ID | GET ID、app 身份、chat、消息类型、父/根/话题关系、卡片结构、撤回与更新时间 | 关联 ID 的证据证明请求与资源关系；GET 加固核验。**候选 ID + 相同卡片不足以构造关联** |
| send / reply，无 message ID | 分页历史筛选候选，再 GET 核对 | 已冻结请求若没有可读回的唯一 attempt 标记，无法仅凭平台 GET 精确恢复；UUID 不是查询键 |
| PATCH，已有目标 message ID | GET 当前卡片，可确认期望状态是否已经出现 | 目标 ID 只定位卡片，不能定位本次 PATCH。相同 JSON、updated=true 或较新 update_time 都不能独立证明本次写入；需可读回的唯一版本/attempt 标记或原成功响应关联 |
| 建群，已有可信关联 chat ID | GET 当前配置；成员接口另读成员 | GET 不返回创建 UUID、创建 attempt。未知且无 ID 时，list/search 仅提供候选，不能自动按名称/owner 认定原建群成功 |
| reaction add，已有可信关联 reaction ID | 已知 message 的完整列表中找 ID，核对 app、emoji | 精确 ID 能定位 reaction；无原 ID 时，同 app + emoji + action_time 仍不绑定某次 add |
| reaction delete，已知原 reaction ID | 完整成功列表中检查原 ID 是否仍存在 | absent 可证明该观察时点目标已不存在，**不能证明是本次 DELETE 接受造成**。若业务允许按后置状态完成，须记录“已核验不存在”，不可伪造 accepted 收据 |

上表是对下列官方字段和接口契约的实现推论，不是平台额外的事务查询能力。所有未得到充分证据的操作保持 unknown；没有读到资源、权限错误、分页中断、正文不同、等待超过某个时长均不证明请求未接受。GET 未承诺跨分页快照或对写入的线性化屏障，本报告不假设这些保证。[消息读接口](https://open.feishu.cn/document/server-docs/im-v1/message/get.md)、[历史分页](https://open.feishu.cn/document/server-docs/im-v1/message/list.md)、[reaction 分页及错误](https://open.feishu.cn/document/server-docs/im-v1/message-reaction/list.md)

## 消息：原始卡片、身份和 UUID

`GET /open-apis/im/v1/messages/:message_id` 的 `data.items` 返回消息。应按 `message_id` 选中目标；不能永远取第一项，合并转发会返回主消息和子消息。消息字段包含 `message_id`、`chat_id`、`msg_type`、`root_id`、`parent_id`、`thread_id`、`create_time` / `update_time`（毫秒字符串）、`deleted`、`updated`、`sender`、`body.content`。应用发送者必须同时核对 `sender_type=app`、`id_type=app_id`、`id=本部署 app_id`；有已知租户时同时核对 `tenant_key`。不要拿 bot open_id 与 sender.id 比较。[官方 GET 字段](https://open.feishu.cn/document/server-docs/im-v1/message/get.md)、[固定 SDK Message/Sender](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/service/im/v1/model.go#L5682-L5715)、[Sender](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/service/im/v1/model.go#L7694-L7709)

读回卡片必须显式传 `card_msg_content_type=user_card_content`。官方称此值返回发送时的原始卡片 JSON，支持 1.0 和 2.0；未传时返回接收消息格式，不能获取原始发送 JSON。`schema` 区分卡片 JSON 1.0 / 2.0，**不是某次更新的 revision**。同页响应说明 `body.content` 为当前消息内容，因此可用于当前 PATCH 后置状态检查，但文档没有原字节一致、每个默认值必定保留或每次 PATCH 历史版本的保证。此处不能以第三方所说的 `raw_card_content` 当已核实契约：调查的官方 GET/list 和 v3.12.0 注释只明确默认及 `user_card_content` 两种行为。[官方 GET](https://open.feishu.cn/document/server-docs/im-v1/message/get.md)、[官方 list](https://open.feishu.cn/document/server-docs/im-v1/message/list.md)、[SDK Get builder](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/service/im/v1/model.go#L14077-L14142)

**exact-card 校验建议（推论）**：对冻结请求的 card JSON 与读回 `body.content` 做严格解析后的结构比较，允许对象键排序和无意义空白不同，保留数组顺序、文本、按钮值、类型、数值、通知内容及所有字段差异。不做抽取正文后比较、不盲目删除“看似默认”的字段；服务端格式变化或缺字段应返回 inconclusive。结构完全相同仍只是当前状态一致；在未有唯一关联证据时不得据此绑定原 attempt。[原始卡片返回契约](https://open.feishu.cn/document/server-docs/im-v1/message/get.md)

reply 须核验实际 `parent_id` / `root_id` / `thread_id` 语义，不能一律要求 parent 等于调用路径 ID：官方回复接口说明话题回复的 parent 始终为话题内根消息 ID。当前一期普通群 `reply_in_thread=false` 应按该冻结模式及已知父消息核验；未来话题模式需单独定义关系规则。创建/更新时间只能排除明显不可能的候选，存在客户端时钟偏差、同毫秒多操作及后续更新，不能作为唯一关联键。[回复消息字段](https://open.feishu.cn/document/server-docs/im-v1/message/reply.md)

send/reply 请求的 `uuid` 支持一小时去重；其响应返回 `message`，其中没有 `uuid`。GET 按 message ID，list 按容器/时间分页；已检查的这些读 API 和固定 SDK 没有按发送 UUID 查询或响应回显 UUID 的能力。这是所调查接口的文档边界，不宣称不存在任何未公开平台能力。相同 UUID 重发属于写操作，且只有时间窗内至多一次的保证，不能混入只读核验，也不把去重成功当原请求的审计证明。[send UUID 与响应](https://open.feishu.cn/document/server-docs/im-v1/message/create.md)、[reply UUID 与响应](https://open.feishu.cn/document/server-docs/im-v1/message/reply.md)、[SDK Message](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/service/im/v1/model.go#L5682-L5715)、[SDK 出站/查询方法](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/service/im/v1/resource.go#L1183-L1510)

## PATCH：可读当前结构，没有写入版本

项目所用 `PATCH /open-apis/im/v1/messages/:message_id` 请求只有 `content`，成功响应是 `code/msg`，没有版本或 attempt UUID。GET 的 `updated` / `update_time` 没有请求关联含义。某个旧卡片已经有相同内容而本次 PATCH 未发出，与本次 PATCH 成功后读到相同内容，在这些读字段上可能完全无法区分；若后来有人覆盖了卡片，读到不同内容也不证明原 PATCH 未接受。[官方 PATCH](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message/patch.md)、[SDK PATCH 请求与响应](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/service/im/v1/model.go#L14434-L14540)

官方限制：更新前后都须显式 `config.update_multi=true`，调用身份须与发送身份一致，只支持未撤回且 14 天内发送的卡片；单条更新 5 QPS，卡片最大 30 KB。send/reply 卡片请求同样最大 30 KB，样式和模板展开可能增大实际大小。这些是写入限制；GET 未公布一个可当作完整性保证的最大响应 body 大小，不应把写端 30 KB直接等同于读端 buffer 上限。[PATCH 限制](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message/patch.md)、[send 大小](https://open.feishu.cn/document/server-docs/im-v1/message/create.md)、[reply 大小](https://open.feishu.cn/document/server-docs/im-v1/message/reply.md)

CardKit 实体接口有另行的 `card_id` / `sequence` / `uuid` 协议，不能把其版本能力移植到当前 im message PATCH。这些不是本次冻结请求携带的字段，且写入 sequence 也不等于存在可查询原 attempt 的接口。[CardKit 全量更新](https://open.feishu.cn/document/cardkit-v1/card/update.md)、[当前 message PATCH](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message/patch.md)

## 历史分页与建群

消息 list 默认 20、每页 1–50，通过 `has_more/page_token` 翻页。GET 本身没有分页，但合并转发 items 可多项。历史查询 `start_time/end_time` 是秒级，返回 create/update 为毫秒，不能混用；排序与第一次分页一致。普通群 `container_id_type=chat` 只读到话题根消息，话题回复须查 `thread`；`thread` 不支持时间范围。因此“扫完群历史”不自动等于扫完所有话题回复。必须明确候选搜索覆盖范围和完成状态，任何预算截断都保持 inconclusive。[官方历史消息](https://open.feishu.cn/document/server-docs/im-v1/message/list.md)、[SDK list 参数](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/service/im/v1/model.go#L14148-L14281)

list 仅允许 `container_id_type=chat`（单聊 p2p 和群聊 group）或 `thread`。官方描述起止时间但**未说明 start/end 是否包含端点**；候选扫描应留重叠，不能因边界查不到就否认接受，thread 模式不要传时间过滤。SDK 查询 builder 不进行这些合法性/范围校验，reader 应按官方范围构造请求。[官方 list 参数](https://open.feishu.cn/document/server-docs/im-v1/message/list.md)、[固定 SDK builder](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/service/im/v1/model.go#L14148-L14255)

GET/list DTO 字段与缺失处理如下；官方响应表没有“必返”列，SDK 字段均为指针且 `omitempty`，因此不能将解码后的零值当作已观察事实。[GET 字段](https://open.feishu.cn/document/server-docs/im-v1/message/get.md)、[list 字段](https://open.feishu.cn/document/server-docs/im-v1/message/list.md)、[SDK Message](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/service/im/v1/model.go#L5682-L5715)

| JSON 字段 | 明示存在条件 / reader 规则 |
| --- | --- |
| `message_id`, `msg_type`, `chat_id` | schema 字段；需要核验的值缺失则 inconclusive。字段是 **msg_type**，不是入站事件的 message_type |
| `sender.id`, `sender.id_type`, `sender.sender_type` | schema 字段；应用应明确返回 app/app_id/本appID，任一缺失不得按调用token推定发送者 |
| `create_time`, `update_time` | schema 定义毫秒字符串；不宣称总会返回。缺失/不可解析不能补当前时间；仅当证明依赖时间条件时必须阻断 |
| `parent_id`, `root_id` | 官方明确仅回复场景返回；普通 send 可按文档接受 absence 为无回复关系，reply 必须有足以证冻结关系的值；不能把 reply 缺字段补成父消息ID |
| `thread_id` | 非话题不返回；普通非话题允许缺失，话题核验需要实际值 |
| `deleted`, `updated` | schema boolean；保留 present 与 value，缺失不能伪造 false。deleted=true 或明确撤回的消息不能用于卡片内容确认；updated只表示曾更新，不是版本 |
| `body.content` | 当前内容的 JSON 序列化字符串；官方明确撤回消息不会返回 content。缺失、非JSON、未取得要求的原卡结构则 inconclusive |
| `data.items`, list `has_more/page_token` | 合并转发可多项，按ID选择。完整扫描须明确 has_more=false；true必须有新的有效token。缺失/重复token/不完整响应不能作为“扫描完整” |

建群 `uuid` 是 query 参数，最大 50 字符；相同 `uuid + owner_id（若有）` 的请求 10 小时内只可成功建一个群。当前冻结的是 intentID，不是每个 attemptID，因此即使能恢复去重关联，也首先关联同一 intent，不能区分该意图的多个网络请求。官方 create 响应没有回显 uuid。[官方建群](https://open.feishu.cn/document/server-docs/group/chat/create.md)、[SDK create UUID](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/service/im/v1/model.go#L9560-L9583)

群 GET 由路径 chat ID 查询，返回名称、描述、owner、模式/类型、权限、租户与数量等当前字段；没有创建 uuid、原请求版本、创建者或创建时间。list/search 没有 uuid 过滤/返回字段；list 最大每页 100，实际数量可能更少，结束须按 has_more。名称/owner/成员可能改变，同名群也可能存在；完整 list 没有候选仍不证明建群未接受或从未存在。恢复原未知建群须已有原响应/可信记录的 chat ID，或明确的人工认领证据；人工选择只是采用该群，不能冒充平台已证明创建 attempt。[群 GET](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/chat/get.md)、[群 list](https://open.feishu.cn/document/server-docs/group/chat/list.md)、[群 search](https://open.feishu.cn/document/server-docs/group/chat/search.md)、[SDK GetChat 数据](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/service/im/v1/model.go#L9802-L9866)

## Reaction：身份可精确核验，操作因果不可推导

`GET /im/v1/messages/:message_id/reactions` 返回 `reaction_id`、`operator.operator_type`、`operator.operator_id`、`reaction_type.emoji_type`、`action_time`（添加时间，毫秒）。官方明确 `operator_type=app` 时 operator_id 是 app_id；user 则按请求 user_id_type。自动核验应要求 message 路径、精确 reaction ID、app 类型、本部署 app_id 和冻结 emoji 全部匹配。v3.12.0 的通用 `Operator` 注释声称仅 user/open_id，**这条注释与当前 reaction 文档冲突**；实际 struct 是两个字符串字段，能解码 app/app_id，应以 reaction API 官方字段语义为准，不能把机器人身份按用户解释。[官方 reaction list](https://open.feishu.cn/document/server-docs/im-v1/message-reaction/list.md)、[SDK MessageReaction](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/service/im/v1/model.go#L5999-L6007)、[SDK Operator](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/service/im/v1/model.go#L7117-L7122)

list 默认 20、最大 50，可按 emoji 筛选，必须遍历全部相关页至 has_more=false。GET 权限为 `im:message.reactions:read` 或 `im:message:readonly`；写权限 `im:message.reactions:write_only` 不包含这里的独立读能力。机器人需在消息会话内，已撤回消息不可查询；无访问权限、不可见、消息不存在/撤回、无效分页等错误均不是空列表。[list 权限/限制/错误](https://open.feishu.cn/document/server-docs/im-v1/message-reaction/list.md)、[add 权限](https://open.feishu.cn/document/server-docs/im-v1/message-reaction/create.md)

add 请求只有 emoji、没有客户端 attempt UUID；返回唯一 reaction ID。官方并未提供按 add attempt 查询的标识。相同 app/emoji/action_time 是候选证据，可能来自此前或其他同身份操作，不能补造原 ID。官方错误 231015 描述重复请求处理中，不提供可用于恢复的客户端 token 契约。[官方 add](https://open.feishu.cn/document/server-docs/im-v1/message-reaction/create.md)

delete 按已知 message/reaction ID，只能删自己添加且真实存在的表情。返回被删 ID、operator、action_time、emoji，**action_time 字段仍定义为添加时间，不是 DELETE 接受时间**。删除未知后，如果完整成功 list 不含原 ID，可按业务规则确认旧资源当前不在、清除旧 ID，避免阻塞下一状态；但日志要保留 unknown 原 attempt 与“目标不存在”的观察证据。只看 emoji 不存在、第一页没 ID、231011 无法定位 ID 或消息查询失败均不足以写 accepted。若目标 ID 仍在，可证当前存在，但不能由此自动再发；原请求可能尚在处理，分页也没有强一致快照保证。[官方 delete](https://open.feishu.cn/document/server-docs/im-v1/message-reaction/delete.md)、[官方 list](https://open.feishu.cn/document/server-docs/im-v1/message-reaction/list.md)

## 新请求可读回 marker 的候选（未实现、未真实验收）

官方 JSON 2.0 富文本 `tag=markdown` 支持 `element_id`，用于组件操作。限制是 **同一张卡片全局唯一、字母开头、仅字母/数字/下划线、最多 20 字符**。所以 `cf_<标准 UUID>` 共 39 字符且含连字符，不合法。element_id 是组件元数据，文档未把它定义为显示文字；在已有首个 markdown 上设置合法值，理论上不改变正文，但仍须验证实际桌面/手机 UI 与交互。[官方 Markdown 2.0](https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-json-v2-components/content-components/rich-text.md)、[2.0 变更说明](https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-json-v2-breaking-changes-release-notes.md)

**可实现推论**：在发送前持久分配合法短 marker，建立数据库唯一的 `marker → exact attemptID` 映射，把 marker 和最终卡片一起冻结。禁止只截 UUID/hash 后假定永不碰撞；生成后查重并持久约束。send/reply 的历史候选若含该 marker，再核验 app/chat/type/父根话题关系与 exact-card，可在受控单写入器、同身份其他客户端不复制 marker、marker 不复用的边界内形成强关联；PATCH 在已知 message ID 上读到本次新 marker，可区分内容相同的旧版本。关联能力来自本地协议，不来自平台原生 UUID 查询。[元素标识定义](https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-json-v2-components/content-components/rich-text.md)、[GET 原始 JSON](https://open.feishu.cn/document/server-docs/im-v1/message/get.md)

本任务可采用的具体新协议：`c` 加 attempt UUID hash 的 19 个 base32 字符，共 20 字符、95 bit 候选空间；base32 字母数字满足字段限制。在覆盖所有出站 attempt 表的全局 `card_request_markers` 上设置 UNIQUE，将 marker、归属 attempt 和带 marker 的完整 card 在同一事务冻结，碰撞则在 RPC 前失败或重新分配并落账，禁止接受两个 attempt 共享 marker。确认仍要求 app_id/chat/parent（按实际回复模式）/interactive/marker/完整 JSON 等价全部通过；缺字段或格式不等价保持 unknown。合法字段、发送前不可变证据及本地唯一约束使其可用于**新请求协议**，现有无 marker unknown 继续不能靠内容恢复。此方案属于本地设计与验收条件，尚未实现或验证平台 round-trip。[官方字段约束](https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-json-v2-components/content-components/rich-text.md)、[官方读回契约](https://open.feishu.cn/document/server-docs/im-v1/message/get.md)

**保证与缺口**：GET/list 的原始卡片 JSON 契约意味着合法 element_id 应可读回，但官方未单独明示此字段在发送、reply、PATCH 后必定逐值保留，也没有本次真实 API 证据。因此 marker 是待验收候选，不能宣称本报告已证明其 round-trip。已冻结无 marker 的请求无法事后补证；若后续版本覆盖 marker，查不到旧 marker 也不证明旧 attempt 未接受。扫描返回多份相同 marker 时应阻断而非选第一份；只扫描一段时间且找到一份，也不证明全域唯一。若允许同 app 的外部写入者复制卡片，marker 不能提供对该写入者的独立来源证明。[GET/list 返回结构](https://open.feishu.cn/document/server-docs/im-v1/message/get.md)、[历史消息](https://open.feishu.cn/document/server-docs/im-v1/message/list.md)

候选真实验收需覆盖 send/reply、同内容连续 PATCH 的不同 marker、重启后 no-ID 分页找回、超过一页、普通群及话题关系、完整结构与 element_id 保留、更新后字段变化、超时后延迟出现、跨页失败/重复 token、人工复制 marker 冲突。当前任务不发任何真实请求、不修改旧卡片加入 marker。

## 本次证据与未决事项

- 阅读 README、02/03 及仓库约束；CodeGraph 状态与索引均缺失后使用直接读取，未主动建索引。
- 官方页面 HTML 的 `rel=alternate` 提供 `.md`，通过这些公开官方 URL 获取完整字段，而不是引用第三方复刻。HTML-only web 提取为空不等于文档不可得。
- 本地 module cache 的 v3.12.0 `model.go`、`resource.go` 与官方 tag raw 文件逐字节相同，SHA-256 分别为 `978002449250a0d8e08bee36450d24c222caff7d2e1ab97963148ef6c6d10008`、`3dc9d125d1d286ad447ba27797d742fbe5612faba961f42b32ea9c959bb24cb3`。[官方 pinned model](https://raw.githubusercontent.com/larksuite/oapi-sdk-go/v3.12.0/service/im/v1/model.go)、[官方 pinned resource](https://raw.githubusercontent.com/larksuite/oapi-sdk-go/v3.12.0/service/im/v1/resource.go)
- GET 当前卡片格式/element_id round-trip、平台读一致性、原响应丢失后的可信 ID 来源均未通过真实探针，不能写 pass。当前无需密钥才能落实严格 reader、分页与证据类型；自动将既有 unknown 提升为原 attempt accepted 仍受上述关联缺口阻断。

## 本地开发实现与验收状态

本轮 schema 16 已实现镜像范围的新协议，实际表名为 `outbound_request_markers`，通过唯一约束与原 attempt 外键保留标记。cards helper 只给现有空闲 Markdown 的 element_id 赋值，不增加可见元素、改变正文、字号或按钮；完整 wire card 在 RPC 前冻结。原始 renderer 的分段同时预留 36 字节标记及配置接收者的完整结构化 @，避免长代码卡因附加字段超过既有总字节预算。旧非 JSON2.0 / 旧无映射 attempt 继续保持原载荷，不能猜标记；标记协议未切换实际 M0。

当前 GET/List reader 与 unknown 镜像核验已接入正式服务开发代码。PATCH 仅读精确已知目标；send/reply 必须完成普通群候选扫描、没有冲突，随后 GET 唯一候选，核对 app_id/chat/父关系/消息类型/明确未撤回/非话题及完整结构。JSON 比较允许键顺序和空白变化，不忽略内容、类型、数组顺序、重复键、控制值、mention、字段或标记差异。每次检查先保存 30 秒冷却，最多 20 页、总预算 5 秒；截断和读失败都保持 unknown，读失败仍允许独立 ready 消息继续。没有 GET 反证拒绝或 POST 重发路径。其余建群、reaction、命令确认和待处理提醒的 unknown 尚需各自整合，不因 reader 可用而宣称覆盖。

独立只读审查实际复现并修复两项问题：GET 失败曾跳过其他 ready 投递；损坏 JSON 曾被当成无 marker，漏掉第二候选。正式隔离回归覆盖这两项，此外覆盖分页找回、候选读回后改变、同内容不同 marker、身份/父消息/撤回/话题/缺字段、重复 token 与预算截断、原回复关系、未知不重发、重启保留冷却、迁移不造证明、marker proof write 被忽略则不发请求，以及长正文带 @ 与标记的总字节预算。真实 element_id round-trip / 卡片显示 / 故障恢复 UI 仍 pending；`phase1_released=false`。

验证：组合先通过 `go test -race ./... -count=1`；随后对总 GET 预算、总字节 / @ 预留和版本能力的最终修改，重跑 app / sync / cards / feishu / CLI 的 race 检查及 store health / marker / proof 定向 race 检查。`go vet ./...`、`git diff --check` 通过。没有实际飞书业务发送、群 / reaction 操作、原生输入、服务部署或旧 M0 数据库迁移。

## 命令确认与 App 待处理卡的共用核验

开发 schema 17 增加共享 `card_request_markers`。schema 16 镜像标记拷贝到共享命名空间，保留原映射和已持久检查间隔；不是给旧无标记请求补造标记。镜像、命令、提醒的标记不可互相复用，领取必须先存在精确 source attempt，同一事务保存实际带标记载荷和 ready→dispatching 的状态证明。非 JSON2 旧载荷保留原值，不进入标记核验。

`FindMarkedCard` 从原镜像算法提取，共用既有 GET 预算、完整分页、原卡结构、发送身份、chat/parent/撤回/非话题与标记唯一证明。命令可在未绑定会话的本人私聊核验，提醒则要求原 host/app/thread/binding；两者保持 30 秒持久间隔，只检查 exact unknown attempt。读失败不会饿死其他 ready 回复。成功只调用既有 atomic Finish，不再执行任何目录 / 通知命令或 native 输入 / 审批。请求已经 resolved、连接待重新确认或 @ 已关闭时，reader 可以核验此前是否送达；它不会恢复 pending、重新渲染旧 @ 或发送新的提醒。

新增隔离案例覆盖：旧 off 回复在随后 on 后只读确认，规则仍为 on；提醒在 resolved/off/restart 后确认原卡且不查询群成员、不重开原请求；读取失败不阻塞后来命令；原 wire card 不变；重启 / schema 16 镜像保留 / 旧命令不造证明；source 缺失和被忽略的 marker 写入回滚；外部 host/thread 不能领取；共享 proof DTO 不序列化正文。命令逻辑结果卡保持原样，实际每次请求卡的隐藏标记不同，测试分别核验可见正文与不可变原 wire payload。

原 M0 仍未迁移或切换，marker 的真实飞书原值读回 / UI 仍 pending，建群与 reaction 的未知结果也未由本切片覆盖。一期 release 标志保持 false。

本切片组合通过 `go test -race ./... -count=1`，CLI capability 的最终更新另跑 CLI 测试；`go vet ./...` 与 `git diff --check` 通过。没有真实外部写入、原生输入、接管或部署。
