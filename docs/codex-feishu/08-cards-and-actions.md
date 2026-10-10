# 08 原生卡片与动作

负责 `internal/cards/`、卡片 JSON 样例和移动端验收图。依赖领域 DTO、02 的动作收据、03 的发送接口。

## 设计依据

借鉴 [assistant-ui QueueItem](https://www.assistant-ui.com/docs/api-reference/primitives/queue-item) 的 Steer / Remove，以及 [AI Elements Task](https://elements.ai-sdk.dev/components/task) 和 [Message](https://elements.ai-sdk.dev/components/message) 的正文、状态与详情分层。实现使用 [飞书 Card JSON 2.0](https://open.feishu.cn/document/feishu-cards/card-json-v2-structure)，不使用 code-anywhere 卡片或 Web React 组件。

详细原生组件约束见[卡片研究](../research/codex-feishu-card-ux-2026-10-10.md)。公开组件能展示一个状态，不代表后端已经确认该状态。

## 视图与布局契约

| 视图模型 | 从上到下 | 操作 |
| --- | --- | --- |
| TaskView | 短标题、文字状态、项目、最近动作、更新时间、折叠详情 | 运行：停止；停止且有队列：继续执行；待审批：回 App |
| QueuedInputView | 已排队与真实序号、输入摘要、结束后执行说明、来源页脚 | 主：追加引导；次：移除 |
| HumanMirrorView | 人类正文、底部灰色来源引用区 | 不添加 AI 操作或伪造人名 |
| AIReplyView | 公开正文、必要的折叠执行摘要、底部 AI 来源引用区 | 生成中同卡更新；最终正文固定 |
| CreationDraftView | 主机、唯一已选目录、任务内容、跟进模式、目标、提交 | 主：创建任务；次：取消 / 选择其他目录 |
| NotificationView | @ 开关、生效原因、暂停结束、每日 quiet、范围 | 常用暂停 / 恢复；完整状态可用命令查询 |

全局：单列、短标题、真实状态文字；颜色辅助而不承担唯一语义。首屏最多两个主要动作；手机主按钮 large / fill；长路径完整值放详情，不把省略文本回传执行。没有实际计划或百分比时不画进度条。

来源引用区采用原生 notation / grey 文本，明确“人类发送”或“AI 回复”、来源和时间，置于正文和详情的最下方。长来源可以换行，不依赖 hover。模板对正文中的原生标签作安全编码，不让正文生成可执行按钮或 @。

## 状态驱动渲染

cards 是纯渲染：输入后端视图模型，输出 JSON，不在模板内发 RPC、计算队列序号或修改配置。

所有成功文案须有 confirmed 事实。pending 是“已收到，正在处理”；unknown 是“正在核验是否已接收”；disconnected 显示最后状态时间并禁用实时操作。按钮禁用原因也写到正文，不能仅靠灰色。

变更正文/状态递增 revision，发送更新由 outbox 顺序控制。旧 token 点击返回“状态已更新，请查看最新卡片”；同一已领取动作重复点击返回原处理结果。

## 动作协议

```json
{
  "action": "steer_queue_item",
  "token": "opaque-server-issued-token",
  "revision": 3
}
```

token 由服务端映射到 binding、receipt/native queue ID、动作、可操作人、有效期和 revision。客户端 value 中的 threadId、路径、操作者或正文即使存在，也不能替代服务端记录。表单目录/任务内容是草稿输入，需再次校验。

流程：原始 callback → 身份与令牌验证 → ClaimAction 短事务 → 3 秒内 pending toast → 异步用例 → 更新原卡。动作领取失败或数据库失败时不进入异步用例。[回调规范](https://open.feishu.cn/document/feishu-cards/card-callback-communication)

## 创建表单与确认

原生 form 只放顶层，不嵌套在 column_set / collapsible_panel；name 唯一、必填明确。提交值从 event.action.form_value 读取。目录只有一个 selected_directory：常用列表选中值；“其他目录”打开独立选择卡，确认后返回草稿，不在同一卡上设置三个互相覆盖的目录输入。

停止/移除是否需确认按动作损失决定；如使用确认，普通 callback 不假定支持 submit 才适用的 confirm 弹窗，使用独立确认卡。引导需要立即反馈，不额外增加一次无必要确认。

## 开发切片与验收

先完成 Task/Human/AI/Queue 四种一期模板和动作，再加 Notification 和二期 Creation。JSON 样例只做协议形状测试，真实 Feishu 渲染是独立门槛。

1. 桌面与手机首屏看得到正文、状态和引导按钮；长路径、长代码、长页脚不遮挡操作。
2. 生成中 → 完成、排队 → 追加、unknown → 核验结果均更新同卡，不闪回旧状态。
3. 非授权人、token 篡改、过期卡片和重复点击没有副作用。
4. 审批显示回 App；未验收能力没有按钮。
5. 表单提交只产生一个有效草稿/创建意图；选择其他目录回到原草稿。

起步样例：[队列卡](examples/queue-card.example.json)。它通过 JSON 语法检查，尚待 M0 原生渲染确认；不可作为已经上线的卡片。
