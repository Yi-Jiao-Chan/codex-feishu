# 正式消息 outbox 的限流恢复（2026-10-12）

本切片覆盖正式服务的公开消息、每轮过程卡、最终回复及共用镜像 outbox 的队列/操作卡。schema 10 新增持久限流记录。当前 M0 二进制、schema 1 数据库、配置与进程没有切换；此次仅查询官方文档、使用本机 HTTP fixture 与临时 SQLite，没有真实飞书业务发送、建群、@ 或原生输入。

## 分类依据

完整错误 JSON 中 `99991400` + HTTP 400/429，是官方频控策略列出的失败请求；发送、回复、PATCH 卡片接口的 `230020` + HTTP 400，官方也要求降低频率后重试。依官方失败定义和重试指引，这里将它们归为可延后重试的明确限流拒绝；这是对公开契约的实现判断，不把任意错误码或 HTTP 429 当作可重发依据。[频控策略](https://open.feishu.cn/document/server-docs/api-call-guide/frequency-control.md)、[发送](https://open.feishu.cn/document/server-docs/im-v1/message/create.md)、[回复](https://open.feishu.cn/document/server-docs/im-v1/message/reply.md)、[卡片更新](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message/patch.md)

`99991403` + 429 是月度额度耗尽，保留明确拒绝，需管理员处理。`230049` 表示消息发送中，不适用于重发。未知 429、超时、5xx、读取失败、非法/缺失/null code 都保留 unknown，不盲重发。[通用错误码](https://open.feishu.cn/document/ukTMukTMukTM/ugjM14COyUjL4ITN.md)、[发送错误码](https://open.feishu.cn/document/server-docs/im-v1/message/create.md)

固定 Go SDK v3.12.0 的 core.Request 没有通用限流重试；内部仅对拨号失败及指定 token 无效代码有最多两次尝试，不能靠它恢复消息。继续保留 token cache，网关一次调用返回 typed RateLimited，由 outbox 保存状态。[固定 SDK 实现](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/core/httptransport.go#L260)

## 持久恢复与通知

1. 所有消息写入前保留原不可变 attempt 和冻结的目标、父消息及正文。
2. 明确限流后在同一事务记录该 attempt rejected、outbox ready、下一次等待时间及拒绝次数。
3. `x-ogw-ratelimit-reset` 是接收响应后等待的秒数；有效平台下限不向下截断。没有有效头时采用 1 秒起、最多 60 秒的持久指数退避。等待期间不反复读取群成员，不占用同线程其他独立最终/队列卡的发送资格。
4. 到期后领取新 attempt，合并最新正文并重新核验提醒规则与实际群成员。旧请求已明确拒绝，下一次使用新 UUID；这不授权重新发送未知结果，平台 UUID 的一小时去重窗口也不授权未知请求跨窗口重发。
5. 成功或永久失败清理等待状态；重试过程中崩溃仍恢复 unknown。配置改变、off、quiet、snooze 都在下一次发送资格中核验，成员查询结束后重新采样时间，防止查询跨越免打扰起点。
6. 尚未被远端接受的过期尾段在本地结束，不另发撤换通知；已有消息仍更新原卡，未知/执行中的请求保留核验边界。阶段变化与 SQL 更新处于短事务，无远端 RPC 在事务内执行。

## 自动验证

- 本机 HTTP fixture 验证两个精确 allowlist、路由外业务码、额度/权限错误、消息发送中、未知 429、5xx、不完整 JSON、限流秒数与 overflow；模糊发送始终不增加自动调用。
- 隔离 SQLite 验证等待跨重启、due 边界、重复回调不延长等待或增加计数、指数退避与平台下限、更新合并、独立线程、成功清理、unknown 不重开及 schema 9→10 保留原日志。
- 正式 Outbound 验证最终结果等待后继续引用原输入、更新始终 PATCH 原卡、等待不查询成员、等待期间关闭 @ 生效、成员查询跨 quiet 边界后抑制提醒。
- 长回复缩短及请求执行期间尾段过期的交错测试验证未送达尾段不会新建消息；unknown/已送达尾段保留既有规则。

最终验证：`go test -race ./...`、`go vet ./...`、`git diff --check`。这些是开发自动测试证据，不是一期真实服务完整验收。

## 仍需完成

建群、reaction、命令回复及 App 待处理提醒的持久限流恢复尚需分别接入；unknown 的远端读回核验、旧 M0 迁移接管、受控 verify、用户服务真实安装与一期验收也仍 pending。`phase1_released=false` 保持不变，不能把新 capability 当作整个一期发布通过。
