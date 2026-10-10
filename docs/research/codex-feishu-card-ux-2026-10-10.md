# 飞书 Codex 桥接卡片交互参考

核验时间：2026-10-10 15:43 中国标准时间。只读取一方组件文档与官方示例；未安装参考项目、创建会话或发送飞书消息。本文是设计建议，组件具备展示能力不代表桥接后端已实现对应操作。code-anywhere 仅作为建群、群聊跟进、mute、关闭 @ 等产品能力参考，不采用其卡片样式或实现。

## 选用三个参考

| 参考 | 已核验能力与可借鉴之处 | 飞书落点与边界 |
| --- | --- | --- |
| assistant-ui | `QueueItemPrimitive.Text / Steer / Remove` 分别展示排队输入、将该项引导进当前运行、移除排队项；最贴合“默认排队，显式追加引导”。[队列组件](https://www.assistant-ui.com/docs/api-reference/primitives/queue-item) | 每条排队输入独立卡片，按钮直接命名“追加引导”“移除”。组件文档不证明 Codex 运行时具备该语义，必须由后端确认。 |
| Vercel AI Elements | Task 把摘要与详情分开；Queue 按状态组织队列；Tool 区分运行、审批、完成和错误；Message 区分人类/AI 内容及回复操作。[Task](https://elements.ai-sdk.dev/components/task)、[Queue](https://elements.ai-sdk.dev/components/queue)、[Tool](https://elements.ai-sdk.dev/components/tool)、[Message](https://elements.ai-sdk.dev/components/message) | 用标题、正文、折叠面板表达层次；等待审批单独显示。示例状态与任务数据不能当作真实进度，不照搬悬停操作或浏览器组件。 |
| 飞书 Card JSON 2.0 原生文档及组件示例 | `body.elements`、按钮、折叠面板、表单、单选下拉、文本具备所需布局与输入能力。[结构](https://open.feishu.cn/document/feishu-cards/card-json-v2-structure)、[按钮](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/interactive-components/button)、[表单](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/form-container) | 直接使用原生组件；不引入 Web 渲染层。表单只放顶层，不能放进分栏或折叠面板。 |

assistant-ui 的消息错误组件采用消息内错误提示，适合借鉴“错误留在相应卡片上”；ActionBar 支持复制完成反馈，也有悬停隐藏模式，后者不适合这里的手机群聊。[错误](https://www.assistant-ui.com/docs/primitives/error)、[ActionBar](https://www.assistant-ui.com/docs/api-reference/primitives/action-bar)

## 四类卡片的具体契约

### 1. 任务状态卡

从上到下：`header` 的短任务标题；`div` 的文字状态；远程机器与目录；当前动作及更新时间；默认收起的 `collapsible_panel` 执行详情；操作区。颜色仅辅助辨识，始终保留“运行中 / 等待审批 / 已完成 / 已停止 / 失败 / 连接中断，状态待核验”等文字。目录首屏显示项目名，详情显示完整路径；不截断后把省略路径当成可操作值。

运行中显示“停止当前轮”和“查看详情”；停止后保留结果与停止时间。连接中断时显示最近已知状态及时间，禁用依赖当前轮的操作。审批与普通错误置于折叠区域外；只有后端确认支持且不会重复执行时才出现“重试”。不按耗时编造百分比，有真实计划步骤时才展示完成数。参考 [Task](https://elements.ai-sdk.dev/components/task) 与 [Tool](https://elements.ai-sdk.dev/components/tool) 的摘要/状态分离；详情采用[原生折叠面板](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/collapsible-panel)。

### 2. 排队消息卡：默认排队，按需“追加引导”

收取输入后先持久化排队，再显示“已排队 · 第 N 条”、输入摘要、“当前轮结束后执行”、可见按钮“追加引导”和次要按钮“移除”。第一版不提供拖拽排序；位置只在后端确认后刷新。引导按钮仅在当前轮允许接受直接输入时可用，其他情况下保留队列并显示原因。来源页脚记录输入者及来源。

状态转移为：`已排队 → 正在追加 → 已追加`。点击后禁用按钮，卡片显示“正在追加引导”。按主 agent 在共享 daemon 0.162.0 的本地核验，后端需先写操作 journal，再调用 `thread/queue/delete`；仅返回 `deleted: true`、确认尚未被消费时，才调用 `turn/steer(expectedTurnId)`，收到接收确认后同卡显示“已追加至当前轮”。已被消费的项不再注入；轮次过期等明确失败可恢复排队，但不承诺原位置，也不声称跨 RPC 原子性。请求结果未知时显示“正在核验是否已接收”，结合 `clientId` 与历史核验，禁止盲目重发或自动新开一轮。以 [assistant-ui Steer / Remove](https://www.assistant-ui.com/docs/api-reference/primitives/queue-item) 为交互参考，不以其 UI 推断后端能力。

以下是可供实现起步的原生结构示意；标识和值由服务端生成并验证，尚未通过飞书实际渲染测试：

```json
{
  "schema": "2.0",
  "header": {
    "title": {"tag": "plain_text", "content": "已排队 · 第 2 条"},
    "template": "blue"
  },
  "body": {
    "elements": [
      {"tag": "markdown", "content": "请先补充失败原因，再继续修改。\n当前轮结束后执行。"},
      {
        "tag": "button", "element_id": "steer", "type": "primary",
        "size": "large", "width": "fill",
        "text": {"tag": "plain_text", "content": "追加引导"},
        "behaviors": [{"type": "callback", "value": {"action": "steer_queue_item", "token": "opaque-server-issued-token"}}]
      },
      {
        "tag": "button", "element_id": "remove", "type": "text",
        "text": {"tag": "plain_text", "content": "移除"},
        "behaviors": [{"type": "callback", "value": {"action": "remove_queue_item", "token": "opaque-server-issued-token"}}]
      },
      {"tag": "div", "text": {"tag": "plain_text", "content": "人类 · 张三 · 来源飞书 · 15:42", "text_size": "notation", "text_color": "grey"}}
    ]
  }
}
```

### 3. AI 回复卡与来源页脚

正文优先，用 `markdown` 放回答；工具调用和长执行记录放折叠面板，失败说明保留在正文附近。生成中显示“AI 正在回复”，结束后改成“AI 回复”；避免每个 token 新发一张卡。正文下使用 `div + plain_text + notation` 页脚，例如“AI · Codex · 来源桌面 App 会话 · 远程机器 / 项目 · 15:42”；镜像人类输入使用“人类 · 实际输入者 · 来源 CLI · 15:41”。AI 卡片可补一行“本轮输入来自……”，但多条混合来源应如实列出。

来源取自持久化事件元数据，不从正文猜测，不把所有 App/CLI 输入归给飞书触发人；不能识别的人名或入口写“未知”。镜像人类消息保持角色标识，避免用户把机器人转发的文字当成 AI 建议；转发事件不得再次进入输入链路。参考 [Message](https://elements.ai-sdk.dev/components/message) 的角色区分，页脚采用[原生文本组件](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/content-components/plain-text)。不展示隐藏推理过程，也不照搬分支、重新生成按钮而引入未实现的会话行为。

### 4. 二期新建任务卡

使用顶层 `form`，依次放：远程机器摘要；目录 `select_static`（常用/最近目录）与可选绝对路径 `input`；任务说明 `input`（`multiline_text`，`rows: 3`）；会话位置 `select_static`（“群聊”默认，“话题”可选）；“创建任务”提交按钮与“取消”。群聊选择保持当前产品的建群策略，并明确显示目标；不要让用户猜测将进入现有群还是新群。话题仅在后端确实支持时提供。

表单项 `name` 唯一；必填字段设置 `required`，提交按钮设置 `name` 和 `form_action_type: "submit"`，服务端从 `event.action.form_value` 读取。目录校验由远程主机执行：存在、可访问、与所选机器一致；列表过长用分页卡片，不做全文件树。提交后显示“正在创建”；若群已建成但会话失败，显示已有群链接及“继续创建会话”，不要重复建群。[表单](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/form-container)、[单选下拉](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/interactive-components/single-select-dropdown-menu)、[输入框](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/interactive-components/input)

## 移动端、回调与实现边界

- 默认单列，主操作 `size: "large"`（官方说明移动端 48px）、`width: "fill"`；首屏最多两个动作。若使用 `column_set`，`flex_mode: "stretch"` 在窄屏变为上下堆叠。不要把关键按钮只放悬停提示；禁用原因同时写在卡片文本里。长页脚可分两行，日志默认折叠。[按钮](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/interactive-components/button)、[分栏](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/column-set)
- 回调须在 3 秒内返回；先用 `toast` 和卡片更新表示“已收到，正在处理”，耗时工作异步完成后更新。Toast 之外保留持久状态与错误原因。按钮禁用只改善反馈，不能保证去重：服务端按事件和动作令牌幂等处理，并核验操作者、会话、队列项版本；旧卡返回“状态已更新，请查看最新卡片”。重复点击返回同一处理结果。[回调结构与响应](https://open.feishu.cn/document/feishu-cards/card-callback-communication)
- 停止和移除的确认需针对原生能力设计：按钮文档注明 `confirm` 仅对带提交属性的按钮触发，因此普通 callback 不直接假设支持确认弹窗；可使用独立确认卡。输入框 `auto_resize` 仅 PC 生效，手机按固定行数设计。[按钮](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/interactive-components/button)、[输入框](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/interactive-components/input)
- React/Radix、Tailwind、Streamdown、浏览器剪贴板、StickToBottom、拖拽和悬停动画不能直接装进飞书卡片。这里只迁移信息层次和状态语义；复制、代码折叠、数学公式等能力应先核验飞书原生支持，再提供操作。[Message](https://elements.ai-sdk.dev/components/message)、[Conversation](https://elements.ai-sdk.dev/components/conversation)、[ActionBar](https://www.assistant-ui.com/docs/api-reference/primitives/action-bar)
- 目录 mute 使用“禁止该目录的新会话自动建群”，显示目录范围及当前值；通知设置使用“关闭 AI 回复的 @ 提醒”，仅取消对配置用户的 @ 提醒，保留消息同步。群聊默认仍需用户 @bot 才响应，这两个设置不改变该触发条件。卡片视觉和布局沿用上述原生设计。

## 验收范围

上线前用飞书真实客户端检验手机布局、按钮禁用、长路径/代码、表单提交与回调更新。需覆盖重复点击、多人同时操作、过期卡片、轮次结束时追加、接收结果未知、断线重连，以及建群成功但建会话失败；以最终后端状态决定卡片文案。当前交付只验证文档字段和 JSON 语法，没有验证原生渲染或真实 Codex 操作。

来源取证说明：assistant-ui / AI Elements 使用官方组件文档；飞书文档页面的浏览提取为空时，读取同一官方站公开 `document_portal/v1/document/get_detail?fullPath=...` 返回的文档正文，引用仍指向正式文档页面。没有使用第三方教程或访问用户消息。
