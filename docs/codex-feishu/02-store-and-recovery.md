# 02 存储、收据与恢复

负责 `internal/store/`、迁移、恢复协调器。依赖 00；必须在输入提交与自动建群之前可用。

## 当前实现边界（2026-10-12）

schema v4 已实现公开镜像、outbox、不可变投递 attempt、原生输入与飞书收据关联、完整历史扫描水位的事务保存。待建群的 eligible 会话可以先入库，绑定及父消息确认后才领取发送；过程更新合并到最新版本，终态不回退，同一线程只允许一个 dispatching 写请求。send / reply / PATCH 都在调用网关前落账，启动时把遗留 dispatching 转为 unknown，不自动重发。通知规则按 app/user 持久保存，以 revision 防止并发修改丢失；重新初始化默认值不覆盖已有设置。

完整历史到公开镜像的转换已实现，并通过真实 daemon 只读探针，后续已接入正式 `serve` 的开发实现。现有 M0 listener 未切换到这些新表；未知请求的远程核验和完整一期真实部署仍待完成，不能据此认定一期已支持生产恢复。基础验证记录见[投递基础](../research/m1-mirror-outbox-foundation-2026-10-12.md)、[历史同步与规则](../research/m1-history-sync-and-rules-2026-10-12.md)。

后续整合状态：正式 serve、通知安排与投递决策已有开发实现；schema 10 新增 `outbound_throttles`，为正式镜像、过程、队列卡的明确限流拒绝记录持久等待下限及退避次数。拒绝 attempt 保持不可变，下一次 claim 使用最新正文并重新核验提醒；unknown 不进入重发路径。该机制已通过隔离故障测试，现有 M0 未迁移；App 待处理提醒的限流已有下述隔离实现，unknown 读回核验仍待补齐，详见[限流投递进展](../research/m3-rate-limit-delivery-2026-10-12.md)。

## SQLite 结构

schema 11 新增 `legacy_adoptions`。schema 1 须经显式接管，不能由普通启动自动升级；库存摘要、旧绑定与 confirmed 消息 ID 和正式表在同一事务保存，原日志保留。存储基础与纯历史 mapper 已通过隔离测试，迁移 CLI、一致备份及锁已通过隔离测试，真实切换仍待验收，见[M0 接管进展](../research/m3-legacy-adoption-2026-10-12.md)。

schema 12 新增 `reaction_throttles`：reaction 的明确限流拒绝保存原操作日志和等待时间，下一 revision 依据最新输入状态领取。旧 reaction 的成功删除与 ID 清除在同一事务确认，后续 add 被拒绝时不再删除旧 ID。changing/unknown 不进入重试，reaction 失败不触发原生输入重试；status 的 `reactions.deferred` 表示等待或到期尚未完成的反馈。详见[限流投递进展](../research/m3-rate-limit-delivery-2026-10-12.md)。

schema 13 新增 `group_attempts` 与 `group_throttles`：建群请求引用原意图中冻结的名称、owner 和成员。每次领取记录独立 attempt，平台 UUID 沿用 intent ID；明确限流在同一事务保存 rejected attempt 和等待后 ready。未知请求不重发，旧 schema 的未决意图也不补造请求证据。到期重新核验目录/分类并领取，保存 confirmed chat ID 后再单独绑定；诊断提供 `group_intents.deferred` 和 `group_attempts` 聚合计数。

schema 14 新增 `command_reply_attempts` 与 `command_reply_throttles`：确认回复的每次实际请求先保存原卡、原消息关系及 UUID；明确限流只重开回复，不重新执行命令。下一次使用新 UUID，保留 rejected 的原 attempt；未到期的回复不阻塞其他命令。unknown 和旧版未决回复不自动重发，status 提供 `command_replies.deferred` 与 attempt 聚合计数。

schema 15 新增 `request_reminder_throttles`：App 待处理提醒的明确限流拒绝与等待记录原子保存。重试使用新 UUID、保留原引用，并重新核验请求仍 pending、当前连接已确认、群成员与通知策略。等待期间经过完整静默窗口或 off / snooze 后恢复允许，不复活旧提醒；unknown 不重发。status 提供 `requests.reminder_deferred`，已解决 / suppressed 不计入待重试。

schema 16 新增 `outbound_request_markers`：新镜像 attempt 的隐藏 element_id、完整实际载荷、原 UUID、接收者和 outbox 领取在同一事务冻结。标记全局唯一且外键关联原 attempt；普通发送、引用回复、PATCH 使用新的标记，历史 attempt 不补标记。unknown 只通过 GET 核验，不重新发送；查不到 / 格式不同 / 分页截断不代表拒绝。核验间隔持久化为 30 秒，单次总预算 5 秒，失败不阻塞独立 ready 消息。确认与原 revision、消息 ID / 后续 PATCH 调度原子保存，status 提供 `outbound.readback_available`。原 schema 15 未决记录保持 unknown。真实飞书读回 / 显示仍待验收，见[接口与证明边界](../research/feishu-unknown-readback-2026-10-12.md)。

schema 17 新增 `card_request_markers`，在镜像、命令确认、待处理提醒之间统一保留唯一标记；原 schema 16 镜像映射原样保留，并登记到共享表，原镜像检查 deadline 不变。命令和提醒只核验原不可变 attempt 的实际带标记载荷；登记、原卡冻结和领取在同一事务，缺 source / 忽略 proof write 均不发请求。检查失败继续投递独立 ready 回复；已解决的 App 请求仍可确认此前送达，不恢复 pending 或通知资格。旧无标记命令 / 提醒继续保持 unknown。status 提供 `command_replies.readback_available`、`requests.reminder_readback_available`，仍不输出卡正文。

schema 18 新增 `verification_cards`，保存同一管理实例内显式授权的本人测试卡：原 build commit、运行二进制 SHA-256、UUID、令牌、完整卡和到期先落盘，随后 CAS 领取。重复 ready 仅匹配当前运行二进制摘要的构建可继续，dispatching 对外按 unknown 不重发；confirmed / rejected 均保持原结果。实际回调可能先于发送响应，记录原 message/chat ID，只有匹配 confirmed send ID 才报告回调组件 pass。私聊验证命令只记录证据，不产生任务收据。完整 P1 / App UI 不从组件记录推导；日志 DTO 不输出 token 或原卡，真实 M0 未迁移。

schema 19 新增 `verification_groups`：固定 owner 的名称、成员 JSON、UUID、commit / 运行二进制 SHA-256 与有效期在创建前保存，ready CAS 领取，未决不重建。创建与成员 GET / owner 群 @ 观察独立保存；只有原 confirmed chat 才能记录入站，不生成线程 binding 或输入收据。成员观察的首次时间不被重放延长，私有结构不输出成员名单 / UUID。只读查询不触发成员 GET，两个组件均通过也不提升 P1。

schema 20 新增 `verification_native`：内部隔离创建步骤冻结受限制请求与构建摘要，调用前单次领取并记录 generation 和每连接随机身份。重启后的相同数值 generation 不构成同连接证明；unknown / dispatching / rejected / confirmed 不重新创建。确定响应保存原出生历史，不代表新连接已附着。私有请求不输出，完整 verify、绑定与真实 UI 仍待完成，见[创建日志进展](../research/m3-native-verification-journal-2026-10-12.md)。

schema 21 新增私有 `verification_bindings`：仅将同一请求中已确认的验收群和验收线程原子登记到 observation、原意图与普通绑定表。记录构建、原目标和两个组件有效期的交集；重放不延长窗口，不覆盖其他绑定，也不修复缺失的派生记录。验收 observation 仍 excluded，不伪造首条输入。内部登记管理器尚未接入运行时授权、输入或 CLI；不能将登记视为已可运行的验证链路，见[绑定基础进展](../research/m3-verification-binding-foundation-2026-10-12.md)。

以下是逻辑表，不要求通用 ORM。使用迁移版本、外键、busy timeout；单写入器配合短事务。数据库含对话与身份数据，目录 0700、文件 0600。

| 表 | 主键 / 唯一约束 | 主要字段 |
| --- | --- | --- |
| deployment | host_id | app_id、基线建立时间、schema 版本 |
| thread_observations | host_id + thread_id | 已见、首条输入、分类、排除原因、发现水位 |
| bindings | host_id + thread_id；有效目标唯一 | group/topic、chat/root ID、状态、创建意图 |
| create_intents | intent_id；幂等业务键唯一 | 目标、阶段、远程步骤结果、unknown 状态 |
| input_receipts | receipt_id；app_id + message_id 唯一 | 输入、client 相关 ID、queue/turn/item、状态 |
| action_receipts | action_token + revision + action | 操作者、绑定、原始目标、处理结果 |
| rpc_attempts | attempt_id | 收据/意图、connection generation、RPC ID、请求摘要、响应分类 |
| mirrors | host/thread/item + segment_index | 角色、飞书 message/card ID、正文版本、来源 |
| outbound | delivery_id | send/update/reply、目标、稳定幂等标识、状态、重试时间、通知类型 |
| history_watermarks | host_id + thread_id | 已确认 turn/item 水位、补齐状态 |
| directory_rules | canonical_path + recursive | 规则与添加者；同一路径可有精确和递归两条 |
| notification_rules | app_id + open_id | enabled、snooze_until、quiet 时间段、时区、版本 |
| drafts | draft_id | 二期草稿、版本、唯一 create intent、有效期 |

动作令牌可额外独立表，也可和 action_receipts 合并；必须支持未执行令牌的授权、有效期与版本校验。不要将“按钮当前不可点”当数据库约束。

## 收据与操作记录

输入流程：`received → pending_submit → queued/executing → confirmed`。明确失败进入 failed；RPC 结果未知从任意发出阶段进入 reconciling。删除/引导有独立动作记录，不覆盖原始输入内容。

每次远程写入采用以下边界：

1. 事务内领取入站消息/动作，持久化输入、目标和操作意图，提交。
2. 写入当前 RPC attempt 为 prepared，再标记 dispatching，随后发 RPC。
3. 收到响应后另起短事务保存 confirmed/rejected 及远程标识。
4. 超时、连接断开或进程崩溃留下 dispatching 时，恢复为 unknown，排入核验器。

prepared 且能证明从未进入发送阶段的操作可以执行；dispatching 即使恰好尚未发出也按 unknown 保守处理。不能在“响应已到、数据库没写”时自动再次发送。

## 恢复顺序

服务启动先获取部署锁，打开并迁移数据库，加载绑定与规则，再建立两侧连接。正常新事件可持久接收；对一个尚未核验的会话暂停冲突写操作。已确认绑定可以同步补齐，不能因重连重新建群。

恢复器按会话串行完成：重新加入 → 读取完整原生队列 → 分页用户历史和公开回复 → 对照 clientId/远程 ID → 修正收据 → 补齐 outbox。先登记事件与镜像 outbox，再推进内容水位，避免“水位已过但消息未记”。

原生 cursor 不是可靠的永久事件位点。持久水位基于 thread/turn/item 标识；每次补齐保留一个重叠窗口，对旧 item upsert。长断线根据上次已确认历史继续分页，不能只查最近一页。

## Unknown 的核验规则

| 操作 | 足以确认的证据 | 不能据此下结论 |
| --- | --- | --- |
| queue/add | 队列中对应相关 ID；或历史 userMessage.clientId，保存实际 queue/item/turn | 某次队列没有该项 |
| steer | 同一相关输入已在目标 turn 的用户 item；配合动作阶段 / 已知队列删除结果 | 队列项已消失 |
| queue/delete | 本次成功响应 deleted=true；结果未知时结合队列与历史标注已移除/已消费或继续核验 | 消失就表示桥删成功 |
| 群创建 | 返回 chat ID，或通过受支持的稳定创建标识核验到同一群 | 群列表里没看到、群名相同 |
| 飞书发送 | 返回 message ID；或受支持的幂等 token/readback 确认同一次投递 | 超时就代表未发出 |

若查不到足够证据，保持“正在核验”，告知操作者具体阻塞；不能用等待若干秒来证明未接受。只有得到明确未接受的结果才自动重试非幂等操作。对于平台明确保证幂等的同一 token 请求，可在其保证范围和有效期内重试，不能把 token 换掉。

## Outbox 与通知

本地镜像 upsert 和 outbox 入队同一事务完成。发送/更新远程操作在事务外进行。更新同一张卡片按目标合并，只保留最新需要展示的版本；完成状态不得被晚来的生成中更新覆盖。

限流与网络重试只用于已确认可安全重试的操作。稳定 delivery_id 不代表飞书一定识别它：由 03 验证接口 UUID/幂等字段后决定实际重试策略。送达时重新计算 @ 规则；outbox 不存已拼接、可能过期的 @ 列表。

## 验收

1. 同一消息和按钮事件各重放十次，只生成一张收据、一次远程操作。
2. 在发送前、响应前、响应后保存前分别终止进程；重启没有重复输入或重复群。
3. queue/add 已执行但响应丢失，能从历史 clientId 找回，不重发。
4. 历史超过一页、断线超过重叠窗口，补齐没有漏项；已投递 item 更新原卡。
5. SQLite 磁盘满 / 锁超时，未持久化成功的操作不发 RPC；卡片明确接收失败。
6. 两个 serve 进程争用同一部署，第二个退出并报锁持有者，不抢占或删除锁文件。
