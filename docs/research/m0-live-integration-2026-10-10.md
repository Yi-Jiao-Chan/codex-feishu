# M0 真实联调进展

日期：2026-10-10。状态：**开发验证，完整 M0 与一期发布尚未通过**。复用当前系统用户的既有 Codex daemon，所有外部测试限定本人测试应用 / 测试群与独立测试会话；未改变全局 Codex 配置或原线程权限。

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
| Go 原生双客户端协议 | opt-in 集成测试使用已有 daemon + 本地模型 fixture，证明 queue/add、exact delete、expectedTurnId steer、原生消费各一次；独立线程清理 | pass（协议，不能代替 App UI） |
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

## 当前实现边界

`m0-listen` 仅桥接显式提供的一个 App 测试线程和已批准测试群；发现与自动建群未启用。当前不是 `serve` 或发布版本。不要把 `doctor` 只读健康、API 请求接受、原生队列接受分别误当成全部交付 / 最终执行成功。

过程按 turn 聚合，2 秒最短更新，只保留最新公开 commentary / partial_answer 摘要（最多800字）及工具状态计数；不发送 reasoning、原始命令输出。终态压缩过程卡，原生 final_answer 合并成独立结果；未知 phase 兼容最后一条公开输出。过程卡失败不阻断独立最终发送。最终投递未确认时不添加完成勾。

出站操作先落 journal。网络丢响应保持 unknown，不盲重发；reaction 失败作为辅助反馈记录，不阻断 Codex 输入。M0 对未知操作保持冻结，完整恢复 worker / outbox 在 M1–M2 实现。当前最终长文本沿用3500字符分片，还需补段落 / 代码边界及 JSON 字节上限，不能把短文本通过当作长代码验收。

运行方法（开发专用，命令支持范围以 `version --json` 为准）：

```sh
go build -o bin/codex-feishu ./cmd/codex-feishu
bin/codex-feishu doctor --config /absolute/path/to/config.toml --json
bin/codex-feishu m0-listen --config /absolute/path/to/config.toml --thread APP_TEST_THREAD_ID --chat VERIFIED_TEST_CHAT_ID --json
```

本机配置、secret、DB 与证据日志保存在仓库外的用户配置 / 数据目录，`.env` 和二进制不纳入 Git。测试继承 GOCACHE / GOMODCACHE。

## 仍需完成的门槛

1. 真实 App UI：手机 / 长代码 / 权限拒绝场景。追加引导 UI 已由用户确认。
2. M0 API：群成员 GET 实际返回99991672，缺 im:chat.members:read（已请用户开通并发布）；自身消息与回复 parent_id 已可读回。仍需真实 @ 提醒；UUID 去重有效期对应的实际重复调用。已修正建群 UUID 在 query，发送 UUID 在 body，SDK / 本地 wire 测试证明形状；原先首个群的 UUID 放 body 不作为去重通过证据。
3. M0 分类：首条输入、fork / worktree / 子代理边界。App 与 CLI 均可能呈现 source=vscode、originator=codex-tui；不能据此分类。
4. M1–M3：自动发现 / 唯一建群与绑定、持久 outbox、重连 / 恢复 / 限流、队列全部竞争场景、停止 / 显式恢复、目录 mute、@ / quiet / snooze、正式 setup / 服务 / 兼容与权限清单、一期矩阵。

M0 协议成功或用户确认局部界面不免除剩余发布门槛。参见[集成验收](../codex-feishu/11-integration-and-acceptance.md)、[过程卡与 reaction 的一手来源研究](progress-cards-reactions-2026-10-10.md)。
