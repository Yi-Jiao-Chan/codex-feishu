# 群描述中的会话工作目录（2026-10-13）

飞书建群请求 `POST /open-apis/im/v1/chats` 支持字符串 `description`；官方建议不超过 100 字符，没有将此建议列为硬长度校验。SDK v3.12.0 的建群、更新群请求体也提供 `Description *string`。目录应完整保留，不能截断或用项目根替换 worktree 的实际 cwd。[创建群](https://open.feishu.cn/document/server-docs/group/chat/create)、[官方 SDK 请求模型](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/service/im/v1/model.go)

改动前，`internal/feishu/gateway.go` 的建群描述固定为“跟进同一个 Codex 会话的任务与公开回复”，未携带工作目录。实际会话目录来自已附着的 `codexclient.Thread.CWD`；`thread_observations.cwd` 可以随发现更新，`ProjectRoot` 仅用于目录授权，不能充当实际 cwd。原建群意图仅冻结名称、owner、成员及 UUID，并未冻结 cwd。描述也必须在远程调用前持久冻结，明确限流后的重启重试沿用原 UUID 和描述，未知响应保持不重发。

已有群可按如下路径修正，操作只针对已经核实的绑定 chat ID：

1. `GET /open-apis/im/v1/chats/:chat_id?user_id_type=open_id`，保存原描述并核对 `chat_mode=group`、群名、owner、编辑权限和绑定对象。GET 支持 `im:chat`、`im:chat:read`、`im:chat:readonly` 任一权限；完整群信息要求机器人仍在群内。[获取群信息](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/chat/get)
2. 持久记录目标、原描述和待写描述，单次 `PUT /open-apis/im/v1/chats/:chat_id?user_id_type=open_id`，请求体仅为 `{"description":"工作目录：<actual cwd>"}`。官方明确未传参数保持原值，因此不携带群名、owner、成员及其他配置。PUT 支持 `im:chat` 或 `im:chat:update`；同时受群内身份及编辑权限限制。机器人创建群且具备 `im:chat:operate_as_owner` 时可更新全部信息；普通成员需群设置允许成员编辑。[更新群信息](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/chat/update)
3. 再次 GET 比对 description，确认群名及其他核对字段仍相同。超时或未知响应先 GET 核验，不盲目重发 PUT。

文档读取注意：`server-docs/group/chat/update.md` 和 `get.md` 在本日返回的是群发言权限接口，不能作为群描述的更新/读取依据；以上采用官方 `reference/im-v1/chat` 的实际文档。未执行真实群写入；真实飞书客户端显示和超过建议长度的路径尚未验收。

开发版已新增 schema 23 的冻结描述与建群传参：先 Attach、核验实际绝对 CWD，再冻结 `description`、领取 attempt 和发送请求。M0 原建群接口保留固定描述。迁移对旧已有 attempt 保留固定描述，只允许未曾尝试的 ready 意图首次填入工作目录。

隔离验证：`internal/bindings`（含 race）、`internal/store`、`internal/feishu`、`cmd/codex-feishu` 全量通过；覆盖附着 cwd 与发现目录不同、完整 Unicode 长路径、重启后限流重试请求冻结、22→23 迁移与 unknown 保留。相关 `internal/app` 群创建/发现用例通过；app 全量的既有受控绑定过程卡用例仍在等待进度卡确认时超时，尚未归因，不能宣称全仓验收通过。真实数据库迁移与既有群读回更新由主任务另行执行。
