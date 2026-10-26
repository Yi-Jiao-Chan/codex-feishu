# 飞书原生 CoT 接入边界

契约来源：[飞书接入参考](https://bytedance.larkoffice.com/wiki/ZfyXwOQImiLURtkRPtWcf2B2nNh)、[AG-UI 事件参数](https://bytedance.larkoffice.com/wiki/UDxqwqxBnitkFHkYc1WcVC6InDg)。实现与恢复规则见 [03 网关](../docs/codex-feishu/03-feishu-gateway.md#原生-cot-进度通道)、[02 存储](../docs/codex-feishu/02-store-and-recovery.md#schema-31原生-cot-投递状态)。

| 操作 | 请求与必要结果 |
| --- | --- |
| 创建 | `POST /open-apis/im/v1/message_cot?receive_id_type=chat_id`；`receive_id`、可选 `origin_message_id` / `reply_in_thread`；必须取得 `message_id` 和 `cot_id` |
| 追加 | `PUT /open-apis/im/v1/message_cot`；`message_id`、`cot_id`、`events`；每项含 `event_type`、JSON 对象编码成的 `content` 字符串、毫秒 `timestamp` |
| 完成 | `POST /open-apis/im/v1/message_cot/complete/{cot_id}?message_id=...&reason=done或error` |

每批最多 50 项；实现保守按整个序列化事件限制 4096 字节。外部调用不使用 `timeout`。基础 CoT 已开放，自定义 `statusText` 单独灰度，本实现不依赖它。创建无已核实的幂等键；普通消息 GET 不能核验内部事件或完成状态，任何 unknown 均不盲重试。

事件仅含公开进度与工具的展示副本。公开消息使用 `REASONING_START` / `REASONING_MESSAGE_*` / `REASONING_END` 作为展开区载体，不读取或上传模型隐藏推理；ARGS 为空对象占位。展示版本 2 将 `toolCallName` 与最多 32 字符的 `title` 分离；RESULT 的字符串正文承载实际命令/脚本、参数、输出或 diff 的脱敏节选，用闭合代码围栏分开执行内容和结果。已核实成功/失败分别带 `status=completed/error` 与 `isError=false/true`，退出码和毫秒耗时有原生字段才发送；正文不重复退出码 0、耗时和有实质内容时的“已完成”，非零退出码仍显示。缺失状态不生成成功结论，未知/拒绝且未开始的项只作公开说明。

无可用文本时保留文件/动作详情，最多 6 条、每条 160 字符。输入节选最多 1200 字符、输出最多 2400 字符；完整事件按实际二次编码的 4096 字节边界再次裁剪，分配空间保留结果并标注省略。常见凭据、控制字符、媒体/resource 和宿主目录按[同步规则](../docs/codex-feishu/05-message-sync.md#新执行进度使用原生-cot)处理，截断不代替脱敏。相同 `toolCallId` / `messageId` 的新 RESULT 用于补齐或更正内容，ACK 比较全部结果字段；不靠重复 START 修改标题。旧展示版本 1 的活动轮次保持原 payload 和 ACK 语义，新轮次才使用版本 2；已有版本 2 的未完成消息可补齐可选节选，已完成消息不重写。`TOOL_CALL_END` 表示参数结束，不能当作执行成功；完成需 `TOOL_CALL_RESULT`。正常完成用 `RUN_FINISHED(status=done)`，中断用 `interrupted`，失败用 `RUN_ERROR`，再完成生命周期。字段已于 2026-10-26 重新核对官方事件文档，新内容的真实客户端效果仍待验收。

原生停止是普通 `/stop`，不含卡片动作令牌；群聊默认隐藏入口时使用对应任务群或话题命令。接收时冻结唯一活动轮次，不声称能复原网络延迟前点击的旧 run。本人私聊合成样式已由用户确认，正式 Gateway 合成闭环通过；群聊、移动端与真实停止仍分别验收。权限清单保持已有已核实 scope，不因单应用调用成功推断新的通用 scope 映射。

2026-10-23 重新核对上述接入参考与[官方 agent 接入文本](https://feishu-agui-playground.gf-boe.bytedance.net/llms.txt)：原生停止图标发送用户可见命令，没有已公开的替换回调/隐藏命令参数。`cot_hidden` 仅用于完成后隐藏 CoT。本地可以省略额外成功回执与空正文停止结果卡，不能据此宣称客户端命令已隐藏；用户消息不撤回。
