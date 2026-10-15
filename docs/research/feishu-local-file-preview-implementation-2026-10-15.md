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

## 未通过项

用户已确认电脑 Chrome 修复后可用；飞书内置浏览器、手机访问和布局仍 pending。新增 `/preview` 的用户设备实际命令验收另行记录，自动测试不替代该验收。HTTP 直连的传输由用户所用网络承载，需要传输加密时可接 HTTPS。

现有 `instance.bindings_reconciled`、`instance.operations_known` 等诊断 pending 保留；本功能不将它们或一期发布矩阵提升为 pass。全仓测试、本机请求和远端投递确认均不能代替真实 App/飞书/浏览器验收。
