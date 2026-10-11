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

App 待处理提醒的持久限流恢复尚需接入；reaction、建群及命令回复已完成下述隔离实现。unknown 的远端读回核验、真实 M0 迁移接管、受控 verify、用户服务真实安装与一期验收也仍 pending。`phase1_released=false` 保持不变，不能把新 capability 当作整个一期发布通过。

## Reaction 的持久限流恢复

正式 `Feedback` 的 add/delete 使用现有网关 typed RateLimited，未扩大 HTTP/code allowlist：通用 `99991400` 且 HTTP 400/429 可延后，reaction 的 `230020` 不支持重试。等待从响应接收时间计算，server 秒数下限与持久指数退避共同生效；超时、5xx、不明确的 429 和永久拒绝不进入此路径。

schema 12 的 `reaction_throttles` 保存 receipt、原操作 key/revision、等待下限和连续拒绝次数；限流拒绝与重新开放已知 reaction 状态在一个事务提交，原操作保持 rejected。下一领取重新核验所属 binding 和当前 receipt.state，生成新 revision，使用最新期望表情。waiting 阶段既不发写请求，也不让 received-loop 反复读取该组成员。完成/已无需更新时清除 throttle。重启不重置等待；changing 按旧规则转 unknown，不能自动重发。

删除成功时，原 delete 操作 confirmed 与旧 emoji/ID 清除共同提交。随后 add 限流不重复删除旧 ID；删除自身限流则保留已知旧 ID，到期再删除。remote 成功但本地确认失败保持 changing，恢复为 unknown，而非盲重试。所有反馈仍独立于原生 queue/steer 提交，不改变输入状态或 client ID。聚合诊断的 `reactions.deferred` 只给数量，不泄露正文。

新增隔离测试覆盖持久等待与重启、响应时钟、server floor、指数退避和溢出、最新状态、delete/add 两阶段、SQL 故障回滚、unknown/永久拒绝不重开、schema 11→12 保留已知 reaction、状态摘要与 received-loop 筛选。网关模拟 HTTP 验证 reaction 的通用频控与不支持代码；真实 M0 未迁移，无外部消息或实际 reaction 请求。

Reaction 组合实现通过 `go test -race ./... -count=1`、`go vet ./...` 和 `git diff --check`；仍只声明该开发切片，未执行真实部署或一期发布。

## 自动建群的持久限流恢复

schema 13 新增 `group_attempts` / `group_throttles`。每次领取在同一事务将意图改为 dispatching、记录独立 attempt；原意图冻结名称、owner、成员，所有请求沿用 intent ID 作为平台 UUID。不依赖 UUID 窗口处理未知请求：只有网关 typed RateLimited 的明确通用频控拒绝才会把 attempt 保存为 rejected、保存响应接收后的 deadline/退避次数并将意图重开为 ready。幂等重放 deferral 不推迟时间；server 下限可高于 60 秒。

Manager 在 waiting 阶段先读本地 deadline，不 Attach 或调用创建 API；到期用实际元数据重验授权，事务领取再检查 observation 仍为 eligible/new、已有 binding、等待下限和未决 attempt。目录 mute 已导致 excluded 时不复活。当前配置的 owner/成员集合若与冻结意图不一致，未创建意图暂停，不按旧名单建群或擅自改原请求；等价的成员顺序/重复不影响核验。成功时 attempt 与 chat ID、意图 confirmed 在一事务确认；binding 另行保存，绑定失败保留群结果。按 exact attempt ID 保存结果，旧请求不能覆盖下一重试；意图级兼容结果接口仅允许首次 dispatch、权威 unknown readback 或同 chat 的 confirmed 幂等，重试中的实时结果必须携带 exact ID。

恢复把遗留 dispatching 的 attempt/intent 共同标为 unknown。schema 12 升级不会替没有 attempt 的旧未决意图编造结果，仍维持 unknown。权限拒绝、额度耗尽、空 chat ID、5xx 和网络未知不进入重试路径，网关 allowlist 没扩大。

线程 actor 收到 deferred 结果后按持久 deadline 在现有 tick 唤醒，不立刻重复读历史或创建 goroutine；等待时仍接收和保存公开过程与最终回复，群确认后正常补齐。没有新的 App/飞书输入也能恢复；错误回调不清除此前已收到的真实事件唤醒，避免限流结束竞争丢掉最新历史同步。状态摘要新增 `group_intents.deferred` 和 `group_attempts` 计数，不输出创建参数或原文。

隔离测试覆盖响应时钟、重启保留等待、原请求参数/UUID、SQL 故障回滚、unknown 不重开、并发到期唯一领取、stale attempt 和意图级 API 不能覆盖新 dispatch、授权变化/排除、backoff 与溢出、schema 12 旧未决数据，以及真实组合 runtime 在限流期间保存最终结果、无新输入到期重试和绑定后补齐。独立只读审查定位并修复竞争分类与兼容接口覆盖的问题；没有访问实际 M0、native 或飞书。

本轮 `go test -race ./... -count=1` 通过，之后针对缺失 exact ID、成员授权和事件唤醒的修正分别重跑 store / bindings / app；关闭补偿扫描的定时器恢复测试连续 10 次通过，`go vet ./...` 和 `git diff --check` 通过。真实 M0 未切换，整个一期仍待部署与 UI 验收。

## 命令确认回复的持久限流恢复

schema 14 新增 `command_reply_attempts` / `command_reply_throttles`。原命令效果、去重记录与回复卡仍在一个事务创建；投递领取先确认 logical reply 状态更新成功，再保存本次原卡、原消息/目标/用户/线程身份、UUID 和时间。网关继续只调用原消息的 reply API，不退化为 send 或 PATCH。

只有 typed RateLimited 才同时保存 rejected attempt、响应接收后 deadline/退避次数、生成下一 UUID 并重新开放该回复。新的领取再次检查时间，原卡和引用关系不改；历史 attempt 不改 UUID 或正文。重复 deferral 不改变等待；暂停中的早期回复不挡住后续已可投递命令。成功/永久拒绝清除等待，未知结果保持冻结；schema 13 旧未决回复没有新 attempt 时不补造证据，恢复为 unknown，保留原 UUID 供权威 readback。

重放入站命令仍查原 app/message 去重记录，不重做任何效果。测试覆盖：旧 off 回复恢复时保留随后 on 的规则与审计；旧 mute 恢复时保留 unmute；stop 消息重复五次和回复重试仅保留一个原生 action。回复收到限流不重做控制 RPC。unknown 即使在已限流重试之后产生，也不继续发下一次。

隔离故障测试覆盖 SQL 失败/忽略领取时不发请求、deferral/confirmation 原子回滚、并发唯一领取、重启保留 response-time floor、冻结 body / parent 与新 UUID、旧结果不能确认新 attempt、持久指数退避/平台下限/溢出、旧 schema 迁移，以及状态的 deferred / attempt 计数。gateway allowlist 不扩大。未访问真实 M0 或发送飞书消息；App 待处理提醒、unknown 远端核验和正式部署仍待完成。

命令回复的最终组合通过 `go test -race ./... -count=1`、`go vet ./...` 与 `git diff --check`。额外注入零行 proof write，验证命令效果/领取/等待/确认均回滚；批量恢复独立处理零至多条记录。未执行真实部署，一期 release 标志仍为 false。
