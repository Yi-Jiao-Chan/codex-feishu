# 验收绑定的运行时授权与逐次原生写入检查（2026-10-12）

本切片在 schema 21 绑定基础上提供专属授权、资源操作上下文、Runtime 激活与控制器的逐阶段检查。受控绑定入口仍未开放；完整验收与一期发布仍 pending。普通 App 会话的分类、首装基线、原生排队 / 显式引导语义保持原流程。

## 专属操作授权

`BindingManager.BeginOperation` 先按 host / thread 识别私有标记，再核验实际 app / owner / bot、commit 与二进制摘要、出生与群证据、原绑定及 confirmed intent、固定原请求和资源。存在标记但作用域 / 构建 / 期限不符、证据缺失或读取失败时，返回 handled 与错误，不能退回普通目录授权。普通无标记线程不调用 fixture API，也不领取 fixture 资源。

每次有效操作重新 GET 原群的 bot 与完整 owner-only 名单，并在原 generation / 随机连接身份上执行受限制的无配置覆盖附着。返回后再次检查资源、连接、原绑定和意图。资源模型停止、原连接 Done、父上下文取消、冻结绑定窗口到期都会取消操作上下文；期限锚定首次时间采样，不因读取证据耗时或重放延长。调用者必须用该上下文领取操作、执行 RPC 和保存结果，并在完成后 release。关闭资源等待这些在途操作，失败路径也释放 watcher。

`Authorize` 自动发现模式一律拒绝验收标记；显式已有绑定模式还核验传入元数据仍为原 cwd、验证来源、非 ephemeral、无父代理 / fork。普通来源变化不能绕过已登记标记。当前策略证明来自准确版本专用附着适配器，不修改线程策略，也不据此宣称 App UI 已验收。

## Runtime 与控制器

Runtime 在普通分类前调用专属回调，handled 失败或查询错误都拒绝，不回退。`Activate` 只向有界通道入队，由 Run 主循环读取已确认绑定与实际元数据、授权后再启动 actor；避免外部 goroutine 与 shutdown 的 WaitGroup 竞争。未知 Wake 仍只走发现。动态激活 actor 不会补建群；验收授权 actor 即使绑定随后消失也不进入自动建群分支。

原生验收草稿可能在首条输入前返回 typed BeforeFirstInput。只有本次已通过专属授权的 actor 可以将该结果视为空历史进入 hydration；不写虚构 userMessage，不放宽普通线程或其他 RPC 错误。

Controller 的可选 `BeginWrite` 在每个 ClaimSubmissionAttempt 前领取独立操作上下文，覆盖 add / restore、delete、steer、interrupt 和 queue/start。拒绝时不新增 attempt，原收据 / 动作留待后续核验；删除已成功后下一阶段被拒绝，不会盲目 steer 或 restore。领取后取消时，RPC 前再次检查上下文，未决 attempt 保持保守恢复语义，不将取消伪装为远端明确拒绝。release 在本阶段保存结果之后执行，包含不领取、错误和取消路径。nil hook 保留普通流程。

Service 已组合上述 Runtime 回调和 Controller hook；即便未配置专用管理器，只要数据库识别到标记也拒绝普通授权。管理器的存储指针、host / app / owner / bot 还须与本 Service 相符，另一份数据库的同名 fixture 不能提供授权。Receiver 在 bound 消息 / 命令和 stop、continue、cancel_pending、steer、remove 按钮进入本地变更前取得操作上下文，专属群仅 owner 可操作，验证卡 / 群探针保留原入口。HistoryPreparing / HistoryStaged 及独立任务领取也核验专属权限，失效时清除 actionsReady。canDeliver 重新核验原群身份与权限，但其 error 型接口没有将操作上下文传入后续投递，此限制尚未解决。

## 验证与未接入项

隔离测试覆盖有效操作重放无期限更新、无标记零 fixture 调用、跨 app / owner / 构建、过期、群成员变化、旧连接、资源停止、缺失 / 改变路由和意图、附着期间变化、请求不符、取消与关闭等待；Runtime 覆盖激活和关闭并发、专属分类、空历史 hydration、无自动建群；控制器覆盖各阶段独立 lease、拒绝零 attempt / RPC、删除后授权失效、领取后取消与清理。

Service 的临时数据库 / 本地资源 / mock native 与 Feishu 组合证明实际 ActorTick 会重新领取权限并执行一次 Add；授权失效的首写保持 received、零 attempt、零 Add。入站命令和全部任务 / 队列按钮在无管理器、成员变化、非 owner 或缺 guard 时均未变更。上述 fixture 不调用真实 daemon / 飞书，也不代替真实 UI 验收。

本切片通过整仓 `go test -race ./...`、`go vet ./...` 与 diff 检查。复查发现配置成另一份有效 fixture 数据库的管理器可能提供错误作用域，已增加 Service 存储 / 身份匹配并通过拒绝测试。独立审阅发现证据读取耗时可能推迟相对 timeout，已改为首次采样锚定 deadline，新增单连接阻塞 80ms 的回归测试并通过。最后两项修正重跑受影响 App / verification race 与整仓 vet，原 deadline 问题经只读复核确认消除。本轮未迁移 M0、切换服务或发送真实测试消息。

后续仍须补齐每次飞书消息 / reaction / 命令确认 / 提醒投递的完整操作上下文，配置组合根与显式绑定管理入口、受控 queue / steer 编排和证据记录，再进行真实 App / 飞书 UI、远端清理和部署验收。仅在 Runtime 授权或 canDeliver 检查时取得并释放操作上下文，不能代表它已覆盖后续网络写入；因此本切片不开放 CLI 绑定能力，也不提升任何 P1 用例。
