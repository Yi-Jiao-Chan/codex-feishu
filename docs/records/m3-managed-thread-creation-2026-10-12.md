# 管理实例的受控线程创建（2026-10-12）

本切片将已有私有资源和持久原生创建步骤接入正式 `serve` 的开发组合、同用户控制通道和 CLI。只提供隔离线程出生组件：不启动 turn、不提交输入、不绑定测试群、不 archive。完整队列 / 引导、App UI、部署与 P1 继续 pending；一期未发布，运行中的 M0 未替换。

## 运行实例与授权

`serve` 使用物理数据库旁的专用 `verification-native` 根目录（0700），已存在但不私有 / 非物理的目录会拒绝启动，不能 chmod 修复或替换。启动仅准备这个本地根目录；模型监听、独立 cwd 和线程均等到明确 Begin。数据库路径的目录别名先解析为物理路径，不因合法 `/home` 别名建立非物理资源根。

Service 的 `VerificationCreator` 只返回正在运行且已连接的当前 initialized native 对象，不能调用 Connect；旧适配器缺少创建能力、reader 已关闭或 runtime 不存在均返回空。持久创建步骤继续比较 generation 和每连接随机身份，断开 / 替换后不会向新连接重发原尝试。线程标签仍由发现器排除自动建群。

管理通道增加 `verify.thread.begin/state`，唯一输入是 32 位小写 hex request ID。同 UID、socket 私有权限、完整配置 identity 和严格 JSON 原机制保留；pair/card/group/thread payload 互斥，不能携带目录、模型、prompt、配置、群或任意 RPC。State 只读原日志和本实例资源状态，不创建记录、恢复模型或新开原生连接。

`VerifyThreadCreation` 校验结果状态、出生 pass/fail/pending、实际 thread ID / generation、构建与二进制摘要、期限、资源互补状态和非负计数。旧卡 / 群 / 配对 / 健康解码器也拒绝混合 thread 结果。完整管理包与 CLI 应使用同一兼容构建；不能仅凭新增一个 capability 推导完整验收已实现。

## CLI 行为

`--allow-test-thread --request-id ID` 只授权固定本机模型的只读隔离创建。仅 thread 标志不发卡或建群；需要其他组件时显式添加对应 allow。所有请求组件的支持和依赖先核验，缺少 thread 的旧实例不会因组合标志部分发送卡片或建群。原 ID 无 allow 查询只读取实例支持的已有卡 / 群 / 线程日志；缺失组件 skipped，不当作拒绝或启动替代探针。

线程结果公开在 `data.verification_thread_creation`，不包含模型 URL / cwd / 请求或随机连接 ID。出生确认、原连接仍当前、本地资源在窗口内三个事实分别检查；`verification-managed-thread-creation-observed` 只表示这些组件，仍 `ok=false`、退出 5、全部 12 项 P1 pending。重启 / 到期后保留已确认出生历史，资源和当前连接不满足时继续 pending；旧构建、明确拒绝、unknown 单独报告，原请求不重发。诊断降级时只读证据仍可查询，保留诊断非零退出码，不执行授权变更。

版本字段新增 `verify_managed_thread_creation=true`，但 `verify_managed_thread_ui_verified=false`、`verify_managed_tests=false`、`phase1_released=false`。它们分别描述开发入口、真实 UI、完整验收和发布，不相互替代。配置 Agent 不为组件测试安装或替换未发布服务。

## 验证与未完成项

默认隔离测试覆盖真实本地 Unix 管理 socket、严格方法 / 字段 / identity、旧结果混合拒绝、畸形 thread 状态拒绝、缺失记录只读；CLI 单独标志、组合能力缺失无部分操作、原 ID 只读查询、历史资源 / 连接 / 到期 / 构建和拒绝 / unknown、诊断降级不启动，以及 12 项 P1 不提升。

Service 集成测试通过真实本地管理 socket、现有 receiver 和内存 native creator，验证一次连接 / 接收器、并发策略下重复授权不再创建、出生标签不自动建群、无输入 / 飞书消息 / 新 binding、原 generation 与 nonce 已落盘。它不冒充真实共享 daemon 或 App 页面验收。本轮只读查询确认实际 CLI / 共享 daemon 仍为 0.162.0、原 daemon running；未启用原生 opt-in 测试或运行真实 serve。

下一步接入有持久证据的群绑定与普通队列 / 显式引导验证，再完成远端清理、真正的 App / 飞书 UI 和正式部署矩阵；不能仅凭本切片将 Agent 配置报告为成功。

本切片通过 `go test -race ./...`、`go vet ./...` 和 `git diff --check`。整仓首次执行发现健康摘要测试误将 capability 字段名中的 thread 当作原路由 ID，已改为核验实际带引号的路由值并重验。独立只读审阅未发现新增的具体授权、连接生命周期、typed readback 或重发问题。
