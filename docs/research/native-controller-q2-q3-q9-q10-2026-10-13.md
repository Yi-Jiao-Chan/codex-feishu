# Controller 原生 Q2/Q3/Q9/Q10 opt-in 验证

2026-10-13，复用已运行的共享 `0.162.0` daemon，以实际 Go Controller、独立 SQLite、
helper 自己新建的私有 validation 线程及固定 loopback SSE 模型完成本轮验证。
测试在 `internal/submissions/native_queue_integration_test.go`，复用既有 owned fixture 与清理。
默认不连接 daemon，显式 `CODEX_FEISHU_NATIVE_CONTROLLER_TEST=1` 才执行；Go caches 继承原值。
未改 production、实际用户线程、飞书配置/API、服务或共享 daemon 生命周期。

| 范围 / 符号 | 本次真实 native 证据 |
| --- | --- |
| Q2 `TestNativeControllerQ2Q3MixedQueueOrderAndMiddleSteer/Q2_normal` | 第二 native client 排 A，Controller 排 B/C；真实 queue 为 A/B/C，bridge B/C 序号为 2/3。释放固定模型后历史为初始轮→A→B→C，每个 client ID 恰好一次，B/C consumed。 |
| Q3 同测试 `/Q3_middle_steer` | 相同混合来源 A/B/C；只删除中间 B，steer 只指向原 held 活跃轮，delete=1、steer=1，无 restore。queue 保留 A/C；完成后 B 在原目标轮恰好一次，A→C 各一次，原生队列不再执行 B；B journal 保留 steered，C consumed。 |
| Q9 `TestNativeControllerQ9ConcurrentAuthorizedCallbacksAndReplay` | `/same_button` 与 `/steer_remove` 各 12 个同时放行的独立 callback Controller claims；SQLite 只产生一个 action ID。实际 native dispatch 为单个动作流程：delete=1，若 steer 胜则 steer=1，若 remove 胜则 steer=0；没有 add/restore 重发。完成动作重放三次返回同一结果，冲突旧版本拒绝，多次 Tick 不再原生写。历史与胜者一致；本轮运行分别观察到 remove 与 steer 竞争胜出。 |
| Q10 `TestNativeControllerQ10ContinueRacesSecondNativeStart` | 真实 StopExpected interrupt=1；再补 C 不自动启动，队列保留 A/C。`/native_wins_busy` 在 bridge 持久化 start attempt 后由第二客户端先真实 start：bridge 原生 start 被拒，steer=0。`/simultaneous_start` 同时放行两客户端原生 start，恰好一个成功，另一个 RPC 拒绝；两种场景最终 A→C 各一次，无丢失、复制或 steer。 |

Q9 **假定操作者在 Controller 边界已经授权**，验证并发本地登记、版本检查及真实 native
动作去重；不冒充多身份飞书鉴权。这里“一个动作”不等于 steer 只需一个 RPC：其 delete、
steer 两阶段各恰好一次。Q10 是两个合法 native API client，不是 App UI 操作证据。
调度 hook 只控制真实 RPC 的先后或放行，不伪造 native response。

Q3 曾在成功 steer 后立即读取历史，B 尚未异步可见；保留原 target 与次数断言，完成固定
模型后历史通过。没有重试 steer。测试还纠正了对 receipt 状态的误解：生产 reconcile
有意保留 steered，不能要求其成为 consumed。无 production 修复。
既有 cold/no-input 跨客户端 resume 限制见 `native-controller-q4-q8-2026-10-13.md`；
本轮所有 busy fixture 均先由 creator 授权物化，再一次 typed attach，失败后不 resume/prime。

已运行检查（全部继承 Go caches）：

```sh
# fixture 通过，新增 native tests skip；无 daemon 操作。
go test -race ./internal/submissions \
  -run '^(TestNativeQueueFixture|TestNativeControllerQ2Q3|TestNativeControllerQ9|TestNativeControllerQ10)' -count=1 -v

# Q2 normal 通过；该次 Q3 的过早历史断言失败，后续保留原目标等待完成。
CODEX_FEISHU_NATIVE_CONTROLLER_TEST=1 go test -race ./internal/submissions \
  -run '^TestNativeControllerQ2Q3MixedQueueOrderAndMiddleSteer$' -count=1 -v -timeout=4m

# 两类 Q9 及两类 Q10 均通过；此正则不匹配 Q2Q3。
CODEX_FEISHU_NATIVE_CONTROLLER_TEST=1 go test -race ./internal/submissions \
  -run '^TestNativeController(Q2Q3.*Q|Q9ConcurrentAuthorizedCallbacksAndReplay|Q10ContinueRacesSecondNativeStart)$' \
  -count=1 -v -timeout=5m

# 最终 Q3 通过。
CODEX_FEISHU_NATIVE_CONTROLLER_TEST=1 go test -race ./internal/submissions \
  -run '^TestNativeControllerQ2Q3MixedQueueOrderAndMiddleSteer/Q3_middle_steer$' -count=1 -v -timeout=110s
```

所有真实测试均在 `-race` 下运行，自有资源清理无错误。已有 Q4–Q8 成功证据复用，未重复运行。
这组有界调度不证明所有可能的竞争交错、跨进程 callback dispatch、真实网络中断或飞书/App UI；
也不替代当前用户的 quiet/until、通知及完整 P1 验收。
