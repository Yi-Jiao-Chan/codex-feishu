# 02 存储、收据与恢复

负责 `internal/store/`、迁移、恢复协调器。依赖 00；必须在输入提交与自动建群之前可用。

## Schema 35：最终回复纠正重试与拒绝证据

`outbound` 新增 `retry_of_attempt_id`；`outbound_attempts` 新增同名来源字段以及飞书拒绝的 code、HTTP status、request ID。原 attempt、冻结卡片和结果保持不可变。只有终态 final 首次创建被明确拒绝、没有远端消息、没有 dispatching / unknown 或限流等待，且后续完整快照生成了不同的安全卡片时，才把该 final 重新置为 ready；新 attempt 必须引用被拒绝的来源。unknown 继续只读核验，不能因正文变化进入重发。

明确限流的后续 attempt 同样保留 retry 来源。带来源的 final 重试若再次被明确拒绝，保存受限错误字段，并在同一事务创建一张独立、纯文本、无提醒的投递失败卡；失败卡只显示安全的 code、HTTP status 和 request ID（无安全 request ID 时显示本地 attempt ID），不包含原回复、平台错误正文、路径或凭据。失败卡自己的拒绝是终态，不再生成下一张失败卡。旧 schema 34 的 rejected 记录不会在迁移时自动重发或补造失败卡，须由真实的新安全快照触发纠正。

## Schema 34：工作树快照恢复

新增 `thread_worktree_identities`、`worktree_restore_grants`、`worktree_restore_attempts`。第一张表保存已授权会话的原始 cwd、工作树根、源仓库、common-dir、相对子目录及不可变绑定；普通飞书输入首次入账时，在同一事务保存恢复授权。恢复日志冻结原收据、快照 commit、原目标路径、临时检出路径和物理身份，不复制消息正文。旧表、会话设置表单、原生 unknown 与出站日志保持原值。

迁移本身不发消息、不恢复目录。actor 只给确认从未提交的普通待处理收据补记 `legacy-pending` 授权；创建首条输入、异步答案、原生 attempt、queue/history/M0 写入证据均排除。collecting 附件保留 FIFO，封存且全部就绪后才能恢复；没有普通待处理输入的历史唤醒不触发恢复。

每线程最多一个未完成恢复，状态为 `prepared / dispatching / unknown / confirmed / blocked`。每个文件系统写阶段先保存 checkpoint；重启将 dispatching 作为 unknown，只读核验准确路径、快照、私有代际收据及双向 Git 指针。足够证据确认某阶段完成后才继续下一阶段；无法证明结果时保留 unknown，不重复 worktree add，不改投新线程。原创建 birth 不变，新恢复代际独立记录并参与 Runtime 与审计授权。

恢复中本地取消仍只取消原输入。后续写阶段重新检查收据；若核验已完成恢复，只记录物理事实，不提交取消的消息。未移动的完整阶段可记录 blocked/input-canceled 并保留临时目录，释放后续普通输入；已移动但无法确认的结果继续核验。命名工作树延迟发布，原输入取消后，只允许严格晚于它的普通收据在真实原生历史可见后完成发布。存在 schema 34 或恢复记录时不得回退不理解此协议的旧二进制，也不得用旧数据库覆盖新活动。详见[恢复记录](../records/feishu-worktree-auto-restore-2026-10-23.md)。

## Schema 33：会话设置表单

`session_settings_forms` 与 schema 32 的 `session_settings_requests`、`reply_settings_sources` 并存。每次不带参数的 `/model` 在同一事务中保存命令收据、表单和初始 UI mirror/outbox；表单阶段为 `loading`、`editing`、`saving`、`canceling`、`closed`。`loading` 完成后才发布编辑表单，编辑中的草稿不调用原生设置接口，也不阻塞普通队列输入或停止。

点击保存或取消时，先在本地事务中核验当前操作者、绑定、原卡消息、token、revision 和有效期，并冻结首个动作；同一动作、同一 revision 的精确重复回调幂等确认，动作或选择已变化的重复回调、竞争动作、过期、旧 revision、转发卡片或其他发起人的回调均失败关闭，首个保存/取消动作获胜。保存只提交一次已冻结的 `thread/settings/update` patch；取消不写原生设置，而是重新读取当前值。结果和 compact terminal 卡片都沿同一 UI mirror/outbox 更新原始卡片。

打开表单后若原生设置已变化，保存拒绝覆盖并要求重新发送 `/model`。原生写入结果为 unknown 时只读回核验冻结 patch，绝不重发；表单过期后须重新发送 `/model`。本地实现与协议证据不替代真实 App、飞书桌面端和移动端验收，后者仍为 pending。

## Schema 32：会话设置收据与回复引用区

`session_settings_requests` 保留原有设置请求、冻结 payload、dispatching/unknown 恢复和去重收据；没有新表单的旧请求仍走兼容恢复路径：`ready` 且尚未发送的请求执行此前已授权的 intent 一次，`dispatching` 和 `unknown` 只读回核验，绝不重发。schema 33 的新表单仍以对应请求收据作为持久父记录。写入前将准确参数冻结为 payload；重启后的 dispatching 和丢失响应的 unknown 都只读回核验，不重新发送。unknown 每 30 秒核验一次，未决时仍允许 `/model` 查询、普通输入和停止等已有任务控制。

`reply_settings_sources` 按 host/thread/turn 保存首次最终回复的紧凑引用区设置文本；同一轮所有分片复用，后续设置变更与重启不会改写。引用区的设置部分例如 `gpt-6-astra · max` 或 `gpt-6.1-sol · max · fast`，只在 Fast 已明确开启时追加 `fast`，不追加 `Codex`、字段标签或 Fast 关闭文案。历史补同步、设置读取失败或缺字段时省略缺失的思考强度和服务档位；仅模型缺失时显示 `模型未知`。快照不证明该轮每次模型调用采用了相同设置。

## Schema 31：原生 CoT 投递状态

新增 `cot_deliveries` 保存每个进度镜像的 cot_id、已确认公开投影和 completed 标志；原 outbound_attempts 新增 cot_step、cot_id、cot_events_json、cot_state_json、cot_state_before 与 cot_caught_up。继续复用原 mirrors/outbound、绑定、授权租约和启动恢复，不新增接收器或独立出站队列。CoT 文档以 `codex_feishu_cot` 标记与普通 Card JSON 区分；每轮首次投影后固定传输类型，旧卡片、旧 unknown 和回调均保留。

2026-10-26 起新进度文档使用展示版本 2，保存短标题、工具身份、安全详情和可选退出码/耗时；工具输入与输出节选经脱敏、截断后保存在可选 `input_preview` / `output_preview` 中，原始工具载荷不进入镜像。结果字段后到仍更正同一个 RESULT，完整结果参与 ACK 比较。旧版本 1 继续原样投影，活动轮次也不升级。StageSnapshot 拒绝同一镜像切换展示版本；新 ACK 保存 `document_version=2`，已开始但没有该字段的旧 ACK 按版本 1 解释，避免重启后重复标题或结果。停止目标查询同时识别版本 1/2。无需 SQL 迁移；出现版本 2 文档后，不能回退到只理解版本 1 的二进制或用旧数据库覆盖新活动。

单个 claim 只冻结 Create、一次 Append 或 Complete。确认 Create 只保存远程身份；Append 原子安装该批次确认后的投影；终态仅在 Complete 确认后推进整个 revision。运行态追上当前快照时可标记该 revision 已同步，后续快照继续追加。状态哈希不匹配、结果 unknown 或本地提交失败均不推进；重启 dispatching 转 unknown，不能通过普通 post 占位读回解冻。终态事件已确认的进度冻结原投影与完成原因，不因 Complete 前后迟到修正重新打开，最终答复仍可通过原链路更正。

原生 `/stop` 在命令入账的同一事务中冻结唯一未收口且非终态 CoT 的 turn_id，复用原 expected_turn_id 与 actor 的停止核验；有多个未收口过程或状态不明时不创建停止动作。重复入站沿原消息去重。平台裸 `/stop` 不包含可核验的点击时 run token，因此仍是对应线程的当前任务命令；只能保证接收后等待 actor 的期间不漂移到后来轮次，不能声称还原网络延迟前点击的旧 run。单聊继续不路由到任务。

## 当前实现边界（2026-10-15）

schema v4 已实现公开镜像、outbox、不可变投递 attempt、原生输入与飞书收据关联、完整历史扫描水位的事务保存。待建群的 eligible 会话可以先入库，绑定及父消息确认后才领取发送；过程更新合并到最新版本，终态不回退，同一线程只允许一个 dispatching 写请求。send / reply / PATCH 都在调用网关前落账，启动时把遗留 dispatching 转为 unknown，不自动重发。通知规则按 app/user 持久保存，以 revision 防止并发修改丢失；重新初始化默认值不覆盖已有设置。

完整历史到公开镜像的转换已实现，并通过真实 daemon 只读探针，后续已接入正式 `serve` 的开发实现。现有 M0 listener 未切换到这些新表；未知请求的远程核验和完整一期真实部署仍待完成，不能据此认定一期已支持生产恢复。基础验证记录见[投递基础](../records/m1-mirror-outbox-foundation-2026-10-12.md)、[历史同步与规则](../records/m1-history-sync-and-rules-2026-10-12.md)。

后续整合状态：正式 serve、通知安排与投递决策已有开发实现；schema 10 新增 `outbound_throttles`，为正式镜像、过程、队列卡的明确限流拒绝记录持久等待下限及退避次数。拒绝 attempt 保持不可变，下一次 claim 使用最新正文并重新核验提醒；unknown 不进入重发路径。该机制已通过隔离故障测试，现有 M0 未迁移；App 待处理提醒的限流已有下述隔离实现，unknown 读回核验仍待补齐，详见[限流投递进展](../records/m3-rate-limit-delivery-2026-10-12.md)。

## SQLite 结构

goal 自动续跑复用 schema 31：`Mirror.Direct` 明确表示进度或最终卡直接投递到绑定，其持久编码是 `parent_key` 与 `parent_message_id` 均为空，读回时恢复该标志。只有同步器核验原生 goal 来源后才创建这类公开镜像；未明确标记 Direct 的 AI 镜像仍要求恰好一个父消息。投递方式与父锚点一样不可变，重启和后续 steer 不改挂新输入；topic 领取时继续冻结原根消息。存在此类镜像后，旧二进制不能正确投递它们，不能自动回退或用旧数据库覆盖新活动。

2026-10-16 的发现覆盖检查点和置顶快照复用 `m0_operations` 的独立 kind（`codex.discovery_checkpoint`、`codex.pinned_snapshot`），不新增 schema 迁移。键按 host/app 分开，保存前核验 deployment；发现的 last scan/full 时间不改变 baseline 出生 fence。置顶仅在完整读取成功后原子保存分区 ID、完整会话 ID 集合和观察时间，失败不覆盖旧集合。ID 集合是私有恢复数据，不进入普通日志或 status 诊断。服务重连恢复断开时仍 active/retrying 的路由及持久待办，最后确认分区另行更新；其余已同步绑定保持 dormant，由 active/archive 元数据变化或新事件按需激活，不能将首次未知分区身份当成未置顶。自适应调度复用现有绑定、水位和操作日志，不新增 schema，也不把进程内 actor 状态写成新的授权事实。

schema 29 为创建草稿保存 `workspace_mode` / `base_branch`，为创建 workflow 冻结基准 commit、Git 根/common-dir 与工作树路径。旧草稿和 workflow 保持 local，既有正文、远程 ID、收据和 attempt 不变。工作树同样使用 prepared / dispatching / unknown / rejected / confirmed 日志，只有明确 rejected 才能显式 retry；unknown 只核验既有完整检出。Materialize 必须有匹配目标路径的 confirmed 工作树收据，cwd 使用最终执行路径。详见 [09 新建工作树](09-feishu-task-creation.md#新建工作树)。

托管工作树归属修复复用 schema 31：新的命名 bucket 路径在 intent 中冻结，`worktree` result 与外部私有 identity 收据记录物理代际，`worktree-owner` 冻结真实 thread ID 和身份摘要，`worktree-publish` 记录进入桌面扫描的标记发布。两个步骤均在文件操作前持久 dispatch；unknown 只核验，不补造缺失结果。新路径的 Materialize 必须有同 native ID 的 confirmed owner；Complete 必须再有 confirmed publish 和首条输入结果。旧已完成 workflow 不被重写，存量补登记使用独立 `worktree-owner-repair:<identity>` 日志且不改变其完成状态。旧二进制不了解新发布协议，未完成的托管创建不能交给旧版本续跑。

schema 11 新增 `legacy_adoptions`。schema 1 须经显式接管，不能由普通启动自动升级；库存摘要、旧绑定与 confirmed 消息 ID 和正式表在同一事务保存，原日志保留。存储基础与纯历史 mapper 已通过隔离测试，迁移 CLI、一致备份及锁已通过隔离测试，真实切换仍待验收，见[M0 接管进展](../records/m3-legacy-adoption-2026-10-12.md)。

schema 12 新增 `reaction_throttles`：reaction 的明确限流拒绝保存原操作日志和等待时间，下一 revision 依据最新输入状态领取。旧 reaction 的成功删除与 ID 清除在同一事务确认，后续 add 被拒绝时不再删除旧 ID。changing/unknown 不进入重试，reaction 失败不触发原生输入重试；status 的 `reactions.deferred` 表示等待或到期尚未完成的反馈。详见[限流投递进展](../records/m3-rate-limit-delivery-2026-10-12.md)。

schema 13 新增 `group_attempts` 与 `group_throttles`：建群请求引用原意图中冻结的名称、owner 和成员。每次领取记录独立 attempt，平台 UUID 沿用 intent ID；明确限流在同一事务保存 rejected attempt 和等待后 ready。未知请求不重发，旧 schema 的未决意图也不补造请求证据。到期重新核验目录/分类并领取，保存 confirmed chat ID 后再单独绑定；诊断提供 `group_intents.deferred` 和 `group_attempts` 聚合计数。

schema 14 新增 `command_reply_attempts` 与 `command_reply_throttles`：确认回复的每次实际请求先保存原卡、原消息关系及 UUID；明确限流只重开回复，不重新执行命令。下一次使用新 UUID，保留 rejected 的原 attempt；未到期的回复不阻塞其他命令。unknown 和旧版未决回复不自动重发，status 提供 `command_replies.deferred` 与 attempt 聚合计数。

schema 15 新增 `request_reminder_throttles`：App 待处理提醒的明确限流拒绝与等待记录原子保存。重试使用新 UUID、保留原引用，并重新核验请求仍 pending、当前连接已确认、群成员与通知策略。等待期间经过完整静默窗口或 off / snooze 后恢复允许，不复活旧提醒；unknown 不重发。status 提供 `requests.reminder_deferred`，已解决 / suppressed 不计入待重试。

schema 16 新增 `outbound_request_markers`：新镜像 attempt 的隐藏 element_id、完整实际载荷、原 UUID、接收者和 outbox 领取在同一事务冻结。标记全局唯一且外键关联原 attempt；普通发送、引用回复、PATCH 使用新的标记，历史 attempt 不补标记。unknown 只通过 GET 核验，不重新发送；查不到 / 格式不同 / 分页截断不代表拒绝。核验间隔持久化为 30 秒，单次总预算 5 秒，失败不阻塞独立 ready 消息。确认与原 revision、消息 ID / 后续 PATCH 调度原子保存，status 提供 `outbound.readback_available`。原 schema 15 未决记录保持 unknown。真实飞书读回 / 显示仍待验收，见[接口与证明边界](../research/feishu-unknown-readback-2026-10-12.md)。

schema 17 新增 `card_request_markers`，在镜像、命令确认、待处理提醒之间统一保留唯一标记；原 schema 16 镜像映射原样保留，并登记到共享表，原镜像检查 deadline 不变。命令和提醒只核验原不可变 attempt 的实际带标记载荷；登记、原卡冻结和领取在同一事务，缺 source / 忽略 proof write 均不发请求。检查失败继续投递独立 ready 回复；已解决的 App 请求仍可确认此前送达，不恢复 pending 或通知资格。旧无标记命令 / 提醒继续保持 unknown。status 提供 `command_replies.readback_available`、`requests.reminder_readback_available`，仍不输出卡正文。

schema 18 新增 `verification_cards`，保存同一管理实例内显式授权的本人测试卡：原 build commit、运行二进制 SHA-256、UUID、令牌、完整卡和到期先落盘，随后 CAS 领取。重复 ready 仅匹配当前运行二进制摘要的构建可继续，dispatching 对外按 unknown 不重发；confirmed / rejected 均保持原结果。实际回调可能先于发送响应，记录原 message/chat ID，只有匹配 confirmed send ID 才报告回调组件 pass。私聊验证命令只记录证据，不产生任务收据。完整 P1 / App UI 不从组件记录推导；日志 DTO 不输出 token 或原卡，真实 M0 未迁移。

schema 19 新增 `verification_groups`：固定 owner 的名称、成员 JSON、UUID、commit / 运行二进制 SHA-256 与有效期在创建前保存，ready CAS 领取，未决不重建。创建与成员 GET / owner 群 @ 观察独立保存；只有原 confirmed chat 才能记录入站，不生成线程 binding 或输入收据。成员观察的首次时间不被重放延长，私有结构不输出成员名单 / UUID。只读查询不触发成员 GET，两个组件均通过也不提升 P1。

schema 20 新增 `verification_native`：内部隔离创建步骤冻结受限制请求与构建摘要，调用前单次领取并记录 generation 和每连接随机身份。重启后的相同数值 generation 不构成同连接证明；unknown / dispatching / rejected / confirmed 不重新创建。确定响应保存原出生历史，不代表新连接已附着。私有请求不输出，完整 verify、绑定与真实 UI 仍待完成，见[创建日志进展](../records/m3-native-verification-journal-2026-10-12.md)。

schema 21 新增私有 `verification_bindings`：仅将同一请求中已确认的验收群和验收线程原子登记到 observation、原意图与普通绑定表。记录构建、原目标和两个组件有效期的交集；重放不延长窗口，不覆盖其他绑定，也不修复缺失的派生记录。验收 observation 仍 excluded，不伪造首条输入。登记管理器已接入 CLI / 原 runtime 激活与 owner 输入，所有原生及飞书写阶段核验原组件 / 资源 / 窗口。原 ID 查询新增只读输入证据快照，不增加 schema，不回写收据或恢复 unknown；登记 / 本地证据不代表真实 UI 或完整验收，见[绑定基础](../records/m3-verification-binding-foundation-2026-10-12.md)、[管理绑定](../records/m3-managed-binding-2026-10-12.md)与[输入证据](../records/m3-managed-input-evidence-2026-10-12.md)。

以下是逻辑表，不要求通用 ORM。使用迁移版本、外键、busy timeout；单写入器配合短事务。数据库含对话与身份数据，目录 0700、文件 0600。

schema 23 为 `create_intents` 增加 `description`。未曾 dispatch 的意图首次附着实际会话 CWD 后、领取 attempt 前事务冻结群描述；明确限流重试沿用原描述和 UUID。旧已有 attempt / unknown 意图保留旧固定描述，不改已发送请求参数，也不重新创建已有群，见[群工作目录研究](../research/chat-working-directory-2026-10-13.md)。

schema 26 增加附件批次、来源收据和有序资源表，并把附件输入关联到现有 submission。资源按 `(host, app, thread, chat, sender)` 隔离；`source receipt`、草稿 revision、资源顺序和状态在数据库中保存。封存后的 `collecting` 输入在所有资源 ready 前保持等待，整体失败才释放后续普通提交；失败的本地下载重试移到队尾并保留原 client ID，没有 native attempt 才允许重试，unknown 一律不重发。冻结的 `input_json` 原样用于 queue/add、steer 和 restore，`Body` 仅作显示摘要。

资源缓存目录默认位于数据库旁的 `attachments`，目录 0700、文件 0600，完成资源以 SHA-256 校验后原子发布；下载租约 90 秒，重启可恢复。首次 GET 使用真实有界流式读取，限制图片/文件大小、像素、批次和磁盘配额。关闭附件开关后仍完成已经封存的批次，不再接收新资源；未封存草稿保留，但带附件提交会被拒绝并提示重新开启。只读 `attachments status` 返回开关、聚合统计和磁盘额度；cleanup 必须在桥停止并取得应用锁和部署锁后执行，只删除未被非 cleared/nonremoved 状态引用的成品或 partial，queued/unknown 资源保留到会话结束。

schema 27 将 `bindings` 扩展为可区分 group/topic 的 follow target，保存根消息、平台 thread、repair 标记并保持有效目标唯一；topic 的镜像和 outbound 继续沿用目标身份，不因根消息失效退回主群。旧 group 绑定迁移为 `follow_mode='group'`，健康诊断只按 deployment 的 host/app 统计 `group`、`topic` 和 `repair_required` 聚合。

schema 28 新增 `creation_drafts`、`creation_workflows` 和 `creation_attempts`，并为 `m0_receipts` 增加 `reply_message_id`。草稿按 `(host, app, source_message_id)` 去重；workflow 与每个远程步骤分别保存冻结参数和状态，`unknown` 经恢复仍不可直接重发，明确 `rejected` 才可显式创建新 attempt。跨 chat 创建保留原 source message 用于去重，把新目标根消息写入 receipt reply anchor。健康接口在 schema 28 及以上才查询这些新表，并返回状态计数，不输出正文、目录、消息 / thread / attempt ID、payload 或凭据；旧 schema 保持空聚合且不访问新表。

schema 30 为 `creation_drafts` 增加 `attachments_json`；旧草稿迁移为空附件。该字段保存有序资源描述（来源 `key`、`name`、`kind`），资源本身不可改写，草稿允许逐项移除。移除从数据库当前 revision 读取原序列，与正文、目录和其他有效表单字段通过同条 token/card/creator/状态/期限 CAS 更新；普通草稿更新不修改附件，旧版本回调不能再次删除移动到该索引的项目。`MaterializeCreation` 只封存确认时剩余的附件，在同一事务建立批次和 `collecting` 收据；原消息 ID 供资源 GET 使用，新根消息只作为回复锚点。

新交互复用现有字段，不增加 schema：新草稿默认 `workspace_mode='worktree'`，旧草稿不变；主视图的 `browse_path` 保留查询输入，`browse_cursor` 的 JSON 独立保存已执行查询、搜索根和翻页位置，未点击查找的编辑不改变结果。创建附件预览复用私有 `m0_operations`，键按版本、host/app、原消息和资源摘要隔离。图片上传前原子领取 sending，done 缓存应用图片 key，sending/unknown/rejected 均不重传；文本预览只缓存有界内容。取消或过期后不再启动预览，也不开始完整下载、群或会话创建，既有缓存不回滚过去的请求。

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
| creation_drafts | draft_id | 二期草稿、正文、`attachments_json`、版本、唯一 create intent、有效期 |
| attachment_batches | batch_id；(host, app, thread, chat, sender) 的 open 唯一 | 草稿/封存状态、revision、来源 receipt、首条消息 |
| attachment_events | app_id + message_id；receipt 唯一 | 原消息与附件批次关联 |
| attachment_parts | part_id；batch_id + ordinal 唯一 | text/image/file、资源 key、顺序、下载状态、大小、摘要、文件 ID |

动作令牌可额外独立表，也可和 action_receipts 合并；必须支持未执行令牌的授权、有效期与版本校验。不要将“按钮当前不可点”当数据库约束。

## 收据与操作记录

输入流程：`received → pending_submit → queued/executing → confirmed`。明确失败进入 failed；RPC 结果未知从任意发出阶段进入 reconciling。删除/引导有独立动作记录，不覆盖原始输入内容。

新的普通输入可以在首次入账事务内保存收据关联的 `kind=continue` 意图：专用 token 为 `input-continue.<receiptID>.<source>`，source 是固定的源停止 action ID，或已观察到 App 中断时的 `app`；目标轮另存既有 `expected_turn_id`。只在输入确认接收后才领取 start，后到停止优先，unknown 仍核验原 attempt。停止卡片 claim 与对应原生停止动作也同事务入账，避免新输入夹在两次事务间丢失继续意图。旧收据、下载完成与恢复不补记此意图，不增加 schema。

虽然 schema 仍为 31，旧二进制不理解这些新型继续意图的先后约束；存在 `input-continue.%` 且 state 为 ready/running/unknown 时，不能自动回退旧二进制，更不能恢复旧数据库覆盖新活动。CoT 停止的成功命令回执保存为空卡、`state=confirmed` 的本地不投递收据，仅用于去重；它不代表原生停止成功，停止仍以独立 action/attempt 和原生状态为准。

异步问题的显式追加复用现有 schema 30 收据与动作日志，不增加表或迁移。仅新按钮回调在一个事务内保存 interaction attempt、完整回答 receipt、submission input 和 `kind=steer, stage=prepare, queue_id=''` 的初始动作；先补齐较早未被 actor 收取的收据顺序，再登记新答案。旧 queued steer 的 queue_id 非空，仍走删除后引导；旧回答不生成初始动作。发送前把目标 turn 固定在 action/attempt；明确拒绝进入 restore，unknown 按原目标 turn/clientId 核验。重连不得改投新轮、盲目补发或复制一份替代收据。 虽然数据库 schema 未变，旧二进制不能处理这类未完成的初始追加动作；回退前须确认这些动作已安全结束。不得为回退二进制而用旧备份覆盖升级后已产生的会话活动。

每次远程写入采用以下边界：

1. 事务内领取入站消息/动作，持久化输入、目标和操作意图，提交。
2. 写入当前 RPC attempt 为 prepared，再标记 dispatching，随后发 RPC。
3. 收到响应后另起短事务保存 confirmed/rejected 及远程标识。
4. 超时、连接断开或进程崩溃留下 dispatching 时，恢复为 unknown，排入核验器。

prepared 且能证明从未进入发送阶段的操作可以执行；dispatching 即使恰好尚未发出也按 unknown 保守处理。不能在“响应已到、数据库没写”时自动再次发送。

## 恢复顺序

服务启动先获取部署锁，打开并迁移数据库，加载绑定与规则，再建立两侧连接。正常新事件可持久接收；对一个尚未核验的会话暂停冲突写操作。已确认绑定可以同步补齐，不能因重连重新建群。

恢复器按会话串行完成：重新加入 → 读取完整原生队列 → 分页用户历史和公开回复 → 对照 clientId/远程 ID → 修正收据 → 补齐 outbox。先登记事件与镜像 outbox，再推进内容水位，避免“水位已过但消息未记”。

附件恢复先恢复租约和可验证的已发布文件，再按批次状态推进下载；封存但仍 `collecting` 的提交挡住同线程后来的普通输入，全部 ready 后才冻结 `input_json` 并进入原生提交，整体 failed 后释放队列。创建附件下载失败时，原创建卡的“继续创建”复用原会话、群和收据重试，不先提交纯文本；原生写入 unknown 只做核验。队列快照优先读取只含 ID/client ID 的 metadata DTO，避免把图片数据 URL 留在本地收据；历史和事件仍按完整资源引用核验。

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
7. schema 26/30 的附件草稿、封存批次、下载恢复和清理不越过 host/app/thread/chat/sender 隔离，不删除 queued/unknown 仍引用的资源；旧草稿附件为空，关闭开关后已封存批次继续完成，未确认草稿拒绝带附件提交并提示重新开启。
