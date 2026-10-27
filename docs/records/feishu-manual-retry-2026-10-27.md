# 飞书失败卡的手动重试

日期：2026-10-27。协议依据为本机 Codex 0.162.1 导出的 experimental JSON Schema，以及 [OpenAI App Server 文档](https://learn.chatgpt.com/docs/app-server)。Schema 和只读核验不能替代真实写入或客户端验收。

## 行为与实现

最新失败轮次的原生错误分类、完整队列及未决操作状态共同决定是否展示“重试”。只识别明确的暂时故障，不保存或公开 provider 错误正文。按钮放在最终结果首片、设置引用之前，遵守完整 JSON 字节上限；已结束的原生 CoT 不重开。

复用原 callback 身份验证、task control claim、submission action 和 attempt。每次点击只保存一个固定失败轮次的意图，actor 在取得写入授权后再次核验历史与空队列，才沿原 daemon、原线程调用 `turn/start`。请求只有 thread ID、空 input 和带本次 nonce 的来源分类，没有模型、权限或会话策略覆盖。

nonce 由已持久化的 attempt ID 派生，不是原生去重键。调用响应和原生归因同时证明准确的新 turn 后才确认；响应丢失或结果保存前崩溃时，读取同一可信原生日志核验唯一归因。普通 App 活动、队列消失、相似正文和其他 retry 不作为成功证据，unknown 不重发。明确未接受后只允许新的用户点击创建下一次 attempt。

Schema 37 只扩充原生 action kind 的约束，并保留旧 action、attempt 与部分唯一索引；最终结果卡正文压缩后继续保留 callback 能力和摘要。没有新执行队列或完整正文缓存。

## 验证与剩余项

本地回归覆盖：错误分类与正文丢弃；RPC 不覆盖设置且只发送一次；并发 / 重复点击；压缩后的真实结果卡授权；伪造消息、群、revision；新轮次、待执行队列及写入授权期间的竞态；明确拒绝后的再次点击；响应丢失和进程重启的准确来源核验；无证据不重发；首片按钮与完整序列化字节预算；schema 36 到 37 保留原 unknown 日志。

`go test -p 1 ./cmd/... ./internal/... ./compat`、相关 app / codexclient / sync / store 重试回归的 `-race`、submissions 的并发 / 停止 / 继续回归 `-race`、相关包 `go vet`、文档链接与 diff 检查均通过。数据库私有副本升级演练保留 62 张表和 189,435 条记录；逐值摘要一致，除 action kind 约束外的 schema 与索引保持，quick_check 正常、外键错误为零。该副本仅用于本地迁移，没有原生或飞书写入。

经用户明确授权，在现有 0.162.1 daemon 中创建了 1 个 `feishu.bridge.validation` 隔离会话，使用自有 loopback 模型合成容量故障。已验证原线程的空输入重试能够完成，成功轮次没有新增 userMessage，原生日志中准确 nonce 唯一对应本次返回的 turn，且可作为无新输入轮次同步。测试会话已归档并读回确认；没有调用真实模型、创建飞书群或变更原业务会话。

原生写入验收为 **pass**，范围仅为上述版本与隔离模型。探针等待明确的原生完成事件及终态后再归档，不能把收到最终内容作为执行已结束的证据；容量错误的线程状态可能为 `systemError`。App 展示、飞书桌面和手机点击仍为 **pending**；不提升一期发布状态。[协议清单](../../compat/codex-turn-retry.json)
