# 04 会话发现、自动建群与绑定

负责 `internal/discovery/`、`internal/bindings/`。依赖 01、02、03 和 07 的目录判断接口。

## 可自动绑定的会话

处理允许项目范围内、持久化、有实际人类输入的交互主会话。排除 ephemeral、存在父代理关系的子线程、桥 validation/creation 标记的线程。**2026-10-12 用户调整：fork 暂不实现，发现后记录 `fork_deferred`，不自动建群；代码保留 TODO。** 普通 App 主会话和注册工作树仍在本期范围内；不能因为 sessionId 与 threadId 不同就判为子代理。

来源不能写死：本机真正 App 与误用 CLI 的测试主线程都出现过 source=vscode、originator=codex-tui、threadSource=null；这些字段不能区分两种入口。M0 保存真实 App 新建、fork、worktree 与子代理的脱敏样例，再落实分类器。准确版本规则与边界见[分类研究](../research/thread-classification-0.162.0.md)：parentThreadId 与所有 subAgent 变体排除；forkedFromId 不排除；preview 不作首条用户输入证明。至少保证 App 主会话可发现；共享 daemon 内其他符合条件的交互会话可复用入口，来源页脚在无法细分时写“Codex 端”。CLI 回显仍不验收。

只看到 thread/started 不代表已经有首条任务。对候选读取元数据和首条 userMessage，空白草稿保持等待。新 thread/start 尚未生成 rollout 时可短暂读不到，按候选状态重查，不因此建空群或判永久失败。

## 基线与补偿扫描

第一次启动先保存毫秒 fence、连接事件流并持久接收出生通知，建立 thread/list 基线。已在启动前存在的历史只记已见，不批量建群。基线期间的出生通知和正常本机 0.162.0 UUIDv7 的正向新建证据优先于快照忽略记录；不能单凭 createdAt/updatedAt 判定新任务。边界无法判清的会话保持待检查，不能因扫描竞争大量补建旧群。将基线完成标志与历史忽略记录原子保存；重启不重置 fence。首输入前草稿通过 loaded/list + metadata-only read + typed turns/list 探测保留 pending，详见[准确版本基线研究](../research/discovery-baseline-0.162.0.md)。

平时使用 thread/started 快速发现；约每 15 秒按 updated_at 降序分页扫描最近 24 小时的普通/归档会话，并显式 modelProviders=[]。时间边界在翻页过程中判断，不先拉全量再过滤；边界相等包含，时间缺失或顺序异常时保守继续分页。thread/list 没有 updated_since 参数，不虚构该字段。loaded IDs 与持久 pending 始终另行检查，不受时间窗口排除。

首次基线保持完整；首次覆盖检查、断线超过 24 小时、时钟回退和约每 24 小时的低频补偿使用完整扫描，补偿延迟投影和窗口外历史。扫描覆盖时间独立持久化，不移动原始出生 fence；列表失败不推进覆盖，已落盘待检查的单会话错误仍继续重试。所有未绑定候选只有在当前 active 列表明确观察到后才能自动建群；缺少该证据时先降回 pending，出生事件、loaded ID 和可读历史仅保存候选证据，通常等待下一个扫描周期。归档 hold 在记录移出窗口后继续保留，不能把缺席或一次 metadata read 当作解除归档。乱序和重启仍通过已见表、首条输入及绑定唯一键去重。

静默期间创建但未绑定的候选保存最终判定原因。解除目录 mute 后不重新自动激活这些历史候选；用户仍可显式 /bind。

## 自动建群状态机

```text
candidate → eligible → create_intent_saved → creating_group
  → group_confirmed → binding_saved → backfilling → active

creating_group 响应未知 → reconciling_group
明确失败 → pending_retry（依具体可重试原因）
目录 mute / 非主会话 / 不在范围 → excluded
```

创建前在 SQLite 领取 `(host_id, thread_id)`，保存 stable intent 与预期成员，并先附着原生线程接收公开事件；建群在独立步骤执行，不阻塞事件存储。2026-10-14 按用户要求，任务群名统一为 `🤖 <会话标题>`，已有标识只保留一次，App 改名后仍保留此前缀。标题优先采用原生 `Thread.Name`，与 Codex App 的显示标题对齐，不加项目目录前缀、不截短有效 Name。Name 未提供时依次使用 preview、首条已清理附件包装的实际用户正文，附件单独输入使用简短任务名称；回退预览可缩短。工作目录放群描述。默认加入 owner 与机器人；其他用户只有列在 groups.additional_member_open_ids、且确认平台允许时才加入。notification target 不隐式扩大成员或操作权限。

新建群描述写 `工作目录：<首次附着的实际 CWD>`，完整保留 worktree 路径，不用项目根替代、不截断。schema 23 在首次 dispatch 前冻结描述，限流重试与重启保持原描述和 UUID；已有 attempt 保留旧版原描述，既有群由单独 GET / PUT / GET 核验更新，不随发现隐式修改。接口与长度建议见[群工作目录研究](../research/chat-working-directory-2026-10-13.md)。

已绑定群名跟随每次已授权元数据读取；`thread/name/updated` 沿用 threadId 唤醒 actor，即使事件丢失也会在周期元数据读取时补偿。首次建群 UUID 和请求名称保持冻结，不在创建中的请求上改名；绑定确认后以独立 PUT 仅更新 name。已确认标题在 actor 内缓存，未变更时不反复请求平台；新的连接代际会重新读回核验。改名使用现有 `m0_operations` 按 kind 保存私有冻结计划，无新增 schema：发送前事务领取，成功后 GET 核对；unknown / 崩溃 dispatching 仅核验冻结目标，不能被新标题绕过；明确限流保存截止时间，明确永久拒绝不反复发送相同标题。改名失败不阻断正文和队列，诊断仅提供状态数量。

2026-10-16 置顶标识：已确认的 `Pinned` 分区会话使用 `📌 🤖 <会话标题>`，取消后恢复 `🤖 <会话标题>`。名称始终从 Codex 原标题与最新 section 重算，不在飞书旧群名上反复增删图标，也不删除原标题自带的 emoji。首次从完整分区列表中定位唯一 `Pinned`，后续按持久 ID 识别，分区改名不依赖展示名称；首次同名歧义或列表失败时不猜测身份。普通自定义分区不视为置顶。

置顶查询独立于 24 小时窗口，每主机约 15 秒读取该分区的普通及归档完整 ID 集合，与上次成功集合比较；失败页不提交半份集合，进出分区只唤醒已有 actor，不因此为旧历史建群。最后成功集合和分区 ID 随部署保存；当前已授权 thread/read 仍是单会话展示的事实来源。新群使用当时已知的状态，绑定后继续对齐；topic 绑定不会改共享工作群名称。真实 App 的 section 双向变化已确认，本地候选服务已更新，4 个绑定置顶群名已有平台读回证据；客户端改名、限流与完整重启恢复 UI 仍 pending，见[实施记录](../records/pinned-group-names-and-windowed-discovery-2026-10-16.md)。

群 API 的幂等字段由 03 提供。响应未知先核验；没有可靠核验通道时停止该意图并显示阻塞，绝不根据群名猜测并再次创建。拿到 chat ID 后保存绑定，再发送任务卡并由 05 补齐首轮全部人类输入/公开 AI 内容。

即使建群较慢、AI 已完成，也补齐内容而不丢首条人类消息。创建失败的线程保留同步意图；恢复到原意图继续。

当前开发版已实现建群的持久限流等待：只有网关明确通用频控拒绝才重开原意图，原请求参数和平台 UUID 不变，每次尝试有独立记录。建群步骤等待期间不重新 Attach 或调用建群；到期由现有线程 tick 唤醒，无需用户追加输入。重试先用实际线程元数据重新核验分类、目录、mute，再事务领取；已 excluded 的历史不会复活。等待期间继续保存最终回复，拿到群后补齐。unknown、无 chat ID、权限拒绝和额度耗尽不自动重发。未决 dispatch 的崩溃恢复为 unknown。候选服务已实际部署并验证正常发现；平台限流恢复仍按隔离证据与剩余门槛分别记录，见[限流恢复进展](../records/m3-rate-limit-delivery-2026-10-12.md)和[集中验收](12-live-acceptance-handoff.md)。

## 显式绑定

提供 `/bind <thread-id>`：在工作群绑定本群，或在机器人私聊请求为已有会话新建专属群。核验操作者、主机、线程存在及已有绑定；已有绑定时返回现有位置，不静默改绑或另建第二个群。是否把旧上下文带入由绑定卡明确显示；首版默认补最近一轮，不对旧事件 @。

绑定记录至少包含目标模式、chat/root ID、owner、最新原生轮次、状态、首次建群意图和镜像水位。机器人被移除或群不可访问时暂停投递并保留记录，不自动建替代群。

开发验收绑定已接入现有 serve、管理通道和独立 `--allow-test-binding` CLI 授权：同一请求、构建与原连接，重新 GET 确认群仅含 owner / bot，经无覆盖附着检查后原子登记并请求 Runtime 激活。它只跟进已有的测试群与线程，不创建缺失目标或自动提交输入；登记、激活请求与 actor 存在分别报告。普通自动发现仍拒绝验证标签，不能借此绕过分类或目录规则，见[管理绑定组件](../records/m3-managed-binding-2026-10-12.md)。

每次原生写入与飞书投递均重新核验专属绑定和原资源；过期、资源关闭、连接失效不回退普通授权。首条输入前的 typed 空历史只由有效授权放行。只读查询提供已有输入关联与投递证据，不激活或修复绑定；这些本地记录不证明实际 App / 飞书 UI 或完整 P1 已通过，见[操作授权进展](../records/m3-verification-operation-guards-2026-10-12.md)及[受控投递证据](../records/m3-managed-delivery-evidence-2026-10-12.md)。

## 目录与 worktree

调用 07 的同一规范化函数处理 allowed_roots、mute 和二期目录校验。worktree 的实际 cwd 与可核验的原项目路径共同判断；项目 mute 命中任一项目路径即可禁止自动建群。没有可靠项目根时不得猜上级目录，保留 unknown_project_root，并在 M0 用真实 App worktree 解决该信息来源。

## 验收

1. 首次安装不会给旧历史建群；随后真实 App 提交首条输入只建一个群。
2. 草稿不建群，子代理不建群；fork 按用户本期调整保留 TODO，不自动跟进。
3. thread/started 丢失，补偿扫描仍发现；重放事件不重复创建。
4. 建群中断/超时、建群成功保存前崩溃，不出现第二个群。
5. mute / 递归 / worktree 生效；解 mute 不补建历史；显式 bind 仍可用。
6. 一条主会话只有一个有效目标，原群不可访问时不擅自替换。
