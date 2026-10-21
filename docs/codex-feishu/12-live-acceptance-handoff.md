# 12 新版集中验收清单

本页用于 M0 之后的候选部署验收，执行契约仍以 [Agent 配置指南](AGENT-SETUP.md) 和 [一期矩阵](11-integration-and-acceptance.md) 为准。它将需要用户查看 App / 飞书的操作合并，不能代替逐项取证。

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
