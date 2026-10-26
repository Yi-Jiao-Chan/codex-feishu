# 飞书原生 COT 内容质量调研

- 日期：2026-10-26。
- 范围：当前原生 `message_cot` 实现诊断、官方字段、开源实践、改进方案与验收目标。
- 状态：research verified sources；方案落地与验收结果另见[实施与验证记录](../records/cot-tool-details-2026-10-26.md)。下文“已核验”指核对官方协议或固定版本源码，不代表真实飞书客户端 UI 已验收。
- 模块：[codex-feishu 模块索引](../codex-feishu/README.md)。

## 结论

调研基线的杂乱主要来自桥接器的内容投影：同一长标题进入多个字段，每个结果追加同步策略说明，命令识别兜底暴露为“用途未识别”，而真正的结果摘要没有独立数据来源。现有原生 COT 通道可以继续使用，优先修改信息组织。

建议分两步：先用现有数据移除重复提示、缩短标题、补结构化状态；再基于明确的安全字段增加有依据的结果摘要。目标是让用户看出当前动作、实质进展和是否需要介入。开源实现提供了字段分工和摘要取舍的实例，但截断输出不等于脱敏，也不证明所有平台字段已获客户端支持。

## 调研基线实现与问题定位

核验基线为 `496ada2b0ca88c69dc21da6c1681cfe861881c5e`，调研前工作区干净，与本次获取的 `origin/main` 相同。仅检查源码与合成测试；没有读取真实会话、运行日志或生产数据库，也没有发送飞书预览。用户截图用于确认可见症状，本文示例均为合成内容。

### 基线数据链路

```text
原生完整历史快照
  → codexclient.Item（保留类型、状态、commandActions、工具名等）
  → ProgressToolSummary（命令的有界词法识别＋安全 basename）
  → cotDocument（公开 commentary / partial_answer＋工具 Title / Status / Icon）
  → cot.Plan（稳定 ID、确认后增量、AG-UI 事件）
  → 持久 attempt → message_cot Create / Append / Complete
```

主要代码为 [原生 DTO](../../internal/codexclient/protocol.go)、[工具投影](../../internal/sync/progress_tools.go)、[命令摘要](../../internal/sync/command_summary.go)、[CoT 投影](../../internal/sync/cot_projection.go)、[事件规划器](../../internal/cot/planner.go)、[投递账本](../../internal/store/cot.go)及[网关](../../internal/feishu/cot.go)。新轮次走原生 `message_cot`；旧 Card JSON 的折叠布局与样式参数不是这次问题的直接修复入口。[切换条件](../../internal/sync/engine.go)在 `previousCOT || NativeCOT && !hadProgress && inProgress` 时选择原生 CoT。

### 已核实的原因

| 现象 | 源码依据 | 问题与影响 |
| --- | --- | --- |
| 每次完成重复出现“仅同步状态，原始输出未同步” | `planner.go:368–377` 的 `toolResult` 固定拼接 | 这是我们主动生成的结果正文。同步策略说明占据了每条结果的位置，正常工具也显得信息缺失。 |
| 工具展开后重复标题 | `planner.go:329–360` 将同一标题写入 `toolCallName`、`title`，又放进结果 `content` | 同一动作至少重复两次；长标题中的文件、分号和兜底文案随之重复。 |
| `tail` 等命令显示“用途未识别” | `command_summary.go:312–461` 没有为 `tail` 提供具体识别；`:500–517` 的程序白名单兜底会追加该短语 | 识别器覆盖范围的限制变成面向用户的异常提示。保守不猜用途是合理的，但中性名称已经能如实表达已知信息。 |
| 一行混合读取、搜索、运行多个动作 | `command_summary.go:76–92` 最多拼三个动作，标题最长 120 个 Unicode 字符 | 为旧卡片设计的详细摘要直接成为原生工具标题。它准确保留了多个动作，但阅读成本过高，且容易被原生运行入口复用。 |
| 结果很空泛，无法解释失败 | `protocol.go:67–84` 没有建模退出码、耗时或安全结果摘要；`cot/document.go:28–35` 仅有 Title / Status / Icon 等；`cot_projection.go:40–42` 也没有带入已有 `Details` | 展示层拿到的是“做什么＋状态”，没有独立的“得到什么”。这无法靠更漂亮的字符串模板补足。 |
| 失败主要靠文字表达 | `planner.go:330–333` 只有首次观察到 failed 时给标题加失败后缀；`:358–360` 的 RESULT 没有 `status` / `isError` | 已开始的标题被冻结，后续失败只进入结果正文。传输中缺少结构化失败信息，具体客户端是否仍能正确标识需实测。 |
| “状态待核验”可能只是上游没提供状态 | `progress_tools.go:107–119` 强制将 webSearch / imageView 置为 unknown；`planner.go:305–308` 将它们投影成公开说明 | 必须区分“字段缺失”和“执行发生异常”。不能伪造成功，也不应把缺字段一律包装成需要用户处理的问题。 |
| 摘要更正可能插入额外过程段落 | `planner.go:310–354` 追加“工具动作摘要更新”；`:209–269` 对已结束或非前缀文本更正产生新一代消息 | 这是当前追加式协议下的保真措施；频繁改变标题或部署后更换文案规则会增加噪声，应先稳定展示身份与策略。 |

`TOOL_CALL_ARGS` 当前统一发送 `{}`，并非真实参数（`planner.go:338–343`）；`TOOL_CALL_END` 表示参数传完，工具是否结束由 RESULT 表达。直接删掉所有 RESULT 来消除正文重复，会破坏这一区分。

公开 commentary / partial_answer 当前经展示安全处理后保留全文，再映射为 `REASONING_*` 展开区；隐藏 reasoning 已排除。截图中的长进度说明是公开消息，不代表桥接器读取了模型隐藏推理。第一阶段应修复桥自身生成的噪声，不通过关键字规则悄悄删改公开进度。[投影代码](../../internal/sync/cot_projection.go)

失败原因的证据边界：仅凭截图不能判断某次搜索、读文件或脚本究竟为何失败。当前缺少退出码和安全错误分类，不能把它们统一改成成功，也不能推断是无匹配、路径不存在或已经自动恢复。

### 应保留的现有能力

当前实现已经有稳定 native item 身份、确认后推进 ACK、完整快照补齐、50 事件分批、按双层 JSON 编码核算大小、最终回复独立投递，以及 unknown 不盲重试等保护。它们解决的是交付正确性，与内容杂乱属于不同层面。本次建议集中修改安全投影和展示字段，不重建同步链路。[规划器](../../internal/cot/planner.go)、[存储](../../internal/store/cot.go)、[网关](../../internal/feishu/cot.go)

## 官方字段核验

2026-10-26 通过 bytedcli 只读取得[飞书接入参考](https://bytedance.larkoffice.com/wiki/ZfyXwOQImiLURtkRPtWcf2B2nNh)、[AGUI COT 事件构造参数](https://bytedance.larkoffice.com/wiki/UDxqwqxBnitkFHkYc1WcVC6InDg)和[官方 agent 接入文本](https://feishu-agui-playground.gf-boe.bytedance.net/llms.txt)。以下是文档支持的能力，不代表本应用所有客户端已经验收。

| 信息 | 已核实字段或行为 | 对本项目的意义 |
| --- | --- | --- |
| 工具身份与标题 | START 的 `toolCallName` 为工具名，`title` 为展示标题，另有 `icon` | 稳定工具名与用户可读动作应分别构造。 |
| 结果与失败 | RESULT 可含 `status`、`isError`、`durationMs`、`exitCode`；`content` 可为字符串、对象或数组 | 状态无需全塞进标题；退出码和耗时只有在来源可核验时才填。不能把首次观察时间差当执行耗时。 |
| 有用的结果内容 | `content` 支持 text / code / list 及组合 | 可以展示有依据的短结果或文件清单；平台支持代码块不意味着应该外发原始 stdout、命令或 diff。 |
| 更新同一次调用的结果 | 同一 `toolCallId` 后续 RESULT 以最后一次展示 | 摘要或状态修正继续使用原调用身份，不能再生成一个工具调用。 |
| 参数完成与执行完成 | END 表示参数结束；RESULT 表示工具执行结果 | 保留生命周期。只有明确验证客户端支持无正文结果后，才考虑省略成功正文；短期使用“已完成”即可。 |
| 关联公开消息 | START 可带 `parentMessageId` | 有原生关联证据时使用；不能仅凭相邻顺序推断某个 commentary 是工具的父消息。 |
| 阶段事件 | 详细事件文档和 agent 文本将 `STEP_STARTED` / `STEP_FINISHED` 标为不支持 | 接入参考虽有 Step 示例，也不能据此承诺原生阶段分组。阶段进度继续使用已有公开说明。 |
| 自定义整段状态文案 | `input.statusText` 被接入参考标为灰度能力 | 首版改进不依赖它；不能承诺整段入口一定能显示任意业务阶段。 |

官方推荐的内容粒度是：工具开始展示可理解的目的，工具结束展示结果摘要，阶段进展展示结论和下一步；先确定哪些过程值得展示，再做事件适配。我们目前主要完成了生命周期映射，结果内容仍停留在状态占位。[接入指南](https://bytedance.larkoffice.com/wiki/ZfyXwOQImiLURtkRPtWcf2B2nNh)

## 开源实现调研

### 证据范围与结论

本次核验找到三个不同仓库中的真实原生 COT 调用实现，均能从 renderer 追到 `POST/PUT /open-apis/im/v1/message_cot`，不是根据 README 中的“思考过程”措辞判断。三个仓库不等同于三个独立设计来源：存在同一 DSH/Lark 生态中的相似映射，不能据此声称行业普遍实践。另检查 `cc-connect` 和 OpenClaw 的具体飞书进度/流式路径，确认这些路径采用 Card JSON 2.0 / CardKit，不能作为原生 COT 能力证据。

| 项目与核验版本 | 原生调用证据 | 实际传入的信息 | 可借鉴之处与边界 |
| --- | --- | --- | --- |
| `zarazhangrui/lark-coding-agent-bridge`，`589868119ce9a0a860aaf4936b5f9bac14e72681` | [cot.ts:74–114](https://github.com/zarazhangrui/lark-coding-agent-bridge/blob/589868119ce9a0a860aaf4936b5f9bac14e72681/src/bot/cot.ts#L74-L114) 直接 create / update / complete | START 的 `toolCallName` 为原工具名，`title` 为工具名、目标和运行状态的简短描述；brief 模式省略 ARGS，RESULT 为完成/失败摘要；detailed 模式 ARGS 为输入 JSON，RESULT 为限长输出。[事件映射](https://github.com/zarazhangrui/lark-coding-agent-bridge/blob/589868119ce9a0a860aaf4936b5f9bac14e72681/src/bot/cot.ts#L296-L333) | 按模式控制细节，标题提取命令/文件/搜索对象，工具身份与展示标题分开。它仍会把状态和输入复述到 brief RESULT，且 detailed 输入未限长；这些不是应照搬的质量或安全保证。 |
| `moyu-good/dsh-lark-bridge`，`c319bddf7f8afae4e45ca94e84c101ebbc0426de` | [runtime.ts:164–190](https://github.com/moyu-good/dsh-lark-bridge/blob/c319bddf7f8afae4e45ca94e84c101ebbc0426de/src/runtime.ts#L164-L190) 直接 create / append | START 的原工具名与 host 给出的可读标题分离；ARGS 为参数摘要；RESULT 为 `{type:"code",code:短结果}`，失败可带短 `error`。[事件映射](https://github.com/moyu-good/dsh-lark-bridge/blob/c319bddf7f8afae4e45ca94e84c101ebbc0426de/src/cot.ts#L437-L475) | 参数只保留目标信息，大块正文变成字符数；结果保留前三行、最多约 240 字符。不要把完整文件、全文补丁塞进 COT。摘要白名单本身不等于脱敏。 |
| `22-ai-00/dsh-enhanced` 的 `lark-channel`，`091bed9d2fa0eb14686f4c98186e29498502e768` | [sdk.ts:499–536](https://github.com/22-ai-00/dsh-enhanced/blob/091bed9d2fa0eb14686f4c98186e29498502e768/plugins/lark-channel/src/sdk.ts#L499-L536) 直接 create / append | START 的 `toolCallName` 是短工具名，`title` 是“正在使用 …”；允许详情时传 ARGS preview；RESULT 为 code preview 或“已完成/执行失败”，失败另带短错误码。[progress.ts:118–152](https://github.com/22-ai-00/dsh-enhanced/blob/091bed9d2fa0eb14686f4c98186e29498502e768/plugins/lark-channel/src/progress.ts#L118-L152) | 先投影成有界展示对象，再决定受众是否可见；私聊可有参数/结果预览，群聊只发状态。它不外发 reasoning，阶段信息走普通文本事件。 |

上表的“实际”指源码中存在构造并发起该请求的路径，并非现场运行记录；本次未调用任何真实飞书资源。本次没有把 SDK 类型定义、API 包装器的存在，或 README 的截图当成运行及 UI 验收证据。

### 工具身份、标题、参数和结果应各司其职

三个原生实现都使用稳定的 `toolCallId` 关联 START / ARGS / END / RESULT，并把原工具名放在 `toolCallName`，把人读的用途或目标放在 `title`。第一例进一步按工具种类提取输入：Bash 取 command，Read/Edit/Write 取 file_path，Grep 取 pattern 与 path，子代理取 description；未知工具回退到已有 command/path/query，不强行生成“用途未识别”。见 [tool-render.ts:15–18、44–77](https://github.com/zarazhangrui/lark-coding-agent-bridge/blob/589868119ce9a0a860aaf4936b5f9bac14e72681/src/card/tool-render.ts#L15-L77)。这里仍只是输入摘要，不能冒充 agent 已明确表达的目的。

第二例参数摘要的白名单包含 path、command、query、url、pattern、description 等；每个保留值约 120 字符，非白名单的大值只写长度，没有有用字段时写参数数量。结果独立保留前三行、约 240 字符，空输出写“无输出”。见 [cot.ts:110–183](https://github.com/moyu-good/dsh-lark-bridge/blob/c319bddf7f8afae4e45ca94e84c101ebbc0426de/src/cot.ts#L110-L183)。这说明“结果摘要”可以在不透传原始输出全文的情况下存在；但“取前几行”不总能代表结果，诸如测试汇总、退出码应根据工具结构提取，而不是把截断当语义理解。

第三例先在 Delivery 边界把 tool call/result 转成 `tool-started` / `tool-finished`，携带 `argumentsPreview`、`resultPreview`、`failed`、`code`，再由飞书层映射。结果失败来自真实 `event.data.error` 或 `result.isError`，不是从输出文字猜测。见 [agent-runtime.ts:642–670](https://github.com/22-ai-00/dsh-enhanced/blob/091bed9d2fa0eb14686f4c98186e29498502e768/plugins/assistant-delivery/src/agent-runtime.ts#L642-L670)。字段名属于该项目内部模型；不能直接称为飞书要求的字段。

本次检视的三个 renderer 没有依赖通用 AG-UI `metadata.title` / `metadata.summary` 自动渲染；它们把可读内容放在 START 顶层 `title`、ARGS `delta` 与 RESULT `content`。因此建议本地内容模型可以区分 purpose / target / result summary / status，但应通过飞书已确认支持的事件字段投影，不能添加一个 `metadata.summary` 就假定客户端会展示。

### 事件语义：END 不代表工具成功，公开进度不等于 reasoning

AG-UI 的工具调用规范区分“参数传完”与“执行结果”：`TOOL_CALL_END` 后工具可能仍在执行；`TOOL_CALL_RESULT` 承载实际结果。不能收到 END 就写“已完成”。AG-UI 的通用事件、metadata 及推理事件规范也不证明飞书实现了全部事件或扩展字段。见 [AG-UI 官方事件定义](https://docs.ag-ui.com/concepts/events#tool-call-events)。

开源实现对 reasoning 的选择不同：第一例 [直接把 thinking delta 映射为 REASONING_MESSAGE_CONTENT](https://github.com/zarazhangrui/lark-coding-agent-bridge/blob/589868119ce9a0a860aaf4936b5f9bac14e72681/src/bot/cot.ts#L280-L293)，第二例也 [转发 reasoning-delta / reasoning block](https://github.com/moyu-good/dsh-lark-bridge/blob/c319bddf7f8afae4e45ca94e84c101ebbc0426de/src/cot.ts#L410-L432)。第三例明确在投影边界排除 reasoning/thinking 和 provider 原始错误消息；tool、todo、审批等待和中性阶段文案提供公开进度。见 [agent-runtime.ts:606–644](https://github.com/22-ai-00/dsh-enhanced/blob/091bed9d2fa0eb14686f4c98186e29498502e768/plugins/assistant-delivery/src/agent-runtime.ts#L606-L644)，以及 [progress.ts:82–116](https://github.com/22-ai-00/dsh-enhanced/blob/091bed9d2fa0eb14686f4c98186e29498502e768/plugins/lark-channel/src/progress.ts#L82-L116)。

可借鉴的是按来源语义选择公开进度：用户可见 commentary、已发生的工具事实、明确的阶段结论和下一步。开源中的“reasoning 可发送”不构成应该传输完整内部思考的理由；同样不能因为目标字段名带 reasoning，就把所有 hook、诊断或原始事件装进去。

部分项目发送 STEP_STARTED/FINISHED，或用多个独立 TEXT_MESSAGE 气泡表达阶段。前者仅证明开源代码发送过，不能推定飞书原生支持分组或步骤控制；后者会追加可见内容，过于频繁就会堆叠。三个样本均未提供“重复 START 替换已有工具标题”的可靠平台契约，也未证明通用 metadata 可以更新标题。展示标题应尽量稳定，不为纯措辞变化追加说明；必要的事实更正仍须保真。是否能更新同一个标题，应先核验平台对同一 id 的实际语义。

### 开源 payload 的信息分工示意

以下为参考上述实现的合成内容，展示事件 `content` 反序列化后的字段，不是完整请求或已发送的预览：

```json
{
  "toolCallId": "synthetic-call",
  "toolCallName": "search",
  "title": "检查示例配置引用",
  "icon": "search"
}
```

对应 RESULT：

```json
{
  "messageId": "synthetic-result",
  "toolCallId": "synthetic-call",
  "role": "tool",
  "content": {"type": "code", "code": "找到 3 处示例引用。"}
}
```

这里的匹配数是假设上游已核实的合成事实。第三例把 preview 放进 code；本项目短自然语言摘要可优先使用纯文本，避免无必要的代码块。外层 `events[].content` 仍编码为 JSON 字符串。开源 brief 模式省略 ARGS 是其自身选择，不构成本项目可以直接删事件的兼容性证据；首版保留既有空参数与完整生命周期。

### 隐私与长度边界：保留方法，不照搬参数

第三例参数 preview 优先解析 JSON、递归屏蔽常见敏感 key；非法或过大的参数只发 `invalidJson` / `truncated` 状态。结果 preview 对常见 token、凭据赋值、私钥和 URL userinfo 做处理，最终限长。见 [agent-runtime.ts:404–566](https://github.com/22-ai-00/dsh-enhanced/blob/091bed9d2fa0eb14686f4c98186e29498502e768/plugins/assistant-delivery/src/agent-runtime.ts#L404-L566)。飞书层再检查私聊/群聊受众，详情权限在创建进度时固定；后续 target 不一致就不发送细节。见 [progress.ts:198–200、213–247](https://github.com/22-ai-00/dsh-enhanced/blob/091bed9d2fa0eb14686f4c98186e29498502e768/plugins/lark-channel/src/progress.ts#L198-L247)。这些措施仍不能识别所有业务秘密；应按本仓库数据策略选择最小必要摘要，不能直接引入“默认发送所有私聊结果”。

长度存在不同开源假设：第一例 reasoning/text/output 上限为 1200；第二、三例以编码事件 4096 字符为上限，第三例还在 JSON 转义之后缩短 delta/code/message，保留 identity 和 truncated 标记。见 [第一例常量](https://github.com/zarazhangrui/lark-coding-agent-bridge/blob/589868119ce9a0a860aaf4936b5f9bac14e72681/src/bot/cot.ts#L12-L14)、[第二例](https://github.com/moyu-good/dsh-lark-bridge/blob/c319bddf7f8afae4e45ca94e84c101ebbc0426de/src/cot.ts#L50-L54)、[第三例编码](https://github.com/22-ai-00/dsh-enhanced/blob/091bed9d2fa0eb14686f4c98186e29498502e768/plugins/lark-channel/src/progress.ts#L29-L79)。这些是实现约束，不能直接写成已证实的通用平台上限。原项目另有用户报告 1200 delta 加 JSON 开销仍遭“content too long”，其 800 workaround 属于特定环境报告，非平台保证。见 [Issue #251](https://github.com/zarazhangrui/lark-coding-agent-bridge/issues/251)。

### 非原生反例：可借鉴信息设计，不借用其 API 能力结论

`cc-connect` 在 `a35b769c3f06dedd63cfc55ee6ccbd1f45c95549` 的飞书进度路径将 reasoning/tools/updates 分组为 `collapsible_panel`，rich card 用 schema 2.0；创建和更新调用 CardKit，流式文本向 markdown element 写完整累计正文。证据是 [进度分组与折叠面板](https://github.com/chenhg5/cc-connect/blob/a35b769c3f06dedd63cfc55ee6ccbd1f45c95549/platform/feishu/feishu.go#L5087-L5133)、[rich card 渲染](https://github.com/chenhg5/cc-connect/blob/a35b769c3f06dedd63cfc55ee6ccbd1f45c95549/platform/feishu/feishu.go#L7321-L7401)、[CardKit 创建与流式更新](https://github.com/chenhg5/cc-connect/blob/a35b769c3f06dedd63cfc55ee6ccbd1f45c95549/platform/feishu/feishu.go#L5322-L5395)。这些具体路径不是 message_cot；分组、面板展开和整卡替换属于自绘卡片的能力，不能直接推给原生 COT。

OpenClaw 在 `65112cda4acb357a13cd772641ed88acd9bb5759` 的 `FeishuStreamingSession` 使用 `/cardkit/v1/cards` 创建/更新卡片，schema 2.0 与 `streaming_mode:true`，不是 message_cot。见 [streaming-card.ts:188–198](https://github.com/openclaw/openclaw/blob/65112cda4acb357a13cd772641ed88acd9bb5759/extensions/feishu/src/streaming-card.ts#L188-L198)、[cardJson:236–256](https://github.com/openclaw/openclaw/blob/65112cda4acb357a13cd772641ed88acd9bb5759/extensions/feishu/src/streaming-card.ts#L236-L256)。本结论限定于该内置飞书流式路径，没有证明所有第三方 OpenClaw 插件都不使用原生 COT；名称含 `openclaw` 的桥接 adapter 也不等于 OpenClaw 内置插件本身采用了 native COT。

## 改进建议与目标

目标是让用户通过 CoT 判断“正在做什么、有什么进展、是否需要介入”。工具身份、动作、结果、执行状态各有表达位置；正常调用简洁，异常可解释。以下保留调研阶段的分步建议；用户确认后实际完成的范围见[实施与验证记录](../records/cot-tool-details-2026-10-26.md)。

### 第一阶段：用现有数据消除噪声

1. **移除逐条同步策略提示。**完成结果改为简短的“已完成”，不再拼回标题及“原始输出未同步”。如果确有必要说明同步范围，只在帮助说明中集中描述一次。
2. **中性兜底。**识别不完整时显示“运行 tail”“运行 Shell 脚本”或“执行命令”，不显示“用途未识别”。仅在命令形态可以安全确定时，把常见 `head` / `tail` 识别为读取片段；不能把任意脚本猜成业务任务。
3. **缩短标题。**标题目标为 24–32 个 Unicode 字符；保留一个主要动作和必要对象。仅含读取与检索时可概括为“读取与检索文件”；混合其他类型时用“执行复合命令”等中性标题，详情保留已识别动作，不能把其中一步当成整个调用的唯一用途。长度是产品目标，不是平台限制。标题变化不改变真实调用数。
4. **拆分字段。**`toolCallName` 保留安全工具身份或原生类型；`title` 放短动作；RESULT 传已核实状态及最短结果。失败用结构化 `status=error` / `isError=true`，同时保留简短文字以兼容客户端。
5. **如实表达未知。**上游无状态时保留中性活动描述，不标成功、不标失败、不增加未经确认的调用次数。若已开始但结束状态缺失，才显示“结束状态未确认”；真正需要处理的未决投递继续保留原有核验语义。

建议的解码后事件内容示例（外层仍按现有 `content` JSON 字符串编码）：

```json
{
  "toolCallId": "synthetic-call-1",
  "toolCallName": "commandExecution",
  "title": "读取 fixture.go",
  "icon": "read"
}
```

对应有完成证据的 RESULT：

```json
{
  "messageId": "synthetic-result-1",
  "toolCallId": "synthetic-call-1",
  "role": "tool",
  "status": "completed",
  "isError": false,
  "content": "已完成"
}
```

这一步保留 ARGS / END / RESULT 生命周期，不要求引入模型摘要服务，也不扩大原始工具输出的同步范围。结构化状态字段在真实桌面端和移动端的效果须单独验收。

### 第二阶段：让展开内容提供新信息

在现有投影中增加最少量的安全字段，优先复用 `Details` 和可核验的结构化事实：

| 数据来源 | 可以表达 | 不应推断 |
| --- | --- | --- |
| commandActions / 工具名 | 目标文件 basename、读取/搜索/执行等动作 | 命令列表中的每一步都实际执行成功；`&&` 后续动作可能根本没有运行。 |
| 已确认 completed 的 fileChange + changes | 文件变更数量、安全文件名、增删改类型 | 仅凭“准备修改”的记录就写“已修改”。 |
| 已核验并建模的 exitCode / durationMs | 非零退出、真实执行耗时等简短事实 | 用本地首次观察时间计算工具耗时；把非零退出都当业务故障。 |
| 有类型约束的结果或错误分类 | 已核实的候选数、失败类型及可采取的下一步 | 通过截断任意 stdout / stderr 得到“安全摘要”。 |
| 公开 commentary | 已由 Agent 明确表达的发现、下一步及阻塞 | 把前一段说明关联为所有后续工具的业务目的，或凭成功重试猜测先前错误已恢复。 |

例如：已确认修改的文件可以显示“更新 2 个文件”，无额外结果的读文件仍只需“已完成”。搜索“无匹配”需要识别准确的工具/命令形态和对应退出语义；复合 shell 的总退出码不足以归因每个子动作。

允许名单应尽量基于结构化事实；摘要生成前就限制输入范围，而不是先收集完整原始输出再依赖正则脱敏。不为每次工具调用新增一次模型请求，不改全局 Codex 指令，也不改原会话公开历史。

### 展示目标示意

下面是信息层级示意，文案和文件均为合成；折叠、工具组及整段运行入口的具体外观仍由飞书客户端决定。

```text
执行中

已找到两个配置入口，正在比较布局。

▸ 已调用 5 次工具
    读取 fixture.html                     已完成
    读取与检索文件                        已完成
    运行 Node 脚本                        执行未成功
    运行 tail                             已完成
    更新文件                              更新 2 个文件

有明确错误分类时，展开失败项：
    脚本退出码为 1。
    若当前摘要不足以定位，提供一次“在 Codex App 查看详情”提示。
```

示意中的五行对应五次真实调用；不能因一次复合命令含多个动作而增加调用数，也不能把多个 native ID 合为一次。已修改文件数量和退出码属于第二阶段新增的证据字段，第一阶段不能提前生成这些结论。

可见内容的核心变化：动作标题只出现一次；成功项不再附加同步机制说明；失败项有已知原因时给原因，没有原因时坦诚保留最短状态；公开进度讲发现和下一步，最终回答仍独立展示。客户端支持的原生工具分组继续复用，第一阶段不虚构业务阶段、不合并真实调用 ID。

### 实施边界与验收

建议改动集中于 `command_summary.go`、`progress_tools.go`、`cot_projection.go`、`cot/document.go` 和 `cot/planner.go`；第二阶段需要时再扩展已核验的原生 DTO。同步更新 [05 消息同步](../codex-feishu/05-message-sync.md)、[08 卡片](../codex-feishu/08-cards-and-actions.md)及[原生 CoT 契约](../../compat/feishu-native-cot.md)。投递账本和网关保持原职责，只有新增字段确有持久需求时才补兼容。

已完成 CoT 在现有实现中不再更新，因此不能承诺部署后自动清理历史消息。新策略建议按新轮次启用，活动轮次冻结原展示策略；否则已 ACK 的标题/结果改变可能触发“摘要更新”“公开进度已更正”。实现时用现有文档版本或明确的展示版本保留这一兼容边界，而非重写历史投递结果。[完成态与冻结逻辑](../../internal/store/cot.go)

| 验收项 | 通过条件 |
| --- | --- |
| 常规文案 | 合成用例中“仅同步状态”“原始输出未同步”“用途未识别”出现次数为 0；正常结果不重复动作标题。 |
| 短标题 | 常见命令、复合命令、MCP、文件变更标题符合约定长度；详情仍能保留必要安全对象。手机端目测主要标题可快速扫描。 |
| 状态真实性 | completed / failed / declined / unknown / interrupted 各有断言；缺失字段不变成功；工具失败不自动变成整轮失败；失败在开始时或后到达时最终展示一致。 |
| 结果依据 | 每个计数、失败分类、耗时都能回溯到结构化字段；未提供证据时使用中性短文本。 |
| 隐私边界 | 合成凭据、完整路径、原始命令参数、stdout、diff、隐藏 reasoning 哨兵不进入新字段或公开事件；标题和列表同样检查。 |
| 身份与恢复 | 多次同名调用仍保留各自 native ID；同快照无新增事件；跨 50 事件分批、重启、结果更正和 unknown 保持原确认语义。 |
| 客户端验证 | 合成样例核验 title/toolCallName 优先级、结构化失败、空/短结果、原生工具组、运行与完成态；分别确认桌面/手机及群/话题。接口成功不能代替 UI 通过。 |
| 独立最终回答 | CoT 创建或更新失败不阻断最终回复；最终正文、提醒及停止语义不因内容改版退化。 |

## 调研验证与后续实现

调研阶段通过相关既有测试、文档本地链接检查；四段合成 JSON 均可解析。外部源码引用由只读获取固定版本文件核对，文档检查器本身不抓取外链。调研阶段没有发送真实消息或读取真实会话。

用户后续确认了“短标题＋每个工具单独展开详情”的实现方向，并通过初版截图反馈仅有状态、退出码和耗时仍不足以说明执行内容；详情范围因此扩展到实际输入和返回文本的脱敏节选。上文保留调研时的候选取舍，开发、范围调整与回归结果见[实施与验证记录](../records/cot-tool-details-2026-10-26.md)；当前模块行为以 [05 同步](../codex-feishu/05-message-sync.md#新执行进度使用原生-cot)和 [08 展示](../codex-feishu/08-cards-and-actions.md#当前执行进度原生-cot)为准。
