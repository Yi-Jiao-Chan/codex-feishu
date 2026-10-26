# 03 飞书网关

负责 `internal/feishu/`、`compat/feishu-permissions.json`、脱敏事件样例。依赖 00、02；封装平台行为，业务模块不直接操作 SDK。

## 原生 CoT 进度通道

正式网关支持 `CreateCOT`、`AppendCOT`、`CompleteCOT`，复用现有 tenant token、超时与安全错误分类。创建使用 `POST /open-apis/im/v1/message_cot?receive_id_type=chat_id`，保存返回的 message_id 与 cot_id；普通群引用本轮输入，话题仅在校验原根后传原 root 与 reply_in_thread=true，不降级到主群。追加使用同路径 PUT，每批至多 50 个事件；content 是经过 JSON 编码的对象字符串，按整个事件序列化后的 UTF-8 字节保守限制为 4096。结束调用 `/message_cot/complete/{cot_id}`，只允许 done/error。

创建没有已核实的幂等键。三类操作均先记录冻结请求；未知结果不重发、不新建替代 CoT。普通消息 GET 返回 post 兼容占位，不能证明内部事件或生命周期状态；不能复用 Card JSON 的 marker 读回确认。原生停止发送普通 `/stop`，需继续操作者、绑定和轮次核验；群聊默认隐藏停止入口时仍可在对应任务群/话题使用 `/stop`。停止后新消息可以继续会话，也保留 `/continue` 显式继续队列。基础消息能力与可灰度的自定义 statusText 分开；本实现不依赖后者。参考 [官方内部接入契约](https://bytedance.larkoffice.com/wiki/ZfyXwOQImiLURtkRPtWcf2B2nNh)。

## 接入与发布条件

使用飞书企业自建应用，启用机器人，通过官方 Go SDK 长连接接收事件，不要求公网回调服务。当前 M0 锁定 Go SDK v3.12.0，使用 ws + event dispatcher + core.Request，以避免 Channel 发送降级和不明确的重试；该版本已有 Channel，但本项目不使用其出站默认分支。SDK 版本升级须重新核验生命周期和发送语义。[官方 SDK Channel 文档](https://github.com/larksuite/oapi-sdk-go/blob/v3_main/doc/channel.zh.md)

订阅 `im.message.receive_v1` 和 `card.action.trigger`；接收范围须包含操作者与测试群。SDK 负责长连接协议与事件解析，桥负责权限、去重、路由和业务确认。Channel 启动/停止生命周期按固定版本文档执行，不能停止后再次 Start 同一个终态 ws.Client。

准确权限 ID、替代权限的 OR 关系及当前实测边界见[权限研究](../research/feishu-m0-permissions-2026-10-10.md)。现有应用有多项同时授权，请求成功不证明每项都必需；自身群消息读回与官方通用群消息权限说明存在差异，保留独立 capability 状态，不能据此申请全群读取。

发布包必须携带权限能力清单：API、具体权限 ID、官方来源、已验收 SDK 版本、检查方式。下表是必须满足的能力；不要把文档里的中文名称冒充精确权限 ID。M0 用当时平台控制台/官方接口文档落实这些 ID，setup 按发布清单展示申请项。

| 必需能力 | 使用位置 | 如何验收 |
| --- | --- | --- |
| 机器人私聊收消息 | 配对、控制命令、二期创建 | 本人私聊收到 message event |
| 群内 @ 机器人消息 | 同群跟进 | @ 有事件；免 @ 另按下方单人任务群条件核验 |
| 卡片操作回调 | 引导、移除、停止、设置 | 点击真实卡片收到回调 |
| 以机器人发送 / 回复 / 更新消息 | 镜像与状态 | 收到消息，原卡可更新 |
| 创建群聊及管理所需成员 | 自动建群 | 建一个本人和机器人的测试群 |
| 读取群、成员及必要的自身投递记录 | 重建绑定、超时核验、@ 检查 | 能读回已建群 / 目标成员 / 自己的消息；成员最小 `im:chat.members:read` 已发布并实际通过 |
| 原消息状态 reaction | 接收 / 排队 / 执行 / 完成反馈 | 添加、替换并仅删除本机器人 reaction；`im:message.reactions:write_only` |
| 话题回复 | 二期 | 原始 root/thread 信息保留，回复留在话题 |

控制台配置、版本发布和组织审批不能通过“拿到 tenant token”替代。doctor 检查不出来的权限标 pending，真实 probe 才能将其变为 pass。不要为了接收 @ 消息申请读取所有群消息，除非平台当前权限模型确有需要并在发布清单解释。

## 入站归一化

保留原始 message_id、sender.open_id、chat_id、chat_type、mentions、root_id、thread_id、message_type、正文和时间。Channel 归一化没有保留的字段从 RawEvent 提取。

先排除机器人自己、其他应用消息、已知镜像投递，再按用途核验身份：任务输入/目录管理需 operator；已配置通知接收者可用 /notify 管自己的规则；临时配对只接受有效私聊口令。不能用统一的 operator 拦截误禁接收者关闭自己的 @。群消息默认要求结构化 mentions 含本机器人 open_id，不能靠正文字符串“@机器人”判断。只移除实际 @ 本机器人的 mention，保留用户正文、换行和代码。文本及可提取文字的富文本按原规则处理；图片、文件和多附件走下方资源入口，不把文件名当完整输入。

已绑定任务群仅有发送者和当前机器人时，operator 可免 @ 发送 text、post 及其中的指令。每条未 @ 消息先解析准确绑定，再在同一个 2 秒处理预算内读取群信息，要求 `user_count=1`、`bot_count=1`，核验当前机器人在群，并读取完整用户成员列表，确认唯一用户就是发送者。成员列表本身会过滤机器人，不能单独用列表长度判定。各次 GET 不是原子快照；不缓存放行结果，下一条消息重新核验。人数变化、权限拒绝、缺字段、受限成员列表或超时均不放行，用户可改为 @ 机器人。原 operator、绑定授权和普通 queue/add 路径继续生效；未绑定入口群和话题群主流不因此获得免 @，已绑定话题仍须匹配原 root/thread。

此可选能力依赖已发布的 `im:message.group_msg`，与附件开关独立；只有 @ 收消息权限的部署继续使用原入口，不自动申请或扩大权限。doctor 通过独立的 `receive_one_to_one_group_without_mention` 能力报告授权状态，群信息、成员及机器人在群权限沿用既有必需项；权限通过不代表真实免 @ 已验收。平台说明见[接收消息](https://open.feishu.cn/document/server-docs/im-v1/message/events/receive.md)、[群人数](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/chat/get.md)和[成员列表](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/chat-members/get.md)。

普通消息默认路由到当前有效绑定；命令由命令解析器处理。Channel allowlist 随有效绑定更新；二期入口群/工作群来自 creation.workgroup_chat_ids。未绑定入口群的普通 @ 文本仅在二期 creation.enabled 时进入草稿；当 `attachments.enabled=true` 时，私聊和未绑定入口群的普通图文/文件说明也沿现有创建入口建草稿。已绑定任务群或话题中的普通输入继续当前会话，只有 /new 创建新草稿。未启用二期时回一条明确的使用说明，不创建会话。

## 图片、文件与权限入口

附件能力默认关闭。`attachments.enabled=true` 时，单条 `/new` post 可同时携带正文、多个静态 PNG/JPEG/WebP 和顶层 `files`；图片保存为受限本地资源并以 `localImage` 进入原生输入。通用文件不伪装成原生 file part，使用带文件名和绝对路径的文本引用；桥不做 OCR、解压或自动执行。群 post 默认用结构化 mention @ 机器人，或满足上述单人任务群免 @ 条件；独立图片/文件消息可不 @，但仍只接受已绑定任务群和 operator（`receive_standalone`），不为私聊或未绑定入口群扩展独立附件收集，并需要 `im:message.group_msg` 接收范围。单人任务群免 @ 不绕过独立附件开关。

post 的正文只选 `content` 或回退 `content_v2` 一份表示；同条消息的文件另在顶层 `files` 数组中，以 `file_key`、`file_name`、`is_folder` 表达。必须将文件按数组顺序追加到正文资源之后，不能把只含正文节点的消息当成纯文本成功提交。无效文件项或文件夹使整条消息明确失败，不静默丢资源。

入站资源保留原消息 ID、resource key、顺序和发送者，按 `(host, app, thread, chat, sender)` 隔离草稿；附件说明和资源在封存时原子成批，管理命令不消费草稿。`enabled=false` 时不接收新附件，已封存批次仍按本地状态完成；未确认草稿带附件提交会被拒绝并提示重新开启。新增附件能力的权限检查只在对应开关启用时提升 required；关闭安装保持原 required/recommended 基线不变。当前本机候选已升级并开启附件，两项新增权限检查已通过；真实飞书/App UI 验收仍待完成，见[附件实施记录](../records/feishu-inbound-attachments-implementation-2026-10-15.md)。创建附件的真实客户端观察仍待验收。

## 出站接口与幂等

发送类型分为 send、reply、update。每次记录稳定 delivery_id 与平台支持的实际 UUID/去重字段；M0 核验字段、有效期及重试行为。群创建同样核验接口去重能力。无法证明幂等且响应丢失的请求进入 unknown，不能沿用普通 HTTP 重试器。

Card JSON 2.0 用原生 API 发送与更新，保留 message/card ID。每 turn 一张过程卡，同一卡最短 2 秒 PATCH；最终结果独立发送。不要每个 agentMessage 或增量发一条新消息。reaction 是辅助反馈，其失败不能阻断主链路或触发输入重试。发送和更新限流各自按平台错误与 x-ogw-ratelimit-reset 等实际信号调节。原消息引用使用 POST /im/v1/messages/:message_id/reply，持久父消息 ID 与回复 ID；普通群 reply_in_thread=false，不把 reply 失败降级成普通 send。

当前开发版对正式镜像 outbox 的 send/reply/PATCH 实现持久限流恢复：完整错误 JSON 的通用 `99991400`（HTTP 400/429），或这三个消息接口的 `230020`（HTTP 400）才进入延后路径。等待 `x-ogw-ratelimit-reset` 指定的响应接收后秒数，缺失/非法时采用持久指数退避（1 秒起、最高 60 秒；合法平台等待下限可以更长）。每次拒绝保存原 attempt，下一次重新冻结最新正文与 UUID，并重新检查 @ 规则与群成员；没有接受过的旧正文不会强行补发。暂停期间不反复读取成员。超时、5xx、不完整错误、无法解释的 429、`230049` 消息发送中均保持 unknown，不能靠 UUID 去重窗口盲重试；额度耗尽 `99991403` 保留明确拒绝，需管理员处理。reaction add/delete 已接入同一通用限流代码的持久恢复；等待从响应收到时计，保留已确认删除的证据，下一次领取按最新状态更新，unknown 不重试。reaction 接口的 `230020` 不在重试 allowlist。建群已接入通用频控的持久等待、独立 attempt 和到期重验；平台 UUID 与原创建参数不变，unknown 不重发。命令回复也已接入持久等待：复用原命令结果和引用关系，明确拒绝后才换新 UUID，不重做规则或任务操作。App 待处理提醒也已有 schema 15 的独立限流恢复；这些限流能力不代表所有 unknown 已能读回核验。[频控策略](https://open.feishu.cn/document/server-docs/api-call-guide/frequency-control.md)、[发送错误码](https://open.feishu.cn/document/server-docs/im-v1/message/create.md)、[回复错误码](https://open.feishu.cn/document/server-docs/im-v1/message/reply.md)、[卡片更新](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message/patch.md)

最终回复首次创建被明确拒绝后，只有后续完整快照产生不同且已安全过滤的卡片，才允许一次带来源 attempt 的纠正投递；原 rejected attempt 不改写。若纠正投递或明确限流后的重试仍被平台明确拒绝，桥在同一持久事务安排独立失败卡，展示受限的 code、HTTP status 与安全 request ID，提醒回 Codex 查看完整回复。unknown 不走此路径，失败卡被拒绝也不递归。持久字段与旧库迁移见 [Schema 35](02-store-and-recovery.md#schema-35最终回复纠正重试与拒绝证据)。

@ 必须由桥根据 07 的策略生成结构化 mention；用户/AI 正文中的类似 mention 标签作为普通内容显示，不能绕过免打扰。目标须已在群内；不可达成员在状态中明确标注，不退回 @所有人。

**提醒能否由更新卡片触发须实测。** 首版可靠默认：流式卡片不 @；最终事件需要提醒时，发送一条包含 @ 和原回复定位的简短结果通知，和正文卡分开。若最终正文第一次发出时已完成，可在该发送中提醒，无需额外通知。持久 delivery key 保证同一最终事件最多安排一次提醒，补齐旧历史不补 @。

## 话题隔离

FollowTarget 为 topic 时，所有镜像、排队卡、通知、错误和操作结果都使用明确的 reply/root/thread 参数。单聊没有当前群话题；由二期选已授权工作群。

原根消息被撤回、目标话题失效或平台拒绝回复时，把绑定标记需要处理并通知操作者。**不得使用 SDK 将 reply 失败自动退回普通群消息的默认分支**。必要时直接调用原始 reply API，显式禁止该降级。[群管理](https://open.feishu.cn/solutions/detail/groups?lang=zh-CN)、[消息回复 API](https://open.feishu.cn/document/server-docs/im-v1/message/reply)

## 回调处理

3 秒内完成身份/令牌校验、去重领取和短事务保存，返回“已收到，正在处理”；Codex/建群 API 异步执行。数据库失败时返回明确失败，不启动后续操作。完成后通过 outbox 更新同一卡片。

日志记录 error code、request ID 和动作 ID，不输出 app_secret、tenant token、配对码、完整对话正文。诊断可包含脱敏目标及事件形状。[飞书卡片回调](https://open.feishu.cn/document/feishu-cards/card-callback-communication)

## 验收

1. 本人私聊、群 @、卡片回调全部能收到；自发镜像不反向提交。具备全群消息权限时，已绑定单人任务群免 @ 的文字/富文本沿原队列提交；新增用户或机器人后恢复要求 @，读取失败不放行，重复事件不重复提交。真实飞书/App UI 需另行验收。
2. 重复事件由 02 去重；飞书限流不会丢失最终正文。
3. @ 本人确实触发提醒；关闭 / quiet 后不再含 mention；补齐旧回复不提醒。
4. 真实发送超时、创建群超时、更新被限流各走对应核验/重试路径。
5. 二期根消息失效时，没有一条话题内容落入主群。

App 待处理提醒的明确限流恢复已接入开发版：等待期不读群成员，到期读取实际成员后再冻结新提醒卡与 UUID，始终 reply 原输入。未知响应不重发；完整等待区间内出现免打扰或请求解决则终止旧提醒。schema 15 隔离证据见[限流投递进展](../records/m3-rate-limit-delivery-2026-10-12.md)，真实 M0 尚未迁移。

镜像未知结果的只读 reader 已接入开发版：GET/List 明确请求 `user_card_content`，sender 使用 app_id 核验而非 bot open_id，缺 deleted / 父关系等必要证明仍保守处理。普通群分页至多 20 页、总计 5 秒，截断、分页失败或多 marker 候选不确认。旧无标记请求不追认；命令 / App 待处理卡已接入下述共用核验，建群 / reaction 的 unknown 仍需各自证据契约。官方界限与新标记协议见[远端核验研究](../research/feishu-unknown-readback-2026-10-12.md)；真实 marker 读回和 UI 尚待验收。

开发 schema 17 的命令 / 待处理卡共用上述 `FindMarkedCard`。命令回复仍引用原命令，核验不重做目录、通知或任务控制效果；已解决请求的旧提醒只确认已存在的原卡，不重新询问或 @。只有有发送前唯一标记的新 attempt 进入 reader，unknown 查不到不重发。真实读回/UI 验收状态与镜像一致，尚待验收。
