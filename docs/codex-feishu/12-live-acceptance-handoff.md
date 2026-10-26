# 12 新版集中验收清单

本页用于 M0 之后的候选部署验收，执行契约仍以 [Agent 配置指南](AGENT-SETUP.md) 和 [一期矩阵](11-integration-and-acceptance.md) 为准。它将需要用户查看 App / 飞书的操作合并，不能代替逐项取证。

## 2026-10-26 原生 CoT 短标题与工具详情（候选已部署）

本地服务已更新为 `41b0b92b39bd423ed096d0d3d33e7e5bcec683b6`，基于 `main` 的 `d8657aa`，保留单用户任务群免 @ 输入。新轮次使用短标题，逐工具展开显示实际命令/参数和输出/diff 的脱敏片段，移除成功状态、零退出码与耗时的重复正文。已有版本 1 的活动轮次保持原格式，已完成 CoT 不重写。

修正后全仓测试、相关 race、独立审查及文档检查通过。停桥一致备份核验通过，schema 34 无迁移，原配置和共享 daemon 保持；PID 3583274，active/running/enabled，进程、CLI、管理通道的版本摘要一致，两侧连接正常，unknown/dispatching 聚合计数未增加。本次切换前后恢复绑定均为 66/111；doctor/status 仍为两项历史 required pending，整体恢复不能记为通过。

本轮未发送测试输入或预览，新版桌面/手机、群/话题的展开与失败样式仍为 pending，`phase1_released=false` 保持。身份、备份及完整边界见[实施与部署记录](../records/cot-tool-details-2026-10-26.md)。

## 2026-10-26 单用户任务群免 @ 输入（候选已部署）

本地服务已部署 `7029ff92ec8a41fc1ae6eafbeaba5d1a6c9126ab`。已绑定任务群仅有授权操作者和当前机器人时，文字、富文本和指令可免 @；每条消息实时核验群人数、双方身份及准确的话题绑定，失败或超时不放行。应用的 `im:message.group_msg` 授权检查通过，原生队列、消息去重和独立附件开关保持。

停桥一致备份核验通过，schema 34 无迁移，原配置与共享 daemon 保持。PID 2936034，active/running/enabled，进程、稳定 CLI 与管理通道的版本摘要一致，两侧连接正常，历史 unknown 计数未增加。doctor/status 仍只有原来的 bindings_reconciled / operations_known 两项 required pending；恢复绑定数由 70/110 变为 66/110，差异尚未定位，不能记为整体健康通过。

全仓业务测试、相关 race 与文档检查通过。部署后用户确认本轮测试没有问题，并授权合并 PR；该确认不扩展为全部客户端、成员变化边界或完整一期验收通过，两项历史对账 pending 及 `phase1_released=false` 保持。构建身份、备份和验证边界见[部署记录](../records/one-to-one-group-deployment-2026-10-26.md)。

## 2026-10-23 卡片邮箱分隔符替换（修正版已部署）

本地服务已部署 `c60a0ee65dac8e41e73eb230d3f9e9ac37f505ee`。卡片与原生 CoT 的公共文本出口只将邮箱中的 `@` 替换为字面量 `<A>`，保留用户名、域名及原生历史，并避免在邮箱或链接内部切分长文本。生成的尖括号不再编码为会在代码中原样显示的 HTML 实体；输入标签仍转义。全仓业务测试及文档检查通过，schema 34 和原配置保持。

先前 `230028` 拒绝的最终回复经一次授权重发成功后，用户截图指出全邮箱替换与代码中的实体显示不符合要求。修正版已在同一消息完成一次 update confirmed，revision/delivered_revision 均为 3，没有新增 send/reply；两个旧 rejected attempt 和成功重发记录保留，unknown 数量未增加。22:16 CST 平台读回确认两个地址均保留用户名和域名、中间使用真实 `<A>`，没有 `&lt;A&gt;`，原群及父消息关系保持。服务 PID 2107624、active/running，两侧连接正常，仍仅原来的 bindings_reconciled / operations_known 两项 required pending。详见[部署与修正记录](../records/feishu-email-redaction-2026-10-23.md)。客户端 UI 与完整一期验收仍为 pending，`phase1_released=false`。

## 2026-10-23 飞书输入触发工作树恢复（候选已部署）

托管工作树被回收后，普通飞书消息先持久接收，再从原路径的 Codex 快照恢复，在原 cwd 附着同一线程并提交原收据。已经卡住且确认从未提交的普通消息也可恢复，进度或阻塞更新原消息的回复卡。schema 33→34 新增身份、输入授权和恢复日志，保留会话设置与既有收据；unknown 先核验，取消不重发，原创建 birth 不改写。

已 rebase 最新 main 并部署构建 `c816b561399193ab6acf193fdde29f218045476d`。schema 33→34 副本演练及停桥迁移通过，59 张旧表和 179,623 行数据保持，新增恢复表为空。原配置保持，PID 1532952，systemd active/running/enabled，进程与稳定 CLI 身份一致，两侧连接正常。已恢复绑定由 61/104 增至 66/104，仍只有原来的两项 required pending，历史 unknown 未增加。

两条既存普通输入已自动恢复原工作树并 consumed，各有一次 confirmed add 和一条原生历史关联；未额外创建测试资源或发送输入。实现、自动验证与运行证据见[恢复记录](../records/feishu-worktree-auto-restore-2026-10-23.md)。真实 App/飞书 UI、原聊天继续和客户端附件展示仍为 pending。兼容边界仍为 App Server 0.162.1 / 桌面 26.1016.62342；桌面私有附件状态、缓存 gitdir 与已排队清理不由桥修改，`phase1_released=false`。

## 2026-10-23 托管工作树归属修复（待部署）

新增工作树保留在真实 daemon 的 `codexHome/worktrees` 下，以命名 bucket 延迟发布：实际线程 owner 持久登记并读回后才提交首条输入，首轮可见后才进入桌面扫描范围。每次 fresh 解析并双向核验 Git 元数据路径，状态机覆盖重启、unknown 和同路径重建。另提供只读审计与停桥补缺失 owner 命令。此方案替代下文此前将新工作树放到桥数据库同级目录的做法；历史冻结路径不迁移。

兼容边界为 App Server 0.162.1 / 桌面 26.1016.62342；本次实现、自动回归和存量审计见[实施记录](../records/managed-worktree-ownership-2026-10-23.md)。真实 App 创建、活跃/置顶保护及闲置回收 UI 均保持 pending，`phase1_released=false`。已有清理队列和恢复路径缓存仍需桌面修复，补登记不构成撤销既有删除任务的屏障。

## 2026-10-23 模型设置卡片当前部署

用户确认单一 `/model` 表单和取消按钮后，原本地服务更新到 `348dd9935909+model-card-11a9c1d69374`，二进制 SHA-256 `22c15813c51c6b1a99f9715a182e29a89ddb45d0af61d2fae06d2ab3d3545130`。保存和取消后在原卡显示实际当前模型、思考强度及启用时的 fast；新回复引用区使用同样的紧凑格式。删除聊天 `/status`、`/reasoning`、`/fast`，`/help` 保留 12 条指令与详情。

schema 32→33 的副本演练和停服升级通过；原 58 表/179285 行的定义与行数保持，新增表单表为空。PID 1011491，systemd active/running/enabled，进程和稳定 CLI 身份一致，原配置与共享 daemon 保留。Codex 与飞书连接恢复，切换前后绑定恢复均为 61/104；doctor/status 仍只有原来两项 required pending。全仓测试、相关 race 及文档检查通过；部署后用户确认本轮测试无问题并授权 PR 合并。该确认不扩展为逐端、全部异常场景或完整一期通过，`phase1_released=false`。详见[本次部署记录](../records/model-settings-card-deployment-2026-10-23.md)。

## 2026-10-23 会话设置与帮助前次部署记录

用户授权本地部署后，现有服务更新到 `348dd9935909+session-settings-8212a54c826c`，二进制 SHA-256 `58f9ffb39296192b21dc2707c6bdca89315375e82dca4aa67e04d047f6641742`，保留已部署的创建卡更新。schema 31→32 已经副本演练和停服备份后升级，原 56 表/179006 行的定义与行数保持，完整性及外键检查通过；原配置与共享 daemon 保留。

进程和稳定 CLI 身份一致，PID 557845，systemd active/running/enabled，Codex 与飞书连接恢复。doctor/status 仍有 bindings_reconciled / operations_known 两项 required pending；绑定恢复从 65/104 变为 61/104，差异尚未定位，不能记为整体健康通过。会话设置、回复底部信息和 `/help` 详情等待用户测试，完整一期状态及 `phase1_released=false` 保持。详见[本次部署记录](../records/session-settings-help-deployment-2026-10-23.md)。

## 2026-10-23 创建卡前次部署记录

用户明确授权后，现有服务已更新到 `f86c0422922c+creation-card-796ba293d814`，二进制 SHA-256 `588549e8c3d7cc139f4f84dc1d8dda1505307a9b5dbe3b885fcb348fb41d41f0`，保留此前的压缩轮、Goal 同步与桥工作树存储修复。schema 31、原配置与共享 daemon 保持；停桥一致备份 56 表/178532 行通过完整性、外键和逐表行数检查。

进程及稳定 CLI 身份一致，PID 359051，systemd active/running/enabled，Codex 和飞书连接均恢复。部署前后 doctor/status 仅有原来的 bindings_reconciled / operations_known 两项 required pending，没有新增 required fail/pending；`phase1_released=false`。同页查找、默认工作树与附件预览/删除的合成布局已确认，部署后用户进一步确认本轮创建卡测试通过，并授权提交 PR 合入 main。该确认不扩展为手机、双话题或完整一期矩阵通过。详见[本次部署记录](../records/creation-card-deployment-2026-10-23.md)。

## 2026-10-23 Goal 同步与工作树隔离更新

16:56 已将现有桥更新到 `f86c0422922c6b1bf5114593e757d6b9f5ea52ad`，schema 31、原配置与共享 daemon 保持。核验原生来源后的 goal 无输入续跑可直接投递到原绑定，故障会话水位已恢复推进，新增最终卡 confirmed 且平台读回身份与目标群一致。新工作树改用桥数据库同级的持久目录，旧冻结路径继续原地核验。全仓业务测试及来源读取/同步 race 通过；新目录的真实创建和 goal 客户端 UI 保持 pending，原有两项 required pending 与 `phase1_released=false` 不变。构建摘要、目录生命周期证据和恢复读回见 [实施记录](../records/goal-sync-and-worktree-lifecycle-2026-10-23.md)。下列构建信息保留为此前阶段记录。


## 2026-10-23 原生 CoT 前次部署记录

用户已确认本人私聊原生 CoT 样式并授权正式接入。新活动轮次默认使用 CoT；已有 Card JSON 过程卡继续更新原消息，不替换未知投递或补建终态历史。当前本地 build 为 `4b9316497ea8+attachment-stop-fix-20261023-144245-cdf314cc`，二进制 SHA-256 `901c38eecdc814dbf2828dbe0553b14a3690336e75b2184a46f7ab4466ea000a`，schema 31，PID 3627889，systemd active/running/enabled；稳定 CLI 与进程一致。本次停桥一致备份 56 表/176423 行，quick_check 与外键检查通过，保持 schema 31；首次 30→31 显式迁移及旧数据核验见历史记录。配置、manifest 和共享 daemon 保持。

独立 Review 的四项 CoT 状态/分批问题及两项一次性迁移前置/持久化问题已修复并复核，相关普通/race 与业务回归通过。此前排除的建群 deadline 用例已补齐活跃列表前置条件，当前全仓普通测试不再排除该用例并全部通过。正式 Gateway/Planner 的本人 DM 合成闭环，以及 CoT 引用机器人原消息的 parent_id 读回通过。原始本人私聊样式有用户确认；随后任务群截图显示了原生停止命令和 CoT 中断，复现额外回执与新输入未恢复。单次截图不提升所有群/话题和手机验收，本轮修复后的真实 UI 仍待确认；初始部署时的正常业务只读聚合未观察到新 CoT，保留其原时点证据。

随后两项 P2（解释器选项值误作脚本名、运行摘要改回初值漏同步）及复审发现的空文本更正代批次边界均已修复，两名独立 SubAgent 最终复核无未闭合发现；修复后全仓业务回归及相关 race 通过。本次未新增真实平台探针，既有客户端 UI pending 保持，见[修复复审记录](../records/task-progress-deployment-2026-10-22.md#2026-10-23-摘要隐私与回滚修复复审)。

当前构建同时省略有效 CoT 停止的额外成功回执与空正文停止结果卡；停止后新普通消息先正常入队，再以持久继续意图恢复同一会话，不要求另发 `/continue`。原生停止图标仍按平台约定发送可见 `/stop`，没有已公开的隐藏回调参数。消息重放、旧输入、附件延迟就绪和重启不补授权；未提交输入仍可离线取消。独立高风险复审最终无未解决发现，全仓与专项 race 通过，详见[停止与继续记录](../records/task-progress-deployment-2026-10-22.md#2026-10-23-停止反馈与新输入继续)。

最新构建已修复带说明图文额外生成附件回执，以及旧终态 CoT 的远端 Complete 未确认导致新轮 `/stop` 被拒绝的问题；真实排队、下载失败和需核验时保留管理卡。存储业务测试改用独立空库快照副本，完整存储包 race 在原有默认超时下以 172.179 秒通过。部署检查已完成，图文与连续停止的客户端效果待用户测试，见[本次更新记录](../records/task-progress-deployment-2026-10-22.md#2026-10-23-附件回执与停止目标修复及测试优化)。

群原生停止入口默认隐藏时使用对应任务群/话题 `/stop`，继续队列 `/continue`；单聊不路由任务。CoT unknown 无内部事件读回，不自动重发。doctor/status 退出 5，部署前后仅原有 bindings_reconciled / operations_known 两个 required pending，未新增 required fail/pending，`phase1_released=false`。具体边界与逐项证据见[本轮记录](../records/task-progress-deployment-2026-10-22.md#2026-10-23-原生-cot-预览确认与正式链路接入)和[原生契约](../../compat/feishu-native-cot.md)。

## 当前交接点

2026-10-13 用户开始新版联调后重新核验：旧 `m0-listen` 已退出。已使用下表的候选包、独立私有配置/schema 21 数据库和测试目录安装并启动 `codex-feishu.service`，复用原共享 daemon 0.162.0；没有迁移旧 M0 数据库。实际 plan/apply/install/start、启动后 doctor 与实时 status 已取证。配置、路由标识和用户确认保存在仓库外的本人私有证据目录。

当前候选的真实 App 自动建群、两端往返、默认排队与显式引导、停止后保留队列并明确继续、off/on/snooze、每日 quiet、quiet off、until、精确/递归目录 mute 与独立 unmute，均已由用户确认，并核验相应后台记录。on 保留每日 quiet，until 按 Asia/Shanghai 转成固定 UTC 截止时间；相关最终卡真实发送载荷均无被抑制的 @。

目录规则已恢复。本工具用户服务已完成实际 stop/start，恢复原四个普通绑定、两侧连接和相同通知规则/截止时间；输入和投递记录总数未变，没有重放旧消息。用户已确认重启后仍跟进同一 App 会话和原群，跨两个群的 until、手机长代码/路径完整查看与底部小字、恢复 @ 后无积压提醒均正常。原通知设置已恢复。共享 Codex daemon 未重启，不能把此记录当作系统重启或 SSH 注销观察。

独立配置 Agent 只拿到包内指南和必要输入，已完成真实候选首次安装、既有部署两次重跑/no-op、卸载保留数据、原服务恢复。首次安装用全新私有配置/schema 21 数据库与空 scratch；完成基线后零绑定、零建群、零消息。原配置/unit/binary 摘要、身份、四个绑定和全部规则恢复一致，最终 doctor/status 退出 0。无差异 apply 仍要求先停桥、启动后需等待只读健康就绪的实际体验已补入指南。已知本人 ID 路线无需配对；本轮另外完成实际机器人私聊配对，CLI 返回 paired、ok=true、退出 0，本人与既有配置匹配。首尾空白与开头经核验的本机器人 mention 已修复，不改变应用、私聊、owner、口令或有效期检查。

rules/discovery/app/store/sync 五个包已集中完成 `go test -race`，903 个测试或子测试通过；配对修复另有 pairing/control/CLI 定向 race 验证。配置生成、服务安装/卸载、数据库、发现与消息实现从原 UI 候选至当前部署未变，旧记录保留原构建身份并附影响审阅。P1-01 至 P1-08 已有对应覆盖，P1-10 与 P1-11 也已完成；P1-09、P1-12 仍 pending，正式发布标志保持 false。

后续实际 App 测试补齐普通注册工作树：空白草稿期间不建群，首条输入后只有一个主会话群。实际 cwd 是工作树、project_root 是原 fixture 仓库，Git common-dir 一致；四个同目录子代理候选均 excluded，没有绑定。原生 `item/tool/requestUserInput` 和 `item/commandExecution/requestApproval` 均实发一条回 App 提醒，卡片没有飞书回答/审批按钮；本人分别在 App 回答和批准后请求 resolved，各最终结果只同步一次。用户已确认这些 UI。当前新增第五个测试绑定，仍使用原配置。

原真实 UI 与独立配置测试的候选包位于仓库 `dist/`，下表身份保留不变：

| 项目 | 值 |
| --- | --- |
| 文件 | `codex-feishu-linux-amd64-development-d6370da1aa55.tar.gz` |
| 构建 commit | `d6370da1aa550c982e6885a256f9794c84d58ade` |
| 归档 SHA-256 | `59d446ece30ac3bfe938c8fdf097dc454d3ec1773b107f69428fcda4b527c377` |
| 二进制 SHA-256 | `cae1dc1b32c6863c733f71636b5f2adbc92cef313c0db3f814dc4488882bb63d` |
| 状态 | development；`phase1_released=false` |

该包的成员、校验和、构建身份、实际 version/help、指南与 12 项 pending 模板已检查。候选安装与显式报告能力可用，但 `phase1_service_ui_verified=false`、`setup_pair_ui_verified=false`；本地检查不证明新版的真实 UI 或一期发布。后续仅修改这份交接文档不会改变该包的 commit；代码或包内内容变更则重新提供准确候选身份，不将上表套用到另一份二进制。

配对修复后，当前运行实例使用下列构建。中间版本和原二进制均保留；没有重写原 UI 证据或迁移数据库。

| 项目 | 值 |
| --- | --- |
| 文件 | `codex-feishu-linux-amd64-development-7c83060b5c60.tar.gz` |
| 构建 commit | `7c83060b5c60ea6a4a5c77bcdb1304139ec5bed9` |
| 归档 SHA-256 | `6373ec1f5db4036897065a63e5f39665c722d8445e47c12662182ae6112e3e60` |
| 二进制 SHA-256 | `5694a2a4ebfca026223f8e95e94a1f166e5dfaf4c9033082d7c7b6c3c4d22855` |
| 真实配对 | 已有实例唯一接收器；本人私聊；paired / ok=true / exit 0 |
| 发布状态 | development；全部发布/UI 常量保持原值 |

继续工作时实查发现同一 unit 为 inactive/disabled，原因未确定，不能称作已证实的服务崩溃或网络故障。沿用准确二进制/config 的候选 install/start 已恢复 enabled/running；doctor `--require-running`、status 均退出 0，配置字节、四个绑定、目录和提醒规则与启动前一致。这也验证了既有 unit 自动启动状态的修复，不代表已观察系统重启或最后一次 SSH 注销。

## 2026-10-21 异步问答增量

用户已在真实飞书卡提交一次异步单题选择，回答进入原会话：一次动作、一次已确认原生 add、输入 consumed、唯一历史关联与实际答案收据，原卡更新投递已确认。用户确认回传成功。异步填写与回传记为本场景 pass，App 原问题控件的完成显示仍 pending；其他 P1 状态和发布标志保持原值。运行构建、私有证据范围及随后默认配置变更见[异步问答记录](../records/feishu-async-question-defaults-2026-10-21.md)。

## 开始前由 Agent 完成

1. 重新核验候选包、共享 daemon 的实际版本、用户服务条件、已有桥与监听锁。依据指南生成候选配置计划，使用新的私有配置/schema 21 数据库和独立测试目录；不读取凭据正文到聊天，不迁移或复制旧 M0 数据库。
2. 沿用用户已经提供的应用、本人身份与凭据文件位置，不重复索要。只补收测试目录范围与尚未确定的通知偏好；quiet 不擅自启用或清除。
3. 准备旧 M0 的准确停止、重新启动与回退步骤，并确认无未决原生操作。同一应用只有一个接收器；切换时间须等用户方便，不并行启动候选桥。
4. 新的配置 Agent 只收到独立的 `AGENT-SETUP.md`、已核验候选包和必要的输入引用，完成候选 plan/apply/install/start。保存它实际索取的信息、配对动作、命令退出码与二次执行 no-op 证据，作为 P1-11；不要让继承全部开发上下文的 Agent 冒充首次配置体验。
5. 配置 Agent 先读取 version 再使用显式 `--candidate-test` 路径；启动后 doctor `--require-running`、status 必须核验实际实例、两个连接、普通绑定与发现基线。组件 observed、进程启动或 systemd active 均不算整体验收成功。

最初部署由主开发 Agent 执行；后续首次安装及重跑已有独立配置 Agent 的实际记录。两者分开保存，不将主开发上下文冒充新 Agent 的指南体验，也不并行启动第二个接收器。

## 用户集中操作与 Agent 取证

所有输入只进入新测试目录/测试会话。Agent 在每组开始前给出准确目录、对应群和需要点击的卡片；用户可用一次文本回复确认该组观察，不必上传截图。某个观察未回答或失败，只保持对应案例 pending/fail，不要求重做已成功的无关步骤。

| 组 | 用户操作 | Agent 必须核验的事实 | 关联案例 |
| --- | --- | --- | --- |
| A 配置与配对 | 如首次配置需要，私聊完成本人配对并点一次测试卡 | 实际 owner 入站/callback、配置身份、候选实例；配置 Agent 独立完成流程及二次 no-op | P1-10、P1-11 |
| B 新 App 任务与往返 | 在指定 SSH 主机/测试目录新建 App 会话，发“只回复 P1_READY，不修改文件”；查看自动群；在群 @ 发“只回复 P1_FEISHU_READY，不修改文件” | 首条实际输入才触发唯一 intent/chat/binding；App 人类输入与 AI 上下文完整；飞书输入回显到同一个 App 页面；不重复镜像飞书正文，卡片回复引用原消息 | P1-01、P1-03、P1-10 |
| C 忙时、引导与停止 | 在隔离忙时会话连续补充 A/B/C，先不点引导；再点 B 的“追加引导”；另一轮 `/stop` 后补充，再 `/continue`；查看 App 队列和最终回复 | 默认原生 queue/add，真实顺序与 App 队列项一致；B 只进入原目标活跃轮一次；停止后保留队列，显式继续；每轮单过程卡更新、最终卡单独发、原消息 reaction 与引用关系正确 | P1-04、P1-05、P1-06、P1-10 |
| D 目录与通知 | 在指定测试目录使用精确/递归 mute 与 unmute；按提示查看新任务是否建群；用 off/on、snooze/until、quiet 检查本人提醒；手机查看长正文/代码及底部小字引用区 | 目录规则只影响未来自动建群，已绑定会话继续；@ 关闭/静默时正文继续，恢复后无积压 @；策略作用于本人并持久化，发送前复查；实际桌面/手机布局合适 | P1-07、P1-08、P1-10 |
| E 恢复与常驻 | Agent 重启本工具后，继续同一测试会话；在安排的时机退出/重连 SSH 或 App，查看仍能跟进 | 原 binding/chat、规则、水位与待执行项保留，无重建/重复；用户服务实际生命周期与注销常驻条件；卸载仅移除服务，保留数据 | P1-09、P1-12 |

目录命令是 `/mute-dir /absolute/path [--recursive]`、`/unmute-dir /absolute/path [--recursive]`、`/mute-dir list`。通知命令是 `/notify at off`、`on`、`snooze 2h`、`until YYYY-MM-DD HH:mm`、`quiet HH:mm-HH:mm`、`quiet off`、`status`；日期和时区用执行时的真实值，until 须为未来。D 结束恢复原规则，不用全局时间修改制造边界。

受控忙时资源只有原五分钟窗口，创建/绑定/释放分别使用指南中已有的独立授权和管理命令。Agent 用原本机模型门控制忙时，用户跟进才提交输入；资源到期或重启失去原资源时保持冻结，不续期、不重建原 ID、不把查询当作输入授权。若 C 需要多轮，应在每个有效测试窗口分别取证，不把整份清单强塞进一次窗口。

当前受控测试的零输入绑定路径已确认受 0.162.0 的 `no_rollout` 限制阻塞，原 creator 也不能在首条输入前完成 typed resume。它仍 pending，不能自动补发输入或放宽检查；本轮真实队列/引导使用用户主动发起的 App 测试会话，原生故障验证使用独立、明确授权初始输入的固定模型 fixture。详见 [直接核验](../records/native-controller-q4-q8-2026-10-13.md#原-creator-connection-的零输入核验)。

## 不额外打扰用户的覆盖与剩余门槛

下列边界由 Agent 对照模块原场景取证；上表的正常往返不能覆盖它们：

| 案例 | 必须补足的覆盖 |
| --- | --- |
| P1-02 | 首装历史基线无群洪泛；草稿/子代理不建群；注册 worktree 实际 cwd 与项目根。fork 按用户调整仅保留 TODO，不要求用户再做 fork 测试。 |
| P1-05 / P1-06 | Q4–Q10 的双客户端竞争、消费/删除、轮次结束、掉响应与阶段间崩溃。已有本地协议/故障证据可复用，但须核对准确版本和覆盖，单桥 mutex 不能替代。 |
| P1-07 | 前缀碰撞、符号链接、注册 worktree、叠加规则与 unmute 不补建历史。 |
| P1-08 | 跨午夜边界、重启不延长 snooze、多个任务群、限流/离线跨静默窗口不补提醒。真实提醒仍需用户确认；needs_user 的原生发生时间证据限制按模块 07 明确记录。 |
| P1-09 | 断线/崩溃、明确限流、未知结果核验，不重建任务，不盲重发；恢复后的真实群与 App 关系另外确认。 |
| P1-10 | 过期按钮、非操作者、AI 文本不能自行 @、审批/问答只提示回 App。需要第二个真实测试身份时再说明这个具体缺项，不用伪造 owner 事件冒充 UI 验收。 |
| P1-12 | 不兼容 daemon 暂停写入、用户服务的卸载保留数据；系统重启/linger 若无授权或无法验证，保留具体 pending，不用一次 SSH 注销替代。 |

新版已取得上述十项的场景证据，其余 P1-09、P1-12 保留 pending；上表是覆盖核对表，P1-02/10 的实际缺口已由后续 App 工作树、子代理和问答/审批补足。Q4–Q8 已补充实际共享 daemon、Go Controller、SQLite 和受控返回丢失/进程退出验证，见 [原生边界验证](../records/native-controller-q4-q8-2026-10-13.md)；Q2/Q3 的混合队列和中间引导、Q9 的并发登记与重放、Q10 的两客户端继续竞争见 [后续原生验证](../records/native-controller-q2-q3-q9-q10-2026-10-13.md)。Q9 假定 Controller 入口已获授权，不替代多身份飞书鉴权；Q10 原生 API 竞争与用户已确认的 App 正常停止/继续分别取证。这些测试不替代真实 UI，也不等同于网络中断。M0 的用户确认作为可行性证据保存，不只改它的构建身份就套用到新版。每个案例须有对应自动/人工证据和真实观察来源；可以将一次用户确认关联多个明确回答的观察，但不能把“可以”扩成未执行的全部场景。

新增本地 Unix WebSocket + 实际 Service/SQLite fixture 验证首次未验收版本在 dial 前拒绝，以及 0.162.0 连接断开后版本变为 0.163.0 时不新建连接、不再附着、不提交、不建群，离线输入保留。它只补兼容门禁证据；飞书接收器按原离线契约继续运行，Health 表示 native 未连接，具体版本由 doctor 诊断。没有升级或重启共享 daemon，也未制造真实网络中断。

另一个本地真实 Unix WebSocket Client→Service→SQLite→outbox 场景已通过 race：关闭 socket 后添加离线回合，第二代连接分页补齐新增 human/final 各一次；旧内容、revision 和 remote ID 保持，receiver=1、generation=2、建群/native mutation=0，重复完整补齐也不重发。协议 peer 与飞书 gateway 是 fixture；它补足连接恢复的联合证据，不称作真实平台限流或远端网络故障。

实际只读 GET 已补三种现有卡片的 hidden marker 验证：send、reply、PATCH 的标记都保留，应用身份和回复关系正确。但平台移除非默认字号等字段、改写末尾换行，严格完整 wire 比较不成立。当前 readback 因此保持 inconclusive/unknown，不盲重发；这不是完整原载荷恢复通过。正常投递和过程卡更新不受影响，详见[卡片读回限制](../research/feishu-card-readback-normalization-2026-10-13.md)。

## 收尾

Agent 按指南在 0700 私有目录保存 0600 报告与脱敏证据，填写当前候选/部署/协议/SDK 身份和文件摘要。案例未执行、证据不足或有冲突时保留 pending/fail。调用显式 `verify --acceptance-report` 进行只读报告核验，不能用组件计数、`ready_to_release` 字段或修改 version 常量提前发布。

本轮当前 7c 部署的完整 12 项私有报告已实查：CLI/运行桥/配置/协议/SDK 身份匹配、所有引用文件摘要有效，最终十项 pass、P1-09/12 pending，返回 `acceptance-report-pending` / 退出 5。八项、九项和十项的阶段报告分别保存，不覆盖前一阶段。它是已取证的进度，完整一期尚未通过。

用户报告 App 没有单独断开远程连接入口，本轮实际断连/重连未执行，不记作 pass；也没有进行最后一次 SSH 注销、系统或共享 daemon 重启。用户本轮协助测试到此结束，剩余运维观察和平台 unknown 的全文证明限制如实保留，不反复要求已经通过的操作。

全部 P1 通过后再按模块 11 审核发布与 M0 接管；候选服务运行不等于正式迁移。保留旧桥二进制、配置、数据库和回退方案。清理受控组件并恢复原通知偏好；测试群可保留测试标签，不自动解散，不删除真实会话数据。最终交付应包括可安装包、兼容/权限清单、配置指南、真实验收记录及常驻条件。

## 2026-10-16 置顶群名与扫描降频增量

本增量经独立审查、修复并复审后，已通过 PR #4 合入 main，按用户授权更新现有本地候选服务；构建身份 `0c7f2e2373aa97b3ddb7fce5136fe60a0f232c01`。不改写上面的历史构建、P1 状态或正式发布结论。行为、测试与部署核验见[置顶群名、近期发现窗口与空闲降频](../records/pinned-group-names-and-windowed-discovery-2026-10-16.md)。

| 覆盖 | 当前证据与状态 |
| --- | --- |
| App 置顶与取消 | verified：用户在同一真实 App 会话操作，远端 section 空 → Pinned → 空 |
| 0.162.1 分区只读查询 | verified：沿用现有 daemon，分区目录、普通/归档及全 sourceKinds 分页接受，未执行原生或飞书写操作 |
| 本地候选部署 | 已更新：配置与 schema 29 保持，进程摘要/提交匹配，systemd active/enabled，两侧连接正常；整体恢复仍 pending |
| 飞书群名图标和 App 改标题 | 平台已读回确认 4 个绑定置顶群名前缀；客户端添加、移除及置顶中改标题仍 pending |
| 新增恢复与限流 UI | pending：服务已重启，仍待完整恢复、平台限流与 topic 共享群名核验；历史 6 条出站 unknown 保留，status 仍为 reconciling |

## 2026-10-19 本地文件预览与工作树目录反馈增量

按用户授权部署 `4bc4c9ff7c2e+preview-worktree-fix-20261019`，二进制 SHA-256 为 `42b14ea26a08af28d5729f2d1ff388d9e7af322ef27739318b898883f41919c9`。本次包含工作区尚未提交的修复，沿用原 schema 29 数据库、共享 daemon 和固定用户服务；只扩大预览目录范围，保留旧根与口令。完整备份和检查见[预览部署记录](../records/feishu-local-file-preview-implementation-2026-10-15.md#2026-10-19-全本地目录行号兼容与候选部署)。

| 覆盖 | 当前证据与状态 |
| --- | --- |
| 本地候选部署 | 已更新：运行进程及稳定 CLI 匹配候选；两侧连接、发现基线及权限检查通过 |
| 全本地目录与工作树预览 | 本机 HTTP 通过：原根外合成文本、两种工作树父目录、旧链接、登录与隐藏文件拒绝；飞书和用户浏览器点击仍 pending |
| 本地链接行号 | 回归通过：`#L12` / `#L12C5` 转为预览行号，原历史和用户正文保留；本机登记检查通过 |
| 非 Git 目录工作树入口 | cards / creation 回归和 race 通过：提前说明、禁用及有效目录恢复；真实新卡片操作 pending |
| 全仓回归 | 有一项既有自动建群重试测试失败，原始基线重复复现；其他包通过，本次未将全仓结果记作 pass |
| 整体恢复与发布 | 原 `bindings_reconciled` / `operations_known` pending 保留，doctor/status 退出 5，无新 required fail；完整 P1 与 `phase1_released=false` 保持 |

### 代码高亮与 Markdown 阅读

同日继续更新候选为 `4bc4c9ff7c2e+preview-reading-20261019`，二进制 SHA-256 `1a1cca259080b40e9756c62d1dcb294195385bce6a59da9965099fac983fde63`。配置、口令、授权根及 schema 29 保持，稳定 CLI 和实际进程均匹配新构建；[部署与浏览器记录](../records/feishu-local-file-preview-implementation-2026-10-15.md#2026-10-19-代码高亮与-markdown-阅读部署)保留独立身份和备份。

filepreview 回归和 race 通过。真实本机 Chromium 已验证 GFM 阅读、代码文件 / 围栏高亮、源码 fragment 自动展开和手机尺寸无页面横向溢出；实际服务登录与旧链接继续可用，无外部资源请求。用户设备与飞书内置浏览器观察仍 pending。全仓保留上表的一条基线已有测试失败，原恢复诊断 pending 和完整 P1 / 发布结论保持。

### HTML 静态页面预览

当前候选进一步更新为 `4bc4c9ff7c2e+preview-html-20261019`，二进制 SHA-256 `e54d8c114ee3bdad7dabdfba232bf5d986ed41f84e4a1193256c48fc9e9f8058`。HTML 默认显示隔离静态页面与内联 CSS，原文仍可展开高亮源码及行号；脚本、外部资源、表单和跳转不可用。连接提示和嵌套文档在显示副本中移除，sandbox 与原 CSP 继续生效；[部署及隔离核验记录](../records/feishu-local-file-preview-implementation-2026-10-15.md#2026-10-19-html-静态页面预览部署)包含有正对照的预连接验证。

filepreview 测试和 race、本机桌面/手机尺寸浏览器核验均通过，实际服务的 HTML、源码定位、登录及旧链接可用。配置、原口令、授权根及 schema 29 保持，备份完成，实际进程与稳定 CLI 匹配新构建；两侧连接和发现基线通过。全仓仍有同一条既有建群重试测试失败；原恢复诊断、用户设备和飞书内置浏览器观察 pending，完整 P1 与发布标志保持。

### HTML 目录跳转修复

用户实际目录操作暴露了 srcdoc 相对 fragment 错误加载外层预览地址的缺陷，现已修复并更新为 `4bc4c9ff7c2e+preview-html-anchors-20261019`，二进制 SHA-256 `d00a693787c1fddcc1311237e1ed400d5e96e01e9f02b9abf4e617c749535f23`。目录在子文档内跳转，原 HTML、源码、CSP 和 sandbox 保持；[修复及部署记录](../records/feishu-local-file-preview-implementation-2026-10-15.md#2026-10-19-html-目录锚点修复)记录复现与新增回归。

filepreview 测试和 race 通过，本机 Chromium 已核验桌面/手机尺寸的目录、中文锚点和返回顶部，以及用户原链接的 9 个页内目标；跳转不重载文档，不产生导航请求。实际进程与稳定 CLI 匹配候选，配置和 schema 29 保持，备份完成；两侧连接及发现基线通过。用户随后确认原链接目录已恢复正常，并授权提交与推送；真实手机、飞书内置浏览器、原恢复诊断 pending 和发布状态保持，前次全仓既有失败未改写为通过。

### 2026-10-21 HTML 交互预览

用户授权独立 CodeReview 后部署本地，当前运行构建更新为 `b613584f8932+html-interactive-20261021-162439-8850d9d4`，二进制 SHA-256 `775f5a72a4e8bb419ad2edf4a7d379df45f4a1c86dd1c194c1b74a360cd89b58`。自包含报告可运行内嵌脚本和数据，登录外壳继续禁脚本，渲染容器保留来源隔离和受限 CSP。独立审查发现的 fragment 选择器与动态锚点问题已修复，复审无剩余阻断项；[部署记录](../records/feishu-local-file-preview-implementation-2026-10-15.md#2026-10-21-自包含-html-交互预览与本地部署)保存构建、回滚和验证范围。

filepreview/sync race、修复后的合成 Chromium 42 项检查及部署后原链接 22 项检查通过；原报告已生成图表，搜索、排序、展开、动态锚点、源码和既有 Markdown/代码链接正常。实际进程与稳定 CLI 匹配候选，systemd active/enabled，两侧连接和发现基线通过；原配置、口令及 schema 30 保持，一致备份已核验。全仓仍有同一条基线已有建群重试测试超时；`bindings_reconciled`、`operations_known` 原 pending 与 doctor/status 退出 5 保留，无新增 required fail。手机尺寸模拟不替代真实手机或飞书内置浏览器验收，完整 P1 与 `phase1_released=false` 保持。

## 2026-10-22 执行卡布局与工具摘要（本地候选部署）

基于 `196586e` 的本地开发分支 `codex/compact-task-progress`。当前版本恢复根级 `execution_history`：可用停止按钮和状态仅占顶行，下方为全宽“执行中”折叠面板；无可用停止按钮时直接以真实状态作为面板标题，终态默认收起并保留耗时。工具组使用灰色小型 Lucide tool-case 的 `header.title.icon` 前缀，保留独立原生箭头；明细使用同源、增加透明留白的较小前缀，单次调用不折叠。正文、行末 HH:MM、隐私、同卡投递和原 stop/continue 授权链路保留，无图标分栏、负 margin、红色 emoji 或 Markdown 图片标题。用户已通过真实截图确认最新前缀图标效果，正式 renderer 与认可预览的合成布局比对一致；“执行中”是本轮按用户建议修改的文案。具体契约见 [08 卡片](08-cards-and-actions.md)，构建与预览证据见[部署记录](../records/task-progress-deployment-2026-10-22.md)。

| 覆盖 | 当前证据与状态 |
| --- | --- |
| 本地候选部署 | verified：`196586e6fd88+task-progress-20261022-231324-ca78188a`，二进制 SHA-256 `dd4f7170d198a2148e391ebc5b021af0b9e089b727f0efa52c11d6e985e6da63`；实际进程与稳定 CLI 匹配，systemd active / running / enabled，两侧连接和发现基线通过；原配置、schema 30、共享 daemon 与既有两个 required pending 保持 |
| 本地功能回归 | verified：cards 和 sync 全包、app 图标加载与 generation 注入回归通过。覆盖六类图标与原生 fallback、权限/App 作用域/启动时一次加载、根级折叠、面板外停止、完整正文与行末时间、工具组和不可折叠明细、组件预算及唯一 marker。正式 renderer 的合成卡与已认可预览布局一致 |
| 相关并发检查 | 本轮 cards/sync 的 `Task\|Progress\|Clock\|RequestMarker` 定向 race，以及 app 图标加载、TaskControls 注入与 reconnect 定向 race 通过。`foreign_owner` 子测试因 uid 1001 无法 chown 跳过，归属拒绝逻辑已实现；store 全包 race 的历史超时仍未完成 |
| 全仓业务测试 | 本轮 `go test -p 1 ./cmd/... ./internal/... ./compat -skip '^TestRuntimeGroupDeadlinePreservesFinalAndWakesWithoutNewInput$'` 通过。完整 app 仍在该已于未修改 `origin/main` 独立复现的基线用例超时，不记为通过 |
| 桌面及手机卡片展示 | 最新预览的灰色小型标题/明细前缀、全宽正文与行末时间已获真实桌面截图确认。旧负 margin 裁切、齿轮及 Markdown 大图标题方案已弃用；日常卡真实停止、连续更新和手机表现仍 pending |
| 私聊预览及正式投递 | 前缀预览单次 confirmed，GET 保留 title.icon 与明细图标引用；全 JSON 严格比对仍因样式/默认字段转换为 false。用户随后确认客户端图标效果，现已接入日常卡；正常同步聚合已观察到新外层折叠、“执行中”、六角色配置对应的 custom_icon 及原生工具组箭头。没有再发合成卡或注入输入 |
| 执行中刷新及终态 | pending：用户展开后更新是否保持展开；同卡停止、队列继续及完成后移除停止按钮的真实客户端表现 |

上述本地证据不替代实际 App / 飞书客户端验收，不提升现有 P1 或发布状态。

## 2026-10-22 最新执行卡预览客户端确认

用户在真实客户端确认最新私聊预览：标题前缀是灰色、小尺寸的 tool-case 图标，展开后的叶子图标更小；执行卡使用全宽布局，公开正文的接收时间显示在行末 `HH:MM`。按本轮建议，运行态外层过程面板标题改为“执行中”，终态继续使用真实状态标题并保留既有耗时规则。该确认只覆盖预览展示；真实停止、连续刷新后的展开状态和移动端表现仍 pending。

本轮测试事实如下：

- `go test -p 1 ./cmd/... ./internal/... ./compat -skip '^TestRuntimeGroupDeadlinePreservesFinalAndWakesWithoutNewInput$'` 全部通过。
- `go test -race ./internal/app -run 'TaskIcons|TaskIcon' -timeout 120s` 通过；`foreign_owner` 子测试因当前 uid 1001 无法 `chown` 而跳过。
- 未排除该基线用例的完整 app 测试仍在已复现的 `TestRuntimeGroupDeadlinePreservesFinalAndWakesWithoutNewInput` 超时；该例不记为通过。

最新版本已部署至原本地服务并通过进程、连接、私有 manifest 与原 daemon 的核验，当前 build 见上表；构建后只补交付文档。私有 manifest 契约沿用 [08 卡片手册](08-cards-and-actions.md)，此处不重复展开。`phase1_released=false` 保持不变。

## 2026-10-22 宽度及单行折叠后续反馈

源码已将执行卡 `fill` 改为 `default`，主创建卡与结果摘要从 `compact` 放宽为 `default`，独立目录/Busy 保持原宽度。cards 全包及 creation/sync 相关回归通过；任务创建卡的合成加宽预览已单次发本人私聊 confirmed，GET 保留 default 模式、身份和 marker，全 JSON 严格比对因平台规范化仍 false。客户端宽度与换行 pending，本轮没有部署，当前运行 build 仍以上表为准。

用户同时要求“正在处理”自身作为唯一折叠入口，取消其下重复的“执行中”。原生面板没有独立停止按钮槽；回调式单行折叠可保持正文全宽，但会将展开状态变成群内共享，并增加一次更新等待。已请求用户选择交互取舍，未默认改变原生折叠语义或数据库。详见[本轮记录](../records/task-progress-deployment-2026-10-22.md#2026-10-22-宽度修正与单行折叠交互待选择)。

随后已按用户要求发送三张本人私聊对比卡：原生同排可真实折叠，正文受右列约束；回调布局的展开态和收起态各一张，明确为静态样式、尚未接入整体点击更新。同排本身并非必须回调，回调方案是为同时消除正文缩进。三次发送均 confirmed，GET 身份、marker、宽度与布局结构保留，全 JSON 严格比对仍 false；真实 UI 及最终方案选择 pending。无新图片上传、业务数据库变更或服务部署，详见[对比预览记录](../records/task-progress-deployment-2026-10-22.md#2026-10-22-原生同排与回调布局的对比预览)。

用户继续建议停止紧跟折叠箭头右侧，已单次发送面板左列、停止右列且均 auto 的真实原生折叠预览。该列顺序避免正文左侧缩进，但外部按钮随整个面板列的右边界变化，不能保证展开长正文后仍贴着箭头。GET 保留列顺序、标题、marker 和禁用停止；完整 JSON 严格比对仍 false，客户端两态位置 pending，日常服务未替换。见[右侧停止预览记录](../records/task-progress-deployment-2026-10-22.md#停止按钮改放面板右侧的原生预览)。
