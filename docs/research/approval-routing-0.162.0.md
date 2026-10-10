# Codex 0.162.0 审批与原生问答路由

日期：2026-10-10。官方版本 `rust-v0.162.0`，固定 commit `c1382380de69521303b416720a52f42d51af6248`。协议对照本机 `/tmp/codex-feishu-research-20261010/protocol-0.162.0/`，实现与测试读取对应版本官方源码。本次仅调研与写文档；没有操作原生线程、读取用户 rollout/auth/env、执行审批、修改原线程策略或验证 App UI。

## 结论

**同一个 app-server 实例内**，普通命令审批、文件审批、权限请求以及 `item/tool/requestUserInput` 发给线程当时的全部订阅连接。没有“线程创建者独占审批”，也没有“queue/add 发起者独占审批”。App 若仍订阅该线程，就可以收到并处理请求；桥不回复不会让服务器等待桥的一份额外确认。[线程事件分发](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/thread_lifecycle.rs#L346-L366)、[请求发送到订阅集合](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/outgoing_message.rs#L172-L181)

**第一个 JSON-RPC result 或 error 消费同一个全局 pending callback。** 桥如果对“不支持的方法”自动返回 `-32601`，或者返回空 result，会抢在 App 前结束该请求：命令/文件审批拒绝或失败，问答成为空答案。后到的 App 回复只会找不到 callback。这比桥保持静默风险更大。[首个响应取走 callback](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/outgoing_message.rs#L467-L517)、[普通请求无连接 owner 检查](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/outgoing_message.rs#L561-L579)

**没有其他客户端实际回答时，保持静默可以持续挂起。** 普通审批与问答回调没有通用服务端超时，断开桥也不自动拒绝或完成它们；后续同实例 `thread/resume` 会重放普通 pending 请求。线程中断、轮次切换、卸载或服务停止是另外的取消路径，不能当作正常断开桥的行为。[断开清理](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/outgoing_message.rs#L275-L280)、[重放请求](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/outgoing_message.rs#L446-L463)

“同实例”是必要边界：只共享 thread ID、Codex home 或历史存储，不等于共享运行线程、订阅集合和 pending 请求。另启动一个 app-server 再 resume 不能从这套实现推出原 App 会处理桥实例里的审批。`MessageProcessor` 自建 `ThreadStateManager`，callback 表也在该进程 sender 内存中。[实例内状态](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/message_processor.rs#L307-L318)、[callback 与连接集合](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/outgoing_message.rs#L127-L157)

## 实现事实与场景

| 场景 | 0.162.0 实现事实 | 对 M0 的影响 |
| --- | --- | --- |
| 桥 `thread/resume(excludeTurns:true)` | warm resume 仍将调用连接加进线程订阅，发送 response 后重放 pending 请求；`excludeTurns` 只影响历史载荷，不是只收 notification 的 observer 模式。 | 桥会收到已有及后续审批/问答，必须识别 server request；不能当成自己的普通 RPC response。 |
| 桥 `thread/queue/add` | `ThreadQueueRequestProcessor::add(params)` 没有 connection ID 参数，只把输入入队；不设置审批 owner。 | 已经 resume 的桥保持订阅，原 App 若仍订阅也接收后续请求；仅调用 queue/add 本身不是审批订阅或转交协议。 |
| 原 App 创建了线程，仍在订阅 | 每个事件读取当前 subscribed connection IDs，向该集合发同一个 request ID。 | App 与桥都可能收到；创建者身份本身没有额外优先级。App 实际是否还订阅、是否显示交互，仍需验证。 |
| 原 App 切换到别的线程 / 取消订阅 | 请求发送时只取现有订阅集合；没有创建者自动补投逻辑。 | 不能只因原 App 窗口仍开着就保证审批有人处理。App 后续 resume 可以收到重放。 |
| 桥不回复，App 回复 | 一个 callback 等一次响应，App 可完成；不要求桥也回复。 | 可行的 M0“交给 App 操作”路径。桥记录等待状态并等 `serverRequest/resolved`。 |
| 桥先回复 error / 错类型 result | 通用回调先被移除，再由对应 handler 解码；解码失败也不会重新等待 App。 | 不支持的普通审批不能自动返回 method-not-found、空对象或空答案。 |
| 桥断开，App 仍订阅 | 移除桥订阅，保留另一个连接和 listener；普通 pending callback 不属于桥所有。 | App 仍可处理已收到的请求；新的请求只发当前剩余订阅端。 |
| 所有订阅端断开 | 普通 pending callback 不在 connection_closed 中清掉；运行中的线程不会仅因无订阅者立即 idle 卸载。 | 可继续等待；同实例重新订阅重放。不能承诺“断开就解卡”或“总会自动超时”。 |
| pending 时新连接 resume | `pending_requests_for_thread` 返回普通 pending，同 ID 重放到新连接。 | 去重以服务器实例、thread ID、server request ID 为键；重放不是第二次独立审批。 |

依据：[warm resume 订阅](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/thread_lifecycle.rs#L718-L741)、[response 后 replay](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/thread_lifecycle.rs#L802-L838)、[queue/add](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/thread_queue_processor.rs#L76-L101)、[queue RPC 分发](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/message_processor.rs#L1368-L1372)、[移除连接订阅](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/thread_state.rs#L622-L649)、[无订阅者且 idle 才卸载](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/thread_lifecycle.rs#L378-L398)。

## 等待、错误、解析与通知

- 普通命令审批：事件转成 `item/commandExecution/requestApproval` 后生成 oneshot 并另起任务 `receiver.await`；客户端 error/解析失败/回调掉线落入拒绝，部分命令完成状态为 `Failed`。[发送](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/bespoke_event_handling.rs#L786-L824)、[响应处理](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/bespoke_event_handling.rs#L1970-L2054)
- 文件审批：同样等待一次 callback，客户端 error/解析失败转成拒绝；没有 retry App 的分支。[响应处理](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/bespoke_event_handling.rs#L1928-L1965)
- 原生问答：`item/tool/requestUserInput` 的 error、callback 掉线或解码失败被转换为空 `answers` 提交给 Core。Core `request_user_input` 本身等待 answer receiver，不存在按 `autoResolutionMs` 自动结束的服务端计时器。[响应处理](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/bespoke_event_handling.rs#L1639-L1719)、[Core 等待](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/core/src/session/mod.rs#L3088-L3124)
- `isBlocking=false` 不能被桥解读为“服务器会自行完成”。同步工具 handler 设置 Plan 为 true、其他可用模式为 false，但仍 await answer。该版本 TUI 自己根据 false 启用固定宽限与倒计时并提交空答案；这是客户端行为，不证明桌面 App UI 相同，也不是桥应主动照搬的协议保证。[工具等待](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/core/src/tools/handlers/request_user_input.rs#L79-L96)、[TUI 计时](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/tui/src/bottom_pane/request_user_input/mod.rs#L269-L289)、[TUI 空答案](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/tui/src/bottom_pane/request_user_input/mod.rs#L916-L925)
- 独立的 `request_user_input_async` 工具是另一种协议：输出 `phase=final_answer`、`delivery=async`、带 questions 的 AgentMessage，立即返回 `accepted:true`，不创建上述问答 server request。它可以通过普通输入继续交流，不能与 `item/tool/requestUserInput` 的待回复 ID 混为一类。[异步问答实现](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/core/src/tools/handlers/request_user_input_async.rs#L129-L149)
- 回调结束由 listener 串行发 `serverRequest/resolved {threadId, requestId}` 到当时的订阅集合，供所有端移除同一提示。桥应依此消除等待态；应保留请求至明确 resolved、轮次取消/完成或实例失效，不因 App 的输入/输出 notification 自行判定完成。[resolved 分发](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/thread_lifecycle.rs#L886-L911)
- JSON-RPC server `id` 可为 string 或 integer，客户端请求与服务端请求是两个方向；不要把 `params.itemId`、`turnId` 或 shell 的 `approvalId` 当作回复 ID。协议生成文件 `ServerRequest.ts`、`RequestId.ts`、`v2/ServerRequestResolvedNotification.ts` 是准确载荷依据。Feishu 普通聊天进入 queue/add，不能代替该 server request 的 JSON-RPC result。

**例外不可套用普通广播规则**：MCP elicitation 的 `UserVerification` 是具有资格的单个 owner，只有该连接能答，其他订阅者不能重放，owner 断开取消它。`attestation/generate`、认证刷新、动态工具执行等也不应因普通审批调查而统一“保持静默”；需按对应契约单独判断是否属于 M0 的接口集合。[UserVerification 路由与 owner](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/outgoing_message.rs#L338-L402)、[重放排除 owner 请求](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/outgoing_message.rs#L582-L596)

## 可行处理方案

**M0 推荐：明确由原 App 处理普通审批/问答。** 桥识别上述 server request，将对应 turn/card 标为“等待在 Codex App 处理审批/回答”，继续读取 socket，不给 result/error、不占用自己的客户端 RPC pending 表。监听 resolved 清理提示。此方案保持原线程 policy，前提是证明桥与 App 是同一 runtime，并实际证明 App 在该场景接收、显示和回答；单凭 thread/resume 成功不足以通过此门槛。提示只需公共状态、thread/turn 定位，不把命令正文、文件 diff、reasoning 或秘密问答字段透传到卡片。

桥断开后不做自动拒绝或自动答题；重新连接到同实例后 resume、按同 ID 恢复 pending。若 App 未订阅/关闭，提示等待端不可用，避免宣布任务完成；可由用户回到 App resume。主动 interrupt 是改变运行任务的操作，应由明确用户操作触发，不因桥超时自动执行。

若产品将来要求飞书直接回答，必须实现带原始 server request ID 的**显式人类操作**：精确保留可用 decisions、问题 ID 与 `answers` 形状，校验操作人及 thread/turn 绑定；订阅 resolved 废弃按钮，并接受 App 可能先回答的事实。原 App 同时可答，服务端并无普通审批独占锁；不能用重放建立第二份独立授权，也不能先发占位 result。当前 M0 尚未实现此链路时，不宣称飞书回复已完成原生问答。该方案不包含 autoapprove 或修改原线程策略。

## 官方测试证据与未覆盖点

这里只阅读测试，未运行官方 suite，更未据其声明 App UI 已验证。

| 官方测试 | 已提供的证据 | 限制 |
| --- | --- | --- |
| [`thread_resume_replays_pending_command_execution_request_approval`](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/tests/suite/v2/thread_resume.rs#L5467-L5602) | 同一 TestAppServer pending shell approval，再 resume 收到完全同一请求，回答后完成。 | 测试是同一 client、默认 resume；没有同时覆盖原 App + 桥或 excludeTurns。 |
| [`thread_resume_replays_pending_file_change_request_approval`](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/tests/suite/v2/thread_resume.rs#L5604-L5788) | 文件审批也有 pending replay、回复后继续的回归。 | 不是桌面 App 多客户端交互测试。 |
| [`request_user_input_round_trip` / `request_user_input_default_mode_forwards_non_blocking`](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/tests/suite/v2/request_user_input.rs#L49-L179) | true/false 模式，原生 question/answer 往返；resolved 在 turn/completed 之前。 | false 的测试仍显式回答，不证明服务端自动解卡。 |
| [`notify_client_error_forwards_error_to_waiter`](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/outgoing_message.rs#L1474-L1502) | JSON-RPC error 直接完成 callback 为 Err。 | 不单独模拟 bridge/App 抢答，抢答结论依据 callback remove 实现。 |
| [`removing_auto_attached_connection_preserves_listener_for_other_connections`](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/thread_processor_tests.rs#L1225-L1275) | 移除连接 A 后保留 B 的订阅与 listener。 | 不触发真实审批。 |
| [`websocket_disconnect_keeps_last_subscribed_thread_loaded_until_idle_timeout`](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/tests/suite/v2/connection_handling_websocket.rs#L453-L495) | 最后订阅端断开，线程并不立即移除，idle 延迟后卸载。 | 是空闲线程；不证明 pending 审批何时结束。 |
| [`turn_interrupt_resolves_pending_command_approval_request`](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/tests/suite/v2/turn_interrupt.rs#L221-L328) | 显式 interrupt 取消 pending 且发 resolved。 | 不是断开连接的行为。 |
| [`verification_is_delivered_to_one_owner_and_other_connections_cannot_resolve_it` / `verification_cancels_when_its_owner_disconnects`](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/user_verification_ownership_tests.rs#L29-L122) | 单 owner、其他端 result/error 无效、不向其他端 replay、owner disconnect 取消。 | 特殊 UserVerification，不能推导普通审批也有 owner。 |

必要 fixture 验收，使用独立原生运行环境与 mock provider 固定生成审批/问答，不读用户历史；本调研未创建这些 fixture：

1. **同 runtime 两连接**：A 创建并订阅 root；B metadata resume，`excludeTurns:true`。分别触发 A 原生输入与 B queue/add 输入的 shell、file、permissions、Plan 问答；验证 A/B 同 request ID，B 静默、A 人工回答后均收到 resolved、turn 继续，queue 后续输入继续派发。
2. **observer 只有 metadata**：B 在 pending 前与 pending 后分别 resume，证明 excludeTurns 不屏蔽请求、pending replay 同 ID；B 持续读 socket，不自动回 error。第三个只 initialize/未订阅连接应不收到该线程普通请求。
3. **断开桥**：审批已广播后断开 B，A 仍能回答；再触发请求 A 仍收到。仅 B 订阅时断开，重连同 runtime 并 resume 验证 pending replay，不能用重启服务代替重连。
4. **错误回复负例，只在 fixture**：B 先返回 method-not-found，验证审批失败/拒绝或问答空答案且 A 后答无效；证明生产默认 dispatcher 没有该行为。不得在原用户线程上做此负例。
5. **真实 App 必测**：桥 socket 对应的确是原 App 运行实例；原 App 留在该线程、切换页面、隐藏/关闭线程、退出/重开时分别能否接收并显示相同审批/问答。验证 App 是否仍订阅和主动恢复 pending；默认模式问答计时 UI 是 App 自身行为，不能用 TUI源码替代这次验收。
6. **实例隔离与重放边界**：服务重启后桥废弃旧 server request ID，仅恢复新的 pending；字符串/整数 ID、重复 resume、两个端接近同时回答、turn interrupt/resolved、UserVerification owner 例外应有独立固定 fixture。

## 审批 fixture 的 Responses SSE 准确形状

主代理报告独立 fixture 当前收到一次模型调用、queue 输入已 materialized，但 12 秒只有 userMessage、没有审批。本研究没有读取该 fixture 请求或执行它；以下是同版本官方样例核对，不据此宣布其根因已找到。

官方 app-server helper `create_command_execution_sse_response` 与 `create_escalated_command_execution_sse_response` 均只用 **三个 SSE frame**：`response.created` → `response.output_item.done`（完整 function_call）→ `response.completed`。不需要 added、arguments delta、item.id 或 completed.output。队列审批 fixture 使用 escalated helper、独立测试配置 `on-request` 与 `approvals_reviewer=user`。[官方 helper](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/tests/common/responses.rs#L5-L45)、[官方队列审批构造](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/tests/suite/v2/thread_queue.rs#L954-L1009)

可对照该 helper 使用以下 first response（样例替换了 call_id；仅作为独立 fixture 数据，不执行命令、不修改用户原线程策略）：

```text
event: response.created
data: {"type":"response.created","response":{"id":"resp-1"}}

event: response.output_item.done
data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"approval-call","name":"exec_command","arguments":"{\"cmd\":\"python3 -c 'import time; time.sleep(10)'\",\"workdir\":null,\"yield_time_ms\":10000,\"sandbox_permissions\":\"require_escalated\",\"justification\":\"Test approval request.\"}"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp-1","usage":{"input_tokens":0,"input_tokens_details":null,"output_tokens":0,"output_tokens_details":null,"total_tokens":0}}}

```

SSE 格式来自官方 `sse` helper：每帧 `event: <type>`、`data: <完整 JSON>`、双换行终止；JSON 内也要有正确的 `type`。`ev_function_call` 生成的是 **output_item.done**，不是仅一个 arguments.delta；`arguments` 是序列化 JSON 字符串。[帧包装与 created/completed](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/core/tests/common/responses.rs#L753-L796)、[function_call helper](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/core/tests/common/responses.rs#L969-L997)

准确名称与 schema：

- SSE item 使用 `name="exec_command"`；`namespace` 可以省略，router 将缺省/空 namespace 补成 `functions`。显式表示应是 `namespace="functions", name="exec_command"`，不是把 SSE name 改成 `functions.exec_command`。[router](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/core/src/tools/router.rs#L246-L264)、[默认 namespace](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/protocol/src/tool_name.rs#L6-L43)
- exec_command schema 的 required 只有 `cmd`（字符串 shell 命令）；`workdir` 为可选字符串，省略则用 turn cwd，官方 helper 会将没有 workdir 表示为 null；`yield_time_ms` 为 number。不使用 shell 工具的 `command` argv 数组代替 `cmd`。escalated helper 增加 `sandbox_permissions="require_escalated"` 和 `justification`，只有独立 fixture 参照该审批构造。[工具 schema](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/core/src/tools/handlers/shell_spec.rs#L24-L115)、[runtime 参数](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/core/src/tools/handlers/unified_exec.rs#L28-L62)
- item 的 `id` 是可选；`ResponseItemId` 对反序列化保持兼容，所以 `id="approval-tool"` 本身不能被认定为不合法的根因。[模型结构](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/protocol/src/models.rs#L1076-L1094)、[ID 反序列化](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/protocol/src/response_item_id.rs#L9-L30)

这个版本 SSE parser **忽略** `response.function_call_arguments.delta` 和 `.done`；不会帮 fixture 累加 arguments。实际执行依据是 `output_item.done.item.arguments` 的完整字符串。`response.completed.response.output` 也不会补发遗漏的 tool item，completed 只解析 id/usage/end_turn。done item 解析失败仅 debug 日志，不产生 OutputItemDone。[done 解析](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/codex-api/src/sse/responses.rs#L353-L360)、[忽略参数 delta/done](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/codex-api/src/sse/responses.rs#L479-L491)、[completed 结构](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/codex-api/src/sse/responses.rs#L108-L115)

因此，主代理描述的 added → full delta → **done 内完整 arguments** → completed 顺序仍可合法；省略 namespace 也是官方使用方式。不要把“没有审批”直接归因为 delta、namespace 或 ID。最小验证路径是将 **独立 mock fixture** 缩成上述三帧，确认 HTTP 体完整输出/每帧双换行终止、data.type、done.item.arguments 为可解析字符串；只检查该 fixture 自己收到的模型请求是否确实提供 `functions` namespace 下 `exec_command` 的 tool schema（配置不同可能只暴露 code-mode 等接口），并收集该 fixture 的公开 turn/error 状态。模型调用计数为 1 不能证明 done item 已成功 dispatch；`thread/read` 的 userMessage-only 也不代替工具协议验证。以上均是待主代理验证的定位步骤，本研究没有执行。

M0 审批路由验收可接受“App 处理、桥只显示等待”，但必须通过以上正向原生路由和断开场景。只测普通文本往返或进度卡完成不覆盖这项协议风险。


## 隔离 fixture：PermissionRequest hook 与 Default 模式问答

本节仍固定上述 commit，未读取真实全局 hook 配置、未操作线程、未执行官方测试。主代理报告的独立 fixture 先完成 sessionStart/userPromptSubmit，随后 `hook/started` 的 `eventName=permissionRequest`、没有对应 completed，且没有普通审批请求；以下源码能解释该现象，但不证明具体 hook 的来源或最终决策。

**PermissionRequest 能先消费审批。** `Session::request_approval` 明确顺序是 Hooks → Guardian/User：先 await `run_permission_request_hooks`；返回 Allow/Deny 即产生来源为 Hook 的审批结果，只有 None 才进入普通 reviewer。hook runtime 先发 started，再 await hook，返回后才发 completed。因此 pending permissionRequest 与“普通 App 审批尚未被发送”一致，不能用它证明桥/App 路由失败，也不是 SSE 已完成就一定马上收到审批。[审批优先级](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/core/src/tools/approvals.rs#L505-L526)、[started/await/completed](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/core/src/hook_runtime.rs#L255-L289)

**不能用全量 feature 关闭来保留 managed hooks。** 确有 `thread/start.config={"features.hooks":false}`，feature key 是 `hooks`，不是 `codex_hooks`；request config 转成 session CLI overrides，而非写配置文件。但这个版本 engine 在 feature=false 时直接返回空 handlers，或只保留 builtin 并清除 required-load errors，连 managed requirement hooks 也不再强制加载。官方 `required_managed_hooks_allow_disabled_hooks_feature` 测试明确断言这种行为。因此本 fixture 不使用这个开关，不借其绕过 inherited/managed hooks；审批验收继续 pending。[feature](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/features/src/lib.rs#L1296-L1302)、[request override](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/config_manager.rs#L429-L465)、[engine](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/hooks/src/engine/mod.rs#L232-L263)、[官方边界测试](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/hooks/src/engine/mod_tests.rs#L252-L283)

精确的 per-hook `hooks.state[<key>].enabled=false` 则只禁用非 managed、非 builtin；源码使用 `is_builtin || is_managed || state.enabled != false`，官方测试同时给 managed/user hook 禁用 state，managed 仍执行、user 被滤除。state 可以来自 SessionFlags，key 由 source + `permission_request` + group/handler index 构成。但当前仅 event/status 日志不提供该 state key，也不证明来源；本研究不读取全局配置来构造 key，不实际关闭 inherited hook。若以后确需验证这种隔离，必须有已授权且明确非 managed 的 fixture 自有 hook 和匹配 key，不能把 runtime run.id 当成 state key。[state 来源/合并](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/hooks/src/config_rules.rs#L8-L65)、[不可关闭 managed](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/hooks/src/engine/discovery.rs#L815-L821)、[官方测试](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/hooks/src/engine/mod_tests.rs#L829-L914)、[state key](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/hooks/src/lib.rs#L94-L123)

**安全 hook 诊断字段。** 两个通知均为 `{threadId, turnId, run}`。`run` 没有 `name` 字段；使用 `eventName` 展示事件名，`status` 是 running/completed/failed/blocked/stopped，另有 handlerType、executionMode、startedAt/completedAt/durationMs。建议白名单仅记录这些枚举/时间及 threadId/turnId，必要时记录 `source` 枚举；不要转发 sourcePath、statusMessage、entries.text 或 hook 命令。`run.id` 在本版本由 event/displayOrder/sourcePath 拼成，也会暴露来源路径；需要关联 started/completed 时只在内部保存或记录其 hash。[run ID 生成](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/hooks/src/engine/mod.rs#L161-L168)started 的同步路径只发 non-builtin sync hook；对应 completed 在 hook 返回后发。单次未看到 completed 只能证明观察窗口内未收到终态，需考虑订阅/断连，而非认定永久挂起。[通知 schema](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server-protocol/src/protocol/v2/hook.rs#L18-L80)、[run 与 notification](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server-protocol/src/protocol/v2/hook.rs#L101-L157)、[通知过滤](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/core/src/hook_runtime.rs#L875-L929)

**不改 hooks 的原生同步问答验证路径：仅新建自有 disposable 主线程，thread/start.config 加下列一项，然后照常 queue/add。**

```json
{"features.default_mode_request_user_input": true}
```

该 feature 默认 false，available_modes 原本只有 Plan，启用后才包含 Default；handler 即便已被 advertised，也会在 Default 模式不允许时返回给模型 `request_user_input is unavailable in Default mode`，不会发原生问答。这能够解释“提供了 tool schema，但 turn 最终 completed 而没有请求”的一个具体原因，仍需 fixture 自己的工具返回/第二次模型请求证明。[feature 默认值](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/features/src/lib.rs#L1738-L1744)、[允许模式](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/tools/src/tool_config.rs#L17-L26)、[handler 检查与 await](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/core/src/tools/handlers/request_user_input.rs#L69-L105)

准确的另一层工具总开关是 `tools.experimental_request_user_input.enabled=true`（对象 enabled，不是 `tools.experimental_request_user_input=true`）；默认已经 enabled，若 fixture 已 advertised 就无需额外覆盖。若企业约束拒绝 feature override，应报告约束，不能改 managed 设置。[默认启用](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/core/src/config/mod.rs#L2722-L2728)、[工具注册](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/core/src/tools/spec_plan.rs#L1244-L1251)、[feature requirement 校验](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/core/src/config/managed_features.rs#L315-L355)

官方 App server round-trip 测试就在 thread/start.config 打开上述 Default feature，并收到 ToolRequestUserInput，断言 `isBlocking=false, autoResolutionMs=None`；随后向同一 request ID 回 answers。这里 false 是协议/客户端展示语义，handler 仍 await response，不能把它替换成 `request_user_input_async`。SSE 用 `request_user_input` function_call，参数 `questions` 各项必须有 id/header/question 和非空 options（官方样例两项 label/description）；缺 options 也只返回给模型，无法验证原生路由。[官方 fixture 与 Default 参数](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/tests/suite/v2/request_user_input.rs#L25-L145)、[参数检查](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/core/src/tools/handlers/request_user_input_spec.rs#L105-L122)

不通过 thread/start.config 设置 Plan：ThreadStartParams 无 collaborationMode，新的 session 构造为 Default；`config.collaboration_mode` 不是可证实的入口。可在**仅自有 fixture**用 thread/settings/update 的 collaborationMode 或 turn/start 的同名字段选 Plan，再 queue/add；前者是实验字段需按客户端初始化的 experimentalApi 能力规则处理。当前已有安全 Default feature 路径，不需要改变原线程模式。[新 session 默认](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/core/src/session/mod.rs#L808-L849)、[settings 字段](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server-protocol/src/protocol/v2/thread.rs#L278-L285)
