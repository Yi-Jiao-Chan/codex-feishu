# 同一张过程卡与原消息状态 reaction 调研

核验日期：2026-10-10。仅调研 cc-connect 官方仓库与飞书官方 API/Go SDK；未调用业务 API、发送消息、读取凭证。下文“建议”是本桥接器的设计决策，“已验证”来自列出的源码或 API 文档。

## 结论与建议

每个 Codex turn 建立一个发送状态对象：首次出现公开 commentary 时发送一张过程卡；此后只更新该 message_id；turn 终结时把过程卡改为“已完成 / 失败 / 已取消”，再单独发送最终结果。结果发送成功后，才对原用户消息添加成功 reaction。仅公开 commentary/final 可进入卡片；reasoning 不进入发送缓冲。reaction 是辅助显示，失败不能阻断任务或最终回复。

推荐 `OnIt` 表示执行中、`CheckMark` 表示成功且结果已送达、`CrossMark` 表示失败。取消时移除执行中表情，在卡片中明确“已取消”，不冒充成功。以上大小写须原样传入；飞书官方表情表确实列出 `OnIt`、`Get`、`THINKING`、`CheckMark`、`CrossMark`，本次下载的完整官方表中没有 `Done`。cc-connect 示例中的 `done_emoji = "Done"` 不能作为合法枚举依据。[官方表情文案说明](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message-reaction/emojis-introduce)

## cc-connect 的具体实现

截至本次查询，官方 main 为 [`dfad19415a38b00b2c5c288610784d1a7eef337f`](https://github.com/chenhg5/cc-connect/commit/dfad19415a38b00b2c5c288610784d1a7eef337f)，提交时间 2026-09-29 02:41:19 UTC。以下链接固定该提交，不随 main 漂移。

- `compactProgressWriter` 的 card 模式初始发送调用 `SendPreviewStart` 并保存 handle；后续调用 `UpdateMessage`，跳过相同内容；默认只保留最近 10 个条目。`Finalize` 只更新终态，不新增过程条目。最终答复使用另一条消息；飞书卡片终态 footer 明确指向“下一条完整答复”。[过程 writer，295–329、380–523 行](https://github.com/chenhg5/cc-connect/blob/dfad19415a38b00b2c5c288610784d1a7eef337f/core/progress_compact.go#L295)、[飞书终态文案，4699–4717 行](https://github.com/chenhg5/cc-connect/blob/dfad19415a38b00b2c5c288610784d1a7eef337f/platform/feishu/feishu.go#L4699)
- 普通答案 preview 默认 1500 ms、30 个新增字符、2000 字预览上限，并由 timer 补刷；这一套属于 `streamPreview`，并非过程卡 writer。过程 writer 的节流接口是可选的 `ProgressUpdateThrottler`；本提交飞书 `Platform` 没有实现该接口，因此不能推断其过程卡已经以 1.5 秒限流。[答案 preview，11–28、190–231 行](https://github.com/chenhg5/cc-connect/blob/dfad19415a38b00b2c5c288610784d1a7eef337f/core/streaming.go#L11)、[过程 writer 可选节流，307–308、480–493 行](https://github.com/chenhg5/cc-connect/blob/dfad19415a38b00b2c5c288610784d1a7eef337f/core/progress_compact.go#L307)
- 飞书 `UpdateMessage` 对 inline JSON 卡走 `Im.Message.Patch`；引用 card_id 的实体卡走 CardKit 更新 API。不要混用两条更新路径。本桥接器现有 inline 卡可继续使用 PATCH，无须先引入 CardKit。[5372–5409、5443–5461 行](https://github.com/chenhg5/cc-connect/blob/dfad19415a38b00b2c5c288610784d1a7eef337f/platform/feishu/feishu.go#L5372)
- `StartTyping` 给原用户消息创建临时 reaction，保存返回的 reaction_id，停止时删除；`AddDoneReaction` 另行异步添加配置表情。若“已接收”和“执行中”使用相同 emoji，它避免删除持久的接收确认。[1013–1090 行](https://github.com/chenhg5/cc-connect/blob/dfad19415a38b00b2c5c288610784d1a7eef337f/platform/feishu/feishu.go#L1013)

## 飞书已验证约束

| 操作 | 约束与最小权限选择 |
| --- | --- |
| 发送 inline 卡 | `msg_type=interactive`；使用应用机器人身份与现有 `im:message:send_as_bot`。Create 的 uuid 可用于去重，相同 uuid 在一小时内至多成功发一条，最长 50 字符。 |
| 更新 inline 卡 | `PATCH /open-apis/im/v1/messages/:message_id`，body.content 是完整卡片 JSON 序列化字符串；初始及更新卡的 config 都显式设置 `update_multi:true`；token 身份须与发送时相同；只支持未撤回、14 天内卡片；单消息 5 QPS，API 1000/min、50/s；已有 `im:message:send_as_bot` 可调用，不必额外申请 `im:message:update`。 |
| 添加 reaction | `POST /open-apis/im/v1/messages/:message_id/reactions`，`reaction_type.emoji_type`；成功返回 `reaction_id`；机器人须启用、处于该消息会话；不能处理撤回 / 系统消息；权限选择 `im:message.reactions:write_only`。 |
| 删除 reaction | `DELETE /open-apis/im/v1/messages/:message_id/reactions/:reaction_id`；只能删自己添加的表情；同一写权限即可。仅 create/delete 不需要 reaction read 权限或 reaction 事件订阅。 |

约束来源：[发送消息](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message/create)、[更新已发送卡片](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message/patch)、[添加 reaction](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message-reaction/create)、[删除 reaction](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message-reaction/delete)。卡片及富文本最大 30 KB；应检查序列化后的字节大小，并给样式 / 转义留下余量。

本地 Go SDK 已核验 v3.12.0：`client.Im.Message.Patch`、`client.Im.MessageReaction.Create/Delete` 都存在；返回 Go error 与 API code 是两个独立失败入口，调用者须同时检查 err、resp.Success()，并保护 nil Data/ReactionId。源码对应 [`resource.go` 1360–1376、1642–1686 行](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/service/im/v1/resource.go#L1360)。

## 桥接器实现建议

1. 每 turn 保存 progressMessageID、公开 commentary 条目、final item、lastSent 内容、reactionID、terminal 状态。持有该状态的单个 worker 或互斥区串行执行所有 create/patch/final 操作；只在发送成功后推进 lastSent。不要为每个 delta 起独立 goroutine。
2. 在第一次公开 commentary 完成时创建过程卡，此后用 2 秒最短间隔合并最新版（当前实现已选择此值，明显低于单卡 5 QPS）；一次窗口内多个事件只 PATCH 一次，相同内容不发；保留最近 10 项并注明省略。对单条过长 commentary 也截断，不能仅靠条目数限制卡片体积。多 turn 并发时还需应用/租户/API 总预算。
3. final 的文本与 commentary 分开保存。只认公开 final 为最终答案；按 item ID 聚合，delta 后的 completed snapshot 覆盖同 item，不二次拼接。等 turn terminal 再发送最终卡，不能把每个 agentMessage item/completed 都当作 turn 结束。若 final 为空，发送明确的完成状态/无最终答复提示，避免把最近 commentary 伪装为最终结果。此处是当前桥接器协议设计建议，需以本地 app-server 事件模型验证。
4. terminal 先关闭并排空进度刷新，确保没有迟到旧帧覆盖终态；终态仍受 API 限频约束。过程 PATCH 失败也应尝试独立最终发送；最终发送失败不得添加 CheckMark，应记录“执行完成但结果发送失败”。取消/异常时使用独立短 deadline 清理 OnIt，不能使用已取消的 turn context。
5. 每个输入最多一份执行中 reaction；成功创建后保存 reaction_id。若添加请求仍在飞行而 turn 已结束，返回 ID 后也要马上删除，避免遗留“执行中”。取消/失败/成功都清理自己的临时表情，绝不删除其他操作者的表情。单条输入的多 turn/重试不能共享无所有权的删除操作。

cc-connect 的最终结果选择不能照搬：其 `EventResult.Content` 为空时可回退拼接所有 `textParts`；关闭工具显示时甚至优先所有累积文本。这是其抽象事件模型的规则，会混入中途公开文本，不等价于本桥接器的 commentary/final 分离。[`core/engine.go` 5981–5995 行](https://github.com/chenhg5/cc-connect/blob/dfad19415a38b00b2c5c288610784d1a7eef337f/core/engine.go#L5981)

## 错误与重试

| 失败 | 建议处理 |
| --- | --- |
| HTTP 429、code 99991400、PATCH 230020 | 保留最新待发快照并退避；遵循 `x-ogw-ratelimit-reset` 的秒数，缺失时使用有限指数退避。不要丢失 final，也不要不断发送每个旧帧。 |
| 网络超时、连接重置、EOF、服务端 5xx | PATCH 可有界重发同一 ID 的最新完整快照。Create / reaction 未知响应必须按项目 attempt journal 核验；本项目当前冻结而不盲重试。只有证明实际 UUID 去重有效期与对应重试窗口后，才可设计受限恢复；稳定 uuid 不能代替核验。 |
| 卡片 230025 | 收缩过程卡内容；最终卡可按 UTF-8/JSON 大小分片，每片独立 uuid。长度失败不原样无限重试。 |
| 卡片 230011 / 230110 / 230031 | 已撤回/已删/超过 14 天，停止对此过程卡 PATCH；最终回复通过独立发送路径处理。 |
| reaction 231001 | 枚举错误，告警并停用该表情，不重试无效值。 |
| reaction 231002 / 231008、权限失败 | 记录缺权限/不在会话；卡片主流程继续。 |
| 删除 reaction 已不存在 | 按清理完成处理；其他权限错误保留诊断，不影响最终发送。 |

飞书限流包括部分旧 API 的 HTTP 400，不能只看 HTTP 429；官方响应头是 `x-ogw-ratelimit-reset`，不要假定只支持 Retry-After。[官方频率控制](https://open.feishu.cn/document/ukTMukTMukTM/uUzN04SN3QjL1cDN)。cc-connect 当前 PATCH 的重试识别网络瞬态错误，3 次、500 ms 起、带 jitter；它不是完整的业务限频重试方案。[4259–4345 行](https://github.com/chenhg5/cc-connect/blob/dfad19415a38b00b2c5c288610784d1a7eef337f/platform/feishu/feishu.go#L4259)

## 必要验收

- 同 turn 多个 commentary 只产生一个过程 message_id，后续 PATCH 复用它；reasoning 从未出现在请求体。
- 同 item delta + completed 不重复；最终卡只含 final；重复 terminal 只发送一次最终结果。
- 高频事件被合并，相同内容跳过；终态到达后不会被旧 PATCH 重新变为进行中。
- 成功、失败、取消都清理本机器人 OnIt；reaction create 迟到亦被清理；reaction 权限失败不阻断最终结果。
- API 业务非零 code、nil data、429/230020、过程卡被删除、超长卡片与最终发送失败均有可观测处理。
- 本调研自身未调用真实飞书。主代理报告 3 条既有测试输入的 CheckMark API 已成功；这只证明该表情在当前应用/消息上的请求成功，卡片客户端观感与限流行为仍需联调。cc-connect 注释认为 done reaction 可触发推送，本次官方 API 文档没有保证通知/推送行为，不以 reaction 代替最终消息。


## 原生紧凑样式补充（2026-10-10）

以下字段均核验当前官方 JSON 2.0 文档，不依赖 HTML/CSS 或图片模拟：

| 字段 | 官方支持与建议 |
| --- | --- |
| 顶层 `header` | 非必填，可完整省略。无需保留空 header。 |
| `body.padding` | 支持 [0,99]px，可写四边或两值；建议 `"8px 12px"`。 |
| `body.vertical_spacing` | 支持 small/medium/large/extra_large，对应 4/8/12/16px，或 [0,99]px；建议 `"4px"`。 |
| `config.width_mode` | `compact` 宽 400px、`fill` 随屏幕，默认宽度上限 600px；短状态过程卡可 compact，最终答案宜默认或 fill，避免长内容频繁换行。 |
| `markdown.text_size` | `normal` 默认 14px；`notation` 为 12px；建议最终正文 normal、过程摘要/状态/工具统计 notation。普通 Markdown 内容的字号字段就在该元素上，不放在 body 上。 |
| `collapsible_panel` | 官方原生容器；`expanded:false` 为默认收起；header 必填，header.title 支持 `plain_text` / `markdown`；elements 放折叠内容。 |

来源：[JSON 2.0 结构](https://open.feishu.cn/document/feishu-cards/card-json-v2-structure)、[Markdown 元素](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/content-components/rich-text)、[折叠面板](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/collapsible-panel)。

建议首屏仅保留“小字状态 → 当前公开动作”；已完成过程卡压缩为状态与工具计数，最终正文另发。历史公开 commentary 确实有查看价值时才添加一层“过程记录”折叠面板，默认收起，无背景/边框；不为一条短答复增加折叠操作。不要把所有正文都缩成 12px。

可验证的简洁结构：

```json
{
  "schema": "2.0",
  "config": {
    "update_multi": true,
    "width_mode": "compact",
    "summary": {"content": "Codex 正在处理"}
  },
  "body": {
    "padding": "8px 12px",
    "vertical_spacing": "4px",
    "elements": [
      {"tag": "markdown", "content": "**正在处理** · 工具调用 3 次", "text_size": "notation"},
      {"tag": "markdown", "content": "正在核对接口返回值。", "text_size": "notation"},
      {
        "tag": "collapsible_panel",
        "expanded": false,
        "padding": "0px",
        "vertical_spacing": "4px",
        "header": {
          "title": {"tag": "plain_text", "content": "过程记录"},
          "padding": "0px",
          "icon": {"tag": "standard_icon", "token": "down-small-ccm_outlined", "size": "16px 16px"},
          "icon_position": "right"
        },
        "elements": [
          {"tag": "markdown", "content": "已完成配置检查。", "text_size": "notation"}
        ]
      }
    ]
  }
}
```

该示例用于字段和视觉联调，不意味着已在实际客户端验收。`down-small-ccm_outlined` 来自折叠面板官方示例。省略 panel 的 background_color/border 使用透明无边框；不设置 header.width，避免引入其 >=7.32 客户端限制。JSON 2.0 整体需要客户端 >=7.20。面板最多嵌套五层，且不能内嵌 form；此布局只用一层。

主代理当前实现报告：2 秒最短 PATCH、最新 commentary 800 字、工具计数、按 `phase=final_answer` 选择最终答案并兼容无 phase 旧事件。这里不要求为审美修改协议；建议仅使用上述原生间距/字号减少首屏块数，再实测手机与桌面显示。
