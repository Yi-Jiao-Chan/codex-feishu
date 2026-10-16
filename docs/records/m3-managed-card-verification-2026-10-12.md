# M3：现有实例内的本人测试卡验证

日期：2026-10-12。开发功能，不是一期发布。`verify_managed_card=true`，`verify_managed_tests=false`；卡片真实 UI 与正式部署尚待验收。本轮只使用临时数据库、模拟飞书网关和测试 Unix socket，没有读取真实密钥、发送飞书消息 / @、创建群、提交 Codex 输入、开启真实接收器或替换 / 迁移 M0。

## 行为与实现

CLI 在已有匹配管理实例内，只接受单独的 `--allow-test-card --request-id ID`。调用前保留随机 32 位小写十六进制 ID，避免响应丢失后找不到原操作。只读查询同 ID 不带 allow 标志；没有支持能力、没有管理实例或同时请求尚未接入的群 / 线程时，不部分发送。旧 schema 1 准备能力保持既有锁与边界，新的 request ID 不会回退到旧独立连接。

同用户 Unix 通道只新增 `verify.card.begin / verify.card.state`，没有任意消息目标、正文、原生 RPC 或 shell 接口。Manager 固定当前配置的 owner；测试卡由已有 Gateway 发送，回调与私聊 `/verify` 命令通过原接收器路由。不会唤醒线程、创建任务收据或输入原生队列；非 owner、其他 token、错误 message ID 与过期事件不通过。

schema 18 的专用表在发送前保存原构建 commit、实际运行二进制 SHA-256、UUID、token、完整卡与固定 5 分钟到期。CAS 只领取 ready；发送前已有 dispatching，崩溃 / 响应未知不再次发送。明确拒绝、成功和过期记录保持不变，不延长令牌、不改 UUID。回调可能先于 HTTP 响应，先保存原 message/chat ID，只有与 confirmed send ID 相同才报告组件 pass。取消客户端后仍用有界本地提交保存已知结果；提交失败留下 dispatching，对外显示 unknown。serve 只在 Linux 从 `/proc/self/exe` 对正在执行的 inode 计算一次 SHA-256，不读可能已被升级替换的启动路径；失败明确停止启动，不降级成 commit 标签。即使两个构建都标为 `development` 或同一 HEAD，实际二进制摘要不同也不领取旧构建留下的 ready 记录，原记录查询保留其 build commit，并报告 build_changed；其他构建的组件证据不报告为当前构建 observed。

只读 journal 查询在匹配实例下可提供既有证据，即使另一个诊断 degraded；保留其非零退出码。正常前置检查失败不允许启动探针。公开 DTO 不含 token 或卡正文；验证记录与业务健康分开，不将测试未决结果当作任务输入重试。

卡片发送、回调、私聊三个组件通过时，状态为 `verification-managed-card-observed`；`ok=false`、退出 5、全部 12 项 P1 仍 pending。组件记录证明实际接收路径，不证明卡片布局、App 回显、自动建群或整套配置体验。unknown 不由“查询未找到”推定 rejected，也不自动创建新请求。

## 本地验证

测试覆盖 12 个并发重放只发送一次、发送前 journal / UUID / 卡已提交、回调先到、私聊入站先到、取消 / unknown / rejected / 崩溃后重开不重发、owner / token / message ID / 到期边界、相同 commit 标签但二进制变化的 ready / confirmed 两种记录、真实当前可执行文件摘要、schema 17 升级保留旧操作日志、claim / finish 不覆盖已确认结果和私有 DTO 不序列化。

服务集成测试通过真实本地 Unix socket 调用 Manager，并经 Service 唯一模拟接收器提交真实结构的 SDK 回调 / 私聊事件；随后查询原 journal，只有一张 owner 卡、一个 listener、没有测试群或任务收据。CLI 测试覆盖显式 ID、只读 / begin 路由、缺能力、尚未支持的组合不部分发送、管理错误脱敏与 P1 不被组件观察提升。真实飞书与 App UI 验收后再记录对应证据。

验证命令：全仓 `go test -race ./...` 和 `go vet ./...` 通过；独立只读检查发现 commit 标签不足以区分实际二进制，已以上述摘要修正，并补跑受影响包的 race 测试。真实 UI 和完整发布矩阵没有因此提升。
