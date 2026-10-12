# 受控绑定的只读投递证据

输入已关联原生 user item 不代表 AI 回复已经送达。原绑定查询现在从正式镜像 / 投递日志读取 AI 卡片和回复关系的局部证据，方便随后核验真实 App / 飞书体验；不增加远程测试操作。

## 快照与计数

`verify --config PATH --target self --request-id ID --json` 复用原管理查询。可选 `delivery_evidence_status=pass` 仅表示只读快照成功，缺字段或读失败保持 pending / 无快照。输入和投递证据分别读取，不能把两个快照当作同一原子完成记录。

Store 在一个只读事务中重新比较完整私有登记，核验部署 app、原 route / intent / owner / chat，再读取镜像和 attempt 元数据，不选正文、令牌或完整请求。此绑定 gate 与输入证据共用；没有新 schema、远端 GET、PATCH、发送、恢复 unknown 或修复。

| 字段 | 证明范围 |
| --- | --- |
| progress_cards / progress_confirmed | 当前非 retired 原生轮次过程卡数量，以及已有当前 revision 确认和原 reply 证明的数量；排除 `ui:` 队列卡 |
| final_cards / final_confirmed | 当前非 retired 最终分片数量，以及同样确认的数量 |
| final_turns / final_turns_delivered | 有当前最终分片的轮次数；完成需全部最终片及已发送的 retired 清理片都有当前确认 / 引用证明 |
| referenced_cards | 当前 AI 文档的原 confirmed reply 指向匹配的输入锚点，chat / remote / parent 一致；仅普通 send 不成立 |
| unresolved_attempts | 原绑定 scope / chat 的 dispatching / unknown 卡片尝试，包含队列 UI，未决不推导成功 |

输入锚点只能来自同 app / thread / chat 的原收据和该 turn 完整历史关联，或同 scope / turn、具备原发送确认的 Codex 人类镜像。不能仅凭 parent 非空或同名群推导引用。当前版本确认还需当前 revision 的 reply / update attempt；update target 必须是原 remote ID，不能借其他卡确认。

retired 且从未发送的 suppressed 分片可跳过，但需没有已确认创建或 dispatching / unknown 请求；不能把丢失的 remote 字段或不同 chat 的未决创建当成“从未发送”。该读取证明比单看最终 outbox 状态更保守，不修改 Engine 或发送行为。

CLI 分别报告快照、过程投递、整轮最终投递与当前文档的回复关系组件。计数不证明每轮只创建一张过程卡、真实渲染、reaction、@、所有轮次或全部 P1；完整 `verify_managed_tests` 和一期发布保持 false。

## 验证边界

隔离 Store 正常 API、实际 Service.Run / 原 actor / 唯一接收器和本地管理 socket 验证。最终 reply 暂缓返回时，查询不得报告最终分片 / 整轮完成；放行后按当前 revision 和原回复关系确认。错误 scope、parent、revision、缺创建证明、长回复部分确认与未清理尾片保留未完成。

本轮未发送真实飞书消息或原生输入，未部署、迁移或替换 M0。真实 App / 飞书 UI 和完整一期矩阵仍待验收。

验证通过：`go test -race ./...`（app 63.430s、store 131.448s、verification 52.805s）、`go vet ./...`、`git diff --check`。共享绑定 gate 的原输入证据和新增投递证据联合 race 测试已先通过 29.494s，补充空 client 收据反例通过 7.490s；CLI / 控制协议 / 管理器与 Service.Run 的聚焦整合测试也通过。
