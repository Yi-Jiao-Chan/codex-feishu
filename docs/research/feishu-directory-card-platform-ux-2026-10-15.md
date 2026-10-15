# 飞书原生目录选择卡：平台能力与 UX 边界

核验日期：2026-10-15。范围：Card JSON 2.0、当前企业自建应用的 SDK 长连接；只研究设计，不修改或部署生产。依据是下列官方文档的完整 `.md` 正文和官方 Go SDK v3.12.0 源码；没有真实发送卡片或进行客户端验收。

## 可直接用于设计的结论

现有长连接足以实现“搜索 / 进入子目录 / 上级 / 使用当前目录”的原生卡片流程，无需增加 H5、OAuth 或公网域名。表单外 `select_static` 选中后可通过 `card.action.trigger` 返回 `action.option`，可作为“进入此子目录”的导航动作。平台支持立即返回 toast 和更新卡片，也支持随后调用消息 PATCH 更新同卡。[单选组件](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/interactive-components/single-select-dropdown-menu.md)、[长连接回调](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/event-subscription-guide/callback-subscription/configure-callback-request-address.md)、[回调响应](https://open.feishu.cn/document/feishu-cards/card-callback-communication.md)、[更新卡片 API](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message/patch.md)

目前不能承诺的是：下拉框一定有可用的原生搜索、任意数量 options、`select_remote` 经 WS 动态查询选项、只有操作者可见的临时 busy 卡、或随后台任务持续的原生 loading。前四项分别缺少公开能力证明或受 JSON 2.0 共享卡限制；最后一项没有找到 v2 button 的公开可控字段。不能据此直接取消所有服务端分页或上限。

推荐先设计一个紧凑的 picker：顶部短搜索框与就近搜索按钮，下面一个有边框的当前层目录下拉或少量目录行，底部唯一强调按钮“使用当前目录”。导航只改浏览位置，确认才修改草稿，创建任务仍留在创建卡；这是产品建议，不是平台强制规则。其组件基础由[输入框](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/interactive-components/input.md)、[分栏](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/column-set.md)、[按钮](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/interactive-components/button.md)提供。

## 1. 按钮为什么像文字，以及可用 schema

当前 [cards.Button](../../internal/cards/cards.go) 第 35 行显式生成 `type: "text"`、`size: "small"`；[creationButton](../../internal/cards/creation.go) 第 35 行复用它，基础路径没有改这两个字段。这解释了实际卡片的小号文字外观；不是飞书把默认按钮渲染丢了边框。

官方 v2 支持以下配置；默认类型其实是 `default`，默认尺寸是 `medium`。[完整按钮 schema 与字段表](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/interactive-components/button.md)

| 配置 | 已核实的含义 | 对目录卡的建议 |
| --- | --- | --- |
| `default` / `primary` / `text` | 黑字描边 / 蓝字描边 / 黑字无边框 | 导航次操作用 default；不要把 primary 误当实心主按钮 |
| `primary_filled` | 蓝底白字 | 唯一确认动作可以显式使用它 |
| `size` | tiny：PC 24 / 手机 28 px；small：28 / 28；medium：32 / 36；large：40 / 48 | 手机上的核心确认可用 large；small 不宜承担所有操作 |
| `width` | default、fill、自定义至少 100px；超宽会受卡片宽度限制 | 确认可 fill；输入旁按钮不要固定一个挤压输入的宽度 |
| `text` / `icon` | plain_text 最多 100 字符；支持标准图标 token 或图片 key 前缀图标 | 图标辅助识别，文字明确表达动作 |
| `disabled` / `disabled_tips` | 可禁用，支持禁用解释 | 正文也写出原因，不依赖灰色或 hover |

官方同页的 `disabled_tips` 示例注释偏 PC 悬停，字段表描述为触发弹窗；`hover_tips` 明确是 PC 行为。手机提示的实际触发方式需验收。完整 v2 字段表没有 `loading`；不能移植旧版消息卡片示例中的同名结构作为 v2 保证。

## 2. 下拉目录：已证实的交互和未证实的搜索上限

`select_static` 的完整 schema 提供 `options`、`initial_option`、`initial_index`、`placeholder`、`width`、`type`、`disabled`、`behaviors` 等；每个 option 支持 plain_text、图标、唯一字符串 value。`type: default` 有边框，`text` 无边框。`initial_option` 优先于 placeholder / initial_index。不要默认选中第一个子目录并把它误当已确认目录。[官方单选字段表](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/interactive-components/single-select-dropdown-menu.md)

| 问题 | 核验结果 | 设计边界 |
| --- | --- | --- |
| 表单外选择是否立即回调 | 是；官方示例为 `tag: select_static`、`action.option`，回调类型是 `card.action.trigger` | 可实现“选中子目录即进入”，不再加第二个“浏览”按钮 |
| 放进 form 会怎样 | form 本地缓存填写值，一次 submit 回传 `form_value` | 若要立即导航，把导航下拉放在 form 外；搜索可用独立 form |
| 原生输入搜索 | 完整正文未说明搜索能力、匹配规则或开关；官方配图头部呈输入框外观，但这不能证明实际 PC / 手机搜索行为 | 不能说“必然没有搜索”，也不能承诺“已经支持搜索”；须真实验收 |
| options 最多多少 | 本次读到的 v2 单选全文未给数量上限 | 不能把应用的 10 / 20 条分页限额冒充平台上限，也不能据此视为无上限 |
| 下拉内分页 / 懒加载 | schema 未给 cursor、has_more、query 或加载选项响应字段 | 采用应用显式下一页 / 筛选重绘，或验证后使用有界静态列表 |

上述回调结论也被[官方 SDK 的固定版本模型](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/event/dispatcher/callback/model.go#L30-L52)印证：`CallBackAction` 有 `Option`、`Options`、`InputValue`、`FormValue`；当前项目 [gateway.go](../../internal/feishu/gateway.go) 第 166–168 行已经把 `OnP2CardActionTrigger` 接到 `ws.NewClient`。form 的聚合行为见[表单容器](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/form-container.md)。

### `select_remote` / 动态查询不能作为既定方案

本次未在官方 v2 单选完整 schema、卡片交互配置、卡片回传交互及当前官方 SDK callback 模型中找到 `select_remote`、query-options 回调类型或选项列表响应结构。搜索结果为空只能说明未找到证据，不能证明平台永久不具备该能力。因此没有证据支持“现有 WS 能直接响应远程下拉搜索”，也没有证据支持“只加 request URL 就能启用它”。不要套用 Slack / 其他平台的同名能力。[单选 schema](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/interactive-components/single-select-dropdown-menu.md)、[交互配置](https://open.feishu.cn/document/feishu-cards/configuring-card-interactions.md)、[卡片回调](https://open.feishu.cn/document/feishu-cards/card-callback-communication.md)、[官方 SDK callback model](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/event/dispatcher/callback/model.go)

当前可实施替代：普通 input + 搜索提交按钮，后台过滤一层目录，再更新静态 options；这是已有 `form_value` / callback / PATCH 能力的组合。不要承诺逐键实时远程搜索，也不要因原生下拉能收起长列表，就把整个无界文件树写进卡片。

## 3. 回调、busy 与真实完成状态

官方规定收到卡片回调后 **3 秒内响应**；WS 模式同样要求 3 秒内处理回调。项目若选择 2 秒预算，它是内部保守预算，不是官方 deadline。回调可返回 `toast`（info / success / warning / error）及 `card: {type: raw, data: ...}`；官方 SDK 的[响应模型](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/event/dispatcher/callback/model.go#L49-L67)同时包含 Toast 和 Card。[官方回调协议](https://open.feishu.cn/document/feishu-cards/card-callback-communication.md)、[WS 限制](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/event-subscription-guide/callback-subscription/configure-callback-request-address.md)

可执行的状态协议：

1. 身份、草稿 revision、权限和动作领取成功后，快速响应 info toast“正在读取目录”。若能在预算内从已确认状态生成 busy 卡，可同响应返回这张卡，禁用会冲突的操作并在正文显示“正在读取”。这是“已接收 / 处理中”，不是“已更新目录”。
2. 真正 RPC / 文件系统检查结束后，才 PATCH 成新目录或错误状态。若 worker 每秒轮询且串行处理 RPC / PATCH，平台 3 秒响应并不意味着任务 1 秒完成；不要在 toast 中先宣告完成。
3. 持续 busy 来自服务器投影和卡片更新，不依赖客户端点击后的短暂 spinner。重复点击和旧 revision 仍由服务器拒绝，disabled 只是 UX。

立即响应、延迟更新的官方方式见[处理卡片回调](https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/handle-card-callbacks.md)。其中通过 callback token 延迟更新必须在响应成功之后执行，不能提前或并行；token 有效 30 分钟、最多更新两次。既有消息 PATCH 是另一条通道，不应把 token 两次限制套给 PATCH。PATCH 要求共享卡、14 天内消息，单消息 5 QPS，更新卡片不超过 30 KB。[消息 PATCH](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message/patch.md)

**“临时卡”不能描述成私有 busy 视图。** JSON 2.0 的注意事项明确只支持共享卡，`update_multi` 仅支持 true。该页后面的通用字段表仍列 false，但应服从前面的 v2 限制，不能据此承诺独享更新。同步回调替换 busy 卡会对所有接收者可见；授权校验不能使已经发送的目录名称变成私密信息。[JSON 2.0 结构](https://open.feishu.cn/document/feishu-cards/card-json-v2-structure.md)

## 4. 输入旁按钮、目录行和卡片预算

输入和搜索按钮可以相邻：顶层 form → column_set → 两个 column，分别放 `input width:fill` 和 submit button；按钮有唯一 name，`form_action_type: submit`。form 只可放卡片根层，不能把整个 form 塞进 column；form 内可以放 column_set。手机窄屏可使用分栏的 flow 自动换行或 stretch 上下堆叠。`flex_mode:none` 只压缩列宽，不会自动重排；不是可靠的手机布局保证。[form](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/form-container.md)、[column_set](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/column-set.md)

**多个 form 的证据边界：**官方 form 的 `name` 字段必填、在同一卡全局唯一，用于区分提交的数据属于哪个表单容器；规则禁止 form 嵌套，但没有规定同一卡只能有一个。因此，按公开 schema 可设计多个并列的顶层 form，每个容器与交互项有唯一 name，每个 form 至少一个 submit。这是由 schema 得出的可行性判断；本次没有找到官方多 form 完整运行示例，也未真实验证隔离提交。相邻输入和按钮本身只需一个 form，不应为横向布局额外引入多个 form。[form 字段与嵌套规则](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/form-container.md)

目录行可用 `interactive_container` 包含图标 / 名称 / 右向提示并整体 callback；也可用分栏的文字 + 明确“进入”按钮。容器支持 padding、边框、背景、disabled；内含交互组件时，子组件交互优先于容器交互。避免同一行同时承担“进入”和“选用”两种含糊动作。下拉选择或行点击只导航，唯一 `primary_filled`“使用当前目录”负责确认，是可实现的产品选择。[交互容器](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/interactive-container.md)、[按钮](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/interactive-components/button.md)

| 硬边界 | 官方证据 | 应用处理 |
| --- | --- | --- |
| 一张 v2 卡最多 200 个元素 / 组件 | [JSON 2.0 注意事项](https://open.feishu.cn/document/feishu-cards/card-json-v2-structure.md) | 不是 200 条目录行；容器、文本等也占预算，不能按行数等同推算 |
| 发送卡片消息请求体最多 30 KB | [发送消息 content 字段](https://open.feishu.cn/document/server-docs/im-v1/message/create.md) | 统计实际编码后的请求；路径、value、token、转义和其他正文都算开销 |
| 更新卡片最多 30 KB | [消息 PATCH content 字段](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message/patch.md) | 首发和更新都要预算，不能只计算可见文字 |
| 容器嵌套最多五层 | [form](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/form-container.md)、[交互容器](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/interactive-container.md) | 简单表单和单层目录即可，不构建巨型展开树 |
| input 文本长度 1–1000；多行 auto_resize 只在 PC 生效 | [输入框字段表](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/interactive-components/input.md) | 路径太长要有明确错误；不依赖手机自动增高 |

本次未找到“目录列表最多 N 行”的独立平台规定。10 行或其他 page size 是应用设计，不是平台行数硬限制。即使尝试整层静态下拉，也必须按实际 JSON 预算和已验证 options 上限截断，明确提示筛选 / 下一页，不能静默隐藏余项。

## 5. 无域名条件下的实施与验收门槛

继续使用已有企业自建应用长连接，订阅新版 `card.action.trigger`；运行环境只需可访问公网。官方明确不需要公网 IP / 域名；旧版卡片回调不支持 WS，不能混用。服务端用回调的 operator 身份校验创建者，再用服务端草稿 / revision / opaque option ID 解析目标；不要信任客户端路径或客户端自报操作者。后半句是项目的授权与草稿设计要求，不是下拉组件替代的安全机制。[长连接官方文档](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/event-subscription-guide/callback-subscription/configure-callback-request-address.md)、[卡片回调身份字段](https://open.feishu.cn/document/feishu-cards/card-callback-communication.md)

| 可以进入设计稿 | 本轮不得承诺 / 必须验证 |
| --- | --- |
| 显式描边导航按钮、实心唯一确认按钮 | 图片 / 浏览器 mock 与真实飞书完全一致 |
| 表单搜索 + 静态下拉更新；表单外选择即导航 | 下拉原生搜索在 PC、iOS、Android 都可用且匹配完整路径 |
| 同步 toast + 共享 busy 卡，异步确认后更新结果 | 持续原生 spinner、私有临时 busy、后台尚未完成却显示成功 |
| 有界目录列表和安全可见的 labels | 无限 options、无界整层目录、无需字节预算的长下拉 |
| 原生 WS，不新增 H5 / OAuth / 域名 | 未有官方证据的 select_remote/query-options 能力 |

后续真实 UI 验收建议覆盖：20 / 50 / 100 个 options、重名子目录、很长名称、中文和大小写搜索、已选值后搜索、PC 与手机的搜索入口 / 滚动 / 截断、窄屏输入旁按钮是否换行、连续点击期间 busy 是否可见、读取失败和旧 revision 是否恢复可操作状态。这里的数字是调查用样本，不是已确认的平台上限；本轮不执行真实发送。

只有在真实客户端证实原生筛选及容量后，才能决定用整层有界下拉替代应用分页；未证实时保留显式搜索和后续页入口。平台 schema 证明“可以构建”，实际可读性、搜索易发现性、加载反馈时序仍须真实飞书验收。
