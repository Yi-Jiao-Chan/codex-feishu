# 01 共享 Codex 客户端

负责 `internal/codexclient/`、`compat/codex-*.json`、相应脱敏录制。依赖 00；提供所有模块使用的原生协议入口。

## 接入流程

1. 在 App 连接的同一远程系统用户下调用真实命令 `codex app-server daemon version`，解析 status、socketPath、cliVersion、appServerVersion。
2. 连接现有 socket 上的 Unix WebSocket；不另起 stdio app-server 来“恢复”活跃任务，不重启现有 daemon。
3. 发送 `initialize`，clientInfo 使用桥自己的名称，启用 `capabilities.experimentalApi=true`；保存响应中的实际 `codexHome`，成功后发送 `initialized` 通知。该路径用于 goal 来源读取和托管工作树创建、核验与快照恢复，不从桥进程的 HOME / CODEX_HOME 猜测；字段缺失不影响普通连接，但不能提供这些证明。连接 generation 同时保留 discovery 的实际 `appServerVersion`，工作树兼容层只接受 0.162.1，见 [09](09-feishu-task-creation.md#新建工作树)。
4. 单 reader 持续收 JSON-RPC 响应、通知和服务端请求；request ID 关联等待者。未知事件保留方法名供诊断，不让解码错误断开全部任务。
5. 加入已有线程用 `thread/resume`，参数只含 threadId、excludeTurns=true；不传 cwd、model、approvalPolicy、sandbox 等覆盖值。
6. 断线后重查 socket 和实际服务端版本，重新初始化、加入已绑定会话，然后交给恢复器补齐历史与队列。

远程 SSH 工作方式和 App Server 生命周期见 [OpenAI 远程连接](https://learn.chatgpt.com/docs/remote-connections)、[App Server](https://learn.chatgpt.com/docs/app-server)。本手册的 queue 细节以本地 0.162.0 生成协议和实验记录为证据，不能仅凭公开概览判断兼容。

## 方法清单

| 用途 | 原生方法 / 注意事项 |
| --- | --- |
| 发现 / 元数据 | thread/list、thread/read；本地不传非空 originators 过滤 |
| 近期发现 | thread/list 按 updated_at 降序分页，跨过固定 24 小时边界即停止；没有 updated_since 参数 |
| 置顶集合 | threadSection/list 定位分区；thread/list(sectionId, section_position) 分页读取完整集合，覆盖普通及归档、全部 sourceKinds |
| 观察加入 | thread/resume(excludeTurns=true)；复用当前执行器 |
| 分页补齐 | thread/turns/list、thread/items/list；分页 cursor 是不透明字符串 |
| 原生队列 | thread/queue/list、add、delete、start |
| 显式引导 | turn/steer；expectedTurnId 必填 |
| 停止 | turn/interrupt；保留原生队列 |
| 会话设置 | thread/settings/update；仅显式命令发送 model、effort、serviceTier 的部分更新，后续轮次生效 |
| 模型选项 | model/list；分页读取模型、支持的思考强度与服务档位，不固定模型名称或强度列表 |
| 二期新建 | thread/start；cwd、可选 projectId、自建 threadSource |
| 二期项目选择 | project/list，必要时 project/read；实际字段从固定 schema 读取 |

queue/add 参数是 threadId、input、clientUserMessageId；返回 queuedSubmission，其中原生项标识字段叫 **id**。queue/list 分页返回 data 和 nextCursor。删除参数字段叫 queuedSubmissionId，返回 deleted 布尔值。历史 userMessage 的相关字段叫 **clientId**，不要误写成 clientUserMessageId。

空闲 queue/add 会自动执行，因此收到成功响应后立刻刷新队列/历史；若项已被消费，不能卡片永久显示“已排队”。queue/start 忙时会拒绝，不是 steer 的替代方法。

## 当前会话的模型设置

聊天入口只有不带参数的 `/model`。它在原任务群或准确任务话题中打开原生飞书表单；读取使用同一 daemon 上不带覆盖参数的 `thread/resume(threadId, excludeTurns=true)`，读取顶层 model、reasoningEffort、serviceTier，并核对返回线程身份。0.162.1 的 `thread/read` 不返回 serviceTier，Turn 也不提供每轮实际推理配置；因此回复引用区表示会话默认设置，不是逐次模型调用的遥测。

表单草稿只在飞书卡片中编辑，保存前不写入原生设置；点击保存后才使用一次 `thread/settings/update`。请求在写入前冻结 model、effort、serviceTier 的部分更新，不通过 resume 覆盖权限、目录、hooks 或全局配置。省略字段保持原值；serviceTier 的省略、null 和字符串独立表示，不能把缺失或 null 当作 Fast 关闭。Fast 开启仅选择模型目录实际公布的 `fast`，或其 `priority` 档；关闭显式发送 `default`。更新 RPC 可以在当前轮次运行时发送，只改变后续默认设置，不重启或 steer 当前轮次。

保存的远程结果丢失或未知时，先读回原线程并按已冻结字段核验，绝不重复发送；打开表单后原生设置若已变化则拒绝覆盖并要求重新发送 `/model`。取消只读回最新设置，不发送原生更新。模型选项由 `model/list` 的完整分页目录提供，表单中的模型、思考强度和 Fast 均按该目录校验。

协议依据为本机 0.162.1 离线 experimental schema 和合成 WebSocket fixture，见[会话设置兼容清单](../../compat/codex-session-settings.json)。模型目录的结构也可参考 [OpenAI App Server](https://learn.chatgpt.com/docs/app-server#models)；准确版本字段以离线 schema 为准。新设置路径尚未进行真实 App / 飞书 UI 验收，不扩大既有版本发布结论。

## 事件适配

最低覆盖 thread/started、线程状态变化、turn 开始/完成、item 开始/完成、公开 agentMessage 增量、thread/queue/changed。具体 method 名与 payload 从已验收的生成协议绑定，避免按口头命名猜事件。

0.162.1 的置顶由 `Thread.section` 表示：实际 App 置顶使会话进入 `Pinned` 分区，取消后 section 为空；该版本没有独立的 `isPinned` 字段或置顶通知。桥每主机约 15 秒查询一次完整置顶集合，仅唤醒有变化的已绑定会话，再用其最新授权元数据计算群名；不为每条历史会话新增置顶 RPC。分区与近期分页适配拒绝缺失/null data 和重复 cursor，读取失败不能伪装为空集合。[兼容能力清单](../../compat/codex-pinning.json)、[实施与真实只读证据](../records/pinned-group-names-and-windowed-discovery-2026-10-16.md)。

服务端审批/用户输入请求独立于普通通知：M0 记录请求类型，转任务为“待用户处理”，提示回 App。未经真实双客户端验证，不在飞书生成允许/拒绝或问答提交按钮。

普通 server request 必须透传且保持静默，不能自动回 method-not-found 或空 result；同实例内首个 result/error 会消费共享 callback，抢在 App 前拒绝审批或清空答案。用 generation、thread ID、原始请求 ID 去重；`serverRequest/resolved` 按对应请求解除等待，轮次终态清理该轮等待。M0 以同一过程卡显示“等待你在 Codex App 中处理”，不另发一张消息。特殊 UserVerification 不套用普通审批的 owner 规则。[准确版本审批路由研究](../research/approval-routing-0.162.0.md)

隔离原生问答已证明同一请求/replay、桥静默与断开、另一端回答后继续及 resolved；这仍不能代替真实 App 问答 UI。命令审批探针在本机 `permissionRequest` hook 等待阶段超时，尚未到达原生审批，保留 pending。生产沿用原线程的 hooks 和策略；不得为消除测试阻塞关闭 managed hooks。Default 模式同步问答有 feature gate，工具出现在 schema 中不代表该模式可调用；仅自有 fixture 配置开启该 gate，桥不改真实线程配置。

2026-10-14 扩展：新增 `RespondServerRequest`，在原 ID 上发送 `result`，保留整数/字符串类型、验证完整 DTO，统一写入 gate 最终检查 generation/请求/轮次终态。reader 先记闭合 tombstone，迟到 replay 不能恢复回调；不持 reader 锁等待网络。原生响应没有 ack，written 仅记 submitted，`resolved` 仅证明闭合。0.162.1 隔离桥端同步问答已验证；其他类型按[13 审批与问答](13-feishu-approvals-and-questions.md)逐项验证后开启。

事件携带 connection_generation。重连产生新 generation，旧连接的晚响应不能确认新尝试。事件缓存有界；溢出时标记需要补齐并暂停依赖实时轮次的操作，不能静默丢失后继续宣称状态准确。

旧执行卡及原生 CoT 短标题的工具摘要读取结构化 item：命令的 `commandActions` 提供尽力解析的 read / search / listFiles / unknown 分类和 name/path，标题优先展示校验后的 basename；文件变更使用 path/kind 区分写入、编辑、删除，MCP 与动态工具提供名称元信息。已有 `Command` 在本地通过有界的纯词法子集识别具体用途，标题只用固定动作词或安全脚本名，不执行命令或读取目标文件。行内脚本标题概括脚本类型；详情使用下方的独立展示副本。停止控制版本继续复用原有摘要，参数、输出或 diff 的变化不改变控制版本。工具分组、计数和公开 commentary 的展示关联由桥计算，协议没有现成的组摘要字段。MCP 的实时 `item/mcpToolCall/progress` 没有历史消息数组，不作为可恢复详情的唯一来源。

该字段集依据 0.162.1 的本地离线 experimental schema，生成摘要与 [固定协议基线](../../compat/protocol.go) 一致；没有因此修改兼容版本白名单或启动替代 app-server。协议事件和工具类型概览见 [OpenAI App Server](https://learn.chatgpt.com/docs/app-server#items)。

原生 CoT 接收 nullable `exitCode` / `durationMs`：退出码只用于 commandExecution，耗时只用于 commandExecution、mcpToolCall 和 dynamicToolCall；fileChange 没有这两个字段。缺失/null 与 0 分开，投影仅接受 int32 范围退出码及非负 int64 毫秒值，不用首次观察时间推算耗时。字段形状已核对本地 0.162.1 schema 与[固定官方源码](https://github.com/openai/codex/blob/25f50de6ed95627e1ffe7f11ca30d3d62c6e20e6/codex-rs/app-server-protocol/src/protocol/v2/item.rs#L255-L317)。

按工具详情反馈，版本 2 的展开区另外解码同一完整历史已有的文本字段：commandExecution 的 `command` / nullable `aggregatedOutput`，MCP 的 `arguments`、`result.content` 中的 text、无文本结果时的 `structuredContent` 和 `error.message`，dynamicToolCall 的 `arguments` / `contentItems.inputText`，以及 fileChange 的已有 diff。`result` 按原生类型解码，图片生成的同名字符串字段不会被当成 MCP 对象。图片、音频、resource、`_meta` 等非文本字段不透传；工具 `PublicText()` 仍为空。投影前做常见凭据脱敏、控制字符/宿主目录处理和截断，只有展示副本进入镜像；原始历史和诊断输出不改写，不新增读文件、模型请求或工具执行。具体边界见 [05 同步](05-message-sync.md#新执行进度使用原生-cot)。

## 兼容清单

发布清单至少记录：实际 app-server 版本、CLI 版本、协议 schema 摘要、支持的方法与字段、27 项实验结果、真实 App / 飞书联调报告。版本与方法字段同时核验；仅 CLI 版本正确不算通过。

能力检查首先采用 daemon 信息、只读 RPC 和随二进制发布的验收清单。需要写操作才能证明 queue/steer 时，只在独立验收线程运行，不向现有工作会话注入测试。未验收版本默认暂停提交和自动建群，doctor 报具体差异；不自行降级到 exec resume。

## 开发切片

- C1：endpoint discovery、Unix WebSocket、握手、RPC 与通知分流、优雅退出。
- C2：只读元数据、分页历史、队列与断线重连。
- C3：queue/add/delete、steer、interrupt/start 的三态结果与 expectedTurnId。
- C4：二期 thread/start、project 元数据；首条输入前 rollout 不可见的边界交给 creation。

## 验收

1. 与 App 同时加入同一活跃 thread，看到同一 turn；没有覆盖原权限和目录。
2. 增量事件和最终 item 都收到；断开桥连接不影响 App 的运行。
3. 错误 expectedTurnId 明确失败；重复 clientUserMessageId 不被客户端假定去重。
4. daemon 重启导致 socket/版本变化时重查，旧请求进入 unknown 而不重发。
5. 不支持的版本、握手字段或方法返回具体能力故障；没有新起替代服务。

复用[本地验证脚本与结果](../records/probes/codex-feishu-probes-2026-10-10/)作为协议测试依据。重新运行脚本会创建测试线程，按 11 的环境边界执行；无需每改一张卡就重跑全部原生实验。

隔离验证创建入口已有 `Client.StartLocalVerification` 的开发实现：只接受本人私有物理目录与本机 loopback 模拟模型，固定 readonly / never 和测试标签，并核验返回策略 / 元数据。它只复用既有连接、一次创建、不启动 turn；上层须发送前持久领取。默认 WebSocket fixture 已通过，新入口的真实隔离重验、绑定 / 清理与完整 UI 仍 pending；完整 verify 尚未开放。见[隔离创建适配器进展](../records/m3-native-verification-adapter-2026-10-12.md)。

内部 `NativeManager` 已补齐准确请求落盘和单次创建领取。`Client.ConnectionID` 提供每连接随机身份，与进程内 generation 共同核验；新连接不能复用旧连接创建尝试。创建证据与附着 / 完整 UI 验收分开，见[创建日志进展](../records/m3-native-verification-journal-2026-10-12.md)。

验收专用 `AttachLocalVerification` 仍只发送 `thread/resume` 的 threadId 和 excludeTurns，不覆盖原配置；同时检查完整返回中的固定模拟模型、provider、原 cwd、never 与只读无网络策略，以及验收标签和原线程身份。出生时要求空历史，附着时允许已有 preview / turns。它仅是本地协议 fixture 已通过的内部适配器，尚未接入正式运行时，真实 daemon / UI 不由此推导，见[绑定基础进展](../records/m3-verification-binding-foundation-2026-10-12.md)。

`NativeSessions` 已组合私有目录、固定 loopback 模型和上述创建步骤；资源到期、关闭和进程重启不更换原请求端点或重建线程。模型 Release 只是本地时序控制，不是原生 steer / start。开发管理通道 / CLI 已接入创建组件，绑定、输入、远端清理与真实重验仍待完成，见[本地资源进展](../records/m3-native-verification-resources-2026-10-12.md)和[管理创建组件](../records/m3-managed-thread-creation-2026-10-12.md)。

### 原生问答结果的只读收据

0.162.1 的 `serverRequest/resolved` 不携带答案，typed 历史也没有原生问答 output。`QuestionResultReader` 仅读取 daemon 提供的 rollout 路径：验证所属用户、可信 sessions 根目录、session ID、明确 turn 边界、唯一 call/output 和规范化后的原问题；结果独立保存为收据。重复 JSON key、身份歧义、目标 turn 内的历史重写、部分答案和中断后的迟到输出均返回 unavailable。文件和单行有界，不修改原生记录或全局配置。

实际工具结果优先于飞书提交意图。读取成功只证明工具返回了这些回答，不说明哪端赢得竞态。该结果用于飞书已处理卡；App 的外部选择态回显保持已知限制，不使用会改变模型历史的 `thread/inject_items` 伪造 UI 状态。

### Goal 自动续跑的来源证明

0.162.1 的完整历史允许没有 `userMessage` 的 goal 自动续跑，但 v2 Turn 没有导出 `turn_trigger`；`rootTurnId` 自指或当前存在 goal 都不能单独证明来源。`UnanchoredTurnReader` 从 daemon 返回的原生路径读取 `session_meta.id` 和唯一 `event_msg.task_started` / `turn_started` 的 `turn_attribution.turn_trigger=goal`，并要求启动记录与归因中的 `turn_id` 同时匹配目标轮次。不从模型文本、工具参数、压缩摘要或前一轮用户输入推断。

同一读取器也识别精确的 `capacity_retry_manual` 来源：App 遇到模型容量错误后，手动重试可能创建没有新 `userMessage` 的独立轮次。经相同身份校验后，这类轮次可以直接同步到原绑定，不补造人类输入或借用失败轮次的回复锚点。仅凭前一轮失败、缺少输入、根轮次自指或其他未知 trigger 均不放行；不代替用户重试模型请求。

显式点击飞书失败卡的“重试”时，0.162.1 适配器使用 `turn/start`、空 `input` 及 `turnTrigger=feishu_retry_manual:<attempt_id>`，不发送模型、思考强度、服务档位、目录或权限覆盖。suffix 只接受 32 位小写十六进制 attempt ID。读取器同时核验这类无新输入轮次，并可按准确 nonce 返回唯一的新 turn 身份；重复归因、缺失记录和其他 App 轮次不能结算 unknown。`TurnError` 只保留原生分类，不保留 provider 错误正文或额外详情。[重试协议清单](../../compat/codex-turn-retry.json)

可信根仅取同一连接 initialize 返回的 `codexHome/sessions` 和 `archived_sessions`，复用问答读取的普通文件、所属用户、逐级无 symlink 和无外部写权限校验。读取上限为 256 MiB、单行 32 MiB；只保留来源元数据，不保存或输出原始记录。重复 JSON key、重复启动身份、缺失或不匹配的 session 均不能证明目标轮次。缓存核对文件身份、大小、时间及内容摘要；活跃文件追加时复核已读取前缀没有变化，截断或重写须重新核验。成功结果交给消息同步器决定投递方式，不修改原生历史、goal 或会话策略。[兼容清单](../../compat/codex-goal-turns.json)
