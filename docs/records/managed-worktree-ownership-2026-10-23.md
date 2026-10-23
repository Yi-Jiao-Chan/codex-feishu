# 托管工作树归属登记、清理竞态与存量修复

日期：2026-10-23。适用：Linux 桥、App Server 0.162.1、桌面应用 26.1016.62342；实现基线为桥 schema 31。本文记录文件格式兼容层，不把桌面 Git worker 当作原生 RPC。当前实现、回归与库存证据分开列示；真实桌面清理 UI 尚未验收。

## 已确认的故障与版本证据

用户提供的本机记录确认：一处飞书创建的工作树在 15:13:41、15:19:09（Asia/Shanghai）被桌面自动清理成功删除，reason 为 startup-cleanup，force 为 true，当时聊天仍在执行。16:06:53 桌面由原路径快照恢复。桥的私有创建 marker、聊天 cwd 和 Git 注册均不能代替桌面读取的 `codex-thread.json`，旧创建流程缺少该登记。

恢复后，原路径对应的实际 gitdir 没有 owner 文件；该聊天 ID 出现在另一个工作树已经复用的旧 gitdir 槽位，写入时间与恢复一致。这证明出现了归属错位。桌面源码中的 `Q$` 按 checkout/host/git-path 缓存路径，恢复后调用 `n2` 写 owner；本次没有取得当时缓存内容，因此不能把缓存命中认定为此次串写的已证实原因。

经用户授权，从本机已安装 App 提取源码，只读核对文件与 app.asar 中内容一致。版本与四份源文件 SHA-256 保存在[兼容清单](../../compat/codex-worktrees.json)。关键语义如下：

- `jje` 扫描四位 hex bucket 时不要求发布标记；合法命名 bucket 要求普通文件 `.codex-worktree-name`。旧格式 `YYMMDD-NNNN-xxxxxxxx` 子目录还有独立扫描分支，故新创建的仓库名需避开该格式（大小写均覆盖）。
- `Mn` 保护已登记 owner 的置顶或 active 线程，普通 idle 按更新时间与 keepCount 参与清理。ownerless 的一小时保护依赖有效 birthtime；远程记录可能为 null，不能用它覆盖创建窗口。
- 主清理循环缓存 owner ID，删除前只重读这个 ID 的线程状态，不重新读取 owner 文件。晚补 owner 不能撤销已经选中的 ownerless 候选。
- `B1(force=true)` 在 Git remove 正常返回失败后仍调用 `YDe`，后者执行递归 `fs/remove`。因此 Git lock 不能作为该版本的创建保护。
- `hasOtherAttachmentOwner` 只对另一个未归档 owner 返回保护；给同一个新线程登记附件不能补齐首次输入前的空档。本修复不增加附件或伪造保护线程。

实际 0.162.1 生成的 170 个 ClientRequest 方法中没有 managed worktree 创建/归属方法；447 份 schema 的聚合摘要与现有协议清单一致。`set-worktree-owner-thread` 是桌面 worker 操作，未作为 App Server RPC 调用。主机配置的 keepCount 差异另行保留，不通过改数量修复归属。

## 创建与发布协议

新路径始终在真实 daemon `initialize.codexHome/worktrees` 下，使用唯一的 `codex-feishu-<draftID>` 命名 bucket。它在创建期间可以被目录枚举，但尚不满足该版本的工作树扫描条件。没有永久迁出管理目录、永久保护、统一置顶或配置开关变更。

1. 冻结源仓库、commit、最终路径，持久记录 `worktree` dispatch，再执行一次 Git add。
2. 核验双向 Git 指针和完整检出，将物理代际摘要写入私有 identity 收据及 attempt result。缺失该收据的 unknown 不重跑 Git add。
3. 使用原共享连接 `thread/start`，保存经核验的实际 thread ID。每次归属登记重新解析 `--git-path codex-thread.json`，不缓存 gitdir，也不由仓库名推算槽位。
4. 在 `worktree-owner` dispatch 后，核验 checkout/`.git`/gitdir/common-dir 的物理身份、注册路径与双向指针；通过已打开的目录句柄，以 create-only 原子链接写入 version 1 owner，再 fsync、fresh 核验与读回。已有相同 owner 幂等；冲突、符号链接、损坏记录拒绝覆盖。
5. owner confirmed 后才 Materialize 和 queue/add。输入仅排队时仍不发布；出现首条收据 clientId 对应的用户 item，且线程 active 或该轮已终态，才持久领取 `worktree-publish`。
6. 在同一命名 bucket 中原子发布 `.codex-worktree-name`，核对 owner 与代际后，完成创建。清理器第一次将它识别为工作树时已有真实 owner；后续使用原生 active/pinned/idle 清理规则。

owner/publish unknown 只读回原结果；无证据就保留原工作树、线程和输入记录。创建结果不可因超时而替换资源。发布文件也使用目录句柄、私有 marker 精确核对、`O_NOFOLLOW` 与 inode 核验，避免路径在检查后被替换时向另一个 bucket 写入。

## 同路径重建复现与桥侧措施

合成仓库依次执行：创建 A 并记录其 gitdir；删除 A；创建同名 B，使其复用 A 的旧元数据槽位；在原路径重建 A。此时 A 的新 `rev-parse --git-path` 指向另一槽位，而保存的旧路径指向 B。向缓存路径写 owner 会串写 B；桥的新登记在每次操作中 fresh 解析并核对 backlink，只会写当前 A，使用旧物理摘要则直接拒绝。

另有回归把 A 的 `.git` 指向 B：仅检查 show-toplevel、common-dir 和 worktree list 不足以拒绝，必须检查 gitdir 中的反向 `gitdir` 文件。正常编辑、提交以及 owner 文件写入不会改变代际摘要；删除重建、`.git` 替换或 gitdir 槽位复用会改变摘要。

## 存量修复方案

`worktrees audit --config PATH --report PRIVATE_JSON` 只读指定 host/app 的 confirmed 桥创建记录、原生 metadata 和现有 Git 身份。计划文件为私有文件，保存 draft/thread/目标路径/实际 owner/代际；命令输出仅状态计数，不含正文或凭据。缺失目录不触发恢复或 Git add。

核对计划后，停止桥以取得原有应用与数据库监听锁，再执行 `worktrees repair --config PATH --report PRIVATE_JSON`。工具重新核验创建记录、实际 threadSource/cwd 与同一物理代际，只对缺失 owner 的候选执行 create-only 登记；修复步骤独立落入 `creation_attempts`，不把历史 confirmed workflow 改回 working。已登记的同 owner 可读回确认；异主、损坏、目录缺失、来源不匹配或同路径重建都保留并报告。修复结束重新 audit，之后启动同一桥实例。

已暴露目录可能仍在桌面旧清理队列中，补登记不是瞬时清理屏障。存量维护应在桌面清理任务已结束的窗口进行，并让桌面重新读取当前身份；如果期间目录消失，保留 unknown，先按原快照恢复流程核验，不从 base 复制一个替代工作树。此限制不通过永久关闭清理规避。

另一聊天工作树中已有错误 owner 的情形超出本工具“补缺失”的处理范围：先核对那个 checkout 的当前原生聊天、原创建证据和实际 Git 双向身份，保留原文件及摘要；随后通过桌面修复后的 fresh 路径登记流程处理。仅凭一条桥 intent 不覆盖另一个 App 创建聊天的 owner。本次工具会保留并报告冲突。

## 仍需桌面应用处理

- 删除/恢复后失效 Git 路径缓存；登记前 fresh 解析，核对双向工作树身份，避免向复用的元数据槽位写入。
- 执行删除前重新读取 owner 和工作树物理代际；已有清理计划不能只凭旧路径及旧 owner 删除后来恢复的目录。
- 强制清理应明确区分 Git 删除拒绝原因，不能用文件系统递归删除绕过初始化保护。该版本 Git lock 不提供桥可依赖的屏障。
- 提供受支持的托管创建/归属/保护接口，或可查询的桌面兼容版本。桥当前只能对已核查版本提供文件格式兼容，升级后需重验。

## 验证记录

以下验证通过：

- `go test ./cmd/... ./internal/... ./compat`：完整业务测试通过，未排除用例。
- `go test -race ./internal/creation`：完整创建包通过，278.766 秒；存量审计、补登记 CLI 的专项 race 也通过。
- 随后完善清理 fixture，使其实际并发扫描和删除唯一的临时 Git checkout，并以真实 birthtime 可空字段排序。`go test -race ./internal/creation -run '^(TestManagedWorktreeCleanerScansRealLifecycleBoundaries|TestAppCleanupCandidatesUseKeepCountAndOwnership)$' -count=1` 通过，3.683 秒。
- 最后收紧历史 prepared 路径与已暴露旧路径的首次 thread/start：旧记录仅核验已有资源，不在管理目录外新建，也不启动未受保护的聊天。`go test -race ./internal/creation -run 'TestWorktree(Legacy|FrozenCodexHome)' -count=1` 通过，11.221 秒。
- `python3 scripts/check_docs.py`、`git diff --check` 通过。

覆盖包括 fresh owner 写后读回、同异 owner 并发、真实 Git 槽位复用、错误 backlink、symlink/共享写/FIFO、同路径重建、普通编辑提交、取消、重启与登记/发布结果 unknown。清理 fixture 使用真实目录和 Git 指针，并在创建及首次 queue/add 的屏障处持续扫描：无 birthtime 时仍不提前暴露；发布后 active 和 pinned 均不被选中，idle 在 keepCount 压力下实际删除，即使已有 Git lock。该 fixture 按所核对桌面源码建模，并非真实桌面程序。真实清理、置顶保护和闲置回收 UI 保持 pending。

## 本次存量只读清点与执行边界

18:23（Asia/Shanghai）清点同一 host/app 的全部 23 个 confirmed 工作树创建记录：9 个 owner 正确，3 个缺失 owner，11 个目录不存在。不存在不等于已经证明误删，不从基础分支自动重建。3 个缺失项中，1 个位于 Codex 管理目录，另 2 个是此前版本已经冻结的桥持久目录；当前方案不移动这些已有任务。

故障工作树重新解析后仍使用第 8 个同名 Git 元数据槽位，owner 缺失；另一个 App 创建的工作树仍使用第 1 个槽位，owner 指向故障聊天。再次读取两个实际聊天的 metadata，核实各自 CWD 后，确认归属错位仍存在。后一个工作树不属于桥 confirmed creation 清单，因此单列于私有证据，不能把 23 项审计误称为整个主机所有工作树的审计。

清点时运行服务已由另一项会话设置变更升级到 schema 32。当前基于 schema 31 的正式审计命令正确返回 `storage-unavailable`，未放宽数据库版本门禁。只读核对运行构建的 migration 后，确认 32 只新增两张会话设置表，未改变 creation 数据；使用仓库外一次性适配器，以 `mode=ro`、`query_only=ON` 和准确 schema 32 检查复用审计逻辑，输出上述结果。原始计划与两处 Git/原生身份对照均保存为仓库外 0600 文件，不含对话正文。

执行存量写入前，先将此改动集成到当前 schema 32 构建并验证，保留会话设置功能；不能用 schema 31 候选替换当前服务或降级数据库。随后在桌面现有清理任务结束的维护窗口停止桥、一致备份，重新 audit 并运行 repair，仅补 3 个已核验的缺失登记，再读回并重新 audit。冲突项单独保留原文件及摘要，核实两个聊天和 checkout 后用 fresh 路径处理；11 个缺失目录先核对快照、未保存内容和原线程，所需恢复逐项进行。不得因为结果 unknown 重建替代工作树。

本次未部署新二进制、未改写存量 owner、未批量恢复目录，`phase1_released=false`。
