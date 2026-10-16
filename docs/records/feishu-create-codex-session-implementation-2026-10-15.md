# 飞书创建 Codex 会话：实施与验收记录

日期：2026-10-15。本文记录当前开发实现、自动验证、共享 daemon / Codex App 证据和仍待真实飞书验收的边界。创建功能默认关闭；本机候选已按用户授权启用联调，目录范围显式设为 `/`，实际执行目录仍要求 R/W/X。本记录不把本地测试、协议证据或 App 观察扩写成完整飞书 UI 通过。

## 当前候选：工作树创建后的消息续接修复

用户反馈首条回复在 App 可见但没有同步到群，后续群消息停在接收状态。真实只读证据确认 workflow 已 confirmed、原生 cwd 与冻结执行目录一致，首条已接收而后续收据仍 received。原因是 Runtime 授权仍以原仓库 `Directory` 比较新工作树 cwd，且直接将工作树存储目录套用可浏览目录范围，阻断了 actor 接管与普通收发。

已修复为 `ExecutionDirectory()` 精确匹配，重新验证原选择目录及整个 Git 根的授权范围；复用既有只读工作树校验，核验完整草稿 marker、注册的 root/common-dir、实际 cwd/RWX，允许创建后的正常修改和提交。完整 marker 阻止短 bucket 被其他任务复用后串到旧会话。原群、线程、收据和请求参数保持不变，不新建替代资源。

已更新原候选服务，构建 `9d051de7672d+worktree-sync-20261015`，二进制 `/data00/home/chenxiaoyu.lemon/.local/lib/codex-feishu/worktree-sync-20261015/codex-feishu`，SHA-256：

```text
e9cf5b90b6b169817b906bad9c1d6b9d5dcc38bfc8c07db23b01470477b95fb3
```

- 新增真实 Git Runtime 回归在修复前复现失败，修复后仓库根和子目录均可投递原最终回复、消费待提交消息，且不重复建群或提交首条输入。受限项目根之外的私有工作树可继续使用；范围收缩、cwd 漂移、未注册替换目录及同仓重新注册但缺少原 marker 的替换 checkout 均拒绝。
- 最终 `go test ./... -timeout 60s` 全仓通过；app/creation 的创建接管、工作树与授权专项 `-race` 通过。独立审查提出的短 bucket 复用风险已用既有只读 verifier 修正，复核无新增问题。
- 停桥并持有 App / 数据库锁后，一致备份 schema 29 的 55 张表、42,441 行，无迁移。重启后原 36 个绑定、40 条输入收据、6 批附件、14 个附件项、6 个草稿、4 个 workflow 及 82 个创建 attempt 的主键和冻结身份均保留；配置字节、运行 SHA、数据库完整性和外键检查通过。
- Codex / 飞书连接、配置、准确版本及 creation protocol 检查通过；doctor 仍为 `diagnostics-pending`，`phase1_released=false`。
- 仍存在的旧版工作树已由原 actor 补齐最终回复，原收据进入 consumed；真实飞书 GET 核验同群最终卡，公开回复文字与原生历史一致。该证据仅覆盖该旧路径会话，不代替最新缺失目录会话的恢复或客户端 UI。
- 最新测试工作树在反馈后的检查中已不存在，Git 注册也已移除；私有桥 marker 与冻结参数仍匹配。该目录缺失独立于本次 cwd 比较错误，不能将其待提交输入和待投递回复记为恢复通过。已向用户核实是否主动清理，尚未重建目录或原生资源。
- 用户随后反馈当前使用未再观察到问题，记录为当前收发可用的反馈，保留现候选。用户未说明是否清理过原工作树；跟进只读检查仍显示原目录缺失、原首条 accepted 和后续 received、无已投递最终卡，因此这条已移除目录会话的恢复仍未确认，不扩大为该场景或完整发布验收通过。

私有部署与一致备份保存在 `worktree-sync-deploy-20261015/`；原线程只读证据和日志保存在 `creation-sync-debug-20261015-1792068835/`。不向真实群注入测试任务，不提交原文、凭据或原生标识。

## 上一修正：工作树路径对齐 Codex Home

用户实际创建工作树后反馈路径与 App 不同。新建路径改为 `<initialize.codexHome>/worktrees/<4位ID>/<仓库名>`，直接取共享服务握手中的真实 Codex Home；不从桥进程的环境猜测，不改全局配置。已有 App 目录会跳过，创建时原子领取，冲突不写入；桥 marker 单独命名，不伪造 App 元数据。旧候选已冻结的路径和请求参数保持原样，避免移动已创建会话。

已更新原候选服务，构建 `9d051de7672d+worktree-path-20261015`，二进制 `/data00/home/chenxiaoyu.lemon/.local/lib/codex-feishu/worktree-path-20261015/codex-feishu`，SHA-256：

```text
2f80c821d34bb7de9804058c73a59a3f8951578bad777ad9f95c5b69f3ec1edf
```

- 全仓 `go test ./... -timeout 55s` 通过；新增服务端 Home 与进程环境不一致、已有 App bucket、领取竞争和旧路径 unknown 恢复覆盖。路径与握手专项 `-race` 通过；独立路径安全审查未发现新增问题。
- 真实 0.162.1 共享 daemon 已在新形式目录创建工作树，保留原 projectId，接收首条原生输入，并在持久化后由另一连接最小 resume 同一线程。复用原独立验收项目，证据位于私有 `worktree-native-20261015-03/`；飞书 gateway 仍为 fixture，该证据不代替 App / 飞书 UI。
- 停止原桥并持有 App / 数据库锁后，一致备份 schema 29 的 55 张表、42,003 行，无迁移。重启后原 35 个绑定、38 条输入收据、6 批附件、14 个附件项、5 个草稿、3 个 workflow、70 个创建 attempt 的主键均保留，冻结身份和已有工作树路径相同。配置字节未变，运行二进制 SHA 一致，数据库完整性及外键检查通过。私有备份与证明保存在 `worktree-path-deploy-20261015/`。
- Codex / 飞书连接、配置、准确版本和 creation protocol 健康检查通过；历史绑定/未知操作仍保留 pending，doctor 整体为 `diagnostics-pending`。新版真实 App / 飞书 UI 仍待反馈，`phase1_released=false`。
- 路径形式对齐不代表已实现 App 的自动清理、环境 setup、未提交文件复制或 handoff。

## 上一候选：创建独立工作树

新增“当前目录 / 新建工作树”切换与基准分支选择，继续使用紧凑主卡和原长连接。默认当前目录；工作树基于精确分支的已提交内容，以 detached HEAD 检出。工作树由桥持久保留，不复制未提交/未跟踪文件，不运行环境 setup，不接管 Codex App 的工作树清理。行为、范围和恢复契约见 [09 新建工作树](../codex-feishu/09-feishu-task-creation.md#新建工作树)。

已更新原候选服务，构建 `9d051de7672d+worktree-20261015`，二进制 `/data00/home/chenxiaoyu.lemon/.local/lib/codex-feishu/worktree-20261015/codex-feishu`，SHA-256：

```text
3aa0d05ffeda929fb0a00d2373ef11ca5de1024646b3e4e2663da17aebd608c2
```

- 全仓 `go test ./... -timeout 55s` 通过；工作树、迁移和卡片专项 race 通过。实际 Git 测试覆盖固定分支版本、原目录修改保留、子目录 cwd、重复领取、崩溃后复用、unknown 不重发、Git 根授权、私有路径/marker 冲突、初始化锁和半成品检出拒绝、精确 ref 及失效分支重选。
- 真实 0.162.1 共享 daemon 的独立验收项目已创建线程，首条原生输入接收，实际 cwd 为新工作树且保留原 projectId；持久化后另一连接以最小 resume 验证同一线程和策略。该受控测试的飞书 gateway 使用 fixture，不冒充真实建群或 App 页面验收。新生 rollout 尚未持久化时不创建替代线程；后续只读回原 ID 成功。
- 本人专用预览卡的当前目录、工作树和分支不可用视图均通过真实飞书 PATCH/GET 校验。平台接受与读回不代表按钮实际回调、PC/手机布局或创建到 App 续接已验收；已请求用户测试这些新增交互，当前保持 pending。
- 停止原桥并持有 App / 数据库锁后，一致备份并迁移 schema 28→29。55 张旧表的 40,641 行在全部旧列上的摘要一致；原 33 个绑定、37 条输入收据、6 批附件、14 个附件项、4 个创建草稿及 2 个 workflow 主键保留，旧创建模式为 local。配置字节未变，运行二进制 SHA 一致，`quick_check=ok`、外键错误为 0；共享 daemon 未重启。
- 原服务已恢复，Codex / 飞书连接、配置、准确版本和 creation protocol 检查通过。doctor 仍为 `diagnostics-pending`，历史绑定/未知操作与完整发布验收未清除或提升。`phase1_released=false` 保持不变。

私有部署、备份、摘要证明及只读健康记录保存在 `~/.local/share/codex-feishu/worktree-deploy-20261015/`；原生证据保存在 `worktree-native-20261015-02/`，卡片证据保存在 `worktree-card-20261015/`。原文、凭据、原生标识与数据库不提交 Git。

## 紧凑视觉修正（上一候选）

用户已确认浏览优先功能可用，但实际截图中的样式仍不接受。本轮保持动作与后端不变，仅调整创建 renderer：所有常规与主要按钮改 medium，并放进明确 none / auto 内容宽度分栏；form 显式 8px 间距；主任务框默认两行；目录名突出，短父路径灰色显示并可展开完整路径与主机；更改/上一级位于对应目录右侧；最近/下一批和最终确认/取消各自紧凑横排。移除常驻主机 UUID 和“独立目录”技术标记，简化重复说明。具体对照见[视觉修正](../design/feishu-create-directory-ui-redesign-2026-10-15.md#7-用户截图后的视觉修正2026-10-15)。

已部署原候选服务，构建 `f1b6acfb1ebf+creation-style-20261015`，二进制 `/data00/home/chenxiaoyu.lemon/.local/lib/codex-feishu/creation-style-20261015-1792062855/codex-feishu`，SHA-256：

```text
08b81dbfef35168ffa707f087720efe6a9cb74fda2c8f93bde0346c461a28053
```

- cards、creation、app 全包回归通过；最终摘要微调后 cards 全包再次通过，未新增与本次无关测试。
- 主卡、20/50/100 项浏览、同名搜索、大量搜索、只读、等待与保留布局等待卡均通过真实 API 校验；包括 form → column → 折叠完整路径的实际嵌套。读回观察保留文字、选项数量与主要按钮；不据此声称真实视觉验收已通过。
- 沿用当前 unit 及其参数，保留并行交付的附件、文件预览、异步问答能力；配置字节不变，schema 28 无迁移。备份后原 31 个绑定、37 条输入收据、6 批附件、14 个附件项主键全部保留。运行 SHA 一致，数据库完整性和外键检查通过，Codex/飞书连接、creation protocol 健康项通过。
- doctor 整体仍 `diagnostics-pending`，保留历史待核验与客户端验收项目。用户随后确认本轮紧凑样式“没问题了”，浏览优先功能与本轮展示样式已获认可；未单独取得的手机、双话题与故障场景证据继续保持 pending。

私有部署、平台探针与一致备份保存于 `~/.local/share/codex-feishu/creation-style-20261015/`，不将凭据和真实正文纳入 Git。

## 先前候选：浏览优先重设计

用户在新版交互原型中明确选择“浏览优先”。已更新原候选服务，构建 `0cd26da888ac+creation-browser-20261015`；二进制 `/data00/home/chenxiaoyu.lemon/.local/lib/codex-feishu/creation-browser-20261015-1792061551/codex-feishu`，SHA-256：

```text
a5731cd8db3104c153f8fd68894e03f5f039037fe4d96739119e0cfeb1ec26d5
```

本次变更：

- 主创建卡收拢为任务、唯一目录摘要、更改目录、跟进设置和创建。目录视图默认浏览；顶部单一名称/完整路径输入，下拉选择子目录即进入，实心“使用当前目录”负责选用。最近项目与已知目录仍是辅助入口。
- 普通操作采用大号描边按钮，主要确认采用 `primary_filled`；表单外 `action.option` 被显式转交后端，真实目录、项目、创建者、卡片、revision 和期限仍重新校验。所有非主创建视图拒绝最终创建。
- 目录候选批量最多 100，准确计数和整层过滤先于分页；最终 JSON 以 28 KiB 为预算，长路径安全缩减批量并给正确后续游标，不截断执行路径。只有实际容量需要时才加载下一批。
- 操作领取后立即唤醒。持久等待卡沿用前一完整布局和本次提交字段、禁用冲突操作，随后同一出口发布结果。UI 写与目录查询由唯一 UI worker 负责；建群/原生创建保留独立唯一 worker，恢复只处理各自拥有的 dispatch。
- 普通目录浏览不调用 native 项目目录或工作群元数据。UI 读取有 3 秒子预算，最终选择与创建重新核验；主卡真实工作群名采用短预算与展示缓存，不作为授权。

验证与发布边界：

- `go test ./... -timeout 120s` 全仓通过；cards/app 的 Creation 专项 `-race` 通过，creation 全包与并发/唤醒/恢复、store 分 worker 恢复和顺序专项通过并完成 race 检查。
- 在原专用预览卡上，真实飞书 API 接受主卡、20/50/100 项浏览、两项同名搜索、100 项搜索、只读和等待视图；包括保留原布局并禁用控件的等待卡。GET 观察到相应文字、候选数量和主要按钮。该证据证明 schema/API 接受，不证明 PC/手机触控、原生下拉搜索或实际更新时延已通过。
- 真实 GET 会规范化默认字段、补元素 ID，且 form 内 callback 字段可能不完整。因此本轮不把布局观察冒充完整 JSON 读回证明；未知写仍按既有严格证明阻断，不放松比较、不盲重发，可能需要进一步人工核验。模拟 unknown 恢复测试不扩写为这一真实平台场景通过。
- 沿用原 unit、配置、数据库与共享 daemon；保留当前附件、文件预览和已增加的预览命令。schema 仍为 28，无迁移。停桥并持有应用/数据库锁后做一致备份，配置字节不变，原 30 个绑定、36 条输入收据、6 批附件及 14 个附件项主键全部保留；运行二进制 SHA 一致，`quick_check=ok`、外键错误为 0。
- 首次切换的即时进程身份检查未通过，自动恢复原 unit；改为有界等待进程启动后完成切换与身份复核。原配置和数据没有回退或覆盖。部署及回滚记录只存私有 `~/.local/share/codex-feishu/creation-browse-ui-20261015/`。
- 配置、共享 Codex、飞书连接与 creation protocol 健康检查通过。doctor 整体仍为 `diagnostics-pending`，历史绑定与未知操作保持待核验；新版真实飞书 PC/手机操作与原双话题矩阵仍为 pending，`phase1_released=false`。

详细设计和用户选择见[目录重设计](../design/feishu-create-directory-ui-redesign-2026-10-15.md)。重新发送 `/new` 使用新草稿即可进入新版，旧卡片回调继续兼容；本轮不批量改写已发布历史卡片。

## 先前候选：组合目录交互

用户已要求组合速选、搜索和目录面板，并说明没有 HTTPS 域名、希望复用长连接。本轮最终采用全部原生飞书卡片：主卡显示最近 / Codex 项目速选，显式搜索项目名或绝对路径，目录面板提供路径导航、绝对路径前往、当前层筛选和分页；操作复用原 WebSocket 接收，结果更新原卡。没有新增 Web / OAuth 服务、配置项或 TCP listener。选择只改同一草稿，最终回主卡点击创建。

候选已部署到原 `codex-feishu.service`，构建 `8fb72d6627a5+creation-combined-20261015`；二进制 `~/.local/lib/codex-feishu/creation-20261015-combined/codex-feishu`，SHA-256：

```text
619b062a9bfef153c6aab9c214bdd5ef61ad6f82e1d36a82f567c9dc75fea9c5
```

本轮不迁移 schema（仍为 28）。停止原桥并取得锁后保存配置、unit 和包含 WAL 的一致数据库备份到私有 `~/.local/share/codex-feishu/creation-combined-backup-20261015/`，再安装并启动新候选。旧二进制保留，配置字节未变；备份中的 28 个绑定、6 批附件、14 个附件项、33 条输入收据主键全部保留，原绑定身份字段不变。运行进程 SHA 与候选一致，`quick_check=ok`、外键错误为 0、没有 TCP listener；共享 daemon 未重启。启动后健康快照新增的绑定来自原有 discovery，不计为本轮创建功能的 UI 证据。

验证：

- `go test ./... -timeout 120s` 全部通过；目录 API 及组合 / 保稿 / 导航 / 筛选分页 / 项目身份 / cursor 专项 `-race` 通过。检查时修正旧 version 测试的 stdout pipe 先写后读死锁：较小 pipe buffer 装不下完整 JSON，改为临时文件捕获；不影响生产二进制。
- 原专用验收群中的既有预览分别更新为主卡、非空搜索、空搜索、目录面板，四种真实 API schema 均接受并读回；不可据此提升手机 UI 通过。
- 独立审查修正搜索结果误用旧目录风险：搜索视图隐藏创建与第二套目录 selector，服务端也拒绝在该视图创建；选用后回主卡确认。默认目录等于原生项目根时，速选仍会解析 exact project identity。
- doctor 的配置、共享 daemon / 飞书连接、创建协议检查通过；整体仍 `diagnostics-pending`，保留原 5 个历史未就绪绑定及 3 个 unknown 镜像 attempt，发布与新版真实客户端 UI 均 pending。

旧 H5 原型和独立网页验证不属于本候选验收。新版需用户通过 `/new` 复验三个入口、手机布局、选择回填和实际 cwd；其余双话题 / 根失效矩阵按下表保留。

## 当前开发实现

创建链路已经从原方案中的设计状态进入开发实现和验收中：

- 草稿持久保存入口消息、创建者、正文、最终目录、可选项目、跟进方式、目标工作群、revision、token 和期限；同一 `host/app/source_message_id` 只保留一个草稿，取消或过期后重放仍返回原记录。
- 目录在提交时重新校验；创建配置显式保存允许范围，默认不从 discovery 范围推导。功能默认关闭，显式启用仍要求真实允许目录和相应入口 / 工作群范围。
- group 是默认跟进方式。每个创建意图的远程步骤和 frozen 参数分别记录，原生 `thread/start` 和首条原生 queue 输入沿用既有会话 / 收据语义；重复 revision、并发领取和重复 materialize 不创建第二套对象。
- topic 的目标群、根消息、平台 thread 身份保存在 binding，镜像和 outbound 沿用同一目标隔离；group、topic、repair 状态可在只读健康摘要中按 deployment 统计。
- 远程步骤的 `prepared`、`dispatching`、`confirmed`、`rejected`、`unknown` 日志已经落库。未知结果经恢复仍不可直接重发；只有明确 rejected 的 attempt 才能通过显式 retry 形成新 attempt。
- schema 28 增加创建草稿、创建 workflow、不可变 creation attempt 及 `m0_receipts.reply_message_id`。健康诊断只返回状态计数，不返回正文、目录、消息 / thread / attempt ID、payload、token 或 secret。

实现仍复用共享 daemon、原线程和既有原生 queue / steer 边界，不启动第二套 app-server 或改变全局 Codex 配置。创建开关、允许目录、入口群、工作群、topic 和 draft TTL 已接入配置 / setup / doctor 的开发路径；配置向导省略 creation 时保留原设置，计划只展示安全范围计数。

## 自动与协议证据

本轮自动验证记录如下：

| 验证 | 结果 | 边界 |
| --- | --- | --- |
| 全仓 Go 测试 | `go test ./...` 完整通过 | 只证明代码与本地 fixture；不等于真实飞书 UI 通过 |
| 创建、Codex client race | 专项 race 通过 | 覆盖持久创建和协议适配，不代表 App 页面可见 |
| store、feishu、sync 全包测试 | 通过 | 不替代真实群、话题、手机和根消息撤回观察 |
| 创建入口、话题护栏、目录权限 race | app、sync、creation 的 CreationReceiver / Topic / Directory 专项 race 通过 | 覆盖路由隔离、根撤回阻断和只读父级导航；不替代 UI |
| 本轮健康与 creation 定向测试 | store 包 Health / Creation 定向测试通过 | 覆盖作用域、隐私、schema 27 跳过新表及创建持久边界 |

共享 daemon 0.162.1 的真实受控证据已补充一条持久测试线程：首条原生 queue 输入和 history 已保存，另一连接可以 resume。`thread/list` 返回的真实 `threadSource` 为 `null`，不能把它当作唯一来源证明；逐线程 `thread/read` 仍保留完整 `threadSource`。恢复核验已改为逐线程 metadata 加最小 `ReadCreation` 策略，避免因 list 缺少 source 而猜测或重复创建。该版本的 `ThreadStartParams` schema 请求摘要为：

```text
80a40a7fac15b4bf70efb7f893fb353acc0a0d30c68f54aee4f01923deca85de
```

上述协议证据证明共享 daemon 能保留和续接持久线程；它不证明创建入口卡、真实飞书群 / 话题或手机布局已经通过。

## 用户观察与安全复查

用户先前已确认真实 App 能看到独立可行性线程、打开并发送续接消息。2026-10-15 对本次候选的飞书私聊 `/new` 验收，用户再次回复“测试通过”，覆盖所询问的创建卡 / 目录选择、专属群与回复、Codex App 打开并继续；同时明确不满意整体交互，尤其目录选择，要求调研开源方案并提供多版 demo。此处只记录对应功能链路通过，不把它扩展为话题隔离或手机布局通过，也不将功能通过等同体验认可。只读实例核对为本记录 SHA 对应构建，1 个 group creation workflow confirmed，各远程步骤及入口卡 attempt 均 confirmed。

安全审查指出的边界已修正并通过专项测试：首条输入确认前禁止 Runtime 提前 Attach；确认落库失败在同进程下一轮恢复而非依赖重启；持久扫描补偿丢失的 Activate；重复校验失败仍刷新卡片 revision；超过一个 batch 的草稿按上次访问时间公平领取。创建 / Runtime 恢复专项 race 及双话题创建 race 通过。

## 先前候选与首次迁移记录

2026-10-15 已沿用固定 `codex-feishu.service`、原配置和原数据库更新候选；共享 daemon 未重启，未改全局 Codex 配置。构建为 `8fb72d6627a5+creation-repair-20261015`，二进制位于 `~/.local/lib/codex-feishu/creation-20261015-repair/codex-feishu`，SHA-256：

```text
ab296df9355d980e8383f9a7923c4cab7529698b0148ec3533da235ca8873eca
```

停止准确旧桥后保存配置、原 unit 和包含 WAL 的一致备份，再迁移 schema 26 → 28。逐表比较所有旧列的排序内容摘要：49 张旧表、29,908 条记录完全保留；其中 25 个绑定、6 批附件、14 个附件项、31 条原输入收据不变。`quick_check=ok`、外键错误为 0。备份与迁移证明保存在本机私有 `~/.local/share/codex-feishu/creation-upgrade-backup-20261015/`，不纳入 Git。

本机显式开启创建，默认专属群；入口群 / 话题工作群仅包括专用验收工作群，常用目录为空。允许范围根 `/` 可浏览；不可写父级仅用于导航，最终选择与提交重新验证目录权限。创建卡、目录卡均遵守飞书 input 最大 1000 字限制；任务正文最多 4000 字，超过后明确反馈；超过表单可编辑的 1000 字时完整折叠保留，不截断后提交。

启动后运行进程 SHA 与构建一致，管理健康、共享 Codex / 飞书连接、创建协议版本、discovery 基线和权限检查通过。doctor 整体仍为 `diagnostics-pending`：原有部分历史绑定未就绪，原镜像未知记录待核验；这些状态未清除，也未写成创建功能通过。真实客户端验收仍按下表分别记录。后续核对补齐了“创建完成后根消息再失效”的入口提示：repair 与入口 revision 在同一事务保存，已同步入口卡重新进入更新队列；重复检查只刷新一次，不改变原 native / root 身份。失败回滚、重启保持和 manager → outbound → 入口卡专项 / race 测试通过；本次修正的 schema 28 一致备份位于私有 `~/.local/share/codex-feishu/creation-repair-backup-20261015/`。

## UI 与真实飞书矩阵

下表保留 spec 10 所要求的证据分层：自动证据、真实 App 观察和真实飞书观察分别记录。自动证据或 App 通过不提升真实飞书列；未执行项目保持 pending。

| 能力 / 场景 | 自动证据 | 真实 App | 真实飞书 UI |
| --- | --- | --- | --- |
| creation 默认关闭、显式配置范围 | 配置 / setup / doctor 测试通过；本机候选显式启用 `/` | 构建及运行 SHA 已核对 | pending |
| 允许目录、真实路径和范围边界 | 配置校验与 store 边界测试通过 | 用户确认 App 可打开并继续 | 目录可选的功能实测通过；交互需重做，边界仍按自动证据 |
| group 默认跟进和 per-intent source 去重 | creation store 测试通过 | 飞书创建的新线程打开 / 续接通过 | 默认专属群与回复通过；重复提交去重仍按自动证据 |
| topic binding、根消息与 outbound 隔离 | follow target、creation、sync 测试通过 | 未完成双 topic 客户端观察 | 双话题并行 pending |
| 原生 thread/start、首条 queue、恢复 | store / codexclient race 与 0.162.1 受控线程证据通过 | 新入口线程打开 / 续接通过 | 私聊创建到专属群回复通过；故障恢复不因此提升为 UI 通过 |
| 入口创建卡与状态更新 | 创建 / 目录表单通过真实平台校验，卡片测试通过 | 用户确认新会话可续接 | 创建入口功能通过；用户明确不认可当前交互 |
| 目录选择卡的手机布局和长路径 | 脱敏数据与路径校验测试通过 | 未完成手机观察 | pending |
| 新专属群、成员、群描述和首条根卡 | 持久化与目标 binding 测试通过 | 未观察真实群 UI | pending |
| 话题根消息、过程 / 结果 / 操作目标 | topic 目标字段与 outbound 隔离测试通过 | 未观察客户端串话题边界 | pending |
| 根消息撤回 / 话题不可达 | unknown / repair 测试通过；真实根撤回 GET 护栏区分存活 / 删除 | 原生会话不因平台根失效重建 | 客户端待修复显示 pending |
| 未知结果、落库失败、重启恢复 | unknown 日志、同进程确认恢复、公平领取及 Runtime 补偿测试 / race 通过 | 共享 daemon resume 通过 | 飞书投递核验 pending |
| doctor / plan 脱敏诊断 | health / setup / doctor 测试通过；真实双连接及创建协议检查通过 | 仍保留原历史绑定 / 未知投递 pending | pending |

真实飞书 API 已接受创建与目录表单，专用测试群中的两条话题具有不同 root / thread，GET 和 thread 列表确认回复互不跨根。实测还发现平台在根撤回后仍可能接受回复，因此不能依赖 reply 报错；新的 GET 护栏已真实区分撤回根与存活根，阻止向已撤回根继续写入，单元测试覆盖结果、操作、命令和提醒。以上 API 证据不等于客户端观察。用户已确认私聊创建、目录选择、新专属群与回复、App 续接这一功能闭环通过，但当前交互体验未获认可。手机目录布局、同群双话题客户端观察及对应根失效显示仍为 pending。只有逐项取得真实客户端观察，并核对当前部署身份后，才可更新对应列；不能用用户确认的 App 续接扩展成整套飞书验收。

## 交接

- 开发契约：[09 第二期：飞书内创建任务](../codex-feishu/09-feishu-task-creation.md)
- 配置、向导与诊断：[10 CLI、配置与安装契约](../codex-feishu/10-cli-and-setup.md)
- 存储与恢复：[02 存储、收据与恢复](../codex-feishu/02-store-and-recovery.md)
- 本记录不改变 `phase1_released`，也不把 pending 变成 pass；以上构建路径与 hash 只对应本次候选，不沿用为其他构建的验收。
