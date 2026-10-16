# M3：隔离原生线程创建适配器

日期：2026-10-12。当前工作推进到隔离 native 创建入口，尚未实现完整管理线程验收。`verify_managed_tests=false`，`--allow-test-thread` 仍不部分执行卡 / 群。未启动真实新线程、真实模型 / 飞书发送，未修改全局 Codex 配置或现有 M0。

## 协议依据

[官方 OpenAI App Server 文档](https://learn.chatgpt.com/docs/app-server)说明新线程创建、事件订阅与归档；它没有证明此桥的真实 App UI、排队或完整一期验收。当前公开样例和本地版本不能直接混用；适配器使用下面实际生成的 0.162.0 字段。

本轮只读获取实际 CLI / 运行 daemon，均为 0.162.0。执行 `codex app-server generate-json-schema --experimental`，输出至忽略的私有 `dist/native-schema-0.162.0`。这是 schema 导出，不启动 daemon。旧临时缓存与本次实际导出摘要不同，未将旧缓存当作当前协议。选定文件 SHA-256：

- ThreadStartParams.json：`80a40a7fac15b4bf70efb7f893fb353acc0a0d30c68f54aee4f01923deca85de`
- ThreadStartResponse.json：`107fe36c99ebaeb750a371580beec807a28c6451e9c8a02cd7d451a0135f1c91`
- ThreadArchiveParams.json：`dc2c0b133b814a5d9d543cc37afdb3f4f22feca35b7cea4272834b20a1e47705`

本地参数使用 sandbox 的 `read-only` 字符串，响应是带 `type=readOnly` 的策略对象；响应还要求 cwd、model、modelProvider、approvalPolicy。threadSource 是可选 string / null，不具有 App 来源证明力。已有标签 `feishu.bridge.validation` 会被发现器排除；验证不产生普通 App 自动建群候选。

## 已实现

`Client.StartLocalVerification` 复用调用者已初始化的共享连接，只发一次创建请求，不开新连接、执行输入、fork 或改旧线程。入口只有随机 32 位十六进制 run ID、本人 0700 的物理目录及带显式端口的 HTTP loopback `/v1`。拒绝 DNS 名、非 loopback、身份信息、query / fragment、其他路径、symlink / 相对目录和不私有目录。

model 固定 fixture-model，provider 从原 run ID 派生，readonly / never 与测试标签固定。provider 仅在该新线程的 override 中指向本地模拟服务，关闭 OpenAI auth、WebSocket 和请求重试；不接受任意 model、production provider、config map 或 hook 修改。管理通道没有接受这些参数的入口。

响应必须证明实际 cwd、model / provider、readonly 且无 networkAccess、never、非 ephemeral、无 parent / fork、空预览 / 轮次和正确测试标签。不完整 / 被覆盖的结果与丢响应都按 UnknownError，不能当拒绝重建。创建前必须由上层冻结并领取原意图；协议适配器没有把 run ID 当作远端去重键。

原 opt-in native 队列 / 引导实验已经改为复用该入口并在调用前保存私有测试意图。本轮没有打开其 `CODEX_FEISHU_NATIVE_TEST` 开关；新入口的更严格校验仍需真实隔离重验，不能沿用旧 raw RPC 的 M0 结果宣称新版入口已通过。默认 tests 使用本地 Unix WebSocket fixture。

## 验证与剩余工作

`go test -race ./internal/codexclient`、对应 vet 和 diff 检查通过。覆盖冻结请求的固定策略、本地 provider、不额外执行输入、非法 scope 不调用 RPC、错误 / 缺少出生证明、创建响应丢失只发一次。现有 opt-in 创建代码使用同一构建路径，不再另造一份参数。

下一步仍须：管理实例内持久原生创建意图 / connection generation 领取、未知出生的核验边界、测试目录 / 模拟服务生命周期、与测试群的显式测试绑定、原生 queue / steer 与 UI 证据、准确终态和安全清理。只完成创建适配器不能开放受控线程或提升任何 P1；完整发布仍待验收。
