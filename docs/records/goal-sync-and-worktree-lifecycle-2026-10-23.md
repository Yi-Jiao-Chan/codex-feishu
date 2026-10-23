# Goal 续跑同步与工作树生命周期修复

日期：2026-10-23。适用：共享 daemon 0.162.1、桥 schema 31。状态：实现完成；合成回归及真实只读来源/历史投影已验证，客户端 UI 保持 pending。对应 [01 客户端](../codex-feishu/01-codex-client.md)、[02 存储](../codex-feishu/02-store-and-recovery.md)、[05 同步](../codex-feishu/05-message-sync.md)和 [09 创建任务](../codex-feishu/09-feishu-task-creation.md)。

## 断点与证据

合法 goal 自动续跑没有新的 `userMessage`，启动记录明确为 `turn_attribution.turn_trigger=goal`。旧同步器要求所有公开轮次都有用户输入或既存回复锚点，因而整份历史不能入库，后续普通轮次也被阻断。v2 的 `rootTurnId` 自指不构成自动续跑证明。

只读核验复用现有 daemon 和原线程，没有启动、引导或修改原生任务。故障会话完整历史共 34 轮，读取器确认其中 1 个 goal 续跑；新的纯渲染通过，生成 1 个直发过程文档。来源扫描约 716 ms，未变化的再次核验约 33 ms。历史含一条超过 5 MiB 的工具输出，因此 goal 读取器采用 32 MiB 单行上限，不沿用问答结果读取的 4 MiB 上限。真实正文和原始记录不进入仓库。

工作树调查发现：22 个工作树创建 workflow 均为 confirmed，其中 11 个 checkout 已缺失，缺失目录的父目录中仍有桥私有 marker 和 App 的 `.codex-worktree-name`。一处故障目录的桥 marker 创建于 15:09:39，15:19:08 出现 App 标记和路径对应的 Git snapshot，16:06:53 原 checkout 与 Git 管理目录被重建。快照引用的键与该 checkout 路径的摘要一致；本次调查未执行恢复。

桥生产代码只有创建和核验路径，没有删除工作树的调用。上述证据确认 App 快照生命周期已经介入桥创建的目录，但没有取得实际删除事件，不能确定删除者、准确删除时刻或是否触发工作树数量上限。[官方工作树说明](https://learn.chatgpt.com/docs/environments/git-worktrees)描述了 App 托管工作树的清理、快照与恢复；它不能替代本机触发证据。

## 修复

- Goal 来源仅从 daemon 指定、可信目录下的同 session/turn 唯一启动记录取得。缺失或歧义继续拒绝，不借前一轮的用户锚点。过程和最终卡使用明确的 Direct 模式；群直发、话题保留原根，后续 steer 和重启不改变投递方式。
- Direct 复用空父字段持久编码，schema 仍为 31。普通未标 Direct 的无父 AI 镜像继续拒绝，outbox、收据、unknown 核验和水位的事务边界不变。
- 新工作树回到桥数据库同级 `worktrees/<draftID>/checkout`，避免桥的持久目录与 App 托管目录混用。旧冻结路径和请求参数原地核验，不搬迁线程或从初始 commit 自动重建缺失目录。当前部署已允许该预览根，不需要扩大配置。

## 验证边界

合成测试覆盖 goal 连续续跑、正常轮次交错、steer、重放、来源拒绝、超长无关工具输出、可信文件读取、缓存重新核验、Direct 持久化与 CoT/topic 投递，以及旧工作树 unknown/confirmed 原地核验和外来目录拒绝。

- `go test ./cmd/... ./internal/... ./compat`：通过全部业务测试。
- `go test -race ./internal/codexclient ./internal/sync`：通过来源读取与同步并发检查。
- `python3 scripts/check_docs.py` 与 `git diff --check`：通过。

真实只读来源与纯投影不能替代飞书消息投递或客户端 UI。缺失旧工作树仍需逐个核验对应的恢复快照；新目录规则不等于旧目录已经恢复。完整一期发布状态不因本修复变为通过。
