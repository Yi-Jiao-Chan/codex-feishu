# 飞书卡片承载审批与用户问答

核验日期：2026-10-14。范围仅为飞书 Card JSON 2.0、公开回调协议与官方 Go SDK；不推断 Codex 是否支持某类审批或响应值。已读仓库 AGENTS.md、README 与 08 模块手册。仅做公开文档/源码读取，未发送消息、修改运行服务或执行远程 mutation；以下方案未通过真实客户端验收。

## 已核实的能力与限制

| 需求 | 一方证据与实现边界 |
| --- | --- |
| 一次提交多项回答 | 顶层 `form` 在客户端本地缓存输入，点击提交才一次回传；不可嵌在分栏/折叠面板等组件内，不可内嵌另一个 form 或 table。表单至少包含一个提交按钮，各交互项与按钮 `name` 必填并唯一。[表单容器](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/form-container) |
| 文本回答 | `input_type` 支持 `text`、`multiline_text`、`password`（不能误用样式字段 `type`）；`max_length` 只能为 1–1,000（默认 1,000）；多行换行回传为 `\n`。固定行数可用 `rows: 3`、`auto_resize: false`。2026-10-14 紧凑预览改用 `rows: 1`、`auto_resize: true`、`max_rows: 4`；自适应增高仅 PC 生效，手机需实际验证。与提交按钮组合须放 form 内。[输入框](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/interactive-components/input) |
| 单选 | `select_static`，表单提交值为所选 `options.value` 字符串；在 form 外选择会独立回调 `action.option`。问答放 form 内统一提交，不把选项变化当成回答完成；不设置默认答案。[单选下拉](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/interactive-components/single-select-dropdown-menu)、[表单容器](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/form-container) |
| 多选 | `multi_select_static` 仅可在 form 内使用；提交值为所选 `options.value` 的字符串数组。`required` 只确保非空，不能替代后端的选项集合、数量与业务校验。[多选下拉](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/interactive-components/multi-select-dropdown-menu) |
| 原生文字 radio | 现行 JSON 2.0 官方组件总表列出单选下拉、多选下拉与 `checker`，没有文字 `radio` / `radio_group`。因此不把普通设计规范中的 Radio 当成卡片能力；当前可交付单选是下拉。`checker` 是独立布尔勾选器，没有文档化的互斥组语义，未配 `behaviors` 时仅本地有效。[组件总表](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/component-json-v2-overview)、[勾选器](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/interactive-components/checker) |
| 客户端与共享 | JSON 2.0 需要飞书 7.20+；仅支持共享卡片，`update_multi` 只能为 `true`，更新对所有收到这张卡片的人可见。每卡最多 200 个元素/组件。[组件总表](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/component-json-v2-overview)、[JSON 2.0 结构](https://open.feishu.cn/document/feishu-cards/card-json-v2-structure) |

提交按钮使用 `form_action_type: "submit"` 和 `behaviors: [{"type":"callback","value":{...}}]`；`action_type: "form_submit"` 是兼容的历史**卡片组件属性**，不是新版回调里必有的事件字段。新版表单回调通过 `event.action.tag: "button"`、`name`、`value` 与 `form_value` 识别。[表单容器](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/form-container)

## 回调与现有入口

新版 `card.action.trigger` 可以由企业自建应用的同一 SDK WebSocket 与 dispatcher 处理，官方 Go 示例直接注册 `OnP2CardActionTrigger`。无需为审批/问答建立第二条连接或新的公网回调入口；长连接为集群投递，同应用多个 client 中随机一个接收，不能假定广播。旧版 `card.action.trigger_v1` 不支持长连接。[长连接回调](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/event-subscription-guide/callback-subscription/configure-callback-request-address)

需要在 3 秒内响应。长耗时操作先完成身份/令牌校验和动作领取，返回 info toast“已收到，正在处理”，后台处理后更新卡片。官方延时更新接口必须在回调响应完成后执行，提前或并行执行可能失败；`event.token` 有效 30 分钟、最多更新两次。若依赖既有 message PATCH/outbox，则走已有版本顺序，不混用两条更新路径导致旧内容覆盖。已发送卡片可交互 30 天、可更新 14 天，应使用更短的业务有效期。[处理回调](https://open.feishu.cn/document/feishu-cards/handle-card-callbacks)、[回调结构](https://open.feishu.cn/document/feishu-cards/card-callback-communication)

以下是合成字段示意，标识全部为无效占位值；不是完整事件 fixture：

```json
{
  "header": {"event_id": "example_event", "event_type": "card.action.trigger"},
  "event": {
    "operator": {"open_id": "example_operator"},
    "host": "im_message",
    "context": {"open_message_id": "example_message", "open_chat_id": "example_chat"},
    "action": {
      "tag": "button",
      "name": "submit_answers",
      "value": {"action": "answer_request", "token": "invalid_example_token", "revision": 1},
      "form_value": {"question_a": "choice_a", "question_b": ["choice_b", "choice_c"], "question_c": "示例回答"}
    }
  }
}
```

`operator.open_id` 是实际操作人；`context.open_message_id/open_chat_id` 是消息/会话上下文。`user_id` 需相应权限，不应为此新增权限或依赖它；未提供 thread/root message 上下文时从服务端绑定查找。官方 SDK `CallBackAction.FormValue` 是 `map[string]interface{}`，保留字符串、数组等真实类型，不把整个 map 强转为 `map[string]string`。[回调结构](https://open.feishu.cn/document/feishu-cards/card-callback-communication)、[官方 Go SDK，固定 commit](https://github.com/larksuite/oapi-sdk-go/blob/c11ccc66d80b5ab438115233158555daa6bea339/event/dispatcher/callback/model.go)

实现建议：复用 08 模块的 opaque token + revision + 动作收据；token 映射到待处理请求、绑定、可操作人和有效期。校验操作人、消息、群、请求当前状态；客户端 `value` 中的请求 ID、选项或任何路径均不代替服务端记录。前端 `required` 只是体验校验，服务端仍验证名称、类型、允许值和文本长度。并发提交只领取一次；重复回调返回同一收据，过期卡不发起响应，unknown 不盲重试。此为桥接器建议，不是飞书提供的事务保证。

## 草稿、提交与可见性

官方保证的是表单**本地缓存，提交才回调**，与共享卡片**更新对所有收卡人可见**。文档未承诺未提交草稿在多人、跨设备、卡片 PATCH、消息转发之间如何隔离、保留或同步，不能把公共卡片当作持久草稿或多人协作表单。[表单容器](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/form-container)、[JSON 2.0 结构](https://open.feishu.cn/document/feishu-cards/card-json-v2-structure)

提交回调将答案交给应用，不等于自动公开答案；但若应用将 `default_value`、选中值或答案摘要写回卡片，更新就是公共可见。建议处理后仅显示“回答已接收/请求已处理”等确认状态，不回显敏感正文。公共卡片的问题、命令、路径、diff 本身也应按绑定群的可见范围处理。输入框 `password` 仅规定屏幕显示为圆点，没有承诺提交值保密；原请求若标明 `isSecret`，公共卡片只提示回 App，不渲染答案输入，也不以密码遮罩声称安全收集。授权人私聊是另一投递与隐私边界，仍需独立确认可见范围和处理方式，不能靠遮罩或 UI 禁用建立访问控制。[输入框](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/interactive-components/input)、[JSON 2.0 结构](https://open.feishu.cn/document/feishu-cards/card-json-v2-structure)

建议请求卡配置 `enable_forward: false`、`enable_forward_interaction: false`，避免转发后扩大可操作范围；这不替代后端身份与上下文校验。字段默认值分别为 true、false。[JSON 2.0 结构](https://open.feishu.cn/document/feishu-cards/card-json-v2-structure)

## 最小可行卡片

继承现有 08 契约：无彩色 header，小字 notation/grey 状态，正文 normal；`body.padding: "12px 12px"`、`vertical_spacing: "8px"`。短审批采用 compact；多问表单单列并按内容长度决定宽度，不为 compact 挤压长问题。长命令/diff 放表单外的折叠详情，完整执行值保存在服务端，正文只说明真实影响。

| 卡片 | 从上到下 | 操作与语义 |
| --- | --- | --- |
| 简单审批 | 小字“等待你确认”；申请事项与实际影响；可选折叠详情；底部小字来源 | 首屏最多两个 small 按钮“允许本次 / 拒绝”，普通 callback 即可，无额外确认弹窗。若原请求确有更多选项，用 form 中无默认值的单选下拉 + 一个“提交决定”按钮，选项名称/作用域必须来自原请求。不得凭空增添长期授权。 |
| 用户问答 | 小字“等待你回答”；各问题正常字号；每题一个选择/输入组件；必要选项说明；底部来源 | 一张顶层 form，唯一字段名，单选下拉/多选下拉/多行输入；一个“提交回答”按钮，一次提交整份答案。只有协议允许时才展示跳过。 |

“其他”用明确的选项值和独立选填输入框；服务端校验选择“其他”时文本非空，不假定卡片支持条件必填或自动隐藏。无选项的问题直接用文本输入，不造空下拉。长选项说明用独立正常文本列在选择器前，选择器用短且可辨识的标签。form 与折叠详情放在 `body.elements` 同级，不能把表单套进既有过程折叠面板。[表单容器](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/form-container)、[折叠面板](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/collapsible-panel)

超出 1,000 字符的单项回答或复杂交互继续回 App，不静默截断；如后续提供机器人单聊引用回复或分段输入，须通过原请求绑定、实际操作人和明确提交动作收集完整文本，区分补充片段与最终提交，不把普通会话消息误收为答案。此路线需单独实现与验收，现有表单能力不证明它已经可用。不要用 `form_action_type: "reset"` 表示拒绝/取消请求：它只把本地输入重置为初始值。[表单容器](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/form-container)

等待回答期间请求卡独立于过程卡，不随进度 tick PATCH，避免刷新干扰尚未提交的本地草稿；请求作废或被领取时才进行必要状态更新。领取后同卡显示“正在处理”并移除可提交控件；只在原系统确认后显示完成。问题答案与输入中的原生标签继续按既有安全渲染约束处理，不让用户正文变成按钮或 @。这一设计不更改 README 现有“审批/问答回 App”的发布状态；启用需协议能力核验与真实联调通过。

## 未确定事项与验收门槛

- 桌面与手机：表单中途 PATCH 是否保留草稿、多个操作者是否各自持有草稿、跨设备是否同步；这些行为没有找到明确公开契约，必须真实验证，不能写成已保证。
- 回调：桌面/手机各取一次真实单选、多选、文本和留空提交（使用合成数据），核对 `form_value` 的名称/类型、`action.name/value` 与身份上下文；核对选项变化是否只本地缓存。
- 多人/过期：授权与非授权操作人、转发、重复点击、两人同时提交、请求已在 App 完成、回调成功但卡片更新失败、断线恢复；验收以原请求最多一次确认与最终状态为准。
- SDK 衔接：本文查的是官方当前源码，未读取仓库网关实现或确认依赖版本的解码层是否已保留 `FormValue`；整合时检查现有 DTO/handler，避免落在已有入口却丢表单字段。

取证方法：飞书网页正文浏览提取为空，读取同一官方站公开 `document_portal/v1/document/get_detail?fullPath=...` 的正文，引用指向正式文档。未以社区文章、第三方桥接器或普通设计规范证明卡片协议能力。
