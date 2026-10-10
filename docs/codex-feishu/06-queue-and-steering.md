# 06 默认排队与显式追加引导

负责 `internal/submissions/`。依赖 01、02 及 03 的归一化输入，输出 00 定义的状态数据，由 08 呈现；组合根把按钮路由到本模块，不反向导入 cards。这是一期并发正确性的主要模块。

## 普通输入

所有普通飞书输入都走同一流程：校验 sender/绑定 → ClaimInbound → 保存收据和稳定 clientUserMessageId → `thread/queue/add`。不先判断空闲再调用 turn/start；否则 App 可能同时启动，普通输入会误变成引导。

queue/add 的本地验证行为是空闲自动执行、忙时排队。成功响应后刷新 queue/list 与用户历史，以事实更新“已排队 / 已执行”，不假定返回的项一定还在队列。

收据与原生执行队列分开：SQLite 保存操作和核验状态，执行顺序以 Codex 原生 queue 为准。queue/changed 到来后分页读完整队列，显示真实序号，不能把“当前桥第几条”当整体队列位置。

## 追加引导算法

只允许有效队列项和可用活跃轮；状态过期时返回刷新提示。每个 ThreadKey 使用本桥 actor，但不能认为它锁住 App。

```text
1. 领取 action token，核验操作者 / 卡片版本 / binding / 原生 queue ID。
2. 从最近可信的原生状态取得 active_turn_id；持久保存 steer intent。
3. queue/delete(queuedSubmissionId)。
   deleted=false：停止；核验已执行、已删除或其他变化，不再 steer。
   unknown：进入 reconciling，不进入第 4 步。
   deleted=true：持久保存删除成功，进入第 4 步。
4. turn/steer(expectedTurnId=步骤 2 的 turn，原内容和相关 ID)。
   confirmed：保存响应；再用事件/历史关联真实 user item。
   rejected 且明确未接受：按第 5 步恢复。
   unknown：核验目标 turn 的 userMessage.clientId，禁止重发。
5. 明确未接受且内容不在队列/历史：重新 queue/add 保留原相关 ID。
   记录新 native queue ID；提示可能移到末尾。
```

步骤 2 后 App 开始另一轮时，expectedTurnId 必须仍为原值；不能在失败后偷偷换成新轮 ID 再 steer。当前轮已结束的明确拒绝按后续排队输入恢复；空闲时 queue/add 可以自然启动下一轮。

delete + steer 是两个 RPC，没有事务保证。删除成功保存前崩溃、App 同时消费、App 同时引导和轮次结束都必须交给 02 的 attempt journal 核验。**队列项消失本身不是 steer 成功证据。**

原生 clientUserMessageId 只是关联字段。桥同一收据最多一个未完成提交尝试；重复卡片事件读取原动作结果，不能因为相关 ID 一样就安全再发一次。

## 允许的卡片状态

| 后端事实 | 卡片文案 / 操作 |
| --- | --- |
| 尚未提交 | 已收到，正在提交；不能显示已排队 |
| 原生队列仍有项 | 已排队 · 第 N 条；当前轮结束后执行 |
| 领取引导动作 | 正在追加引导；禁用同项冲突操作 |
| 引导确认 | 已追加至当前轮 |
| 明确拒绝并恢复成功 | 当前轮已变化，已保留为后续消息；位置可能变化 |
| 已被原生消费 | 已执行；显示实际轮次，不再追加 |
| 接收结果未知 | 正在核验是否已接收；禁用重试/追加 |
| 移除确认 | 已移除；不再次提交 |

App 端原生排队消息可以展示队列位置。首版只给桥有完整内容和收据的项提供引导/移除按钮；若要操作 App 排队项，先补齐其持久收据、原生 ID 和同样的并发验收，不能仅凭卡片摘要执行。

## 移除、停止和恢复

移除调用 queue/delete：true 更新已移除，false 核验已消费/已移除，不给出假成功。处于引导/核验中的项不接受并行 remove。

停止当前轮提交真实 turn_id 的 turn/interrupt。确认后显示已停止，保留队列，只有“继续执行队列”或 App 自己显式执行才恢复。恢复调用 queue/start，仅空闲有效；遇到 busy 刷新当前轮，不改成 steer。

停止后追加普通消息的行为也须验收。[OpenAI 原生队列测试](https://github.com/openai/codex/blob/main/codex-rs/app-server/tests/suite/v2/thread_queue.rs)覆盖了停止后 add 保留队列与显式 start；当前发布版本还需在本地确认这一边界，不能只验“停止前已排队”。

断开 Codex 时可以持久接收新飞书输入并标待提交，恢复且目标仍有效后提交；用户移除待提交消息只取消本地收据。若有 unknown 尝试，不能把它当从未发送的待提交项取消并重发。

## 验收场景

| 编号 | 场景 | 通过条件 |
| --- | --- | --- |
| Q1 | 空闲输入；忙时输入 | 同一入口分别执行 / 排队，忙时不改变当前轮 |
| Q2 | 忙时连续 A/B/C，包含 App 队列项 | 原生顺序一致，序号真实，正常消费各一次 |
| Q3 | 将 B 追加引导 | B 进入目标活跃轮，原队列不再执行 B；A/C 顺序保留 |
| Q4 | App 同时删除/消费 B | deleted=false 后不再注入 |
| Q5 | 删除后轮次恰好结束 | 明确失败后恢复后续消息；不引导新轮 |
| Q6 | steer 成功但响应丢失 | 从目标 turn 历史确认，只执行一次 |
| Q7 | queue/add 成功但响应丢失 | 队列/历史找回，不二次 add |
| Q8 | delete 响应丢失、阶段间崩溃 | 核验或明确阻塞，没有盲目 steer |
| Q9 | 同按钮重放、多名操作者并发 | 同一动作只执行一次；旧版本拒绝 |
| Q10 | 停止、再补充、恢复、同时 App 启动 | 队列保留；显式恢复；busy 不误引导 |

Q4–Q10 要在双客户端及故障注入下验证。仅用本桥 mutex 的单元测试不能证明这些并发行为。
