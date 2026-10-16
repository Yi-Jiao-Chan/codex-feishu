# 飞书图片、文件与图文混排入站资源核验

日期：2026-10-15。范围仅为飞书侧格式、资源读取、权限与交互边界；不代表完整 Codex 输入方案。依据当前官方文档及项目锁定的官方 Go SDK v3.12.0；未调用真实应用 API、未读取真实附件、未改应用权限或服务。已阅读本仓库开发入口与模块 03。

## 结论

飞书提供用户消息附件下载接口，可以接收 `image`、`file`，也可从 `post` 提取文字和多图。现有“群消息必须结构化 @ 当前机器人”约束可直接用于同条含 @ 和图片的富文本；不能假设普通图片或文件消息可以同时包含结构化 @。[接收内容结构](https://open.feishu.cn/document/server-docs/im-v1/message-content-description/message_content.md)

主方案拟采用一个明确的群 @ 例外：**仅在已绑定的专属任务群内，接收 operator 发送的独立 `image/file` 到附件暂存流程**，明确申请 `im:message.group_msg` 加资源基础读取权限；普通文字与 post 图片仍要求结构化 @。接收附件不等于已提交原生队列，整批发送交互见[实施方案](../plans/feishu-inbound-attachments-plan-2026-10-15.md)。这是产品规则变更建议，尚未实现。最小权限备选是只支持同条 @ 图文，不申请全群读取，但无法完整覆盖群内纯文件输入。

另一个备选入口是对附件作普通引用回复并 @ 机器人，然后补读原消息；官方对应用身份补读群消息要求 `im:message.group_msg`，且话题内 `parent_id` 可能只指向根消息。不能承诺“仅有群 @ 权限就能引用任意附件”；本期不扩展话题附件引用。[读取指定消息](https://open.feishu.cn/document/server-docs/im-v1/message/get.md)、[消息关系](https://open.feishu.cn/document/server-docs/im-v1/message/intro.md)

下载权限与上传权限不同。资源下载的基础权限是 `im:message`、`im:message:readonly`、`im:message.history:readonly` 三选一；`im:resource` 是上传接口列出的权限，不是当前资源下载文档列出的必需权限。[资源下载](https://open.feishu.cn/document/server-docs/im-v1/message/get-2.md)、[上传文件](https://open.feishu.cn/document/server-docs/im-v1/file/create.md)

## 消息格式：已核验

事件类型仍是 `im.message.receive_v1`，不需要为图片或文件新增事件。`message.content` 是 JSON 序列化后的字符串，应按 `message_type` 再解一次 JSON；事件还有 `message_id`、`chat_id`、`sender`、`mentions`、`parent_id`、`root_id` 和 `thread_id`。重复推送按 `message_id` 去重。[接收事件](https://open.feishu.cn/document/server-docs/im-v1/message/events/receive.md)

| 消息类型 | 相关内容字段 | 入站处理建议 |
| --- | --- | --- |
| `image` | `image_key` | 一个图片资源，下载时 `type=image`；原始文件名不是该消息格式的字段 |
| `file` | `file_key`、`file_name` | 一个文件资源，下载时 `type=file`；保留显示名称但不直接当本地路径 |
| `post` | `title`、按行排列的二维 `content`；节点包括 `text`、`a`、`at`、`img`、`code_block` 等 | 按行和节点顺序构建文字、链接和图片；`img.image_key` 逐一下载；只剔除确实指向本机器人的 @ |
| `folder` | `file_key`、`file_name` | 官方明确不能通过 API 下载文件夹，应提示不支持 |

上述格式依据[接收内容结构](https://open.feishu.cn/document/server-docs/im-v1/message-content-description/message_content.md)。该文档未给普通 `post` 节点定义通用文件附件标签；不要把“图文混排”扩张为已证实的任意文件混排。`media` 是视频，不是一般文件。

当前 `post` 还可能有 `content_v2`，其中 `md` 节点尽量保留 Markdown；传统 `content` 会把 Markdown 转成其他节点。二者是替代表示，不能同时遍历后拼接，否则文字和附件可能重复。首期建议以传统 `content` 提取资源及顺序，保留标题、链接、代码块与换行；需要优先采用 `content_v2` 时，另做 CommonMark/GFM 和图片 key 提取，不将任意 Markdown URL 当成可下载附件。真实客户端事件是否包含这两种表示仍需 fixture 核验。[接收内容结构](https://open.feishu.cn/document/server-docs/im-v1/message-content-description/message_content.md)

结构化 @ 的授权依据应继续使用事件 `mentions[].id.open_id`；正文出现机器人名字、`@_user_1` 或用户自填 `<at>` 字符串都不能单独证明 @。富文本 `at.user_id` 在接收表示里可能是 `@_user_X` 占位符，须映射 mentions。[接收事件](https://open.feishu.cn/document/server-docs/im-v1/message/events/receive.md)、[接收内容结构](https://open.feishu.cn/document/server-docs/im-v1/message-content-description/message_content.md)

## 资源下载与元信息：已核验

正确接口为：

```text
GET https://open.feishu.cn/open-apis/im/v1/messages/{message_id}/resources/{resource_key}?type=image|file
Authorization: Bearer <tenant_access_token>
```

图片的 `{resource_key}` 填 `image_key`；文件填 `file_key`，虽然路径参数统一叫 `file_key`。必须使用资源所属消息的 ID，不能使用发起指令的引用回复 ID。前提是启用机器人、机器人与消息处于同一会话，消息 ID 与资源 key 相匹配。官方也支持用户令牌，但本项目建议继续复用应用身份，不新增用户 OAuth。[资源下载](https://open.feishu.cn/document/server-docs/im-v1/message/get-2.md)

不要用 `GET /im/v1/images/{image_key}` 或 `GET /im/v1/files/{file_key}` 作为用户附件入口：这两个接口只允许下载当前机器人自己上传的相应资源，文件接口官方直接指向上述消息资源接口用于用户附件。[下载图片](https://open.feishu.cn/document/server-docs/im-v1/image/get.md)、[下载文件](https://open.feishu.cn/document/server-docs/im-v1/file/get.md)

| 元信息 | 官方证据与可用程度 | 建议 |
| --- | --- | --- |
| 文件名 | `file` 内容有 `file_name`；`image` 内容没有原名字段 | 原名仅作显示；本地用桥生成的稳定安全文件名 |
| 响应文件名 | SDK 从 `Content-Disposition` 解析；当前资源下载文档未承诺每种响应都提供 | 可作备选，不能依赖其必然存在；勿把另一下载接口的请求头条件当成本接口契约 |
| MIME 类型 | 下载文档明确可从响应 `Content-Type` 取得 | 保留声明类型，并结合内容探测验证；不只信扩展名 |
| 大小 | 消息结构没有 `file_size`；下载文档未给独立 HEAD/元信息查询契约 | 有可靠 `Content-Length` 可预拒绝，仍按实际读入字节数执行上限；成功后记录实测大小 |
| 校验与唯一性 | 消息 key 可定位资源；未核验内容哈希或稳定外链契约 | 下载完成计算本地 SHA-256；不把 key 当内容哈希，不生成伪下载 URL |

格式依据[消息内容](https://open.feishu.cn/document/server-docs/im-v1/message-content-description/message_content.md)、[资源响应](https://open.feishu.cn/document/server-docs/im-v1/message/get-2.md)；响应名称行为依据 [SDK FileNameByHeader](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/core/apiresp.go#L127)。表中处理方式为本项目建议，非平台保证。

## 大小限制与流式传输

2026-10-15 当前官方资源文档已经支持文件分片，不能沿用“下载一律最多 100 MB”的旧结论：

| 操作 | 当前官方限制 |
| --- | --- |
| 消息资源 `type=file`，整文件下载 | 文件必须小于 100 MB |
| 消息资源 `type=file`，Range 分片 | 100 MB 及以上文件可 `Range: bytes=<start>-<end>` 分片并本地合并；单次分片不超过 32 MB |
| 消息资源 `type=image` | 不支持 Range；只支持完整下载小于 100 MB 的图片 |
| 上传文件 `POST /im/v1/files` | 不超过 30 MB，不能空文件 |
| 上传图片 `POST /im/v1/images` | 不超过 10 MB，另有格式和分辨率限制 |

下载依据[当前资源文档](https://open.feishu.cn/document/server-docs/im-v1/message/get-2.md)，上传依据[文件上传](https://open.feishu.cn/document/server-docs/im-v1/file/create.md)、[图片上传](https://open.feishu.cn/document/server-docs/im-v1/image/create.md)。上传限制不应套用到本需求的入站下载。

当前资源文档没有给出 Range 文件总大小上限，也没有完整约定 206、`Content-Range`、服务端忽略 Range 等响应细节。错误码 `234037` 的说明仍保留超过 100 MB 的旧描述，与新增分片说明并列存在；大文件能力须以实际响应验收，不能仅凭错误表推断完整协议。[当前资源文档](https://open.feishu.cn/document/server-docs/im-v1/message/get-2.md)

官方 Go SDK v3.12.0 的 `MessageResource.Get` 虽然返回 `io.Reader`，实际把 `ApiResp.RawBody` 包装成 `bytes.Buffer`；核心响应读取使用 `ioutil.ReadAll`，`WriteFile` 又完整读取一次。因此 SDK 的返回类型不代表网络流式落盘，直接使用会在内存里缓冲整个文件。[Get 实现](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/service/im/v1/resource.go#L1734)、[核心读响应](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/core/utils.go#L507)、[响应模型及 WriteFile](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/service/im/v1/model.go)

建议第一版限制在产品明确的较小字节额度内，例如单资源 20 MiB、单输入总量 50 MiB、最多 10 个资源；这些是待产品确认的桥端预算，不是飞书上限。若要求真正流式，应在网关内复用现有 token 获取能力，以受控 HTTP 客户端顺序读取、限制实际字节、写临时文件并原子完成；不应只把 SDK 的 `io.Reader` 传给 `io.Copy` 就宣称解决内存问题。Range 与断点续传可后置为单独能力。

## 权限与群内接收

| 行为 | 当前官方要求 | 对方案的影响 |
| --- | --- | --- |
| 接收用户私聊 | `im:message.p2p_msg:readonly` 或旧 `im:message.p2p_msg` | 可收到私聊的所有用户消息类型；不等于私聊已绑定某个任务 |
| 仅接收用户在群内 @ 机器人 | `im:message.group_at_msg:readonly` 或旧 `im:message.group_at_msg` | 适合现有 operator + 结构化 @ 门槛；不能保证收到不带 @ 的独立附件消息 |
| 接收群内所有用户消息 | `im:message.group_msg` | 可收到纯图片/文件；仍须桥端 operator 和绑定过滤；权限属于敏感范围 |
| 下载已知消息/key 的资源 | `im:message` / `im:message:readonly` / `im:message.history:readonly` 任一 | 当前资源下载文档本身没有另列全群读取权限；不能据此反推未推送原消息一定可读取 |
| 按 ID 补读群消息 | 上述基础读取权限之外，官方明确要求 `im:message.group_msg` | 影响“引用未推送附件”入口；无需为此读取整段群历史，但仍有该权限要求 |

接收权限依据[事件文档](https://open.feishu.cn/document/server-docs/im-v1/message/events/receive.md)，资源权限依据[资源下载](https://open.feishu.cn/document/server-docs/im-v1/message/get-2.md)，补读权限依据[指定消息读取](https://open.feishu.cn/document/server-docs/im-v1/message/get.md)。表中排除接收其他机器人消息的权限，因为本项目不接受其他机器人作为操作者。

“群全消息权限”和“接收所有消息作为任务输入”是两件事。主方案中只接受已绑定专属任务群的 operator 独立图片/文件，并对其他文字与 post 保持结构化 @ 门槛；其他成员、其他群和其他机器人即使推送也不进入 Codex。每条独立附件保留自己的来源消息 ID；按显式的同群、同会话、同发送者草稿归属整批提交，不按“最近一条文字”猜测。新增此敏感权限须在主方案和 capability 清单中明确，不能偷偷扩大现有权限。

## 文字 @ + 引用附件的可行链路与边界

官方可组合出的链路是：

1. 接收 operator 的文字或富文本 @ 指令，持久化其 `message_id` 和 `parent_id`。
2. 对直接父消息调用 `GET /im/v1/messages/{parent_id}`；拿到原消息 `msg_type`、`body.content`、`chat_id`、sender、删除状态。
3. 确认同群、绑定/话题范围正确；建议首版只允许原附件同样由 operator 发送，且只展开一层直接引用。
4. 从原消息提取资源 key，用**原附件消息 ID**调用资源下载接口；把指令正文和附件组成一次输入，沿用原生 queue/add。

步骤 1、2、4 分别依据[事件字段](https://open.feishu.cn/document/server-docs/im-v1/message/events/receive.md)、[读取指定消息](https://open.feishu.cn/document/server-docs/im-v1/message/get.md)、[资源匹配要求](https://open.feishu.cn/document/server-docs/im-v1/message/get-2.md)。步骤 3 是建议的产品约束。这是官方接口组合推导，未经过真实 App/飞书联调。

必须保留以下限制：

- `parent_id` 是上一层被回复消息的 ID，`root_id` 是根消息。话题内回复特殊：官方说明 `root_id`、`parent_id` 均指根消息。不能假设它们总能标识用户在话题里想引用的任意附件。[消息关系](https://open.feishu.cn/document/server-docs/im-v1/message/intro.md)
- 文字中粘贴的“消息链接”或正文里的引用片段不是结构化父消息关系，首版不要自动抓取链接、向上爬消息树或猜测“最近的附件”。
- 普通回复不等于合并转发。资源接口仍以 `234043` 标识合并转发及其子消息、卡片等不支持的类型；不承诺递归展开。[资源错误码](https://open.feishu.cn/document/server-docs/im-v1/message/get-2.md)
- 原消息已删除、不可见、防泄密、机器人不在会话、外部群不支持等都可能使链路失败；“可见消息 ID”不等于“可下载”。[资源下载](https://open.feishu.cn/document/server-docs/im-v1/message/get-2.md)

## 错误、持久性与验收建议

平台可明确区分的失败包括：删除 `230110`，消息/key 不匹配 `234003`，机器人不在会话 `234004`，外部群限制 `234009`，保密/防泄密 `234038`，消息不可见 `234040`，不支持类型 `234043`，Range 格式/范围错误 `234045/234046`。这些应记录脱敏 code/request ID，向用户反馈可操作原因，不能把错误响应当附件字节；鉴权、租户密钥和存储问题也应保持失败。[资源错误码](https://open.feishu.cn/document/server-docs/im-v1/message/get-2.md)

下载 GET 可在限时和限次数内重试临时网络错误或明确限流，但任何失败均不得提前标记“已提交 Codex”。下载成功与输入提交分开持久化；去重仍以指令消息 ID 为一次逻辑输入，一个失败下载重试不产生第二次 queue/add。多图不能静默丢图后只提交剩余文字；建议首版按整个输入原子成功或失败。重启后按已完成的资源记录恢复，半成品不能暴露给模型。以上为实现建议，需由主代理与现有队列状态机整合。

下一阶段需经用户授权后完成的真实验收：

1. 主方案：全群读取权限下，已绑定专属任务群仅 operator 的独立 image/file 可直接进入；非 @ 文字/post、其他发送者与其他群不能错误提交。最小权限备选：只有群 @ 权限时，同条“@ + 文字 + 两张图”post 能否收齐事件和下载，非 @ 纯附件不提交。
2. 有/无 `im:message.group_msg` 两组独立权限证据下，对未推送的纯图片、文件作普通引用 @，验证父消息补读及资源下载。不能用同时拥有多种权限的一次成功证明最小权限。
3. 普通群回复与话题内回复分别查看脱敏 `parent_id/root_id/thread_id`，不将根消息误认成具体附件。
4. 桌面和手机的 post、新旧 content/content_v2、重复 image_key、文件中文名、缺 Content-Length、MIME 与扩展名不一致。
5. 资源撤回、消息不可见、防泄密、外部群、超桥端上限、中途断流、磁盘失败及重启恢复；原消息 reaction 仍正确且飞书正文不再次镜像。
6. 若做 Range，另测 >=100 MB 文件、206/Content-Range、边界、服务端忽略 Range及分片失败；当前版本不将其写成已经支持。

## 实施估算口径

本附录用于接口事实和风险核验，不单独叠加工作量。当前 Agent 的基础版本执行估算统一见[附件输入方案](../plans/feishu-inbound-attachments-plan-2026-10-15.md)：预计 4–8 小时有效工作时间，权限发布与等待联调另计。引用补读、话题附件、Range 大文件与通用文档解析属于独立增强，启用时另行估算。
