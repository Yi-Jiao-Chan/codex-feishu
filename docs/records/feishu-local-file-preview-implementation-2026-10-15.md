# 本地文件网页预览：实施与本机验证

日期：2026-10-15。用户选择桥自身的受控网页预览，并要求使用 IP 直连。候选于北京时间 18:26 部署，后续已部署 Chrome 登录 403 修复；本机真实 Chromium 登录和文档展示通过，用户随后确认电脑 Chrome 已可用，`phase1_released=false`。

## 实现

- 新包 `internal/filepreview` 提供只读文件登记、私有持久映射、访问口令登录、签名 cookie、CSRF/Host 校验和网页展示。Markdown 使用 goldmark v1.8.6 的安全渲染；不执行原始 HTML，图片显示为文字，普通代码使用转义文本和行号。
- 文件 ID 为带用途隔离的 HMAC，映射按文件独立原子持久化，CLI 与服务可并发登记。文件范围由独立 `file_preview.allowed_roots` 授权，访问通过 `os.Root` 约束并拒绝软链接、隐藏/凭据文件、二进制和超限文件。没有目录列表或任意路径读取入口。
- `sync.Engine.PreparePublic` 在验证完整快照后，仅转换 assistant 的展示副本，再按实际卡片字节分片；原生历史、用户输入和原文件保持。登记失败不保存半份新快照，普通诊断不含口令、正文或底层路径错误。
- 标准本地 inline Markdown 链接、带空格的 angle 目标、`:line[:column]`、`file://` 本地引用可转换；也修复能确认为显式本地范围的 `http://data00/...` inline 或裸 URL。通过解析器精确记录源范围，代码围栏、行内代码、图片引用和正常网站地址保持原义。angle 目标转换为无需尖括号的网页目标，兼容既有卡片安全过滤。
- 首次登录表单提交到当前文件路由，配合无 fragment 的 303 跳转保留浏览器行号；本机真实 Chromium 已验证登录后保留 `#L12`，用户设备仍需验收。
- `serve` 管理预览监听器的启动、失败和退出。默认关闭、默认只监听 loopback；IP 直连需显式配置监听地址。setup 省略预览对象时保留设置，显式设置时整体替换，口令变化使原 plan 失效。
- CLI 新增 `file-preview init-secret` 和 `file-preview link`。普通 JSON 只返回文件引用路径或网页 URL，口令保存在本人 0600 文件中。

安装步骤与完整访问边界见 [14 本地文件网页预览](../codex-feishu/14-file-preview.md)。首版展示当前文件版本；文件里的相对 Markdown 引用显示为文字，不递归登记。

## 自动验证

以下检查通过，继承原 GOCACHE/GOMODCACHE，未隔离或清理缓存：

```sh
go test ./...
go test -race ./internal/filepreview ./internal/sync ./internal/app ./cmd/codex-feishu
```

补充裸错误 URL 的解析后，重新通过全仓测试及 `go test -race ./internal/filepreview`。测试覆盖登记与重启、并发写入、secret 撤销、路径与软链接替换、未授权读取、Host/CSRF/跳转、Markdown 注入、行号与角括号、代码与网站边界、原文保留、分片预算、稳定重放、失败不 stage 和 HTTP 生命周期。

## 候选切换与证据

- 构建：`0cd26da888ac+file-preview-20261015-102338`；包含当前工作区已有功能及本次变更，属于未发布候选。
- 二进制 SHA-256：`32e0fd8858159cc48654daf2f663708c961c221ba1e4479bf03dd56f8a18d2fc`。版本目录为用户私有 lib 下的 `codex-feishu/file-preview-20261015-102338/`。
- 先核验旧进程和配置摘要，停止准确的桥服务，保存原配置、服务单元、setup 状态和 SQLite 一致备份。备份名 `backup-file-preview-20261015-102601`，位于原候选配置目录；55 张业务表共 35,046 条记录，`quick_check=ok`。
- 经既有 `setup --candidate-test --plan/--apply` 和 `service install/start --candidate-test` 更新。数据库 schema 仍为 28，未迁移，未停止 Codex daemon。配置前后按类型比较，原设置语义保持，预览范围仅当前 codex-feishu 仓库。
- IP 入口为 `http://10.37.119.109:8787`，仅绑定本机该网卡；实际进程二进制摘要与候选相同。
- 本机经该 IP 验证：匿名读取返回 401 登录页；POST 登录返回 303；随后样本文档返回 200，含 Markdown article；HEAD 无响应正文；未登记 ID 返回 404；错误 Host 返回 403。未输出或保存真实文档响应正文和访问口令。
- 运行管理快照确认 native / Feishu 均 connected，25 个普通绑定已 hydrated。只读数据库统计看到 2 张卡含预览链接，2 次相应投递为 confirmed；这证明后端投递，不证明实际客户端点击。

私有 preparation、plan/apply、服务操作和 HTTP 聚合证据保存在该版本目录，包含配置/口令的文件引用，不保存口令本身；口令来源为原候选目录的私有 `file-preview.key`，后续 `/preview` 产生的敏感回复另存于私有命令 outbox 和飞书私聊。

## Chrome 登录 403 修复

用户在 Chrome 输入正确口令后看到 Forbidden。本机使用缓存的真实 Chromium 和原链接复现：匿名页面 401，原生表单 POST 携带 `Origin: null`，返回 403。原 HTTP 探针手动填写 Origin，未覆盖浏览器自动生成请求头的行为。

原因是登录页的 `Referrer-Policy: no-referrer`。根据 [Fetch 标准的 Origin 请求头算法](https://fetch.spec.whatwg.org/#append-a-request-origin-header)，该策略会使普通表单 POST 的 Origin 为 null。改为 `same-origin`，保留同源登录来源并抑制跨源 Referer；Host、Origin 和 CSRF 校验继续生效，仍拒绝 null 和外部来源。回归测试检查登录页策略，全仓 `go test ./...` 和 `go test -race ./internal/filepreview` 通过。

- 修复构建：`0cd26da888ac+file-preview-originfix-20261015-104138`；二进制 SHA-256：`be9078e506fcab32f4482c2e5ce5d1ec3f7f9e87af317b161b6f8a7e4c6cae60`。
- 经既有 service stop/install/start 切换，配置、访问口令和文件 ID 保持；原候选目录新增 `backup-file-preview-originfix-20261015-104138`，SQLite 一致备份 `quick_check=ok`。运行中进程的二进制摘要已核对。
- 使用新临时浏览器配置再次打开原链接并原生点击提交：Origin 为同源，文档为 200，存在 Markdown article，保留 `#L12`，无 Forbidden。探针只保存状态码和布尔结果，不记录口令、请求正文或文件正文。CDP 的 Document 响应事件记录 `[401, 200]`，中间重定向未计入该事件列表。
- 聚合浏览器证据在原版本目录 `browser-before.json`、`browser-after.json`；修复构建、服务操作和运行状态证据在私有 `file-preview-originfix-20261015-104138/` 版本目录。原先已经打开的登录页需要刷新，以获取新的响应策略。

## 主动获取访问口令

用户确认 Chrome 可用后，要求通过命令和飞书斜杠指令获取口令，代替查找私有文件。

- `file-preview show-secret --config PATH` 显式向标准输出打印当前口令和换行，拒绝 JSON、文件登记和输出文件参数；仍使用既有私有文件归属、权限和格式校验。错误响应不含口令。
- 部署所有者私聊机器人发送 `/preview`，收到可复制的口令卡片。群和话题只提示转私聊，其他操作者和通知接收者收到权限说明，陌生人静默。`/help` 在预览启用时提供说明。命令不创建任务、不写原生输入队列。
- 敏感回复沿用私有命令 outbox：完整卡片和意图先持久化，投递前再核验所有者、操作者身份、预览开关和私聊范围，沿用原消息 parent、UUID、防重和 unknown 只读核验。因此私有桥数据库和飞书私聊中会保存口令回复；普通日志、诊断和本次聚合验证证据不保存口令。
- 全仓 `go test ./...` 及 `go test -race ./internal/app ./internal/store ./cmd/codex-feishu -run 'PreviewCommand|PreviewShowSecret|PreviewSecret' -count=1` 通过。覆盖所有者私聊、其他身份、群范围、关闭/文件不可读、重复事件、先持久化后投递、未知响应不重发和投递前权限撤销。
- 构建 `0cd26da888ac+file-preview-commands-20261015-105038`，二进制 SHA-256 为 `9f09628ebd0b07acdfe6a4f82bf75c63f6160750da4bfe75106fea1b75e8b048`。经既有 service stop/install/start 切换；配置、口令和文件链接保持。私有备份 `backup-file-preview-commands-20261015-105038` 的 SQLite 一致性检查通过。
- 安装稳定 CLI 链接 `/data00/home/chenxiaoyu.lemon/.local/bin/codex-feishu`，指向本次构建，不修改 shell PATH 或全局 Codex 配置。用户可按该绝对路径调用 CLI。构建、备份、服务操作和不含口令的验证结果保存在私有 `file-preview-commands-20261015-105038/` 版本目录。

## 独立提交范围与验证

发布分支 `codex/file-preview` 从 `0cd26da888ac` 单独抽取文件预览、Chrome 登录修复、CLI 和 `/preview` 口令获取及对应文档。独立分支沿用原数据库 schema；本机较新候选的附件、任务创建和话题协议不在本次提交中，上文部署记录仍描述当时完整工作区候选。

独立工作树已通过 `go test -timeout=90s ./...`。预览 CLI 测试改为自带配置写入辅助函数；版本能力字段使旧版输出捕获超出管道缓冲区，因此同时保留临时文件捕获的测试修正。只读核验确认实际配置口令未出现在待发布文件中；原工作区分支和暂存区保持。

## 未通过项

用户已确认电脑 Chrome 修复后可用；飞书内置浏览器、手机访问和布局仍 pending。新增 `/preview` 的用户设备实际命令验收另行记录，自动测试不替代该验收。HTTP 直连的传输由用户所用网络承载，需要传输加密时可接 HTTPS。

现有 `instance.bindings_reconciled`、`instance.operations_known` 等诊断 pending 保留；本功能不将它们或一期发布矩阵提升为 pass。全仓测试、本机请求和远端投递确认均不能代替真实 App/飞书/浏览器验收。

## 2026-10-19 全本地目录、行号兼容与候选部署

用户报告本地文件链接未转换，并明确要求覆盖所有本地目录。核验发现原预览服务仍在运行，但授权根只有原仓库，新工作树与其他目录不在范围内；解析器也拒绝 `#L12`、`#L12C5` 这类行号 fragment。

本次允许显式配置 `/` 根，并兼容上述本地链接行号。运行配置保留原根以维护旧链接，追加 `/`、共享 Codex Home 的工作树父目录和旧桥私有工作树父目录，共 4 个根。只登记回复引用的文件，不扫描全盘；原口令、文本上限、隐藏/凭据文件与软链接保护继续适用。工作树创建卡的目录预检一并部署，详见[创建记录](feishu-create-codex-session-implementation-2026-10-15.md#2026-10-19-工作树目录预检部署)。

- 构建身份：`4bc4c9ff7c2e+preview-worktree-fix-20261019`，包含 `codex/fix-preview-and-worktree` 工作区改动，构建时尚未提交或发布。
- 二进制 SHA-256：`42b14ea26a08af28d5729f2d1ff388d9e7af322ef27739318b898883f41919c9`。
- 用户授权部署后，沿用既有 CLI 的 stop → plan/apply → install/start 切换固定用户服务。私有版本目录为 `~/.local/lib/codex-feishu/preview-worktree-fix-20261019-jz3h7n27/`；备份和聚合证据保存在其中 `deployment-20261019-084127/`。
- 备份包含原配置、setup journal、unit、CLI 链接目标和 SQLite 一致备份；55 张表、73,815 条记录，schema 29，`quick_check=ok`。此次无数据库迁移；配置读回确认仅 `file_preview.allowed_roots` 改变。
- 实际进程文件与候选二进制匹配，稳定 CLI 链接同步更新；原共享 daemon 沿用。systemd active/enabled，Codex / 飞书连接及发现基线检查通过。
- 本机经原 IP 入口核验：匿名 401、口令登录后 200、已有登记链接 200。原仓库外的临时目录及两种工作树父目录中的合成文本均可登记并读取，`#L2C1` 转换为 `#L2`；各目录中的隐藏文件仍拒绝。合成文件和本次专用映射已清理，未发送测试消息或创建测试会话。

相关 config、cards、creation、filepreview、sync 测试及 `go test -race ./internal/creation ./internal/filepreview ./internal/sync` 通过。全仓业务测试有一项 `TestRuntimeGroupDeadlinePreservesFinalAndWakesWithoutNewInput` 在等待首次建群限流记录时失败；未修改的 `4bc4c9ff7c2e` 基线独立运行 3 次均同样失败。测试在新增线程前把扫描间隔设为一小时，候选缺少 active-list 资格，未进入首次建群；本次未修改该无关流程，不能将全仓结果记为通过。

部署前后 `bindings_reconciled`、`operations_known` 均保留 pending，doctor/status 仍退出 5，未出现新的 required fail。以上是后端和本机 HTTP 核验，新卡片、飞书点击及用户浏览器观察仍待确认；`phase1_released=false` 保持。

## 2026-10-19 代码高亮与 Markdown 阅读部署

用户继续要求代码高亮和 Markdown 格式阅读。原渲染器已支持基本 GFM，本增量完善阅读样式，并通过 Chroma v2.27.0 为代码文件与带语言的代码围栏生成静态 token HTML / 内嵌 CSS；源码保留行号和目标行提示。Markdown 默认阅读，原文位于可展开源码区；未知语言、词法错误或超过高亮预算时保留完整转义文本。

围栏语言仅从已注册名称/alias 的只读映射查找，长度上限 64 字节，并在查找前检查累计 256 KiB 高亮预算。审查发现的未知语言反复触发文件名 glob 查找问题已修复并补回归。原始 HTML、危险链接、远程图片、鉴权和 CSP 边界保持，浏览器不加载脚本或 CDN。

- 构建身份：`4bc4c9ff7c2e+preview-reading-20261019`，包括本分支尚未提交的修复；二进制 SHA-256 为 `1a1cca259080b40e9756c62d1dcb294195385bce6a59da9965099fac983fde63`。
- 沿用已授权的部署流程，保存旧二进制引用、配置、setup journal、unit、CLI 链接目标及 SQLite 一致备份。55 张表、77,192 条记录，schema 29，`quick_check=ok`；配置字节、口令和授权根保持。
- 使用原 CLI 停桥，再由新 CLI 执行候选 install/start，更新稳定 CLI。进程文件摘要与候选一致，systemd active/enabled；构建输入摘要含新增源码文件，未把临时浏览器 fixture 留在仓库。
- `go test ./internal/filepreview` 与该包 race 通过；新回归覆盖 Go/JSON/Python/TypeScript、GFM、跨行注释/CRLF/空文件、恶意语言标记、未知与超长语言、累计预算完整回退。全仓仍仅原有 `TestRuntimeGroupDeadlinePreservesFinalAndWakesWithoutNewInput` 失败，其余包通过，原因与上段基线核验相同。
- 本机真实 Chromium 在隔离的合成 fixture 验证桌面 / 390px 手机尺寸布局、GFM 表格与任务列表、7 种代码颜色、登录和 `#L` 定位；指向 Markdown 源文行的 fragment 会自动展开原生 details。
- 切换后又在原 IP 的实际服务验证匿名 401、原口令登录、代码高亮、Markdown 阅读、源码定位及旧链接 200；无外部资源请求或页面脚本错误。这里只保存合成页面截图或公开项目手册的页面截图与聚合结果，不保存登录请求、口令或真实对话。

私有构建、源差异、新增文件副本、备份和聚合浏览器证据位于 `~/.local/lib/codex-feishu/preview-reading-20261019-bw9mt1bl/`。原 Codex / 飞书连接与发现基线通过；`bindings_reconciled`、`operations_known` 原 pending 保留，doctor/status 退出 5。手机尺寸模拟不等于真实手机或飞书内置浏览器验收，用户实际观察仍 pending，发布标志保持 false。

## 2026-10-19 HTML 静态页面预览部署

用户进一步要求 HTML 页面渲染。本次为 `.html`、`.htm`、`.xhtml` 增加默认静态页面视图，保留带高亮和行号的完整原文；页面无法解析时直接展开源码。静态视图支持内联 CSS，不执行脚本、不加载图片或外部资源，也不提供相邻资源路由。HTML 位于空权限 sandbox 的 `srcdoc`，模板数据使用普通 string 经属性转义，外层 CSP 未放宽。

浏览器核验发现，CSP 阻止资源加载仍不足以阻止 `link rel=preconnect` 建立 TCP 连接；隐身上下文还可能造成测试假阴性，与 [Chromium 的问题记录](https://issues.chromium.org/issues/521929402)一致。因此使用与 Go 1.25 兼容的 `golang.org/x/net/html v0.58.0` 解析并重新序列化显示副本，去掉所有 link、iframe、frame 子树；原文件及源码不变。其模块要求将现有 x/sys 从 v0.44.0 升至 v0.47.0，Go 版本保持 1.25.6。

解析关闭 scripting，并将 noscript 转成普通 div，以保留无脚本 fallback 和转义文本，避免 renderer 将编码后的 link 文本重新变成活动标签。外层响应增加 DNS prefetch off header，删除文档中用户提供的 DNS 控制 meta，在子文档 head 最前注入固定 off，覆盖子文档自行重新开启预取的情况。这些处理与 sandbox/CSP 一起约束静态视图，不作为所有浏览器零网络连接的承诺。

- 构建身份：`4bc4c9ff7c2e+preview-html-20261019`，包含本分支尚未提交的修复；二进制 SHA-256 `e54d8c114ee3bdad7dabdfba232bf5d986ed41f84e4a1193256c48fc9e9f8058`。
- 沿用授权的候选 stop → install/start 流程。配置、setup journal、unit、稳定 CLI 链接目标和 SQLite 一致备份已保留；55 张表、79,925 条记录，schema 29，`quick_check=ok`。没有配置变更或数据库迁移。
- `filepreview` 回归和 race 通过，覆盖扩展名大小写、鉴权、模板属性注入、原文完整与行号、连接提示及嵌套文档过滤、SVG/MathML/template、noscript 字面文本、DNS 策略和过深文档回退。全仓 24 个包通过，仅原有 `TestRuntimeGroupDeadlinePreservesFinalAndWakesWithoutNewInput` 在相同断言失败；已有基线核验见上文。
- 真实 Chromium 在合成 Service fixture 中验证内联 CSS、桌面与 390px 手机尺寸、源码 fragment 展开、Markdown/代码回归，以及脚本、父 DOM/cookie 读取、表单、刷新、页面跳转和弹窗阻止。另用 persistent context 和本机 TCP listener 做有正对照的连接验证：未过滤原文触发 4 次预连接，实际渲染器返回的过滤后 srcdoc 为 0 次，HTTP 数据为 0 字节。
- 切换后在原 IP 入口以原口令登录，实际 HTML 卡片设计文档正确显示内联样式，脚本交互未执行，源码 `#L114` 自动展开定位。匿名 401，原 Markdown 与代码链接均 200，手机尺寸无外壳横向溢出；无外域响应或页面脚本错误。保存的截图仅合成页面及项目设计文档，不含登录页面、口令或真实对话。

私有二进制、461 份构建输入摘要、完整工作区差异与新增文件副本、备份和浏览器聚合证据位于 `~/.local/lib/codex-feishu/preview-html-20261019-c51vuy04/`。进程文件与稳定 CLI 匹配候选，systemd active/enabled，两侧连接和发现基线通过。原恢复诊断和客户端 UI 验收 pending 保留，doctor/status 退出 5，无新增 required fail；真实手机、飞书内置浏览器与完整一期发布结论保持原状态。

## 2026-10-19 HTML 目录锚点修复

用户报告点击 HTML 目录后，内嵌页面显示浏览器“内容被屏蔽”。在用户提供的既有链接复现：文档使用普通 `href="#章节"`，但 srcdoc 的相对 URL 基址继承外层 `/f/ID`，点击后尝试在 iframe 里加载外层预览地址，被原 CSP 拦截。问题属于预览器；上一轮验证了跨页面跳转阻止，却漏测正常目录导航。

显示副本中的 a / area fragment 链接现转换为 `about:srcdoc#章节`，目标固定为 `_self`，使目录、中文与编码锚点、返回顶部在同一子文档内跳转；原链接的 target 或 base target 不能将其带到其他窗口。外部/其他文件链接、CSP、sandbox 和资源过滤保持，原文件与高亮源码不修改。

- 构建身份 `4bc4c9ff7c2e+preview-html-anchors-20261019`，二进制 SHA-256 `d00a693787c1fddcc1311237e1ed400d5e96e01e9f02b9abf4e617c749535f23`；私有证据目录 `~/.local/lib/codex-feishu/preview-html-anchors-20261019-rfsi52u7/`。候选由构建时工作区编译，Git 提交与正式发布分别记录。
- `go test ./internal/filepreview` 与该包 race 通过。新增回归覆盖普通/中文/编码/空 fragment、空白、显式 target、base target、area、SVG，以及其他文件/远端/脚本 URL 保持与源码完整性。本轮改动只在预览模块，沿用前次全仓回归及其既有失败记录，未重跑无关业务测试。
- 合成 Service 的真实 Chromium 在桌面与 390px 手机尺寸核验 10 次目录/返回操作：目标正确、原 DOM 保留、无导航请求，父页面和权限边界保持。源码、匿名 401、原口令登录和跨页面拦截继续通过。
- 切换后在用户原链接逐一核验 9 个有效页内链接，包括跳到正文入口，目标均正确；同一子文档未重载，没有新增导航请求，父页面不变且仍拒绝读取父 DOM。保留的聚合结果不包含文件正文或实际目录标题，未保存该私有文档的新截图。
- 部署前保存配置、setup journal、unit、CLI 链接目标与 SQLite 一致备份；schema 29、55 张表、81,246 条记录、`quick_check=ok`。配置与数据库结构保持，实际进程和稳定 CLI 匹配新候选，systemd active/enabled；两侧连接与发现基线通过。

原恢复诊断 pending 和 doctor/status 退出 5 保留，没有新增 required fail。用户随后确认原链接目录跳转已正常，并授权提交与推送；真实手机和飞书内置浏览器观察仍待单独确认，完整 P1 和发布结论保持。

## 2026-10-21 自包含 HTML 交互预览与本地部署

用户授权支持报告内嵌 JavaScript，并要求独立 SubAgent CodeReview 通过后部署本地。HTML 外壳继续禁脚本，新增受同一登录与文件校验保护的渲染容器；容器使用 HTTP CSP 沙箱和 `sandbox="allow-scripts"` 的 `srcdoc`，不授予 `allow-same-origin`。内嵌脚本与 JSON 可生成图表、搜索、排序和展开交互，外链资源、连接、表单与弹窗继续受限；源码与原文件保持。动态连接提示、WebRTC 等残余风险仍按 [14 模块边界](../codex-feishu/14-file-preview.md#阅读与代码展示) 说明，不将其当作任意脚本的绝对离线执行环境。

- 独立审查发现并修复 P2 锚点兼容问题：改写 `href="#panel"` 会破坏脚本选择器，动态 fragment 链接还可能导航到容器地址。显示副本现保留 fragment，在用户脚本前注入可信 `about:srcdoc` 基址，容器 `base-uri` 仅允许 `about:`。复审无剩余阻断项；使用实际 Go 输出的 Chromium 选择器、标签页、动态英文/中文锚点和隔离检查 16/16 通过。
- 构建身份 `b613584f8932+html-interactive-20261021-162439-8850d9d4`，包含本次尚未提交的改动；二进制 SHA-256 `775f5a72a4e8bb419ad2edf4a7d379df45f4a1c86dd1c194c1b74a360cd89b58`。构建目录 `~/.local/lib/codex-feishu/preview-interactive-20261021-162029-bg19hpit/` 保留源码差异、583 份输入摘要、审查结论与部署聚合证据。
- `filepreview`、`sync` 的 race 回归与文档检查通过。全仓回归仅原有 `TestRuntimeGroupDeadlinePreservesFinalAndWakesWithoutNewInput` 超时；将全部变更 Go 文件通过 overlay 恢复到 HEAD 后，同一测试仍失败，其余包通过。
- 最新源码的合成 Service 在 Chromium 153 完成 42/42 检查：内嵌 JS/JSON、DOM/SVG/Canvas、点击交互、目录与手机尺寸正常；父 DOM、cookie、存储、资源请求、连接和导航隔离按预期工作。本地接收端未收到报告探针请求，正对照确认计数有效。这不替代其他浏览器或飞书客户端验收。
- 沿既有 CLI stop → install/start 切换同一用户服务，未重启共享 Codex daemon。切换前保留原 unit、配置、setup journal、CLI 链接目标和 SQLite 一致备份；schema 30、55 张业务表、124,532 条记录，`quick_check=ok`。没有配置或数据库结构变更，预览口令保持。
- 部署后原报告与旧链接的真实浏览器检查 22/22 通过：匿名 401、原口令登录 200，脚本生成 29 个图表；搜索、排序、展开、动态 fragment、源码行号、桌面和手机尺寸正常，既有 Markdown 与代码链接仍可读。仅保留布尔和计数等聚合证据，未保存真实正文、截图或网络明细。

实际进程和稳定 CLI 均匹配新二进制，systemd active/enabled，两侧连接与发现基线通过。`instance.bindings_reconciled`、`instance.operations_known` 保留切换前的 pending，doctor/status 仍退出 5，无新增 required fail 或原 pass 项退化。真实手机、飞书内置浏览器和完整一期验收仍 pending，`phase1_released=false` 保持。
