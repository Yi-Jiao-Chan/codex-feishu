# 受控绑定的管理入口（2026-10-12）

本切片将已实现的绑定证据、运行时授权和逐次投递检查接入开发 serve、同用户管理通道与 CLI。只将同一请求的原测试群和原线程绑定并请求跟进，不创建缺失目标或自动测试输入。正式 queue / steer 验收编排、远端清理、真实 UI 与发布仍待完成。

## 组合与结果

serve 将同一 NativeSessions、GroupManager 和 Service 组合为 BindingManager。Activate 回调重新核验原私有标记、资源 / 连接 / 成员 / 附着，再由 Run 自己消费有界激活队列；不启动另一连接或接收器。队列满、实例关闭、作用域不符或取消返回不可激活，不把本地登记当作已启动。重复授权只重查并请求原 actor；不建第二个群 / 线程、不延长窗口。

BindingResult 新增两个事实：activation_requested 只在本次 Begin 已入队且当前本地登记仍有效时为 true，State 从不设置；actor_started 只报告同一当前 runtime 的本地 actor 存在，并受本地构建 / 期限 / 资源 / creator 连接检查限制。这不证明 actor 刚刚通过远程成员核验、输入、送达或 App UI。失去资源后保留原登记，不恢复监听地址、替换模型或重建线程。

## 管理通道与 CLI

`verify.binding.begin/state` 唯一参数为 32 位小写 hex request ID，沿用同 UID、私有 Unix socket、完整配置 identity、严格 JSON 与请求上限。卡 / 群 / 线程 / 绑定 / 配对 payload 互斥；旧客户端也拒绝混合返回，不能夹带 prompt、目录、目标或任意 RPC。

`--allow-test-binding` 为独立授权，先核验全部请求组件支持与依赖，再开始任何变更。单独绑定只查已 confirmed 的原群和出生，缺失 / unknown / 未有 owner 观察保持 pending；无独立连接或 legacy fallback。创建标志不隐含绑定；组合授权也不跳过群 owner 验证消息。

不带 allow 的原 ID 查询支持四组件，只读已有日志与 actor / 资源本地事实；缺组件 skipped，全缺 not-found，不登记或激活。CLI 将登记、原构建、窗口、资源、连接和两个生命周期事实分别报告。`verification-managed-binding-observed` 仍 `ok=false` / 退出 5，全部 12 项 P1 pending；version 的 verify_managed_binding=true 仅表示开发入口，完整 verify、UI 和发布标记均 false。

Agent 指南与输入样例增加独立 `verification.bind_test_group_and_thread`。计划保留此授权并将其纳入 plan ID，不执行绑定；用户未允许时默认为 false，不能从创建权限推导。已允许的 owner 群跟进与自动化固定测试输入不同，当前 verify 不提交后者。

## 证据

真实本地 Unix 管理 socket、唯一 Service / Receiver 与 mock native / Feishu 组合验证：绑定前没有组件时不创建；缺 owner 群观察不登记；原接收器的 `/verify-group ID` 记录后，显式绑定入队并启动 actor；重放 / State 不创建第二目标或提交输入。连接、接收器、原生出生与群创建各一次，输入收据和 Add 均为零。这仍是隔离证据，不冒充实际 daemon 或 App UI。

客户端畸形 / 混合结果、历史与激活事实边界、独立 flag / 能力预检、legacy 不回退、默认只读、全部 P1 pending、输入授权解析与计划稳定性均有隔离测试。下一步完成受控输入编排和准确场景证据，再进行真正的 App / 飞书验收与部署。

本切片通过整仓 `go test -race ./...`、`go vet ./...` 和 diff 检查。新增独立配置授权后，setup 完整 race 与开发包测试也通过。只读独立审查未发现新增资源 / 连接、重放、Close / WaitGroup 或锁序问题；客户端额外拒绝 pending 却声称激活 / actor 存在，以及 State 声称本次激活的响应，控制通道 race 已重验。
