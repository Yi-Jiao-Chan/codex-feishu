# 飞书打开服务器本地文件：浏览器链接方案

日期：2026-10-15。范围：官方接口能力、当前仓库边界与建议落地切片；本次仅调研，未部署预览服务、上传实际文件或发送飞书消息。

后续用户已选择受控网页预览，并明确使用 IP 直连；首版因此支持 HTTP/IP 加访问口令，也可配置 HTTPS 入口。实施契约和当前验收边界见 [14 本地文件网页预览](../codex-feishu/14-file-preview.md)。下文保留方案比较的调研依据。

## 结论

建议为桥接器增加可选的**受控只读 HTTPS 预览**：把回复中的本地文件引用转换成浏览器可达的页面链接，保留当前工作区内容及代码行号。前提是已有或能够提供用户设备可达的 HTTPS 入口。

如果暂时没有这样的入口，可以把 Markdown 导入飞书新版云文档，再发送导入结果中的 `url`。这会产生飞书中的副本，需配置应用与查看者权限，并维护更新关系。

IM 文件消息适合“把附件发给用户”，但上传得到的 `file_key` 不是浏览器预览 URL，不能替代上述页面服务或云文档。[上传文件][im-upload]、[下载文件][im-download]

## 为什么当前链接打不开

- 用户给出的形如 `http://data00/home/...` 的字符串，在标准 URL 语法中把 `data00` 当作主机名、`/home/...` 当作 HTTP 路径，不表示服务器的 `/data00/home/...` 文件。仅做语法分析，未访问该地址。[URI 语法][uri]
- 服务器上的绝对路径属于服务器文件系统；飞书客户端不会因为路径存在而替它创建 HTTP 服务。
- 改成 `localhost` 或 `127.0.0.1` 后，浏览器会连接点击者设备的回环地址；只有显式建立本机转发等链路时才可能到达服务器。[localhost 规范][localhost]
- `file://` 不在飞书卡片 Markdown 官方支持的超链接协议范围内；它也不能自动给点击者提供远程服务器的文件访问能力。[卡片 Markdown][card]
- 目前没有证据判定错误 scheme 是模型原文自带，还是飞书对无 scheme 链接的处理结果；不能把某一方认定为根因。

## 飞书平台允许怎样的链接

官方卡片 Markdown 文档明确：文字链接与超链接必须包含 scheme，目前只支持 HTTP、HTTPS；所以可以发送普通 `[查看文件](https://实际可达地址)`。[卡片 Markdown][card]

这是对链接格式的支持，不包含读取服务器文件、创建代理或为 URL 附加应用 API 鉴权的能力。预览目标的连通性与用户授权仍由目标服务负责。

当前项目使用 Card JSON 2.0。官方富文本组件说明旧的 `href` 差异化跳转字段不再支持 JSON 2.0；普通 Markdown 链接已足够表达预览目标，无需借旧字段改写地址。[富文本组件][card]

“页面可访问”和“在外部浏览器打开”还需分开验收。飞书帮助中心确认，桌面端 V7.0 及以上可在个人头像 → 设置 → 效率 → 链接打开方式，选择优先在浏览器打开云文档。该说明适用于云文档及上传的云盘文件，不能据此承诺所有平台、所有链接均强制唤起外部浏览器。[默认打开方式][browser]

## 三种选择

| 选择 | 用户得到什么 | 主要前提 | 对本问题的适用性 |
| --- | --- | --- | --- |
| 受控只读 HTTPS 预览 | 工作区文件的网页，可提供 Markdown 和代码行号 | 可达的 HTTPS 入口、身份鉴权、受限文件读取 | 推荐；可覆盖未提交文件与多类代码引用 |
| Drive 文件或 Markdown 导入云文档 | 飞书托管文件/在线文档的访问链接 | 云空间权限、目标文件夹与查看者授权、上传/导入流程 | 无自建网络入口时的备选 |
| IM 上传并发文件消息 | 聊天中的附件 | 机器人能力、上传及发送权限 | 附件交付；不等于浏览器页面 |

### 方案一：受控只读 HTTPS 预览

这是本项目的设计建议，不是飞书提供的文件托管接口。

建议链路：本地引用 → 桥接器登记文件 → 生成稳定页面 URL → 卡片普通 HTTPS 链接 → 用户鉴权 → 读取允许的文件并预览。

最小范围：

1. 优先复用已有受控 HTTPS 入口，由反向代理转发至只读预览模块；如果使用现有隧道，需要同样满足设备可达、证书有效、身份访问控制和入口稳定性。反代本身只负责转发，不替代文件服务和鉴权。[NGINX 反向代理][proxy]
2. 链接使用稳定 opaque 文件 ID，服务端维护 ID → 私有本地文件的映射；不把绝对路径直接作为公开的任意读取参数。
3. 登录后授权给当前 owner 或允许查看该任务的用户。优先采用稳定、可撤销的链接；短时签名 URL 可选，但会使旧卡片过期，必须设计续期入口，且持有链接不应被误称为用户身份验证。
4. 只读取显式授权的工作区/文件范围，防止 `..` 和软链接越界；不公开整个 home 或 `/data00`，不向外暴露 Codex daemon。
5. Markdown 安全渲染；代码以转义文本展示并支持行号。原 `:line[:col]` 引用转换成页面的 `#Lx` 定位，列号作为可选定位信息保留。
6. 明确版本语义：首版可显示“当前工作区版本”，后续编辑会改变内容；历史结果若需要固定版本，再采用快照。不能承诺当前文件的旧行号永远准确。

推荐插入点是桥接展示流程中、卡片拆片和字节预算之前：解析并登记符合范围的本地引用，替换目标 URL，再调用现有卡片渲染。`cards` 保持纯渲染，不直接读文件、访问网络或上传；原始回复仍按现有契约保留。

### 方案二：Drive 托管，Markdown 优先导入为云文档

**直接上传文件**：Drive 上传接口把文件放到指定云空间目录，返回 `file_token`。普通上传接口限制非空、最多 20 MB；它与 IM 的 `file_key` 属于不同资源体系。[Drive 上传][drive-upload]

随后可以调用文件元数据接口，以 `doc_type=file`、`with_url=true` 获取返回的 `url`。官方定义其为文件访问链接，适合发送给用户；不应自行猜测或拼接租户域名与 token。[文件元数据][drive-meta]

但“获取访问链接”没有证明 Markdown 会按期望渲染为文档。本次没有上传实际 `.md` 做客户端验证；如目标是阅读研究文档，优先使用官方的 Markdown 导入路径。

**Markdown → 新版云文档**的已核实流程：

1. 官方支持扩展名 `md`、`mark`、`markdown`，目标可以是新版文档 `docx`。[导入概述][import-guide]
2. 先上传源文件，取得源文件 token；官方支持临时素材上传和普通 Drive 上传两种方式。临时素材导入后删除源文件，普通 Drive 上传的源文件保留并占云盘空间。[导入概述][import-guide]
3. 创建导入任务，指定实际扩展名、源 `file_token`、目标 `type=docx` 和目标文件夹，返回 `ticket`。接口是异步的，创建成功不等于文档已经可读。[创建导入任务][import-create]
4. 查询任务，确认 `data.result.job_status=0`，使用 `data.result.url` 作为用户链接；同时检查 `extra`，它可能表示内容截断或部分资源失败。[查询导入结果][import-get]
5. 确保点击者已获阅读权限，再发送链接。可通过协作者权限接口给用户或群组授予 `view`；操作身份必须有相应管理/分享权限，组织策略仍可能限制分享。[增加协作者][permission]

导入 API 可使用 `docs:document:import` 等官方允许的权限；上传、目标文件夹编辑权限和为用户共享是另外的前提，不能把“申请了 API scope”当作“所有目标文档均有权限”。[创建导入任务][import-create]、[增加协作者][permission]

代价：内容复制到飞书，后续本地修改不会因存在 URL 自动同步；相对文件链接、代码行号、图片与复杂 Markdown 的保真度需要另行验证。浏览器登录的是有权查看的飞书账号，不是在地址中携带应用凭据。

### 方案三：IM 附件交付

官方 IM 上传支持 `stream` 类型用于未落入专用类型的文件，返回 `file_key`；单文件不得为空且不超过 30 MB。可用这个 key 发送 `msg_type=file` 文件消息。[IM 上传][im-upload]、[消息结构][message]

附件在聊天中的预览/下载体验与普通网页链接不同，本次未验证 `.md` 的客户端渲染。官方 IM 下载接口 `GET /im/v1/files/:file_key` 要求 `Authorization: Bearer tenant_access_token`，只允许下载当前机器人上传的文件，返回二进制流。[IM 下载][im-download]

因此不能把该 Open API 地址直接贴进卡片当作可用预览链接，也不能把应用 token 放进 URL。若由自己的服务代下载并提供页面，仍需要方案一的用户鉴权、受控入口和内容展示。

## 当前仓库依据

以下为实施前的代码读取与 Git 状态证据，当时尚未实现预览能力。后续用户选择 IP 直连的受控预览，实施情况见[实施记录](feishu-local-file-preview-implementation-2026-10-15.md)。

- [`public_markdown.go:245`](../../internal/cards/public_markdown.go#L245) 匹配本地绝对路径引用；`:247–323` 的 `publicCodeReferences` / `publicCodeLinkLabels` 只为带行列的标签补充行号，保留原目标，不生成网页地址。
- [`engine.go:291`](../../internal/sync/engine.go#L291) 的最终回复路径进入 `cards.SplitMessage`；当前没有本地文件链接登记和公开 URL 转换步骤。
- [`media_upload.go:18`](../../internal/feishu/media_upload.go#L18) 当前仅有 `UploadImage`，未有 `UploadFile`。生产管理入口为 Unix socket，当前未发现文件 HTTP 预览监听器。
- [卡片模块契约](../codex-feishu/08-cards-and-actions.md)明确 `cards` 只渲染，不做 RPC；新增文件读取/上传应留在渲染模块外。
- 用户举例的研究文档在工作区存在且为未跟踪文件（`git status` 为 `??`）。单靠代码仓库网页链接不能打开这个尚未提交的版本。

## 下一步与验收边界

优先确认一个用户电脑和手机浏览器可达的 HTTPS 入口及可用的登录方式，再实施方案一；若没有入口且愿意把文档复制到飞书，选择方案二。

实施后至少验证：从真实飞书卡片点开示例未提交 Markdown；代码行号定位；原始文本与代码中的非链接内容保持原义；未授权用户、越界路径、失效映射不能读文件；桌面/手机打开方式；服务重启后旧链接可用或能明确说明失效。

尚未解决：当前可复用 HTTPS 入口与身份系统、手机网络范围、文件实时版/固定快照的产品选择、云盘 API 权限与分享策略的实际可用性。本文不能作为这些能力已配置或客户端验收通过的证据。

## 官方来源

飞书开放平台网页提供同源 `.md` 原文链接；本次经该官方原文核实字段，未使用第三方 SDK/博客作为能力证据。

[card]: https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-json-v2-components/content-components/rich-text
[im-upload]: https://open.feishu.cn/document/server-docs/im-v1/file/create
[im-download]: https://open.feishu.cn/document/server-docs/im-v1/file/get
[message]: https://open.feishu.cn/document/server-docs/im-v1/message-content-description/create_json
[drive-upload]: https://open.feishu.cn/document/server-docs/docs/drive-v1/upload/upload_all
[drive-meta]: https://open.feishu.cn/document/server-docs/docs/drive-v1/file/batch_query
[import-guide]: https://open.feishu.cn/document/server-docs/docs/drive-v1/import_task/import-user-guide
[import-create]: https://open.feishu.cn/document/server-docs/docs/drive-v1/import_task/create
[import-get]: https://open.feishu.cn/document/server-docs/docs/drive-v1/import_task/get
[permission]: https://open.feishu.cn/document/server-docs/docs/permission/permission-member/create
[browser]: https://www.feishu.cn/hc/zh-CN/articles/360038713913
[uri]: https://www.rfc-editor.org/rfc/rfc3986#section-3
[localhost]: https://www.rfc-editor.org/rfc/rfc6761#section-6.3
[proxy]: https://nginx.org/en/docs/http/ngx_http_proxy_module.html#proxy_pass
