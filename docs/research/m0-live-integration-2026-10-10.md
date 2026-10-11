# M0 真实联调进展

日期：2026-10-10，2026-10-12 补齐 App 分类。状态：**M0 可行性联调通过，一期发布尚未通过**。复用当前系统用户的既有 Codex daemon，所有外部测试限定本人测试应用 / 测试群与独立测试会话；未改变全局 Codex 配置或原线程权限。

## 已验证

| 项目 | 证据 | 结果 |
| --- | --- | --- |
| 共享 daemon、线程分页 | CLI / app-server 0.162.0；Unix WebSocket；初始化与分页 thread/list，530 条完整读取 | pass |
| 官方飞书长连接 | Go SDK v3.12.0 的 ws + event dispatcher；实际 ready / 私聊事件 / 群结构化 @ / 卡片点击回调 | pass |
| 本人私有测试群、原生卡片 | 真实建群；原生 JSON 2.0 发送 / PATCH；同应用机器人身份 | pass |
| 真正 App 发起的线程 | 用户第一次测试误用 CLI，明确纠正并重新从 App 发起；旧 CLI 绑定停用保留审计，未改原生线程 | pass |
| 飞书 → 同一 App 线程 | 用户确认 App 实时出现飞书输入与 AI 回复 | pass |
| 忙时默认排队 | 用户确认 App 原生队列及群内操作卡，未点击引导时不注入当前轮 | pass |
| 飞书人类输入不重复镜像 | clientId 关联收据；撤回旧重复卡；空闲接受不发额外输入卡，队列卡不带输入正文 | pass |
| 过程同卡更新、最终单独结果 | V3 实际运行：一张过程卡被多次 PATCH，同一 turn 只发一张最终结果；用户答复“可以了”确认实际交互 | pass |
| 原消息 reaction | V3 实际添加 Get → 替换 OnIt → 替换 CheckMark；只删除机器人自己取得的 reaction_id；用户确认 | pass |
| 紧凑样式 | 无 header、compact、body padding 8px 12px / vertical_spacing 4px；过程与状态 12px，最终正文14px，来源底部12px引用区；PATCH 实际接受且用户确认 | pass（桌面短文本） |
| App 实际追加引导 | 用户确认补充消息从原生队列进入当前轮，最终结果包含引导标记；后台 exact delete / expectedTurnId 指向同一轮，两个输入完成 reaction | pass |
| 原消息引用回复 | 真实 Reply API 发送、PATCH 后 GET 读回 parent_id 与用户补充消息 ID 相同；业务按 turn 首条输入 / 操作卡对应输入选择父消息，相关单元测试通过 | pass（API / 关联逻辑） |
| 群成员与机器人读回 | 用户开通并发布 im:chat.members:read 后，实际 GET 群 / 全量成员 / 机器人是否在群 / 自己消息全部成功；owner_in_group、bot_in_group 均 true | pass |
| 实际 @ 与手机长代码 | 引用原用户消息发送含本人显式 @ 的 24 行长代码卡；用户对实际提醒、手机完整查看及底部引用区答复“没问题” | pass（本次 fixture） |
| UUID 实际去重 | 同一进程内，建群 query.uuid、发送 body.uuid、引用回复 body.uuid 各立即重复一次，返回 ID 均与第一次相同；每次请求独立 journal，未知不重试 | pass（即时重复，不证明过期后行为） |
| fork / worktree 协议 | 既有 daemon + 独立本地模型 / 临时 Git 仓库：fork 新 ID、forkedFromId、parent 空、实际 cwd、注册 worktree/common-dir、继承 turn ID 均正确；两条 fixture 线程归档清理 | pass（协议，真实 App 分类仍待确认） |
| 真实 App fork / worktree / 子代理分类 | 2026-10-12 用户提供工作树目录；App 工具读取实际新增测试输入、父子最终标记；原生详情确认 forkedFromId 指向原 M0、主线程 parent 空、子代理 source/parent 明确；Git 确认注册工作树和相同 common-dir | pass（真实 App 与元数据交叉核验） |
| Go 原生双客户端协议 | opt-in 集成测试使用已有 daemon + 本地模型 fixture，证明 queue/add、exact delete、expectedTurnId steer、原生消费各一次；独立线程清理 | pass（协议，不能代替 App UI） |
| 原生问答与桥断开 | 同 daemon 独立模型 fixture；Default 问答专用 feature 仅在自有线程开启，两个连接收到同 ID，桥不答且断开，另一端回答后完成，收到对应 serverRequest/resolved；线程归档清理 | pass（协议，App 问答 UI pending） |
| 首条输入 / 未加载历史 | M0 不再依赖 preview 的标记，必须首个真实 userMessage 带测试标记；summary/notLoaded 补齐 items 全部分页，拒绝跨 turn 内容；本地 race 测试通过 | pass（本地门槛与分页 fixture） |
| 竞争与未知响应 | Go race 测试包含输入去重、journal 重启、监听独占锁、RPC / 消息丢响应不重复发送、同卡聚合、结果去重、reaction 替换与未知冻结 | pass（本地故障注入） |

## V3 可核验标识

这些是本次本人测试产生的远程资源标识，不包含凭据或真实业务正文。

- App 线程：`01a12524-92d1-7c10-bcc3-9e3d58976116`
- 测试群：`oc_ac5e4afa8c526821d9636715688139c6`
- V3 原飞书消息：`om_x100b6394e5e8a4b8b27f6f333de6917`
- V3 原生 turn：`01a12539-ce50-7140-95c1-b27426b5731e`
- V3 唯一过程消息：`om_x100b6394e589a8b0dfa3d2ce53aaedd`
- V3 最终消息：`om_x100b6394e0b6d8b8c34df60c2ede9e7`

追加引导轮：`01a12540-b39d-7ad2-97a7-d5244ddbbcf6`；原输入 `om_x100b639489936088b18322eac554966`，引导输入 `om_x100b6394869448a0b18e657371eb3ee`。后者 native queue/delete 确认 true，turn/steer 确认 expectedTurnId 为该轮；用户确认当前轮与结果正确。

引用回复 probe：`om_x100b63949337b0a4deb07bedc020768`；GET 读回 parent_id 为 `om_x100b6394869448a0b18e657371eb3ee`，与目标一致。原消息回复关系在发送时确定，旧直接发送的消息无法通过普通 PATCH 改成引用回复；正式后续投递使用 Reply API。

既有三条已完成的飞书测试输入也成功添加 CheckMark。旧忙时测试的三条公开过程卡收敛为一张状态卡，撤回另外两条；撤回一个旧“已进入会话”操作卡。原生聊天记录及最终回复保留。

补充验证：UUID 测试群 `oc_a3087ee5c360729cf922b6580bceca32`，发送去重消息 `om_x100b639569a3c8a8debb78f71693978`；主测试群 @ / 手机布局引用卡 `om_x100b6395695edcb4c2fd3919234b2e0`。相同 UUID 两次立即调用只得到同一个 chat_id/message_id，引用卡 parent_id 也正确。

原生 fork fixture：源线程 `01a1255a-c725-7d70-9bd4-0d1af6e58ac3`，fork `01a1255a-d082-70a1-863b-8966761c1e0f`；测试后均归档，临时仓库与 worktree 删除。无用户工作线程变更。

补齐历史后的 Go 原生队列 / fork / worktree 回归再次通过：源线程 `01a12575-2c1f-7fd1-8803-2cafb78c41ae`，fork `01a12575-359a-7252-9a7b-49478da987a5`；均已归档。原生 Default 问答 fixture 首次通过线程 `01a12579-b6a5-79f2-a29a-2b6a6c1c4f6b`，与 App UI 来源无关，不作真实 UI 验收证据。

补充精确 resolved 断言后的问答 fixture `01a1257c-8464-7380-9226-60424879ba5e` 再次通过，确认返回 threadId/requestId 与原请求完全对应；同样已归档。

## 当前实现边界

`m0-listen` 仅桥接显式提供的一个 App 测试线程和已批准测试群；发现与自动建群未启用。当前不是 `serve` 或发布版本。不要把 `doctor` 只读健康、API 请求接受、原生队列接受分别误当成全部交付 / 最终执行成功。

过程按 turn 聚合，2 秒最短更新，只保留最新公开 commentary / partial_answer 摘要（最多800字）及工具状态计数；不发送 reasoning、原始命令输出。终态压缩过程卡，原生 final_answer 合并成独立结果；未知 phase 兼容最后一条公开输出。过程卡失败不阻断独立最终发送。最终投递未确认时不添加完成勾。

收到普通审批 / 原生问答时，用既有引用过程卡显示“等待你在 Codex App 中处理”；原消息执行 reaction 暂变 THINKING。重复 resume 的请求不重复新增卡片，只有对应 generation/request ID 的 resolved 或该轮终态消除等待。桥始终不回答这些请求、不返回“不支持方法”。状态与 ID 关联由本地测试覆盖；真实 App 等待卡与审批 UI 尚未验收。特殊 MCP UserVerification 不套用此普通请求路径。

审批定位：隔离的打印命令探针在 55 秒内停于 `permissionRequest` hook running，未出现 hook/completed 或原生 requestApproval，未证实命令审批路由。准确版本源码表明 hook 先于 Guardian/User，可直接消费 Allow/Deny；本次不是自动审批明确拒绝的证据。保留该项 pending，不关闭全局或 managed hooks，不改用户会话策略。独立问答的首次失败则因 Default 模式 gate 默认关闭，尽管工具已 advertised；仅自有 fixture 开启 `features.default_mode_request_user_input` 后通过。[路由与定位依据](approval-routing-0.162.0.md)

出站操作先落 journal。网络丢响应保持 unknown，不盲重发；reaction 失败作为辅助反馈记录，不阻断 Codex 输入。M0 对未知操作保持冻结，完整恢复 worker / outbox 在 M1–M2 实现。当前最终长文本沿用3500字符分片，还需补段落 / 代码边界及 JSON 字节上限，不能把短文本通过当作长代码验收。

运行方法（开发专用，命令支持范围以 `version --json` 为准）：

```sh
go build -o bin/codex-feishu ./cmd/codex-feishu
bin/codex-feishu doctor --config /absolute/path/to/config.toml --json
bin/codex-feishu m0-listen --config /absolute/path/to/config.toml --thread APP_TEST_THREAD_ID --chat VERIFIED_TEST_CHAT_ID --json
```

本机配置、secret、DB 与证据日志保存在仓库外的用户配置 / 数据目录，`.env` 和二进制不纳入 Git。测试继承 GOCACHE / GOMODCACHE。

## 仍需完成的门槛

1. 真实 App 分类已于 2026-10-12 补验通过，见[脱敏证据](m0-app-classification-2026-10-12.json)。实际 fork 为 `01a12bb4-59ae-7100-9db0-58b6dac81c66`，子代理为 `01a12bb4-de40-7772-b1ad-31d66991a503`。工作树不在原目录物理前缀内，但已注册且同仓库；M1 将按仓库身份及子目录范围落实 worktree 授权。App/CLI 仍不能仅凭 source 区分。

   分类维护检查覆盖活跃及归档谱系，支持显式 App 样本 ID。实际 thread/list 的 fork 血缘为空，thread/read 才得到 forkedFromId；因此发现器必须按候选读详情，不能用列表字段代替分类。原会话 7 个用户项，新会话继承后新增测试输入共 8 项；父子标记均读回最终 agentMessage。只读验证不 resume 归档线程，不输出标题或正文。此前无样本的结果已由本次真实操作补齐。

   开发检查的参数为位置参数，不使用 `--config`：

   ```sh
   go run ./cmd/m0-maintenance /absolute/path/to/m0.toml APP_TEST_THREAD_ID --classification-check APP_SAMPLE_THREAD_ID
   ```
2. M0 API：权限阻塞已解除；@、手机长代码、群成员、引用 parent_id、UUID 即时重复均已通过。去重期限采用官方建群同 owner 10 小时、消息/回复1小时契约；本次未等待期限到达或超期再请求，也不会用未知请求去验证是否产生重复。原先首个群的 UUID 放 body 不作为去重通过证据。仅测新发送含 @；PATCH 触发提醒行为仍未测，首版过程 PATCH 始终不含 @，最终新发送才含策略生成的 @。
3. 一期权限拒绝 / 长内容分片 / 过期按钮等剩余矩阵继续保留 pending；手机 fixture 通过不泛化到所有内容。
   审批需核验真实 App 处理及桥等待卡；命令审批原生探针需在保留平台 hooks 的条件下到达审批请求。同步问答的双客户端协议通过不能代替这些验收。
4. M1–M3：自动发现 / 唯一建群与绑定、持久 outbox、重连 / 恢复 / 限流、队列全部竞争场景、停止 / 显式恢复、目录 mute、@ / quiet / snooze、正式 setup / 服务 / 兼容与权限清单、一期矩阵。

本轮 `go test -race ./...`、`go vet ./...` 与 opt-in 原生队列 / fork / worktree 集成测试通过。M0 维护工具新增只读 `--scope-check` / `--classification-check` 与一次性 `--uuid-probe`；重复启动 probe 不重发已确认或未知调用，去重有效期不跨进程猜测。原测试桥接重启后长连接 ready、同一绑定保留。

继续验证时，`CODEX_FEISHU_NATIVE_TEST=1` 启用已通过的独立队列 / fork / 问答 fixture。命令审批探针单独由 `CODEX_FEISHU_APPROVAL_TEST=1` 启用，当前在本机保留 pending，普通测试跳过不表示已通过；不要为了获得绿灯关闭 hooks。所有 fixture 使用既有 daemon，只调整自己新建的测试线程，结束后归档。

权限选择详见[精确权限研究](feishu-m0-permissions-2026-10-10.md)。

M0 协议成功或用户确认局部界面不免除剩余发布门槛。参见[集成验收](../codex-feishu/11-integration-and-acceptance.md)、[过程卡与 reaction 的一手来源研究](progress-cards-reactions-2026-10-10.md)。
