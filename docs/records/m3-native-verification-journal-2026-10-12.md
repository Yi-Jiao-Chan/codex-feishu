# 隔离原生验证创建日志（2026-10-12）

本切片补齐内部创建步骤的持久领取和连接身份，不开放完整 verify，不部署或替换 M0。管理工作流中的本地模拟模型服务、私有目录生命周期、绑定、排队/引导、App UI 与清理继续待实现和验收。

## 实现边界

schema 20 新增 `verification_native`。请求 ID、host/app/owner、构建 commit、运行二进制摘要、私有目录、本地模型 URL、受限制的准确 wire 请求和 5 分钟窗口在调用前冻结。并发调用通过 ready→dispatching CAS 单次领取；构建或载荷不同不能继续原 ready。创建不会启动 turn，测试标签仍由发现器排除自动建群。

调用只使用上层提供的已初始化连接，不调用 Connect、不启动 daemon、不修改全局配置。领取记录数值 generation 和每次 Connect 的 128 位随机连接 ID；数值 generation 在进程重启后可能重复，必须同时核验随机身份。领取后发现连接已经替换或断开，保守保存 unknown，不换连接重新创建。

确定的成功响应保留原创建线程 ID；明确 RPC 拒绝保存 rejected；掉响应、取消、证明不符和遗留 dispatching 均保持 unknown，不盲重试。响应落盘使用脱离调用取消的 5 秒上限。重连后，已确认出生仍是原尝试的历史证据，但 `creator_connected=false`，不能据此假定新的连接已附着、核验或绑定。

只读 State 不创建记录、不发 RPC。公开结果不含目录、模型 URL、准确请求或连接随机 ID；完整日志结构禁止 JSON 输出。该内部管理器不接收任意模型配置、prompt 或策略。CLI 的 `--allow-test-thread` 继续拒绝，不因本切片启用完整 managed verifier 或改变一期发布标志。

## 本地验证

存储测试覆盖从 schema 19 升级保留卡、群与旧 M0 日志；不可变载荷、作用域碰撞、并发单领取、时间边界、generation/随机连接 ID 与终态匹配。管理器测试覆盖 12 并发重放、发送前已落盘、unknown/rejected/取消/证明不符/崩溃重开不再创建、同一数值 generation 的新连接、旧连接响应保留历史但不表示新连接就绪、构建/载荷/目录/期限冲突和只读查询。真实 Unix WebSocket fixture 核验初始化后的连接身份格式与独立性。

测试仅使用临时数据库、私有临时目录、内存 creator 和本地 Unix WebSocket fixture。真实 native opt-in 测试未启用，运行中的 M0 数据库、服务及飞书群未改动。自动测试与完整一期真实部署验收分别记录，组件创建 pass 不提升 P1。

本切片通过 `go test -race ./internal/codexclient ./internal/verification ./internal/store`、`go test -race ./...`、`go vet ./...` 和 `git diff --check`。独立只读审阅未发现新增的具体并发、重启或 unknown 重发问题；审阅未执行真实操作。
