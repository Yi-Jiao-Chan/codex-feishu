# Controller 原生 Q4–Q8 opt-in 验证

2026-10-13 使用已运行且版本严格为 `0.162.0` 的共享 daemon、实际 Go Controller 和独立 SQLite。
证据在 `internal/submissions/native_queue_integration_test.go`；资源辅助在 `native_fixture_test.go`。
默认 `go test` 跳过 daemon 操作；仅 `CODEX_FEISHU_NATIVE_CONTROLLER_TEST=1` 创建专属 fixture。
Go 检查均继承原有 `GOCACHE`/`GOMODCACHE`，没有启动新 app-server 或修改全局配置。

每个 fixture 使用 candidate project 外的私有 scratch cwd、exclusive intent/owned-thread 标记、
typed birth、固定 loopback Responses SSE 模型，无工具或外部转发。busy fixture 先由 creator
提交授权的 held 初始输入使 rollout 落地，再由第二客户端 typed attach，所有策略检查不变。
idle Q7 只用 creator，无初始输入或第二客户端。只读本 fixture 的 queue/history；不读全局事件。
清理验证出生 ID/cwd/validation label 后，仅清空、interrupt、archive 自有线程，关闭模型与连接。
当前候选服务、配置、DB、二进制、用户线程、飞书群和凭据未改动。

| 用例 / 测试符号 | 真实 native 结果及 journal / 历史断言 |
| --- | --- |
| Q4 `TestNativeControllerQ4OtherClientDeletesBeforeBridgeDelete` | 第二客户端真实删除 B，bridge delete 返回 false；add=1、delete=1、steer=0、restore=0；动作 rejected/changed，删除 journal confirmed/deleted=0，queue/history 无 B。 |
| Q5 `TestNativeControllerQ5OldTurnEndsBeforeSteerAndNeverRetargets` | bridge delete 后旧轮实际 completed，第二客户端开始不同新轮；steer 仍只针对旧 turn，native 拒绝一次，restore 确认一次；B 后续独立轮 history 恰好一次，未进入 replacement。 |
| Q6 `TestNativeControllerQ6AcceptedSteerReplyDiscardedAtBoundary` | 真实 steer 成功后丢返回，重开 SQLite；原目标轮中 client ID history 恰好一次，动作恢复 confirmed/steered；steer 总调用=1。 |
| Q7 `TestNativeControllerQ7AcceptedAddReplyDiscardedAtBoundary` busy / idle | 真实 add 成功后丢返回，重开 SQLite；busy 唯一 queue 关联，idle 唯一 history 关联；add 总调用=1、唯一 add attempt confirmed。 |
| Q8 `TestNativeControllerQ8AcceptedDeleteReplyDiscardedAtBoundary` | 真实 delete 成功后丢返回，重开 SQLite、多次 Tick；唯一 delete attempt unknown，receipt unknown；无 history、无 steer/restore/重复 delete。 |
| Q8 `TestNativeControllerQ8ProcessExitAfterDeleteBeforePersistence` | 实际子进程在 native delete 成功并写入 Sync 的自有证明后、Controller confirmation 前 `os.Exit(86)`；父进程重开先确认 attempt dispatching，再恢复 unknown；无 steer/restore/re-delete。 |

Q6/Q7/Q8 丢返回是 **Controller 边界 fault injection**，不是实际网络中断证据。
Q8 子进程退出为真实 OS process exit，与普通 DB reopen 独立。所有上述用例已在 `-race` 下通过；
验证 native primitive、Controller 和 SQLite 恢复行为，不替代 App/飞书 UI，也不声称完整 P1。
Q9/Q10 及跨进程 daemon/network 故障不在本轮范围。

已运行命令（所有 native fixture 均完成自有资源清理）：

```sh
# 默认 opt-out：本地 SSE fixture 通过，其余 skip，不连接 daemon。
go test -race ./internal/submissions \
  -run '^(TestNativeQueueFixture|TestNativeController|TestNativeQueueChildCrash)' -count=1 -v

CODEX_FEISHU_NATIVE_CONTROLLER_TEST=1 go test -race ./internal/submissions \
  -run '^TestNativeControllerQ4OtherClientDeletesBeforeBridgeDelete$' -count=1 -v -timeout=110s
CODEX_FEISHU_NATIVE_CONTROLLER_TEST=1 go test -race ./internal/submissions \
  -run '^TestNativeControllerQ5OldTurnEndsBeforeSteerAndNeverRetargets$' -count=1 -v -timeout=110s
CODEX_FEISHU_NATIVE_CONTROLLER_TEST=1 go test -race ./internal/submissions \
  -run '^TestNativeControllerQ[678]' -count=1 -v -timeout=8m
CODEX_FEISHU_NATIVE_CONTROLLER_TEST=1 go test -race ./internal/submissions \
  -run '^TestNativeControllerQ8ProcessExitAfterDeleteBeforePersistence$' -count=1 -v -timeout=110s
```

Q6/7/8 合并命令首次运行时前三项通过，子进程用例因测试错误地期待 queued 而失败：
原 `ClaimSubmissionAction` 已持久化 steering。修正为严格检查 steering 后，最后一条命令通过。
没有修改 production，未放宽 typed attach 或行为断言。

## 已验证的 cold/no-input resume 限制

初始 setup 在 fresh 无输入线程上尝试跨客户端 typed attach 时，`thread/start` 返回已经通过
fixture model/provider、private cwd、never、readonly/no-network、non-ephemeral、validation label
检查。`thread/resume` 仅发送 `threadId` 和 `excludeTurns=true`，无参数覆盖；真实 daemon
在返回策略字段前拒绝：`code=-32600`，脱敏错误 `no rollout found for thread id [owned-thread]`。
因此没有成功 resume response 可比较 model/provider/policy/cwd/endpoint；这不是策略字段不匹配。
失败 fixture 未补发输入或以 override 修复，已清理。后续新建 busy fixture 按上述构造顺序通过。
这一限制可能影响 managed binding 的 pre-input join；本轮没有改动该 production 路径或扩大调查。
