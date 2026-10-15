# 14 本地文件网页预览

将 Codex 公开回复里的本地文件引用转换成浏览器链接。文件留在原工作区，预览服务按当前文件内容读取；Markdown 显示排版，代码提供行号。功能默认关闭，不需要飞书云盘权限。实现位于 `internal/filepreview/`，同步接线在 `internal/sync/`，监听生命周期由现有 `serve` 管理。

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

也可由部署所有者私聊机器人发送 `/preview`，机器人回复可复制的浏览器访问口令。在任务群或话题中使用会提示转到私聊；其他操作者、通知接收者和陌生人无法取得口令。命令不进入 Codex 原生会话，不新建任务。`/help` 在预览启用时列出入口。

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

可显式登记一个文件，以便发送既有文档链接：

```sh
codex-feishu file-preview link --config /absolute/path/config.toml \
  --file /absolute/path/project/docs/report.md --json
```

返回 `data.url`；代码可传 `--file /absolute/path/project/main.go:42:5` 保留行号定位。登记命令不发送飞书消息，不修改原文件，不表示用户设备已能访问。正常任务输出由同步模块自动转换，无需手动登记。

## 访问边界与恢复

- 只有经过登记且位于显式范围内的常规文本文件可读；没有目录列表、任意路径读取、写入或执行接口。
- 文件映射以稳定 opaque ID 命名，保存在本地 0700 目录的 0600 记录中，先原子持久化再把链接交给消息 outbox。重启沿用映射，CLI 和服务可并发登记。
- 文件读取通过 `os.Root` 约束，拒绝隐藏/凭据文件、路径越界、软链接逃逸、二进制与超限内容。访问时再次校验，登记后的文件替换不能突破边界。
- Markdown 禁止原始 HTML 和危险 URL；源码以转义文本显示。原生历史、用户输入和原文件不因展示转换而改变；代码围栏、行内代码和普通网站链接保持原义。
- 浏览器使用口令登录和受签名保护的 cookie；HTTPS origin 使用 Secure cookie。随机文件 ID 不能替代登录。口令文件变更后须重启桥，原会话和原 ID 失效，新的同步会使用新链接。
- 页面展示当前工作区内容，后续编辑会改变文件及行号。文件删除、权限改变或超限时旧链接明确不可读；首版不提供历史快照。

## 验收

自动测试覆盖鉴权、路径边界、Markdown 安全展示、登记持久化、重启、链接语法及同步分片。doctor 只核验本地配置和私有口令，报告 `ui_verified=false`；不会探测陌生网络地址或把本地监听当作飞书点击已通过。

真实验收至少包括：从飞书打开未提交 Markdown；输入口令后阅读全文；代码行号定位；电脑/手机网络；浏览器会话；桥重启后旧链接。未授权请求、越界文件、失效映射不得得到文件正文。未取得用户实际点击证据前，客户端验收保持 pending，不改变一期发布结论。
