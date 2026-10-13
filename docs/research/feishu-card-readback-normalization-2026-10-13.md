# Card JSON 2.0 原值读回与 unknown 确认边界

日期：2026-10-13。范围：飞书 IM v1 GET/List 的 `user_card_content`、项目固定 Go SDK v3.12.0，以及 send / reply / PATCH 响应未知后的只读证明。调查基线为仓库 HEAD `7c83060`；只新增本文，不修改实现、测试、服务或会话政策。

官方资料通过公开文档及官方 SDK 源码核对。本调查未调用真实飞书 API、读取密钥或读取私有正文；下文真实探针结论由主代理提供，仅记录差异形状，不保存卡片正文、目标 ID 或 marker 原值。

本文的“完整冻结文档 / 原 wire”沿用当前核验器的含义：严格解析后，所有字段和值、JSON 类型、数组顺序及 Markdown 字符串一致；对象键排序与 JSON 序列化空白不要求原字节相同。字段省略和 Markdown 内换行变化属于实质差异，不能与序列化差异混淆。

## 结论

**当前公开 GET/List 没有已核实的完整 wire JSON 原值读模式。`user_card_content` 是正确的读回参数，但“返回原始卡片 JSON”的文档描述不能作为每个输入字段、文本换行均原值保留的实测证据。** 所调查的官方文档只说明默认格式和 `user_card_content` 两种行为；固定 SDK 对该字符串参数直接透传，没有另外的原字节、原请求 UUID 或 PATCH 历史版本查询能力。没有找到可依赖的 `raw_card_content` 官方契约；这不等于证明平台不存在任何未公开能力。[官方 GET](https://open.feishu.cn/document/server-docs/im-v1/message/get.md)、[官方 List](https://open.feishu.cn/document/server-docs/im-v1/message/list.md)、[SDK Get/List builder](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/service/im/v1/model.go#L14077-L14233)

主代理对三条真实已发送、此前已 journal confirmed 且已由用户确认 UI 的卡片进行只读 GET：唯一 marker 保留，身份与消息关系符合冻结请求，但严格结构比较失败。差异包括服务端省略字段以及正文换行改写。因此 **marker 的可读回能力已有正向证据，完整冻结卡片的 unknown 自动恢复仍未通过**；两者不能合并成一个 pass。

**本期唯一推荐：保留生产严格比较，所有无法证明完整冻结文档的读回继续 inconclusive / unknown，不盲重发。** 正常 send / reply / 持续 PATCH 仍按原成功响应确认；本结论限制的是响应丢失后的完整文档自动证明，不否定这些真实卡片已经发送或正常更新的事实。后续若另建“平台归一化后内容”的证明协议，应明确改变证明含义并单独验收，不能在当前 `confirmed` 契约下暗中丢字段。

本报告补充并收紧 [2026-10-12 的读回研究](feishu-unknown-readback-2026-10-12.md)：先前基于官方原始结构描述提出的严格比较仍能安全拒绝不足的证据，但不能据此承诺实际常规 Card JSON 2.0 卡片必然可完整读回。

## 已核实的 API 与 SDK 契约

| 来源 | 核实结果 | 对恢复的含义 |
| --- | --- | --- |
| [IM GET](https://open.feishu.cn/document/server-docs/im-v1/message/get.md) / [List](https://open.feishu.cn/document/server-docs/im-v1/message/list.md)，`card_msg_content_type` | 默认返回接收消息结构；传 `user_card_content` 切换至发送卡片结构，支持 1.0 / 2.0。`schema` 区分卡片格式版本 | 已经使用正确参数；默认格式不能替代，`schema=2.0` 不是某次 PATCH 的版本证明 |
| 同上，响应字段 | `body.content` 描述为当前消息内容，另有 app 发送者、chat、parent/root/thread、deleted 与时间字段；没有请求 UUID 回显或写入历史 | 可核对资源身份及当前关系；没有公开的按 attempt UUID 查询原请求能力。List 响应表仍有“不支持原始卡片”旧描述，与新增 query 说明存在文字不一致，应以实际探针约束实现 |
| [SDK v3.12.0 Get builder](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/service/im/v1/model.go#L14106-L14111) / [List builder](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/service/im/v1/model.go#L14224-L14230) | builder 接受任意 string 并设置 query；注释重复上述两种公开行为 | SDK 能填入某个字符串不等于平台支持该读模式，不能用猜测值绕过证据缺口 |
| [SDK Message](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/service/im/v1/model.go#L5682-L5715) / [MessageBody](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/service/im/v1/model.go#L6083-L6085) | 消息内容保存在 string；模型没有完整写请求副本或请求 revision | 项目 [reader](../../internal/feishu/message_readback.go) 也保留 `body.content` string，未在读取中主动剔除样式或修剪 Markdown；不能把差异归因于 SDK 本地“恢复原值”功能 |
| [PATCH 文档](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message/patch.md) / [固定 SDK 请求与响应](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/service/im/v1/model.go#L14524-L14539) | 请求为 card `content`，成功响应没有可读版本；更新前后要求显式设置 `update_multi=true` | 已知 message ID 只定位目标；不能用 `updated`、时间较新或相同正文证明本次 PATCH。也不应为迁就 GET 缺字段而删去出站必需配置 |
| [SDK Cardkit v1 resource](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/service/cardkit/v1/resource.go#L28-L168) / [创建卡片实体](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/cardkit-v1/card/create.md) | 固定版本提供创建、更新、设置、批量更新及 ID 转换等操作，没有卡片全文 GET；创建返回 card ID，实体有有效期 | Cardkit 实体 ID 与更新序号本身不能补上本次全文读回证据。全面迁移既增加新远程步骤，也没有已核实的原 wire GET，当前不采用 |

以上 API 范围内，没有保证 GET 是写入的线性化屏障，也没有 List 跨分页快照保证。已有代码的完整分页、唯一候选后再 GET，是降低错误关联的实现约束；不能把“没有找到”反推为远端拒绝。[官方 List 分页说明](https://open.feishu.cn/document/server-docs/im-v1/message/list.md)、[当前核验器](../../internal/sync/readback.go)

## 真实只读探针与字段分类

主代理运行 [只读探针](../../internal/sync/live_readback_integration_test.go)，分别读取已确认的 send、reply、update 卡。三条的 marker 均精确保留；sender 为本 app 的 `app/app_id`，chat、parent、deleted、非 topic 关系符合预期。冻结与读回正文只保存在主代理的私有证据目录，未进入本文。本调查没有独立重跑此探针，因此以下为可追溯的任务内实测报告，不扩大为所有卡片与租户的普遍保证。

| 主代理报告的差异形状 | 官方语义与保守判断 |
| --- | --- |
| `config.update_multi=true` 未返回 | 2.0 全局配置默认共享，且当前仅支持共享卡片；缺省与 true 可以作为**语义默认值**的候选等价。但读回无法证明原 JSON 显式含有该键，PATCH 的出站要求仍保留。[Card JSON 2.0 配置](https://open.feishu.cn/document/feishu-cards/card-json-v2-structure.md)、[PATCH 前提](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message/patch.md) |
| 额外 `enable_forward_interaction=false`、`streaming_mode=false` | 两者官方默认值为 false。未来协议最多允许缺省与**同一确切默认值**等价；true、类型改变或其他字段不在这个例外中，不能通用删除 config。[全局配置](https://open.feishu.cn/document/feishu-cards/card-json-v2-structure.md) |
| `body.padding` 从双值改为相同的四值展开；`width_mode=compact` 保留 | 主代理报告的展开保留了上下、左右的边距语义。这类有值、可计算的表示转换与字段完全丢失不同；未来需要只在指定 padding 路径按明确语法展开并比较全部四值，不能忽略所有 layout。当前严格比较仍拒绝结构差异。[2.0 正文结构](https://open.feishu.cn/document/feishu-cards/card-json-v2-structure.md)、[官方双值边距示例](https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-json-v2-breaking-changes-release-notes.md) |
| `body.vertical_spacing=4px` 未返回 | 2.0 文档支持该属性，但未核实正文缺省一定等于 4px；各类容器默认值并不统一。不能把示例值当正文默认值，也不能从缺字段恢复原间距。继续 inconclusive。[正文字段示例](https://open.feishu.cn/document/feishu-cards/card-json-v2-structure.md)、[容器默认值变更](https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-json-v2-breaking-changes-release-notes.md) |
| Markdown `text_size=normal` 与 `text_size=notation` 均未返回 | 官方默认是 normal（14px），notation 是有效的 12px 值。前者缺省可成为有限语义默认值候选；后者丢失不能当 normal 等价。读回未返回也不能证明客户端实际字号已经改变；只能证明该接口没有提供原字号证据。[Markdown 字段说明](https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-json-v2-components/content-components/rich-text.md) |
| send 的正文末尾一个 LF 被移除；footer 末尾增加一个 LF | 已观察到文本不同，无法从结果唯一还原输入。官方 Markdown 说明换行参与软/硬换行且渲染依实现而定，没有尾 LF 必定无意义的保证。不能用 `TrimSpace`、删除所有换行、逐行 trim 或只比提取文本绕过；代码块、缩进、原始字节的保留要求仍有效。[Markdown 语法](https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-json-v2-components/content-components/rich-text.md)、[镜像正文契约](../codex-feishu/05-message-sync.md) |
| mention 的 id 属性双引号被移除，目标及其他字符相同，没有名称注入 | 官方例子支持未加引号的 id 写法，因此此**具体形状**可作为将来精确语法等价的候选；不能删整个 `<at>`、替换成姓名或忽略目标。应保留每个 mention 的位置、数量、目标、其他属性和周围文本。超出这一形状的改写继续 pending。[Markdown @ 语法](https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-json-v2-components/content-components/rich-text.md) |
| 原本没有 ID 的 footer / mention Markdown 元素新增自动 `element_id` | 官方将该字段定义为组件操作标识，并要求卡内唯一。未来协议可以专门研究无行为、无引用的叶子上额外 ID 的影响；不能通用删除所有 element_id，更不能删除 attempt marker 或调用方原有的 ID。[2.0 元素标识](https://open.feishu.cn/document/feishu-cards/card-json-v2-structure.md) |

这些候选语义等价**尚未组成发布协议，也没有在生产 comparator 放宽**。尤其非默认字号、未知正文间距缺失和正文换行改写，使这批真实卡片无法通过完整冻结文档证明。用仅覆盖“能解释的差异”的局部 comparator，然后跳过其余差异，会把未验证信息混成 pass。

## 唯一 marker 能证明什么

当前 [marker 构造器](../../internal/cards/request_marker.go) 把符合 2.0 格式约束的唯一标记放入空闲 Markdown 元素；主任务已核实发送前持久保留 marker 与不可变 wire、全局预留避免复用。[核验器](../../internal/sync/readback.go) 对未知 send/reply 完整扫描唯一候选后再 GET，对 PATCH 只 GET 已知目标；它同时核对 app、chat、消息关系与未删除状态，最后比较完整 JSON。

该流程的推论分为两层：

1. 若 marker 只属于这个冻结 attempt、任何后续版本都生成新 marker、同 app 的其他写入者不复制它，那么 marker 加身份/关系证据可以关联本 attempt 与远端资源。相同 body 没有此关联作用；相同 sender、时间或 message ID 也不能替代 PATCH 的版本 marker。
2. 关联不能补出读回已省略的字号、间距或文本，也不能证明这些输入区别在平台存储和客户端中都保持。`element_id` 不是平台签名的请求收据；若后来写入保留旧 marker 或另一个同 app 写入者复制它，单看 marker 更会将旧版本与当前版本混淆。

因此不能将“marker 找到了”直接升级为当前存储契约的完整 `confirmed`。服务端如果把不同输入映射到相同读回值，比较这个读回值只能证明落在同一归一化结果中，无法恢复被合并的输入区别。即使 marker 帮助定位原请求来源，这种信息损失仍然存在。以上是基于 marker 使用约束与实测差异的分析，不是官方额外提供的事务保证。

## 最小实现选项与本期决定

| 选项 | 收益 | 风险及决定 |
| --- | --- | --- |
| 保留 strict + inconclusive | 保持完整冻结请求、身份和关系的现有证明要求；无需新依赖或新远程写步骤 | 部分真实已发送卡片的 unknown 无法自动确认。**本期采用**，如实保留 pending；未读到、缺字段或内容变化均不引起重发 |
| 将来新增明确的归一化内容证明协议 | 可研究已知默认值、精确 padding 展开、受限 mention 语法转换，以及平台生成的叶子 ID | 必须定义比完整 wire 更窄的证明含义、profile 版本、允许路径/值和不适用卡片，持久区分关联证据与完整文档证据；非默认值及不可逆正文差异仍需保守处理。本期不实施，也不宣称这批探针已完整恢复 |

只比 body、只比 marker、丢全部样式、忽略所有未知字段或修剪全文，不是当前契约下的安全最小修复。完整文本、mention 目标、按钮和行为、权限/转发/流式配置、调用方 ID、数组顺序及 JSON 类型仍是重要证据，不能为了让三个样例通过而删除。

后续实现若选择第二种协议，必要验证应围绕误确认：旧 marker 随后续 PATCH 保留、同 marker 多候选、错误 app/chat/parent/topic、字段缺失、notation 与 normal 被合并、mention 换目标、引号变化之外的文本改变、代码块或末尾多 LF、按钮行为改变、非法/重复 JSON 键、分页截断，以及 List 后 GET 已改变。允许的转换必须只接受精确等价形状；任何未覆盖转换保持 unknown。这是未来验收路径，不是本次新增测试任务。

## 验证范围与未决限制

- 已核对当前公开 GET/List、Card JSON 2.0 结构与 Markdown、PATCH、Cardkit 创建文档及固定 SDK v3.12.0 源码；没有发现可承诺完整 wire 保留的替代读 API。
- 真实结果来自主代理对已有本人测试卡的只读探针；没有新发送、更新、重试、Cardkit 迁移或正文公开。三条样本不能证明所有卡片、所有 Markdown 及所有服务端版本都有同样转换。
- 官方没有公开此组归一化的完整、稳定、可逆规则；被省略样式实际是否仍在 UI 生效，需要另有 UI/平台证据，不能从 GET 缺字段猜测。
- 本期 unknown 全文自动恢复仍 pending。保持冻结与严格核验可防误确认，但不能给这种证据缺口设置一个超时后自动成功或重发的出口。
