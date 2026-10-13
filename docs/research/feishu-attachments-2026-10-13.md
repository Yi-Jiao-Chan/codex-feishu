# 飞书附件展示：接口与实现边界

核验日期：2026-10-13。只读研究；未调用真实上传或消息写接口。官方页面正文通过其公开 `.md` 版本读取。CodeGraph 无状态文件、无索引，按仓库规则直接读取源码。App 包装解析、附件来源与安全读取由主任务负责。

## 当前实现

- `internal/feishu/gateway.go:61` 的 `request` 使用官方 SDK `core.Request`，保留安全错误元数据；目前只有卡片发送、回复、更新及群/消息/反应操作，没有 image/file 上传或文件消息发送。`SendCard` 在 133 行，`ReplyCard` 在 246 行。
- `internal/cards/cards.go:36` 的 `Message` 只放 Markdown 与 footer；没有附件图片/文件组件。现有卡片是 JSON 2.0。
- `compat/feishu-permissions.json` 没有 `im:resource` 或 `im:resource:upload`。清单不是实时授权证据；本任务按当前 App 只有消息/群等授权的输入，不假设上传可用。
- `go.mod` 锁定官方 Go SDK v3.12.0。其 `service/im/v1/resource.go:1068,1131` 已有 file/image Create，均通过 `core.WithFileUpload()` 发 multipart。SDK `core/reqtranslator.go:235–252` 的 `NewFormdata().AddField().AddFileWithName()` 可直接作现有 gateway.request 的 body；translator 检测 `*Formdata` 自动进入上传分支。因此可以保留现有鉴权与错误处理，无需另起 HTTP 客户端。传普通 JSON map 内含 reader **不会**自动成为上传。

## 最小可执行链路与权限

开启机器人能力；图片与文件上传均要求 **`im:resource:upload` 或 `im:resource` 任一项**。较窄上传权限足以支撑本需求。发送权限和资源权限是两项独立能力：消息发送要求 `im:message:send_as_bot` / `im:message` / `im:message:send` 任一项；已有发送权限不代表能上传。上传与消息 API 都支持 tenant/user token，现有 gateway 使用 tenant token 可沿用。[上传图片](https://open.feishu.cn/document/server-docs/im-v1/image/create.md)、[上传文件](https://open.feishu.cn/document/server-docs/im-v1/file/create.md)、[发送消息](https://open.feishu.cn/document/server-docs/im-v1/message/create.md)。

上传不是 JSON 请求。`POST /open-apis/im/v1/images` 使用 multipart 字段 `image_type=message`、`image=<binary>`，成功响应 `{"code":0,"data":{"image_key":"img_v3_example"}}`。`POST /open-apis/im/v1/files` 使用 multipart 字段 `file_type=stream`、`file_name=example.txt`、`file=<binary>`，成功响应 `{"code":0,"data":{"file_key":"file_v2_example"}}`。[图片 API](https://open.feishu.cn/document/server-docs/im-v1/image/create.md)、[文件 API](https://open.feishu.cn/document/server-docs/im-v1/file/create.md)。

图片可以直接附在人类卡中，最小卡片内容：

```json
{"schema":"2.0","body":{"elements":[{"tag":"img","img_key":"img_v3_example","alt":{"tag":"plain_text","content":"附件图片"},"preview":true}]}}
```

`img_key` 是上传返回的 `image_key`；默认允许点击放大。本地路径、`file://`、base64 data URL 或任意 HTTP 图片 URL 都不能当 `img_key`。[图片组件](https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-json-v2-components/content-components/image.md)。

文件没有已公布的 Card JSON 2.0 原生 `file` 组件：官方完整组件列表包含 img、markdown、button 等，不包含文件组件。因此不要构造未经支持的 `{"tag":"file","file_key":...}`。稳妥方案是关联人类卡旁发送独立文件消息，其飞书客户端提供文件点击体验；网关/出站协议须另加文件发送或回复能力。最小发送请求为 `POST /open-apis/im/v1/messages?receive_id_type=chat_id`：

```json
{"receive_id":"oc_example","msg_type":"file","content":"{\"file_key\":\"file_v2_example\"}"}
```

若文件必须从卡片点击，则需应用提供一个用户可访问、有适当访问控制的 HTTPS 地址，再用 `button` 的 `behaviors:[{"type":"open_url","default_url":"https://example.invalid/attachment"}]` 或 Markdown 链接；上传返回的 `file_key` 本身不是下载 URL。不得把宿主本地路径变成卡片跳转地址，也不得把需要 Bearer token 的 OpenAPI 下载端点当用户可直接打开的公开链接。[组件列表](https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-json-v2-components/component-json-v2-overview.md)、[文件消息内容](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/im-v1/message/create_json.md)、[按钮](https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-json-v2-components/interactive-components/button.md)。本任务没有核验通用文件预览 URL/消息跳转接口，不能承诺只靠 file_key 做卡内下载。

## 硬限制与建议

| 项目 | 官方接口要求 |
| --- | --- |
| 图片大小 | 非空，≤10 MB |
| 图片格式 | JPG、JPEG、PNG、WEBP、GIF、BMP、ICO、TIFF、HEIC；TIFF、HEIC 转 JPG |
| 图片分辨率 | GIF ≤2000×2000，其余 ≤12000×12000；头像 ≤4096×4096（本需求不用头像） |
| 文件大小 | 非空，≤30 MB |
| file_type | opus、mp4、pdf、doc、xls、ppt、stream；其他类型用 stream；file_name 必须带后缀 |
| 卡片/富文本消息请求体 | ≤30 KB；模板内容也计入，样式可增加实际体积 |
| 图片/文件上传限频 | 1000次/分钟、50次/秒 |
| 同一群消息 | 群内机器人共享5 QPS |
| Card 2.0 客户端 | 飞书7.20及以上；更旧版本显示升级提示 |

来源：[图片 API](https://open.feishu.cn/document/server-docs/im-v1/image/create.md)、[文件 API](https://open.feishu.cn/document/server-docs/im-v1/file/create.md)、[消息 API](https://open.feishu.cn/document/server-docs/im-v1/message/create.md)、[Card 2.0 组件列表](https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-json-v2-components/component-json-v2-overview.md)。官方没有在 file_name 字段列出最大字符数，本次不编造限制。

图片组件另外**建议**图片在1500×3000 px范围、大小≤10 M、高:宽≤16:9。这是显示清晰度建议，不应误当上传接口的硬分辨率限制。[图片组件](https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-json-v2-components/content-components/image.md)。

## 持久 remote journal 可复用边界

`internal/store/store.go:92–118` 的 `Begin/Finish/Operation` 与 `m0_operations` 是通用 key/kind/state/remote_id 记录，schema `internal/store/migrations.go:52` 没有 kind/state 枚举约束。可以复用它为附件上传持久记录已知结果及 image_key/file_key；`Begin` 对任何已有记录（含响应丢失）均返回 false，适合禁止未知上传盲重试。建议操作键包含应用身份、确定的附件身份/内容摘要，避免不同应用复用资源 key；不要把真实路径放进日志/普通诊断键。

这是可实现性判断，不是现有附件能力。现有 `outbound_attempts` 在 `migrations.go:107` 只允许 send/reply/update，内容字段是 card_json；`internal/sync/outbound.go:147–157` 只调 Card 方法。不能把上传直接塞进现有 mirror attempt 而不改变 schema/dispatch。最小图片实现可先完成单独资源 journal，再由原有卡片 journal 发送包含 img_key 的卡片。成功上传但本地 Finish 失败时保留未知态，不重传；读取确认 image_key 后复用。文件独立消息需额外投递类型和相应恢复规则。未知上传目前没有已核验的按幂等键读回恢复 API。

## 验收边界

需先验证资源权限缺失时保留正文及简短附件占位，不暴露包装/路径；已知上传可重用 key；非空/大小/格式限制在发送前生效；网络超时或成功后落库失败不盲重传；img_key 卡片真实预览和文件消息真实点击必须另做授权联调。本研究没有真实飞书 UI 或上传验收证据。
