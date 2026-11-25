# 大型交互 HTML 预览调研

日期为 2026-11-25。范围为超过当前 8 MiB 渲染上限、依赖 JavaScript 的 HTML 产物。2 GiB 指源文件或产物包的字节上限；渲染容量、缓存配额和客户端验收单独衡量。状态为调研与方案建议。静态长文不在本轮范围。 [当前预览契约](../codex-feishu/14-file-preview.md#大文件与按需缓存)

## 建议

**可改生成器的产物采用小型查看器和按需数据，作为 2 GiB 在线交互的主方案。** 对已有单文件，先用代表性样本核验独立沙箱文档流式返回的兼容性，在实测上限内开放。单体脚本或数据仍超预算的报表，按已知格式转换，或使用现有源码入口。两条路径复用飞书稳定链接、权限校验、准备进度和独立源码页。 [当前文件入口](../../internal/filepreview/artifact_http.go#L12) · [当前 HTML 渲染](../../internal/filepreview/render.go#L69)

## 已核实的边界

- **桥侧**。部署中的直接渲染上限为 8 MiB，代码允许配置到 10 MiB，产物准备与源码阅读单独允许 2 GiB。当前 HTML 子页整读文件，经 Go HTML 解析器构造树并序列化，再将整份结果转义进 `iframe srcdoc`。这种做法会在桥侧和浏览器属性处理阶段产生整份中间表示。`srcdoc` 的标准接口为 `TrustedHTML` 或 `DOMString`。Go 的 `Tokenizer.SetMaxBuf` 可限制词法分析缓冲区，评估流式清洗时要专门覆盖超长脚本和数据块。 [部署配置](../records/lazy-file-preview-artifact-deployment-2026-11-25.md#同日更新为默认渲染与独立源码入口) · [渲染上限](../../internal/filepreview/service.go#L28) · [文件读取](../../internal/filepreview/artifact_http.go#L44) · [HTML 清洗](../../internal/filepreview/html.go#L17) · [`iframe` 标准](https://html.spec.whatwg.org/multipage/iframe-embed-object.html#attr-iframe-srcdoc) · [Go HTML tokenizer](https://pkg.go.dev/golang.org/x/net/html#Tokenizer.SetMaxBuf)
- **浏览器侧**。HTML 可以增量解析，脚本会在解析过程中运行，也可能等整份文档完成后才启动。直接 HTTP 流可以减少包装缓冲；脚本仍可能需要整份数据，已创建的 DOM 也会占用内存。Fetch 的 `response.body` 可分块消费，`text()` 和 `json()` 会完整消费响应。浏览器主线程、内存和首屏时间仍要分别实测。 [HTML 解析模型](https://html.spec.whatwg.org/multipage/parsing.html#overview-of-the-parsing-model) · [Fetch Body](https://fetch.spec.whatwg.org/#body-mixin) · [Chrome DOM 性能](https://developer.chrome.com/docs/performance/insights/dom-size)
- **单字符串是独立限制**。V8 标准 64 位构建的单个 JavaScript 字符串上限为 536,870,888 个 UTF-16 码元，约为 512 MiB 的 ASCII 文本。这个数值属于具体引擎和构建配置。若生成器把数据合并成一个字符串并经 `textContent`、`JSON.parse` 或 `atob` 读取，HTTP 成功传完也无法保证脚本能运行。Base64 每 3 字节产生 4 字符，`atob` 返回字符串，随后转成字节和解压还会产生中间表示。 [V8 字符串定义](https://v8.github.io/api/head/v8-primitive_8h_source.html) · [Base64 编码](https://www.rfc-editor.org/rfc/rfc4648.html#section-4) · [`atob` 标准](https://html.spec.whatwg.org/multipage/webappapis.html#dom-atob)

本机机制探针使用隔离的 Chrome for Testing `156.0.8078.4` 和回环 HTTP 服务。独立沙箱 iframe 在服务端放行 32 MiB 合成响应尾部前已创建标题节点并运行首段内嵌脚本，尾段脚本随后运行。探针中子页来源为 `null`，父页 DOM 和 Cookie 访问被拒绝，CSP 拦截了对子页同一 HTTP 主机的 `fetch`，服务端未收到该请求。此证据覆盖增量解析与特定策略行为。旧格式的 2 GiB 渲染、内存边界及飞书端仍待验证。

## 路线比较

| 方案 | 适用情况 | 支持边界 |
| --- | --- | --- |
| 受控沙箱中的独立 HTTP 文档 | 中等体积的旧 HTML，包含已支持的内嵌脚本和数据，生成器无需改动 | `iframe src` 指向鉴权并已清洗的、可分段响应的文档，减少 `srcdoc` 整串与服务端响应缓冲。浏览器最终仍处理整份文档。原基址、子资源和导航隔离需重新验收。 |
| 小型查看器和数据分块 | 可改生成器的报表、图表、长表格及交互工具 | 根据报表实体、时间窗或行组加载，首屏请求独立。原生成器需改造；一块过大继续拆分，浏览器只留有限数量的可见数据块。 |
| 已知产物格式转换 | 大文件仍是单体 HTML，且生产者有稳定的结构契约 | 识别版本和格式后从源文件流式提取，只为明确的格式提供适配。独立数据块的摘要及长度可核验，未识别的单体产物保留源码查看。 |
| 本地打开、整体压缩、直接按字节切 HTML | 桌面端对照、减少传输量、源码查看 | `file://` 的来源行为由浏览器决定，无法解决内嵌数据的字符串与内存约束。压缩只改变传输字节。Range 依赖客户端明确取段，任意切开 HTML 会影响解析状态、脚本和上下文。 [URL 来源](https://url.spec.whatwg.org/#concept-url-origin) · [HTML 解析](https://html.spec.whatwg.org/multipage/parsing.html#overview-of-the-parsing-model) · [HTTP Range](https://www.rfc-editor.org/rfc/rfc9110.html#section-14.2) |

## 2 GiB 路线如何落地

1. **产物协议**。生成器输出有版本的 manifest、小型首屏、可分页索引和独立数据块。首版可使用 JSON 分块或按字节偏移索引的 NDJSON。Trace 以 trace ID 为第一层键，再按事件序号或时间窗拆分单条大型时间线。普通图表以图表 ID、筛选条件及时间窗选块，表格按查询或行区间取数。重数据直接用二进制或分块 JSON，避免整块 Base64 字符串。HTTP 可按记录 ID 暴露受限分块，在单个不可变数据文件需要对应字节时再用 Range。Range 以确定版本的表示为基准，范围上限和返回长度应校验。 [HTTP Range 与表示版本](https://www.rfc-editor.org/rfc/rfc9110.html#section-14)
2. **桥和浏览器**。桥使用固定可信查看器请求通过会话鉴权的分块，数据按 schema 验证并作为文本或数据渲染。本地路径不进入 manifest URL，分块 ID 只在当前报告版本内解析；继续限制路径、符号链接和跨报告访问。若以后要允许产物自带交互代码，可把它留在无 `allow-same-origin` 的独立沙箱，只通过按报告和分块 ID 限定的消息协议传递必要数据，验证发送窗口与消息结构。既有的导航、预连接和子资源控制要覆盖 direct-HTTP 试验；不因分块开放任意同源 `fetch` 或 `Origin: null` 跨域权限。 [HTML sandbox](https://html.spec.whatwg.org/multipage/iframe-embed-object.html#attr-iframe-sandbox) · [CSP sandbox](https://www.w3.org/TR/CSP3/#directive-sandbox) · [跨文档消息安全](https://html.spec.whatwg.org/multipage/web-messaging.html#security)
3. **准备、预算和回退**。缓存始终绑定一份不可变版本，构建索引或校验 manifest 的阶段在进度页显示，所有块就绪后才切换默认预览入口。发布后的页面按需取数据，取消已离开的请求，限制解码大小和驻留块数。实验可先以首屏索引 256 KiB、常规数据块 1 MiB、解码后单块 4 MiB 为初始预算，用样本调优。这些数字是建议测试值；客户端表现通过实测确定。超出旧 HTML 实测渲染预算、存在巨大单条数据或结构未识别时，页面应给出具体原因并保留原始源码。 [当前准备与缓存](../codex-feishu/14-file-preview.md#大文件与按需缓存) · [Fetch 流接口](https://fetch.spec.whatwg.org/#body-mixin)

## 下一步验证

- 从实际失败产物中抽取两至三种数据形状。仅记录总大小、内嵌脚本与数据块大小、Base64 或外部资源占比、脚本的初次取数方式，报告正文留在允许的本地位置。先判断主要体积来自公共运行库、内嵌数据、资源还是 DOM。Plotly 的 `include_plotlyjs='directory'` 可把公共运行库放到独立文件；Bokeh 的 `json_item` / `embed_item` 支持按图表请求数据；DataTables 提供服务端分页、排序和搜索。分别核对导出器和自产物是否支持对应模式。 [Plotly HTML 导出](https://plotly.com/python-api-reference/generated/plotly.io.write_html.html) · [Bokeh JSON 嵌入](https://docs.bokeh.org/en/latest/docs/user_guide/output/embed.html#json-items) · [DataTables 服务端处理](https://datatables.net/manual/core/server-side)
- 旧格式在 8、32、64、128 MiB 样本上对比当前路径和独立沙箱文档，观测首内容、可交互时间、服务端峰值内存、渲染进程内存以及滚动与点击。加入脚本在文档结束后才读数据、单一超长内嵌块、不同体积但 DOM 节点量接近的对照。实际飞书桌面端和手机端分别验收。
- 新格式以 100 MiB、512 MiB、2 GiB 的合成产物核验首屏和详情请求体积。确认点一条 Trace 只取目标时间窗，单条超大事件可继续分页，滚动或多次切换后的内存可回落，取消与重试同一版本可用。覆盖未授权、跨报告块、版本变动、解压膨胀、路径越界，以及旧 HTML 和源码入口回归。网络与内存验收按真实客户端结论记录。
