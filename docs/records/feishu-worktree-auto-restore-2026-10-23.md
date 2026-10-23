# 飞书输入触发原工作树快照恢复

日期：2026-10-23。状态：实现、自动验证和本地候选部署完成；两条既存输入已自动恢复原工作树并进入原生历史，真实 App 和飞书客户端 UI 验收 pending，`phase1_released=false`。

## 问题与结果

工作树回收后，飞书输入已保存并显示 GET reaction，但 Runtime 在附着原线程、校验 cwd 时失败，无法进入提交器。仅修复新建工作树的归属不能恢复已消失的检出。

本次在附着原线程前、以及已附着线程的每次处理前检查普通待提交输入。只在原 cwd 缺失且原始工作树身份、绑定和快照都可验证时恢复，然后让原有输入控制器继续 `thread/queue/add`。收据、client ID、线程、群和相对子目录保留。恢复进度或阻塞通过原 outbox 更新引用原飞书消息的管理卡；没有快照时保留消息，不把当前分支 HEAD 当作恢复内容。

升级前已收到、确认从未提交的普通输入也可使用此流程。原生 attempt、queue/history/M0 写入、创建首条输入及异步答案均不补授权；collecting 附件继续阻挡后续普通输入。只有历史同步唤醒时不主动恢复。

提交前的写入门禁另行 fresh 读取元数据、检查 cwd，发现 actor 检查后发生回收时不建立原生 attempt，交回恢复。真实 RPC 期间仍可能发生外部清理或响应丢失，保持原生 unknown 核验，不据此盲重发。

## 已核验机制与兼容范围

[官方工作树说明](https://learn.chatgpt.com/docs/environments/git-worktrees)说明托管清理前保留快照，并可在重新打开聊天时恢复。用户提供的 App 恢复证据明确快照位于源仓库 `refs/codex/snapshots/<SHA-1(原始根路径字符串)>`，以 detached HEAD 还原。哈希必须保留原始字符串，不先 canonicalize；快照包含的工作内容会成为提交后的检出，不保留原 staged/unstaged 区分。

现有已核验 App Server 为 0.162.1，桌面应用为 26.1016.62342；协议目录未提供通用工作树恢复或附件状态转换 RPC。实现沿用[归属兼容层](managed-worktree-ownership-2026-10-23.md)与[兼容清单](../../compat/codex-worktrees.json)，不启动新 app-server 或 exec resume，不修改全局配置和原会话策略。实际 codexHome 来自同一 daemon initialize。

原位置限于 daemon 的 `codexHome/worktrees/<bucket>/<checkout>`。桥创建线程使用已确认 creation intent；App 创建线程健康时保存不可变身份，既存缺失工作树可依据先前已授权 observation 的源仓库、准确 cwd 和唯一快照回填。当前操作者、绑定、原生 source/cwd、Git common-dir 与目录授权持续核验。历史数据库同级工作树不在本次自动恢复范围内。

## 恢复与中断边界

schema 34 新增三张表，分别保存工作树身份、普通消息恢复授权和恢复尝试。普通输入与授权同事务入账，恢复参数在任何文件操作前冻结，每线程最多一个 pending 尝试；原有会话设置、输入、出站与 unknown 日志保持。

1. 冻结准确 snapshot commit、原根和相对子目录，在未发布的命名 bucket 检出；源 sparse 规则遗漏 cwd 时，只物化快照中的该子目录。
2. 在私有收据与数据库保存新物理代际，fresh 解析 Git 路径，登记实际原线程 owner 并读回。
3. 原子移动回原根，目标已有文件或目录时拒绝覆盖；只修复该次检出的专属 Git 回指，不运行全局 repair 或 prune。
4. 记录 restored 后，原控制器提交原输入。命名 bucket 等真实 user item 可见、线程活跃或该轮终态后发布；旧四位 hex/时间戳路径原本就在 App 扫描范围内。

每个写阶段先持久记录。重启和数据库结果保存失败后，只读核验准确路径、私有代际收据、物理身份与双向 Git 指针；不能证明时保留 unknown，不重试 add 或创建替代线程。源快照 ref 后续变化不会改变已冻结的 commit。恢复后正常编辑、提交不改变合法代际；同路径重建或 Git 元数据槽位复用不能冒充原结果。

恢复中取消消息只取消输入。后续文件写阶段再次检查收据，已发生的恢复按事实记录；完整的未移动阶段可保留临时目录并结束该取消尝试，后续普通输入继续处理。已完成恢复但原消息取消时，严格更晚的普通收据必须真实出现在原生历史，才能完成延迟发布。旧消息不会为发布标记而重新发送。

原创建 birth、marker 与日志不改写。已确认恢复以单独的新代际证明参与 Runtime 授权及存量审计，旧审计计划不能作用于恢复后的检出。

## 验证

下列自动测试使用合成身份、SQLite fixture 和临时真实 Git 仓库；部署后实际业务触发的恢复另列于下节。

| 范围 | 结果 |
| --- | --- |
| 精确路径哈希、快照内容、根/子目录、sparse 物化与 frozen commit | 专项通过 |
| 原线程 owner、新代际、并发占用、同路径替换、元数据槽位复用 | 专项通过 |
| 原收据只提交一次、附着前恢复、已附着后回收、无输入不恢复、缺快照回复卡 | 专项通过 |
| 崩溃后核验、结果保存失败、取消后不重发与后续消息延迟发布 | 专项通过 |
| schema 33→34 保留旧输入、原生 unknown、模型设置和表单 | 专项通过 |
| `go test ./cmd/... ./internal/... ./compat` | 通过；保留已集成的模型设置功能 |
| `go test -race ./internal/app -run '^TestWorkspaceRestore' -count=1` | 通过，29.732 秒；含稳定阻塞 queue 快照的清理竞态 |
| `go test -race ./internal/store -run '^TestWorktreeRestore' -count=1` | 通过，10.440 秒 |
| `go test -race ./internal/app ./internal/creation ./internal/store` | 通过；app 517.528 秒、creation 296.595 秒、store 201.770 秒 |
| 文档链接与 diff 检查 | 通过，98 文件、1100 链接；无空白错误 |
| Darwin 原子移动 helper 独立交叉编译 | 通过；creation 完整包交叉编译被已有 attachments/management.go 的 `unix.O_PATH` 阻塞，不宣称 Mac 整包构建通过 |
| 真实 App / 飞书 UI、桌面附件展示 | pending |

## 部署与限制

本分支已 rebase 到 `origin/main` 的 `96572737e3e791fbb4470697b9fd90acc939b5c5`，保留 schema 33 会话设置功能。部署构建为 `c816b561399193ab6acf193fdde29f218045476d`，二进制 SHA-256 为 `512a43a50c43823f8b7976204d30701e56c943db5b7f1568f49622b83682db85`。rebase 前后源码树完全相同，复用上述全仓及 race 结果；本次构建、文档链接与 diff 检查通过。

先使用 SQLite 一致副本演练，再核对原进程、unit、配置与二进制摘要，停止原桥。持应用及数据库监听双锁核验配置事务后，保存不可覆盖的 0600 一致备份和 prepared 日志，通过标准存储迁移升级到 schema 34。实际迁移前后 59 张旧表、179,623 行数据、索引和其他原 schema 对象均保持一致，新增 3 张恢复表为空，完整性与外键检查通过。旧二进制、配置、unit、数据库备份及迁移证据保留在仓库外私有目录。

沿用原 `codex-feishu.service` 安装并启动候选，PID 1532952，systemd active/running/enabled；进程、unit 与稳定 CLI 均指向上述构建，配置字节保持不变。复用原共享 daemon，两侧连接与发现基线通过；104 个普通绑定中，当前代际已恢复的绑定由 61 增至 66。doctor/status 仍仅有原来的 `instance.bindings_reconciled` 与 `instance.operations_known` 两项 required pending，未增加 required fail/pending，历史 unknown 数量未增加，不能称为全部健康通过。

启动后的正常业务处理确认两次恢复：均为 confirmed/restored，原根和 cwd 存在，两条原输入均为 consumed，各自只有一次 confirmed add 和一条原生历史关联。未额外创建测试群、线程或发送测试输入。这是服务及持久记录证据，不替代飞书消息显示、原 App 聊天继续或附件状态的人工 UI 验收。

旧版本不能理解 schema 34 或恢复代际，不能直接回退二进制或用旧备份覆盖后续会话活动。

桥恢复文件、Git 注册与原线程归属，但不操作桌面私有 `archived_worktree → worktree` 附件或关联 PR 状态。它也不能撤销已入队的桌面删除任务、刷新桌面缓存的旧 gitdir，或自动处理残留同路径 Git 注册。这些情况明确保留阻塞，不宣称完成 App 内部所有恢复步骤。真实验收需检查原聊天继续、未提交快照内容、原消息只执行一次、失败回复卡，以及客户端工作树展示；自动测试和平台读回均不替代这些 UI 证据。
