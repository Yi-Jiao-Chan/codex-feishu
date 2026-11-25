# 14 本地文件网页预览

将 Codex 公开回复里的本地文件引用转换成浏览器链接。文件留在原工作区，预览服务按当前文件内容读取；Markdown 默认显示阅读排版，HTML 支持受限的自包含交互页面，代码提供语法高亮和行号。功能默认关闭，不需要飞书云盘权限。实现位于 `internal/filepreview/`，同步接线在 `internal/sync/`，监听生命周期由现有 `serve` 管理。

## 使用与配置

先在已有私有目录里生成访问口令文件：

```sh
codex-feishu file-preview init-secret --output /absolute/private/file-preview.key --json
```

命令仅创建新的本人 0600 文件，不覆盖已有口令，也不把口令写进 JSON。配置完成后，可显式打印当前访问口令：

```sh
codex-feishu file-preview show-secret --config /absolute/path/config.toml
```

该命令只向标准输出打印口令和换行，拒绝 `--json`；读取前仍校验口令文件归属、权限和格式。普通诊断、初始化结果和网页 URL 不包含口令。

也可由部署所有者私聊机器人发送 `/preview`，机器人回复可复制的浏览器访问口令。在任务群或话题中使用会提示转到私聊；其他操作者、通知接收者和陌生人无法取得口令。命令不进入 Codex 原生会话，不新建任务。`/help` 列出入口并标明是否启用，`/help preview` 可查看用法。

口令回复沿用私有命令 outbox，发出前保存完整卡片和投递意图；因此私有桥数据库和飞书私聊中会保存该敏感回复。重复平台事件不重复发送，未知投递只核验、不盲重发，投递前再次核验所有者和私聊范围。口令不进入 Git、普通日志或诊断输出。

按现有部署流程停止桥，再修改配置或通过 setup 的可选 `file_preview` 对象 plan/apply，重启桥使其生效。IP 直连例子：

```toml
[file_preview]
enabled = true
base_url = "http://10.0.0.10:8787"
listen_address = "0.0.0.0:8787"
allowed_roots = ["/absolute/path/to/project"]
secret_file = "/absolute/private/file-preview.key"
directory = "/absolute/private/file-preview"
max_file_bytes = 1048576
```

`base_url` 是用户设备实际访问的 HTTP/HTTPS origin，只允许空路径或 `/`，不含凭据、查询参数或 fragment。`listen_address` 是本机绑定地址；省略时只监听 `127.0.0.1:8787`，因此 IP 直连须显式填写。HTTPS 可由现有反向代理终止，服务不根据 `X-Forwarded-*` 猜测访问地址或身份。

IP 直连仍要求用户网络能访问该机器端口；HTTP 的口令和内容由所用网络传输，HTTPS 可为传输提供加密。浏览器会话是否保留以及飞书使用内置或外部浏览器，分别按真实客户端验收。

`allowed_roots` 必须单独指定，不从 discovery 或 creation 范围继承。`directory`、`secret_file` 省略时分别使用数据库旁的 `file-preview` 和 `file-preview.key`。文件上限默认 1 MiB，允许 1 字节至 10 MiB。

可显式配置 `allowed_roots = ["/"]`，授权所有本地目录中的合规文本文件；口令认证、隐藏/凭据文件过滤和软链接检查继续适用。隐藏目录下的 Codex 工作树需另列显式根，例如 `allowed_roots = ["/", "/absolute/home/.codex/worktrees"]`。服务选取最具体且符合过滤规则的授权根，工作树内部的隐藏文件仍不可预览；不会自动加入工作树目录。

扩大范围时保留已有根并追加 `/` 或工作树根。已登记链接关联原授权根，直接用 `/` 替换旧根会使旧链接不可读。

App 可视化 HTML 通常保存在 Codex 数据目录的 `visualizations` 子目录。要在飞书打开这类文件，须保留已有根并单独追加实际产物根，例如 `/absolute/home/.codex/visualizations`；`/` 不会绕过 `.codex` 隐藏目录过滤，工作树根也不覆盖其兄弟目录。不需要将整个 `.codex` 加入范围。该范围仍受文件大小、扩展名、凭据名称、隐藏子目录与软链接检查约束，程序不自动扩大授权目录。按已有部署流程配置并重启桥后才生效。

可显式登记一个文件，以便发送既有文档链接：

```sh
codex-feishu file-preview link --config /absolute/path/config.toml \
  --file /absolute/path/project/docs/report.md --json
```

返回 `data.url`；代码可传 `--file /absolute/path/project/main.go:42:5` 保留行号定位。登记命令不发送飞书消息，不修改原文件，不表示用户设备已能访问。正常任务输出由同步模块自动转换，无需手动登记。

公开回复中的 inline Markdown 本地链接支持绝对路径、`file://` 以及 `:行号[:列号]`、`#L行号`、`#L行号C列号` 定位，例如 `[main.go](/absolute/path/project/main.go#L42C5)`。网页定位到对应行，列号不单独定位；非行号 fragment 或混用两种定位语法的链接保持原样。正常网站链接及其 fragment 不变。

公开 assistant 的 App `visualize` 包装也可登记为网页入口，仅接受含绝对 HTML 路径的已知完整包装，复用相同持久文件 ID、口令和受限 HTML 预览。标记中的路径按文件路径处理，不接受远程 URL，也不作为网页查询参数公开。没有可预览文件时显示回 Codex 查看提示；已有同一文件的正文链接时不重复新增入口。代码中的示例保持原样。详细同步、降级和旧卡片更新规则见 [05 消息同步](05-message-sync.md)。

## 大文件与按需缓存

在已有 `[file_preview]` 下加入以下两项可开启。`artifact_roots` 的每一项须同时列在 `allowed_roots` 中，并且不能是文件系统根 `/`。例如，可以只为已授权的 Codex 可视化产物目录启用。

```toml
artifact_roots = ["/absolute/home/.codex/visualizations"]
artifact_cache_quota_bytes = 8589934592
```

- 同步阶段打开原文件核对类型和大小，将稳定 `/f/<id>` 链接登记后发到飞书。文件正文留待用户登录并访问后读取。对 App 可视化，输入就是原目录已有的 HTML，桥按需生成一份固定版本的文件快照。
- 同一版本的并发点击共用一次准备任务，每个桥最多同时准备 2 份。准备页显示已复制字节和百分比，每 2 秒刷新一次，准备完毕自动打开预览。桥以固定大小缓冲校验文本和复制文件，源文件在复制期间发生变化时不发布中间结果。源文件替换后，原入口会准备新版本。访问仍要求当前源路径可读且在授权范围内。
- `max_file_bytes` 以内的 HTML 和 Markdown 默认渲染正文，通过「查看源码」单独打开带行号的源文件。超过该值且不超过 **2 GiB** 的文件可分页阅读源码。HTML 和 Markdown 的主入口会显示渲染限制，并提供源码链接。服务端每页读取约 64 KiB，以完整 UTF-8 字符分隔页面，源码页支持 `?page=` 跳转。
- 已准备成品在每次授权访问后续期 **48 小时**。桥启动时和运行期间每 15 分钟清理一次到期且无活跃读取的成品，访问已经到期的链接也会即时重新准备。启动时清理上次异常退出留下的临时文件。默认配额 8 GiB，已发布文件和正在准备文件的预留字节均计入；配额不足时显示提示，源文件保持原位。

## 阅读与代码展示

预览页使用浏览器可用宽度，文件名和修改时间收在顶部的工具栏中。HTML 正文铺满工具栏下方的窗口，保持原页面的响应式布局。Markdown 默认使用宽屏排版，勾选「舒适宽度」可将正文收窄。HTML 和 Markdown 均可通过「查看源码」进入独立页面，再用「返回渲染视图」返回。源码页长行横向滚动时行号保持可见。

Markdown 文件（`.md`、`.markdown`、`.mdown`）默认展示正文，支持标题、列表、表格、任务列表和行内代码。带语言标记的代码围栏提供语法高亮，任务列表只供阅读。既有 `#L行号` 链接会转到独立源码页并定位原文件对应行。

HTML 文件（`.html`、`.htm`、`.xhtml`，扩展名不区分大小写）默认显示隔离的交互页面，支持 HTML 排版、内联 CSS，以及使用自包含内嵌 JavaScript 和数据生成的 DOM、SVG、Canvas 图表、搜索、排序和展开/收起。内嵌 JSON 可供页面使用，图片仍不加载。过深等无法解析的文档在渲染区域显示提示，工具栏仍提供完整的源码入口。

HTML 预览通过登录、文件登记、显式授权根和路径校验后，由固定可信渲染容器使用 `srcdoc` 和 `sandbox="allow-scripts"` 加载报告。父容器的 `frame-src 'none'` 限制报告自行导航，直接访问容器入口时报告仍在隔离的 `srcdoc` 中。脚本无法读取预览外壳、父页、登录 cookie 或 `localStorage`。HTML 外壳和 Markdown 阅读页仅通过 CSP 哈希允许固定的旧行号跳转脚本。登录页和源码页使用 `script-src 'none'`。子文档允许内嵌 `<script>`，禁用内联事件属性（如 `onclick`）、`eval`/`Function` 字符串执行、`fetch`、XHR、WebSocket、Worker，以及外链 script、img、CSS、font 和同目录资源。

页面经 HTML parser 规范化，移除资源连接提示、嵌套页面和 `meta refresh`，并关闭 DNS 预取；`<noscript>` 按启用脚本的语义保留，不显示其 fallback 内容。显示副本移除原 `base`，在脚本前设置固定的 `about:srcdoc` 基址，容器 CSP 的 `base-uri` 只允许 `about:`。目录中的 `#章节` 保留原始 fragment，兼容脚本选择器和动态添加的链接，支持中文、编码锚点和返回顶部；原文中这类链接的 target 或 base target 不会改变跳转所在窗口。跨页面链接、表单和弹窗不可用。报告直接访问或脚本自主导航仍受容器与浏览器导航边界约束；动态资源提示、WebRTC 等浏览器能力存在残余风险，不能保证任意 JavaScript 绝对零出网或完全离线安全沙箱。只应打开信任来源的报告；依赖外链资源的完整网站效果不在此预览内。源码与原文件均保持原样。

Go、JSON、Python、TypeScript 等代码按文件名识别语言，显示高亮和可点击行号；未知语言或较大内容退回转义后的普通文本，内容不截断。单个源码文件或同一份 Markdown 的围栏累计最多 256 KiB 参与高亮，不改变文件读取上限。高亮由服务端生成静态样式，页面内嵌 CSS，不依赖 JavaScript 或 CDN。高亮不执行代码，跨行注释、空行和 CRLF 文件保持原有行号。

## 访问边界与恢复

- 只有经过登记且位于显式范围内的常规文本文件可读；没有目录列表、任意路径读取、写入或执行接口。
- 文件映射以稳定 opaque ID 命名，保存在本地 0700 目录的 0600 记录中，先原子持久化再把链接交给消息 outbox。重启沿用映射，CLI 和服务可并发登记。
- 文件读取通过 `os.Root` 约束，拒绝隐藏/凭据文件、路径越界、软链接逃逸、二进制与超限内容。访问时再次校验，登记后的文件替换不能突破边界。
- Markdown 禁止原始 HTML 和危险 URL；独立 HTML 文件只在隔离区域受限渲染，HTML 源码视图以转义文本显示。原生历史、用户输入和原文件不因展示转换而改变；代码围栏、行内代码和普通网站链接保持原义。
- 浏览器使用口令登录和受签名保护的 cookie；HTTPS origin 使用 Secure cookie。随机文件 ID 不能替代登录。口令文件变更后须重启桥，原会话和原 ID 失效，新的同步会使用新链接。
- 稳定入口展示当前工作区版本，后续编辑会改变文件及行号。文件删除、权限改变或超限时旧链接不可读。大文件缓存为当前版本的在线阅读保留固定快照。

## 验收

自动测试覆盖鉴权、路径边界、Markdown 阅读排版与安全展示、HTML 受限交互渲染、CSP/sandbox 与连接提示过滤、常见代码及围栏高亮、源码行号、登记持久化、重启、链接语法及同步分片。doctor 只核验本地配置和私有口令，报告 `ui_verified=false`；不会探测陌生网络地址或把本地监听当作飞书点击已通过。

真实验收至少包括：从飞书打开未提交 Markdown；输入口令后阅读全文；代码行号定位；打开信任来源的 HTML 交互预览并检查图表、搜索排序和展开/收起；电脑/手机网络；浏览器会话；桥重启后旧链接。未授权请求、越界文件、失效映射不得得到文件正文。未取得用户实际点击证据前，客户端验收保持 pending，不改变一期发布结论。
