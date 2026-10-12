# 01 共享 Codex 客户端

负责 `internal/codexclient/`、`compat/codex-*.json`、相应脱敏录制。依赖 00；提供所有模块使用的原生协议入口。

## 接入流程

1. 在 App 连接的同一远程系统用户下调用真实命令 `codex app-server daemon version`，解析 status、socketPath、cliVersion、appServerVersion。
2. 连接现有 socket 上的 Unix WebSocket；不另起 stdio app-server 来“恢复”活跃任务，不重启现有 daemon。
3. 发送 `initialize`，clientInfo 使用桥自己的名称，启用 `capabilities.experimentalApi=true`；成功后发送 `initialized` 通知。
4. 单 reader 持续收 JSON-RPC 响应、通知和服务端请求；request ID 关联等待者。未知事件保留方法名供诊断，不让解码错误断开全部任务。
5. 加入已有线程用 `thread/resume`，参数只含 threadId、excludeTurns=true；不传 cwd、model、approvalPolicy、sandbox 等覆盖值。
6. 断线后重查 socket 和实际服务端版本，重新初始化、加入已绑定会话，然后交给恢复器补齐历史与队列。

远程 SSH 工作方式和 App Server 生命周期见 [OpenAI 远程连接](https://learn.chatgpt.com/docs/remote-connections)、[App Server](https://learn.chatgpt.com/docs/app-server)。本手册的 queue 细节以本地 0.162.0 生成协议和实验记录为证据，不能仅凭公开概览判断兼容。

## 方法清单

| 用途 | 原生方法 / 注意事项 |
| --- | --- |
| 发现 / 元数据 | thread/list、thread/read；本地不传非空 originators 过滤 |
| 观察加入 | thread/resume(excludeTurns=true)；复用当前执行器 |
| 分页补齐 | thread/turns/list、thread/items/list；分页 cursor 是不透明字符串 |
| 原生队列 | thread/queue/list、add、delete、start |
| 显式引导 | turn/steer；expectedTurnId 必填 |
| 停止 | turn/interrupt；保留原生队列 |
| 二期新建 | thread/start；cwd、可选 projectId、自建 threadSource |
| 二期项目选择 | project/list，必要时 project/read；实际字段从固定 schema 读取 |

queue/add 参数是 threadId、input、clientUserMessageId；返回 queuedSubmission，其中原生项标识字段叫 **id**。queue/list 分页返回 data 和 nextCursor。删除参数字段叫 queuedSubmissionId，返回 deleted 布尔值。历史 userMessage 的相关字段叫 **clientId**，不要误写成 clientUserMessageId。

空闲 queue/add 会自动执行，因此收到成功响应后立刻刷新队列/历史；若项已被消费，不能卡片永久显示“已排队”。queue/start 忙时会拒绝，不是 steer 的替代方法。

## 事件适配

最低覆盖 thread/started、线程状态变化、turn 开始/完成、item 开始/完成、公开 agentMessage 增量、thread/queue/changed。具体 method 名与 payload 从已验收的生成协议绑定，避免按口头命名猜事件。

服务端审批/用户输入请求独立于普通通知：M0 记录请求类型，转任务为“待用户处理”，提示回 App。未经真实双客户端验证，不在飞书生成允许/拒绝或问答提交按钮。

普通 server request 必须透传且保持静默，不能自动回 method-not-found 或空 result；同实例内首个 result/error 会消费共享 callback，抢在 App 前拒绝审批或清空答案。用 generation、thread ID、原始请求 ID 去重；`serverRequest/resolved` 按对应请求解除等待，轮次终态清理该轮等待。M0 以同一过程卡显示“等待你在 Codex App 中处理”，不另发一张消息。特殊 UserVerification 不套用普通审批的 owner 规则。[准确版本审批路由研究](../research/approval-routing-0.162.0.md)

隔离原生问答已证明同一请求/replay、桥静默与断开、另一端回答后继续及 resolved；这仍不能代替真实 App 问答 UI。命令审批探针在本机 `permissionRequest` hook 等待阶段超时，尚未到达原生审批，保留 pending。生产沿用原线程的 hooks 和策略；不得为消除测试阻塞关闭 managed hooks。Default 模式同步问答有 feature gate，工具出现在 schema 中不代表该模式可调用；仅自有 fixture 配置开启该 gate，桥不改真实线程配置。

事件携带 connection_generation。重连产生新 generation，旧连接的晚响应不能确认新尝试。事件缓存有界；溢出时标记需要补齐并暂停依赖实时轮次的操作，不能静默丢失后继续宣称状态准确。

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

复用[本地验证脚本与结果](../research/codex-feishu-probes-2026-10-10/)作为协议测试依据。重新运行脚本会创建测试线程，按 11 的环境边界执行；无需每改一张卡就重跑全部原生实验。

隔离验证创建入口已有 `Client.StartLocalVerification` 的开发实现：只接受本人私有物理目录与本机 loopback 模拟模型，固定 readonly / never 和测试标签，并核验返回策略 / 元数据。它只复用既有连接、一次创建、不启动 turn；上层须发送前持久领取。默认 WebSocket fixture 已通过，新入口的真实隔离重验、绑定 / 清理与完整 UI 仍 pending；完整 verify 尚未开放。见[隔离创建适配器进展](../research/m3-native-verification-adapter-2026-10-12.md)。

内部 `NativeManager` 已补齐准确请求落盘和单次创建领取。`Client.ConnectionID` 提供每连接随机身份，与进程内 generation 共同核验；新连接不能复用旧连接创建尝试。创建证据与附着 / 完整 UI 验收分开，见[创建日志进展](../research/m3-native-verification-journal-2026-10-12.md)。

`NativeSessions` 已组合私有目录、固定 loopback 模型和上述创建步骤；资源到期、关闭和进程重启不更换原请求端点或重建线程。模型 Release 只是本地时序控制，不是原生 steer / start。开发管理通道 / CLI 已接入创建组件，绑定、输入、远端清理与真实重验仍待完成，见[本地资源进展](../research/m3-native-verification-resources-2026-10-12.md)和[管理创建组件](../research/m3-managed-thread-creation-2026-10-12.md)。
