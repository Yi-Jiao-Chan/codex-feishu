# M3：现有实例内的本人测试群验证

日期：2026-10-12。开发功能，不是一期发布。`verify_managed_group=true`，`verify_managed_tests=false`。本轮只用临时 SQLite、模拟飞书网关和本地测试 Unix socket，没有读取真实密钥、创建真实群、发消息 / @、调用 Codex 输入、启动真实接收器或迁移 / 替换 M0。

## 新增行为

`--allow-test-group --request-id ID` 创建固定 owner 与 bot 的验证群，不继承生产额外群成员。它不发送卡 / 消息、绑定线程或创建 Codex 会话；`--allow-test-card` 仍只授权独立本人私聊卡。两个标志可组合，CLI 在第一项 mutation 前核验全部请求能力，受控线程未接入时不部分执行。测试实例仍由同用户管理通道、唯一接收器及原 Gateway 承担。

schema 19 保存原名称、成员 JSON、UUID、owner / app / host、构建 commit、实际运行二进制 SHA-256 与 5 分钟窗口。发送前 ready→dispatching CAS 已提交，只有匹配构建的原 ready 可离开；响应未知、rejected、confirmed、崩溃遗留 dispatching 均不再次创建。取消后仍有界保存已知远程结果。旧记录、时间、UUID 与首次观察不被重复调用替换。当前新 schema 与生产 schema 1 实例保持分开。

确认 chat ID 后，以 GET 核验 bot 在群、owner 出现在完整成员名单且无其他人员。成员 GET 出错、missing has_more、空身份、分页受限或额外成员均不通过；同一授权请求可以重做 GET，不重开创建。成员列表读取已修正为不将缺少分页信息或未知身份当作完整名单。

本人在原 confirmed 群 @ 机器人发送 `/verify-group ID` 时，仅记录该探针入站。sender=user、bot mention、owner、request ID、app / host 和确切 chat 均须匹配，过期拒绝。消息可以先于成员 GET 成功，只有两份观察都存在才显示组件 pass；这证明窗口内分别取得入站与成员证据，不证明群消息那个瞬间的全量成员名单。验证命令不会进入普通命令或 native receipt，也不会生成 binding。

不带 allow 的 `--request-id` 查询只读实例已有卡 / 群日志，缺项 skipped，全部无记录为 not-found，不启动任何组件。不用查不到记录 / 群名推导平台拒绝或自动新建。owner scope、旧构建和过期记录均保留，`build_changed` 明确标记历史证据。

group / components 的 observed 仍 `ok=false` / 退出 5、12 项 P1 pending：建群组件不能代替 App 发起自动发现、同线程输入、真实 UI、Agent 首装或服务生命周期。

## 本地验证

存储测试覆盖 schema 18→19 保留原卡和 M0 操作、不可变成员 / UUID / 名称 / 期限、scope 冲突、并发 CAS 单领取、结果不回退、原 chat / owner / 构建 / 期限与首次观察保存。Manager 测试覆盖 12 并发重放、发出前已落账、取消 / unknown / rejected / crash 重开不再创建、缺 owner / bot、额外人员与成员 GET 失败只恢复读取、相同 commit 不同二进制不领旧 ready、过期和脱敏 DTO。

服务集成通过真实本地 Unix socket 调用卡与群管理器，由同一模拟接收器交付 SDK 结构的私聊、回调和群 @；一个 listener、一张本人卡、一个验证群，没有原生收据或 group binding。控制通道拒绝混合 pair / card / group 请求、不同部署 identity；CLI 测试覆盖独立授权、组合前置检查、只读查询缺项不发送、未支持 thread 不部分执行及组件不提升 P1。真实飞书 / App 验收待集中执行。

验证：`go test -race ./...`、`go vet ./...`、`git diff --check` 全部通过。独立只读检查未发现额外授权、重复创建或假 P1 通过的问题；成员与入站观察的时间边界已明确写入本指南。
