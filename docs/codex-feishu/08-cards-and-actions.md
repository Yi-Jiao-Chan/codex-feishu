# 08 原生卡片与动作

负责 `internal/cards/`、卡片 JSON 样例和移动端验收图。依赖领域 DTO、02 的动作收据、03 的发送接口。

## 当前执行进度：原生 CoT

新活动轮次改用独立的 `message_cot`，采用用户已确认的原生预览效果。整段折叠、工具组、停止入口与字体由飞书客户端呈现；下文 TaskView 的布局、custom_icon 和动作令牌规则继续适用于已存在的旧 Card JSON 过程卡，不能用于描述新 CoT。创建、人类正文、最终答复、队列及审批卡仍为 Card JSON 2.0。

只同步公开 commentary/partial_answer 和已有安全工具摘要；工具参数与输出不透传，隐藏 reasoning 不读取。稳定 ID、确认后增量和完成收口由原投递日志驱动，具体规则见 [05 同步](05-message-sync.md#新执行进度使用原生-cot)与 [02 存储](02-store-and-recovery.md#schema-31原生-cot-投递状态)。原生停止发送普通 `/stop`，沿操作者和绑定校验接入原停止 actor，并在命令入账时冻结轮次。群聊原生入口受平台策略影响，仍支持 `/stop`、`/continue`；旧卡片 token/revision 回调继续有效。已确认本人私聊合成预览的客户端样式，生产持续更新、任务群入口、真实停止与手机效果仍需分别记录，不能自动记为通过。

CoT 停止不另发成功收据：有效的停止意图与命令去重收据仍在同一事务保存，收据仅在本地结束，不创建飞书回执。无法核验目标或有未决操作时继续显示说明。已有已确认远程 CoT 的轮次中断且没有最终公开正文时，由 CoT 显示任务中断，不再另发“本轮已停止”兜底卡；真实最终正文、失败和旧 Card JSON 轮次保持既有结果投递。

2026-10-23 重新读取的[官方接入参考](https://bytedance.larkoffice.com/wiki/ZfyXwOQImiLURtkRPtWcf2B2nNh)仍规定停止图标由客户端发送单聊 `/stop` 或群聊 `@bot /stop`，没有已公开的停止回调或隐藏该用户消息参数。`cot_hidden` 只控制完成后隐藏 CoT；它不隐藏停止命令。本实现不撤回用户消息，也不以独立卡片替换原生停止入口。

## 设计依据

借鉴 [assistant-ui QueueItem](https://www.assistant-ui.com/docs/api-reference/primitives/queue-item) 的 Steer / Remove，以及 [AI Elements Task](https://elements.ai-sdk.dev/components/task) 和 [Message](https://elements.ai-sdk.dev/components/message) 的正文、状态与详情分层。实现使用 [飞书 Card JSON 2.0](https://open.feishu.cn/document/feishu-cards/card-json-v2-structure)，不使用 code-anywhere 卡片或 Web React 组件。

本轮紧凑样式、cc-connect 聚合机制和 reaction 约束见[补充研究](../research/progress-cards-reactions-2026-10-10.md)。详细原生组件约束见[卡片研究](../research/codex-feishu-card-ux-2026-10-10.md)。公开组件能展示一个状态，不代表后端已经确认该状态。

执行卡同时保留全宽正文和整段过程折叠。存在可用停止按钮时，顶行显示停止图标和简短状态，其下使用独立的全宽“执行中”面板；收起过程仍可停止。无可用停止按钮时，状态直接作为过程面板标题，终态显示状态和耗时并默认收起，非终态默认展开。没有过程记录或省略提示时只显示状态，不生成空面板。工具组仍可单独展开，组内每次调用直接显示一行具体动作，没有单项折叠。每条公开进度只显示一次，最终正文独立发送。提醒对象放在最终正文开头，普通首段同行，不在来源之后另占一行。本地测试不代替新版飞书客户端验收。

## 视图与布局契约

| 视图模型 | 从上到下 | 操作 |
| --- | --- | --- |
| TaskView | 自适应宽度、可折叠的全宽过程面板；可用停止图标与状态留在面板外顶行；终态默认收起并保留耗时，工具组可独立折叠、单次调用直接列出 | 运行：停止当前轮；停止且有队列：继续执行；待审批或问答：按已启用能力提示处理位置，仍可停止 |
| InteractionView（显式开启的扩展） | 正常字号审批事项/问题、小字状态；命令代码块、完整范围/逐文件 diff 可折叠；紧凑表单与自适应输入；状态变更复用同一张卡 | 允许/拒绝按原请求；权限三个按钮同行；问答一次完整提交；未知/敏感/截断无提交按钮 |
| QueuedInputView | 小字排队状态与真实序号；不重复人类正文，无来源脚注 | 横排小按钮：追加引导 / 移除 |
| HumanMirrorView | 人类正文、底部灰色来源引用区 | 不添加 AI 操作或伪造人名 |
| AIReplyView | 策略允许的 @ 在首段开头、最终公开正文、最底部小字 AI 来源引用区 | 过程在 TaskView 更新；最终结果独立发送 |
| CreationDraftView | 任务、唯一目录摘要、跟进位置、提交；完整路径和主机在摘要中展开 | 主：创建任务；次：取消 / 更改目录 |
| NotificationView | 当前是否会 @ 的结论、暂停结束等短摘要、默认折叠的完整规则与范围 | 保留现有 /notify 命令，不新增快捷按钮 |

全局：不使用彩色 header。执行卡取消撑满聊天窗口的 `fill`，改为 `config.width_mode="default"`；任务主创建卡及结果摘要从 `compact` 放宽为 `default`，目录选择和处理中短卡仍为 `compact`。其余短状态、操作卡与正文卡保持原宽度策略。`default` 在 PC 宽版/iPad 的上限为 600px，`compact` 为 400px，`fill` 随屏幕铺满，见[官方卡片结构](https://open.feishu.cn/document/feishu-cards/card-json-v2-structure)。这些是宽度模式，不是任意像素宽度或随内容精确测量；折叠面板的 `auto_when_fold` 只保证标题收起时适应文字，不能据此宣称整卡边界同步收缩。新宽度仍待真实客户端预览，本地服务当前版本不据此改变。body.padding="12px 12px"，vertical_spacing="8px"。正文 normal（14px）、状态与来源 notation（12px）；来源用底部 Markdown 引用区。首屏最多两个主要动作；队列操作横排 small / text，长路径完整值放详情，不把省略文本回传执行。没有实际计划或百分比时不画进度条。

2026-10-14 正文排版：在安全过滤中保留原文的行首引用标记，引用显示原生竖线；字面转义和代码中的引用字符保持原语义，所有原生标签仍过滤。自然段和简单、显式连续编号的平面列表按 native Markdown 块排版，块间 8px；自动编号、嵌套和多段列表、引用及代码保持完整。正文最多 64 块，超出时合并尾部，不省略文本。绝对文件链接的行号补到可见标签（如 `对应代码 · L1592`），不改变链接目标，不读取文件；字面代码、图片和普通网页链接不装饰。实际发送的完整 JSON 字节决定分片，Raw 仍逐字节保存原文。

同日真实客户端截图修正：飞书会把普通引用的 Markdown 段落拼成一行。普通单层引用改为一个引用块内的明确换行，标题、正文、链接按原有非空文本行显示；空引用行不另占空白显示行，避免双换行撑高卡片。换行标签仅由渲染器在过滤原文后插入，输入中的原生标签继续转义。引用内代码、多层引用、列表等复杂结构不按行拼接。真实读回必须核对非空行数和每行内容，不能把正文被拼接当成无害的格式规范化；完整客户端验收仍单独记录。

回复关系使用飞书真实 Reply API；过程 / 结果引用触发本轮的输入，操作卡引用对应补充输入，不用卡片内复制正文伪装引用。

2026-10-15 附件排队实测修正：同一批附件只保留一张管理/状态卡，收取结束后复用为紧凑 QueuedInputView，不同时展示“附件已收取/已进入原生队列”摘要和另一张排队卡。图片、附件名称和原始消息 ID 不在排队操作卡重复罗列。原草稿卡已投递时保留首次附件的引用；实际过程和最终结果仍引用封批说明或卡片发送对应的提交来源。草稿和下载失败保留附件管理入口。

异步问题的提交按钮明确标为“提交回答并追加”，并说明忙碌时追加当前任务、空闲时继续会话；同步问题仍使用“提交回答”。该动作携带 `append_answer` 决定，旧的普通提交回调保持排队。

异步问答的回答收据也复用原问题卡：同卡更新排队序号、进入会话或移除状态，并保留“追加引导 / 移除”操作。断线、结果 unknown 或卡片截断时不提供原生队列操作；实际答案经历史核实后显示“已回答”。升级前已有的独立排队卡继续使用原投递日志，避免替换未知投递。此改动的真实双端展示仍待验收。

执行入口标题、公开进度正文、工具组标题及逐调用动作统一采用 notation（12px），元信息使用灰色。面板间距 4px。每条公开记录保留一个完整正文 Markdown，已有接收时间按配置时区显示 HH:MM，不显示秒，并放在正文末尾；普通文字包括换行、多段和已闭合的行内代码，时间直接接在最后一句后，末尾空换行仅在展示副本中去掉，不再因此新增时间组件。代码块、引用、末尾列表/标题、表格和未闭合行内代码保留独立末尾时间，避免破坏原 Markdown 或被代码吞掉。缺失时间不伪造。工具计数是默认收起的组入口，展开后直接列出按原生顺序排列的具体动作，每次调用只是一行灰色文本与动作图标，没有单项箭头、折叠或详情区；公开正文不因此再次折叠。顶行状态使用 Markdown / notation，过程标题使用 plain_text / notation，均沿用正文默认颜色；工具组及时间继续灰色，不重复动作缩略，完整 Markdown 不变。不使用未经核实的 `lines`。过程及工具组面板采用 `header.width="auto_when_fold"`、`icon_position="follow_text"`，箭头跟随标题；width 字段要求客户端 ≥7.32，见[官方折叠面板](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/collapsible-panel)。字号字段依据见[字号与折叠研究](../research/feishu-progress-typography-2026-10-13.md)。这替代了此前每条长记录再次折叠的布局。

停止按钮位于状态顶行最前面，使用 auto 图标列、4px 间距，以及带 `stop_outlined` 图标的 tiny / text 按钮，省略文字以减少占宽，`hover_tips` 提示“停止当前轮”。右侧 weighted 列只包含状态 Markdown，过程面板是顶行分栏的根级兄弟，正文和工具组直接放在该面板中，不嵌在停止按钮旁边的列中；不固定图标列宽，也不使用负 margin。pending、unknown、verifying 或终态不显示停止操作；等待审批或问答不改变既有的停止权限。停止且确认有队列时继续保留原“继续执行队列”操作。

普通执行行的“悬停才显示停止按钮”尚无已核实的原生实现：[button](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/interactive-components/button) 的 `hover_tips` 仅为提示；`pc_display_rule="on_hover"` 只在 [checker.button_area](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/interactive-components/checker) 公开，带有复选框和可勾选语义，不能把它当作普通执行行。当前不向普通按钮或分栏添加未公开的显示字段。折叠标题区也没有公开按钮槽；因此保留独立状态与停止顶行，下方增加全宽“执行中”折叠入口；终态没有停止按钮时只保留状态折叠入口，不把整段历史放入图标右列。

记录时间表示桥接器首次完整快照记录该公开 item 的接收时间。执行记录保留 HH:MM 展示，schema 22 将 item 时钟与过程卡同事务持久化。原生 Item 没有发生时间字段，不能使用 turn 开始时间冒充。首次补同步、旧卡片缺少时钟或时钟为零时，以本次成功完整同步的接收时间兜底，只补一次，刷新和重启不重新计时；这是接收时间，不代表 Codex 生成时间。历史缺失值若能从已有卡片投递快照恢复接收时间，可优先回填该证据。逐条展示同时受完整 JSON 字节上限及最多 32 条记录限制，省略旧条目时明确提示，时间元数据仍保留。

来源引用区采用原生 notation / grey 文本，明确“人类发送”或“AI 回复”和真实来源，置于正文和详情的最下方；仅在有相应元数据时显示时间。长来源可以换行，不依赖 hover。模板对正文中的原生标签作安全编码，不让正文生成可执行按钮或 @。

`WithMentions` 只前置经过校验的提醒对象，去重并保留现有通知规则。普通正文与 @ 同行；标题、列表保持其 Markdown 结构；代码块或表格等必须从行首开始的内容使用独立前置段，避免破坏代码。分片预算预留这个分隔符以及请求标记的真实编码长度，只有首个最终结果分片提醒。

`TaskView.Progress` 按完整、去重的 native 历史快照投影公开 commentary / partial_answer 和相邻工具 item 的分组，终态继续保留；不包含隐藏推理、人类输入或最终正文。分组只是历史顺序的展示规则，不代表原生任务阶段或 commentary 与工具的因果关联。工具状态分别统计执行中、完成、失败、拒绝和未知；普通组标题为“调用 N 个工具”，展开后可见每次调用，重复名称的不同 native ID 不能合并。数量取原生 inProgress / completed / failed 的条数，表示已开始的调用尝试，不是成功次数；进行中、失败、拒绝和未知分别附注，拒绝与未知不计入调用次数。只有拒绝或未知时仅显示对应状态，不虚报调用。每项的动作使用原生分类、安全工具名称和文件 basename，支持读取、写入、编辑、删除等动作；同一原生调用涉及多个动作或文件仍只算一次。文件名优先取 `commandActions` 的 name/path 或 `fileChange.changes` 的 path/kind；命令调用同时在本地对已有 `Command` 作有界、纯词法识别，概括查看 Git 状态、运行 Go 测试、推送提交等具体用途。受控 shell 包装与常见串联可识别，最多合并三个用途；行内脚本只说明脚本类型，不猜测内部业务。无法识别时明确说明用途未识别。该过程不执行命令、不读目标文件、不发额外 RPC 或模型请求；摘要不展示完整目录、搜索 query、凭据、参数、原始命令、输出或 diff。MCP/动态工具没有文件元信息时保留安全工具名，不猜测文件。无逐调用元信息的旧 DTO 仅显示已有计数，不凭计数伪造明细。工具组标题目标为用户截图同款的灰色线框工具箱与“调用 N 个工具”，保留原生 follow_text chevron 和折叠行为，不另设图标列。此前 `⛭` 齿轮及 Markdown 图片标题方案均被用户否决，停止采用。正式渲染沿用本人私聊截图已确认的完整 Markdown 前缀结构：工具箱位于 `header.title.icon`，原生折叠箭头位于 `header.icon`，两者独立。组内明细使用同样的前缀图标方式；没有 `![图片]` 大图块。运行态折叠入口改为“执行中”，终态继续显示真实状态与耗时。工具明细及省略提示直接放在面板中，padding/margin 为 0。组内调用仍是带[原生 Markdown icon](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/content-components/rich-text)的文本，标题前缀能力已有服务端与本人私聊客户端两份独立证据。不另调用模型生成摘要，不依赖无法从历史补回的 MCP 实时进度消息。

2026-10-22 真实客户端截图已证明原负 margin 方案会裁切文字，停止使用跨列回拉。其后本人私聊的全宽合成卡得到用户确认，但删除整段过程折叠不符合原需求；当前恢复根级全宽面板；标题前缀工具箱和较小明细图标已获本人私聊截图确认，用户要求将运行态入口命名为“执行中”。该确认覆盖预览的桌面展示，真实停止、连续更新及手机仍单独验收。

只用有效 native 秒级起止时间计算整轮耗时；未知则省略。工具组不生成 item 时间，公开文本的接收时间语义保持。卡片按实际 JSON 字节预算保留最新记录，公开文本与工具组共同受最多 32 条记录限制，省略条数和单条截短必须写明。每组投影最多保留最新 32 次调用，其余记录为 OmittedItems；状态计数仍涵盖整个组。renderer 先将正文末尾时间及各组计数/省略说明计入真实组件预算；基础布局超限时明确省略最老记录，再将剩余预算分给最新调用；包含标题与图标后，执行历史最多 169 个 tag，为过程面板、状态顶行和操作留余量，全卡不超过 180，低于平台 200 上限。单项只显示最多 120 字符的安全动作摘要。真实 JSON 字节超限时先裁旧记录；只剩一个工具组时再裁该组最老调用详情并增加省略数，计数不变，摘要仍放不下才省略整组。

过程面板保留稳定 `execution_history` ID，工具组的 `element_id` 根据原生 ID 保持稳定且不超过 20 字符，单次调用没有折叠状态或预设面板 ID。请求标记优先使用空闲顶层 Markdown；无顶层位置时仅递归原生折叠面板和 `column_set → column → elements` 的正文，不遍历任意元数据，不替换面板 ID、不增加可见空行，保持原有全卡读回校验。实际 PATCH 后用户的展开状态是否保留，仍需真实客户端确认。

官方标题字段简表未完整列出可用样式；[第一方标题实现](https://github.com/larksuite/openclaw-lark/blob/main/src/card/builder.ts#L735-L757)也使用 text_size、text_color 等扩展字段。不能仅凭简表缺少 icon 就断定标题不支持，也不能仅凭 API 接受宣称 UI 通过；当前方案已有实际服务端转换与用户认可截图，正式卡持续刷新与动作仍分别验收。

图标尺寸与文字字号分别处理：[Markdown 图标字段](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/content-components/rich-text)只公开 tag/token/color/img_key，没有独立 size，不能把面板箭头的 size 字段照搬过来。notation 正文不会自动缩小图标。执行卡用 custom_icon 图片的透明留白缩小明细可见图形，平台控制的图标外框没有改动；不宣称最终精确为 12px，也不把 API 接受当作视觉验收。

图标键通过当前部署的可选私有 sidecar 提供：规范化的数据库路径后追加 `.task-icons.json`。格式包含 `version:1`、当前 `app_id` 和 `icons` 的六个固定角色：`toolbox`（组标题）、`command`、`read`、`edit`、`search`、`generic`（未分类调用）。键只接受 `img_` 前缀、最多 512 字符的字母数字及 `_`/`-`，不能填写 URL。文件和父目录分别为当前用户所有的 0600/0700，拒绝 symlink、未知字段、跨 App、超限或不完整配置；整份无效时采用原生图标并记录固定安全 notice，文件缺失时静默采用原生图标。服务启动时只读加载一次，原生重连沿用内存值，Render 不读文件也不上传；修改文件后需重启桥服务生效，不重启共享 Codex daemon。已上传键不进入 Git，资产来自固定版本的 [Lucide SVG 与许可证](../../internal/cards/assets/task-icons/LICENSE)。

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

任务停止沿用已有领取与原轮次锁定链路。校验已确认投递的卡片时，同时识别顶层按钮和原生 `column_set → column → elements` 中的按钮，必须唯一匹配 action / token / revision；普通文本或任意 JSON 字段中的类似 payload 不构成操作授权。旧的顶层继续按钮保持兼容。文件动作的 path/kind 变更参与任务控制 revision 摘要，diff 不参与；原卡已投递版本的校验保持。

流程：原始 callback → 身份与令牌验证 → ClaimAction 短事务 → 3 秒内 pending toast → 异步用例 → 更新原卡。动作领取失败或数据库失败时不进入异步用例。[回调规范](https://open.feishu.cn/document/feishu-cards/card-callback-communication)

## 创建表单与确认

工作方式采用同一 form 内的“当前目录 / 新建工作树”描边按钮，切换时一并保存正文和跟进选项；只有工作树模式展示基准分支下拉，并明确使用已提交内容。最终创建仍是唯一实心主要操作；分支不可用时保留草稿和改回当前目录的入口。工作方式回调沿用 creator、实际卡片、token/revision 与原卡有序等待反馈，不在回调接收器直接创建工作树。

当前目录不是已有提交的 Git 仓库，或整个仓库超出创建授权范围时，主卡提前禁用“新建工作树”并就近显示具体原因；“当前目录”和更改目录入口仍可用。更改目录后按新目录重新核验，卡片状态不替代后端执行检查。

原生 form 只放顶层，不嵌套在 column_set / collapsible_panel；name 唯一、必填明确。提交值从 event.action.form_value 读取。目录只有一个 selected_directory。用户选定浏览优先：主卡的“更改目录”保存表单并进入目录视图，顶部单一名称/路径查找，form 外下拉选子目录即进入，primary_filled / medium“使用当前目录”返回草稿。最近与项目作为辅助入口；不在主卡堆叠三套目录输入。创建操作使用 medium / default 宽度的描边按钮与实心主要确认，均放入 none / auto 分栏以避免原生纵向布局拉满；表单显式 8px 间距。默认显示目录名和短父路径，点击父路径展开完整路径和主机；次要说明采用 notation / grey。正文框默认两行，独立目录标记与主机 ID 不常驻主界面。不沿用队列卡的 small / text。

停止/移除是否需确认按动作损失决定；如使用确认，普通 callback 不假定支持 submit 才适用的 confirm 弹窗，使用独立确认卡。引导需要立即反馈，不额外增加一次无必要确认。

## 开发切片与验收

先完成 Task/Human/AI/Queue 四种一期模板和动作，再加 Notification 和二期 Creation。JSON 样例只做协议形状测试，真实 Feishu 渲染是独立门槛。

1. 桌面与手机首屏看得到正文、状态和引导按钮；长路径、长代码、长页脚不遮挡操作。
2. 生成中 → 完成、排队 → 追加、unknown → 核验结果均更新同卡，不闪回旧状态。
3. 非授权人、token 篡改、过期卡片和重复点击没有副作用。
4. 同步与异步问答默认可在飞书填写；审批按 `interactions.enabled_types` 逐项开启。未启用、敏感或卡片展示不完整的请求没有按钮；未验收的审批类型保持回 App 提示。合成预览 token 无效，不能替代真实验收，见[13 审批与问答](13-feishu-approvals-and-questions.md)。
5. 表单提交只产生一个有效草稿/创建意图；选择其他目录回到原草稿。

起步样例：[队列卡](examples/queue-card.example.json)。按钮令牌是无效占位值。旧 M0 / P1 的桌面、手机与长代码已经历实际联调；正文优先的新布局与刷新期间折叠状态应单独验收，不能复用旧样式结论。

## 2026-10-23 原生过程消息调研与能力边界校正

前文关于停止按钮、标题按钮槽和整段折叠的限制，只描述当前普通 Card JSON 方案及已核实字段。它们不能推出“飞书原生能力不支持同排停止和全宽展开”，也不能推出该目标必须采用回调式共享消息重绘。此前据此作出的能力判断过强；以下发现补充并校正该边界，既有 renderer 和已部署行为尚未因此改变。

[飞书 OpenAPI COT 接入参考](https://bytedance.larkoffice.com/wiki/ZfyXwOQImiLURtkRPtWcf2B2nNh)明确区分独立的原生 `message_cot` 消息能力与普通卡片：基础能力已全量开放，无需单独白名单；个别明确标注灰度的能力另行申请。其生命周期为 `POST /open-apis/im/v1/message_cot` 创建、`PUT` 同一路径追加事件、`POST /open-apis/im/v1/message_cot/complete/{cot_id}` 完成；保存并使用返回的 `message_id`、`cot_id`，最终正文仍走正常回复链路。自定义运行态/终态文案 `content.input.statusText` 单独标为灰度，不能将基础能力全量扩大为该字段也已对当前应用开放。[AG-UI 事件说明](https://bytedance.larkoffice.com/wiki/UDxqwqxBnitkFHkYc1WcVC6InDg)给出原生工具过程事件，但其摘要字段的实际展示优先级仍待预览确认。过程内容仅使用可公开、脱敏的进度摘要，不透传模型隐藏推理、凭据或真实工具输入输出。

Mew 已发布的[个人设置脚本](https://cdn-tos.bytedance.net/codebase/x/assets/settings-BH6_Dwal.js)与[共享进度设置模块](https://cdn-tos.bytedance.net/codebase/x/assets/lark-agent-progress-EUxImoms.js)提供另一条直接证据：个人设置默认解析为 `Transport="message_cot"`；Labs 的“透明进度卡片”开关设为 `card_summary`，关闭恢复 `message_cot`，且该开关实际绑定“需使用飞书桌面测试版”说明。模块调用 `GetLarkAgentProgressPreference` / `UpdateLarkAgentProgressPreference`，并独立处理显示内容、敏感信息过滤和完成后隐藏。这证明已发布前端区分两种 transport，不是机器人后端源码证据。另一个可读仓库在[固定提交的 createCot / appendCot 实现](https://code.byted.org/CIS/aifee-enterprise-agent/blob/fdd93f0e260a552c9a7542f0cc836403a61ba54d/apps/aifee-agent-server/src/channels/feishu/feishu-api.ts#L186)中实际构造上述创建和追加请求；它不证明 Axon、TRAE CLI 2.0 Oncall 或 Mew 使用同一后端实现。

原生 COT 停止入口发送普通消息命令：单聊 `/stop`，群聊为 `@bot /stop`，不沿用 Card JSON 的 token/revision 回调。接入参考同时注明 7.73 起群聊默认隐藏停止入口，可按用户、群或租户加白；不能因单聊可见而宣称任务群已可用。若后续接入，必须在普通输入投递前拦截命令、鉴权并定位对应运行轮次；完成原任务停止后补齐中断事件及 CompleteCOT，不能仅改变显示状态。当前桥仍使用前文已实现的卡片动作授权，未接入该原生命令路径。

普通卡片的 `collapsible_panel.header.button_area` 仍没有已确认契约。本次仅在本人私聊对 `pc_display_rule="always"` 的这一候选组合尝试一次发送，得到 HTTP 400 / `code=230099`，无 MessageID，未重试。该结果只否定这次具体载荷，不能排除其他组合或独立 `message_cot` 能力。原生 COT 的本人私聊运行态合成预览、安全读回、持续更新、实际停止和桌面/手机 UI 均待独立验收；不将文档、静态源码或第三方调用实现记为当前应用通过。后续证据见[本轮记录](../records/task-progress-deployment-2026-10-22.md#2026-10-23-message_cot-原生能力调研与既有结论校正)。
