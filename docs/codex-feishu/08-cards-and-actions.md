# 08 原生卡片与动作

负责 `internal/cards/`、卡片 JSON 样例和移动端验收图。依赖领域 DTO、02 的动作收据、03 的发送接口。

## 设计依据

借鉴 [assistant-ui QueueItem](https://www.assistant-ui.com/docs/api-reference/primitives/queue-item) 的 Steer / Remove，以及 [AI Elements Task](https://elements.ai-sdk.dev/components/task) 和 [Message](https://elements.ai-sdk.dev/components/message) 的正文、状态与详情分层。实现使用 [飞书 Card JSON 2.0](https://open.feishu.cn/document/feishu-cards/card-json-v2-structure)，不使用 code-anywhere 卡片或 Web React 组件。

本轮紧凑样式、cc-connect 聚合机制和 reaction 约束见[补充研究](../research/progress-cards-reactions-2026-10-10.md)。详细原生组件约束见[卡片研究](../research/codex-feishu-card-ux-2026-10-10.md)。公开组件能展示一个状态，不代表后端已经确认该状态。

2026-10-13 用户选定“正文优先”：运行中突出一句最新公开动作，执行记录默认折叠；最新动作只在上方正文显示，不在下方历史重复，出现下一条动作后才成为可展开历史；完成后所有公开记录（包括最后一条）都保留在可展开历史中，最终正文独立发送。提醒对象放在最终正文开头，普通首段同行，不再在来源之后另占一行。本轮实现与本地测试不代替新版飞书客户端验收。

## 视图与布局契约

| 视图模型 | 从上到下 | 操作 |
| --- | --- | --- |
| TaskView | 小字状态、正常字号最新公开动作、逐条折叠公开记录；完成后保留单行折叠入口与同样的逐条展开 | 运行：停止当前轮；停止且有队列：继续执行；待审批：回 App |
| QueuedInputView | 小字排队状态与真实序号；不重复人类正文，无来源脚注 | 横排小按钮：追加引导 / 移除 |
| HumanMirrorView | 人类正文、底部灰色来源引用区 | 不添加 AI 操作或伪造人名 |
| AIReplyView | 策略允许的 @ 在首段开头、最终公开正文、最底部小字 AI 来源引用区 | 过程在 TaskView 更新；最终结果独立发送 |
| CreationDraftView | 主机、唯一已选目录、任务内容、跟进模式、目标、提交 | 主：创建任务；次：取消 / 选择其他目录 |
| NotificationView | 当前是否会 @ 的结论、暂停结束等短摘要、默认折叠的完整规则与范围 | 保留现有 /notify 命令，不新增快捷按钮 |

全局：不使用彩色 header，短状态和操作卡采用 compact 宽度，正文卡不强制 compact；body.padding="12px 12px"，vertical_spacing="8px"。正文 normal（14px）、状态与来源 notation（12px）；来源用底部 Markdown 引用区。首屏最多两个主要动作；队列操作横排 small / text，长路径完整值放详情，不把省略文本回传执行。没有实际计划或百分比时不画进度条。

2026-10-14 正文排版：在安全过滤中保留原文的行首引用标记，引用显示原生竖线；字面转义和代码中的引用字符保持原语义，所有原生标签仍过滤。自然段和简单、显式连续编号的平面列表按 native Markdown 块排版，块间 8px；自动编号、嵌套和多段列表、引用及代码保持完整。正文最多 64 块，超出时合并尾部，不省略文本。绝对文件链接的行号补到可见标签（如 `对应代码 · L1592`），不改变链接目标，不读取文件；字面代码、图片和普通网页链接不装饰。实际发送的完整 JSON 字节决定分片，Raw 仍逐字节保存原文。

同日真实客户端截图修正：飞书会把普通引用的 Markdown 段落拼成一行。普通单层引用改为一个引用块内的明确换行，标题、正文、链接按原有非空文本行显示；空引用行不另占空白显示行，避免双换行撑高卡片。换行标签仅由渲染器在过滤原文后插入，输入中的原生标签继续转义。引用内代码、多层引用、列表等复杂结构不按行拼接。真实读回必须核对非空行数和每行内容，不能把正文被拼接当成无害的格式规范化；完整客户端验收仍单独记录。

回复关系使用飞书真实 Reply API；过程 / 结果引用触发本轮的输入，操作卡引用对应补充输入，不用卡片内复制正文伪装引用。

2026-10-13 实际截图修正：外层和逐条记录的 `header.title` 显式使用 notation / grey，面板间距 4px，不再用一个 Markdown 和空行拼接所有记录，也不展示工具计数。每条记录默认只显示无换行的短摘要与时间，独立展开查看原文；运行中、完成、失败和停止都保留这个行为。标题字段不使用未经核实的 `lines`，摘要按短显示宽度截短；极窄窗口或放大字体仍可能折行。组件字段依据见[字号与折叠研究](../research/feishu-progress-typography-2026-10-13.md)。

记录时间表示桥接器首次完整快照接收该公开 item 的时间，按配置时区显示，schema 22 将 item 时钟与过程卡同事务持久化。原生 Item 没有发生时间字段，不能使用 turn 开始时间冒充；已有记录没有可恢复时间时显示“时间未记录”。逐条展示同时受完整 JSON 字节上限及最多 32 条记录限制，省略旧条目时明确提示，时间元数据仍保留。

来源引用区采用原生 notation / grey 文本，明确“人类发送”或“AI 回复”和真实来源，置于正文和详情的最下方；仅在有相应元数据时显示时间。长来源可以换行，不依赖 hover。模板对正文中的原生标签作安全编码，不让正文生成可执行按钮或 @。

`WithMentions` 只前置经过校验的提醒对象，去重并保留现有通知规则。普通正文与 @ 同行；标题、列表保持其 Markdown 结构；代码块或表格等必须从行首开始的内容使用独立前置段，避免破坏代码。分片预算预留这个分隔符以及请求标记的真实编码长度，只有首个最终结果分片提醒。

`TaskView.Progress` 按完整、去重的 native 历史快照投影公开 commentary / partial_answer，终态继续保留；不包含隐藏推理、人类输入或最终正文。折叠详情保留真实工具计数，无逐工具字段时不编造时间线。只用有效 native 秒级起止时间计算耗时；未知则省略。卡片按实际 JSON 字节预算保留最新记录，省略条数和单条截短必须写明。

过程面板 `element_id` 保持稳定。请求标记优先使用空闲顶层 Markdown；只剩折叠面板时标记其正文，不替换面板 ID、不增加可见空行，保持原有全卡读回校验。实际 PATCH 后用户的展开状态是否保留，仍需真实客户端确认。

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

起步样例：[队列卡](examples/queue-card.example.json)。按钮令牌是无效占位值。旧 M0 / P1 的桌面、手机与长代码已经历实际联调；正文优先的新布局与刷新期间折叠状态应单独验收，不能复用旧样式结论。
