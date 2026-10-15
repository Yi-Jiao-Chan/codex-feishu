# 飞书入站图片、文件与多附件：实施记录

日期：2026-10-15。基础实现已落在 schema 26，并已升级本机受控候选。新安装默认关闭，本机候选已显式开启附件和独立资源接收。用户已确认图片及文字加文件的基本流程正常；完整矩阵仍有 pending，`phase1_released=false`，原有 P1 pending/unknown 不因本功能改变。本记录只记录已实现边界和已有证据，不把协议探针或本地测试写成用户验收。

## 本轮用户实测与修正

用户截图已证明两次图片输入和一个 ZIP 文件引用到达 App；桥端这三个批次资源均 ready、原生输入均 consumed。这只能证明这些输入的传递与显示，不等于下方完整矩阵已通过。

实测同时发现两个问题：桥自动加入的说明占据了用户 Prompt；同条文字加 EML 文件只提交了文字。修正内容：

- 移除固定附件免责声明；图片输入只保留原文和 `localImage`。文件引用由多段说明改为一行转义后的 `"名称": "绝对路径"`。仅在用户未填说明而点击发送时使用“请查看附件。”。
- 飞书实际 `post` 的正文节点仅有 `at/text`，文件位于顶层 `files: [{file_key,file_name,is_folder:false}]`。原解析器忽略该字段，错误地把输入当纯文本提交。新增该数组的有序提取及整体校验；无效项或文件夹不能降级成仅提交文字。
- 已对原组合消息执行只读资源 GET，成功完整读取 22,911 字节，长度与响应声明相同；验证问题在入站提取，文件下载接口本身可用。诊断只输出结构、类型、字节数和匹配结果，未保存或输出真实正文、密钥或资源 key。

原已提交输入维持原生关联与去重；修正对新输入生效。更新候选后需重新发送一条文字加文件，确认完整闭环。

本次修正已部署为 `8fb72d6627a5+attachments-postfix-20261015`，二进制位于 `~/.local/lib/codex-feishu/attachments-20261015-0618/codex-feishu`，SHA-256 为 `a3ef36e21ad8e4cc6bd20f5f43444d46879c0436c0fdb24fdd7faaefb37cf5d1`。部署前停桥、取得双锁并保存同目录私有 `backup-attachments-postfix-20261015-0618/`（位于候选配置目录），保留 24 个绑定、3 个已有批次和 7 个资源/文字部分；schema 仍为 26，共享 daemon 未停止。启动后两端连接和新增权限检查通过，原有 diagnostics pending 保持。

验证：`go test ./...` 通过；`go test -race ./internal/feishu ./internal/store ./internal/app -run 'Attachment|Inbound|Multipart' -count=1` 通过。新解析器只读处理原组合消息，提取结果为 `[text,file]`，文件 key 与原 `files` 项在内存中精确匹配；真实数据没有进入测试 fixture 或 Git。新增合成 EML/CSV 回归覆盖正文双表示、文件顺序、整批等待及重放去重。

### 队列卡收敛

用户确认上述修正已正常，随后指出排队图片同时出现附件摘要卡和排队操作卡。新输入改为复用原附件卡：草稿/收取/本地下载失败展示附件管理；资源准备完成后由既有 Feedback 投影为紧凑 QueueView，显示真实序号及追加引导/移除操作，不额外发送附件摘要。原生收据、client ID 和整批输入保持原关联，无 schema 变更。

共享卡保留原远程消息 ID 与首次附件的回复关系；过程/结果仍按封批说明的来源引用。回调按同一真实卡 ID、收据版本及 native queue ID 核验，本地取消沿用严格的未提交证明。资源 worker 在 collecting 结束后不再覆盖队列卡，防止后台刷新把离线按钮恢复为在线。已有旧 queue key 则继续沿用，未知 outbox 不迁移、不解冻，不把同一 remote ID 复制到两个镜像键。

该改动已部署为 `8fb72d6627a5+attachments-onecard-20261015`，二进制 `~/.local/lib/codex-feishu/attachments-20261015-0640/codex-feishu`，SHA-256 `c8a3d0429a0256369d8ac6143f8ce67b26cfff4bb43d2faa779ba9ad2a64b7ea`。停桥双锁备份保存于配置目录的 `backup-attachments-onecard-20261015-0640/`，保留 24 个绑定、6 个批次及 14 个资源/文字部分，schema 26 不变；启动后两端连接正常。

全仓测试和 store/app 相关 race 检查通过。store 覆盖同卡 PATCH、草稿后续说明的取消、unknown、旧卡兼容和离线刷新；app 新增共享图片卡的 steer/remove 回调与真实消息 ID 校验。过程/结果来源测试补上实际 Feedback.Stage 流程，两种封批方式仍通过。单卡的新客户端计数效果待下一条实际图片消息确认，不能将历史已发出的双卡视为已合并。

### 排队 reaction

按用户截图，将排队 reaction 改为飞书官方 `OneSecond`（稍等）；附件收取中和等待用户处理继续使用 `THINKING`，执行中使用 `OnIt`。`OneSecond` 已加入持久化反馈转换的允许列表；附件的原始来源消息随同一原生收据同步状态，仍沿用原有已确认删除、添加记账及未知响应保护。枚举见[飞书官方表情说明](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message-reaction/emojis-introduce)。

`go test ./internal/app ./internal/store -run 'Reaction|Feedback|AttachmentHistory' -count=1` 通过，覆盖接收→排队→执行→完成及两种附件封批方式的来源 reaction；`git diff --check` 通过。候选已更新为 `8fb72d6627a5+attachments-queued-reaction-20261015`，二进制 `~/.local/lib/codex-feishu/attachments-20261015-queued-reaction/codex-feishu`，SHA-256 `82b449dd22c336800e5161d8a069b0e42528e27ba3b7fc8caa0fdb86a1803140`。停桥双锁备份位于配置目录的 `backup-attachments-queued-reaction-20261015/`，保留 24 个绑定、6 个批次及 14 个资源/文字部分，schema 26 不变。运行实例摘要匹配，原生和飞书连接正常，已有 diagnostics pending 保持；新表情的飞书客户端显示待实际排队消息确认。

## 已实现边界

- 配置包含 `attachments.enabled` 与 `attachments.receive_standalone` 两个布尔开关，默认均为 `false`。目录省略时使用数据库旁的 `attachments`；限额为每批 10 个资源、5 张图片，单图 10 MiB、图片合计 20 MiB、单个普通文件 20 MiB、批次 50 MiB、单图像素 40M、磁盘 1 GiB，下载 worker 为 2，每个线程最多 1 个进行中的下载。
- 静态 PNG/JPEG/WebP 资源落为受限本地文件并以 `localImage` 进入原生输入。通用文件以带文件名和绝对路径的文本引用提供给同一远程会话；桥不做 OCR、解压或自动执行，文件内容不写 SQLite、普通日志或 Git。
- 带说明的 post 消息必须 @ 机器人，并可包含多个资源。独立图片/文件可以不 @，但只绑定已知任务群和 operator，并受 `im:message.group_msg` 接收范围保护。草稿按 `(host, app, thread, chat, sender)` 隔离；连续资源更新同一张附件卡，附说明即原子封存整批，卡片发送走同一封存路径，管理命令不消费草稿。
- source receipt、draft revision、资源顺序、下载状态和提交关联均持久化。封存批次在资源下载期间为 `collecting`，同线程后来的普通提交按 FIFO 停在该批次之后；全部 ready 才提交，整体 failed 才释放后续输入。关闭开关仍完成已经封存的批次，不再接收新资源，open 草稿保留。
- submission 冻结完整 `input_json`，Add、Steer、Restore 使用同一数组。明确失败的本地下载重试移到队尾并保留 client ID；已有 native attempt 不进入本地重试，unknown 只核验而不重发。
- 缓存目录 0700、文件 0600；完成下载以 SHA-256 校验后原子发布，租约 90 秒，重启可恢复。首次 GET 使用真实有界流式读取；提交前损坏的缓存可在重试时恢复，仍须匹配原大小和摘要。`attachments status` 只读返回开关、磁盘额度和聚合用量；doctor 展示目录与预算并标记 `ui_verified=false`。cleanup 需停止桥并取得应用锁和部署锁，只删除未被非 cleared/nonremoved 状态引用的生成文件或 partial，已提交文件随会话保留。

## 权限和安装边界

附件新增权限只在功能开关需要时成为 required。关闭安装保持原 required/recommended baseline 不变；`receive_standalone=true` 才提示发布敏感的 `im:message.group_msg`，并说明用户权限发布步骤。当前两个附件权限条件检查均为 pass，未因此发起权限发布，也未把权限检查当作真实接收或 UI 验收。

setup JSON 顶层的 `attachments` 是可选对象：省略时保留已有设置，显式提供时整体替换并运行配置校验。plan 展示私有本地存储和预算；doctor 只输出安全聚合，不输出输入正文、resource key 或密钥。

## 已有自动与协议证据

| 证据 | 结果和边界 |
| --- | --- |
| 共享 daemon 0.162.1 原生 Controller 探针 | queue、steer、Q5 restore、unknown add 四个附件场景通过；验证完整多段输入、恢复和未知响应不重复发送。 |
| 原生 wire probe | 通过；queue/add 的两张小 PNG 被 daemon canonicalize 为 `image` data URL，history、items 和 turn/item events 保留 `localImage` 路径引用且不带 data URL 图片字节；`thread/queue/changed` 只含 `threadId`。 |
| Queue 分页/metadata | `limit=1` 分页和只含 ID/client ID 的 metadata DTO 已通过；合成两张各 17 MiB、超过 native 32 MiB 的数据只读分页通过。该证据不等于真实大图发送压力通过。 |
| 原生大图边界 | 真实 0.162.1 daemon 接受两张各约 9.27 MiB 的静态 PNG，总计 19,452,330 字节；canonical response input 为 25,936,637 字节，Add / metadata Queue / full Queue / 删除流程约 1.8 秒内完成。只在受控忙线程排队并清理，未交给模型消费，未证明飞书下载、App 展示或手机内存。 |
| 来源、引用和重放 | Receiver→缓存→Controller→历史同步→出站→reaction 的隔离测试通过，覆盖 @ 说明和附件卡两种封批方式：所有来源消息跟随同一原生输入，最终结果投递确认后才完成 reaction；过程/结果引用正确来源，不重新上传或镜像人类附件输入，重放不重复外发。 |
| 文件和缓存测试 | 本地资源类型、SHA-256、权限、原子发布、租约恢复、限额、清理引用保护和 schema 26 隔离测试已有自动覆盖。 |
| 全仓 Go 测试 | 最终 `go test ./...` 通过。附件、飞书、store、submissions、app、codexclient、setup、CLI 的 race 检查通过；最后的缓存恢复和 reaction 查询修正又分别通过对应 race 检查。 |

协议探针使用合成 tiny PNG 和受控线程，不声称模型看到了图片、文件工具读到了随机标记、App 页面显示正确或手机布局通过。

复查入口：

```sh
go test ./...
go test -race ./internal/attachments ./internal/feishu ./internal/store ./internal/submissions ./internal/app ./internal/codexclient ./internal/setup ./cmd/codex-feishu
CODEX_FEISHU_NATIVE_CONTROLLER_TEST=1 CODEX_FEISHU_REAL_NATIVE_EXPECTED_VERSION=0.162.1 go test ./internal/submissions -run '^TestNativeControllerAttachments$' -count=1 -v
CODEX_FEISHU_NATIVE_CONTROLLER_TEST=1 CODEX_FEISHU_REAL_NATIVE_EXPECTED_VERSION=0.162.1 go test ./internal/submissions -run '^TestNativeAttachmentImageBudget$' -count=1 -v
```

原生测试只操作受控线程，沿用既有共享 daemon，不创建 app-server，不改变真实会话策略。

## 方案逐项核对

下表的自动证据覆盖代码行为，不能代替下方真实验收矩阵。

| 方案要求 | 当前实现及自动证据 | 尚缺的真实证据 |
| --- | --- | --- |
| 同条 @ 图文保留顺序；post 不重复拼两种表示 | `feishu.ParseInbound`；`TestParseInboundPostOrderAndAlternativeContent`、`TestParseInboundV2FallbackAndPresentEmptyContent` | 飞书客户端真实 post 收件和 App 显示 |
| 连续资源按发送者隔离草稿；说明/卡片合批；管理命令不消费 | `Store.ReceiveAttachmentInput`、`AttachmentAction`；store 草稿/双提交/late arrival 与 app Receiver 测试 | 飞书附件卡回调、手机表单体验 |
| 全群接收只处理绑定群 operator；新增权限按开关生效 | Receiver 入口校验、权限与 setup/doctor 测试；当前必需权限 GET 为 pass | 新权限对应的真实独立图片/文件事件 |
| 流式受限下载、原始图片保留、私有路径/摘要/额度、原子恢复 | `Gateway.OpenMessageResource`、Cache、Worker；类型/大小/像素/中断/符号链接/ready-save 失败与租约恢复测试 | 真实飞书资源 GET 与磁盘落盘 |
| 通用文件以真实本地引用交给同一会话；不冒充 native file | `RefreshAttachmentBatches` 冻结带名称和绝对路径的 text；本地 PNG/CSV 闭环测试 | 原线程工具读取文件内部随机 marker |
| 整批 ready 才提交；collecting 挡同线程 FIFO；失败释放，重试移队尾 | store 原子 readiness、Controller FIFO、failed-batch 恢复测试 | 真实群慢下载、失败提示与本桥重启 |
| Add/Steer/Restore 保留完整输入；unknown 不重发 | 多段输入测试、真实 0.162.1 Controller 四场景；旧 schema 迁移保留 unknown 收据测试 | 真实 App 忙时 UI、停止/继续和断线后的用户操作 |
| 多来源 reaction、引用、无重复人类镜像 | `TestAttachmentHistoryFeedbackKeepsSourcesAndReplyAnchor` 的 mention/card 两场景 | 飞书客户端实际 reaction 与引用布局 |
| 大图、多图和消息预算 | 每图/合计硬上限、1 MiB 输入元数据限制；超过 32 MiB 合成分页和 19.45 MB 真实图片排队证据 | 真实飞书→App 大图体验及端到端内存 |
| 生命周期、用量、保守清理不删队列/未知引用 | `AttachmentCacheReferences`、`Cache.Cleanup` 与 CLI 锁/只读/引用保护测试 | 有实际资源的运行时重启验证 |
| 旧数据兼容、备份和部署；不提升一期发布状态 | schema 25→26 迁移前一致备份和逐表摘要核验；候选实际运行，`phase1_released=false` | 本功能真实矩阵完成前保持 pending；既有 P1 不纳入本功能通过声明 |

## 首次候选部署（schema 25→26）

2026-10-15 已停止原桥，持有应用锁和数据库锁完成 WAL 一致备份，再进行 schema 25→26 增量迁移。迁移后逐表比对原列、行数和内容摘要，45 张旧表的 27,205 条记录全部保持，24 个原绑定保留，`quick_check=ok`，外键检查无错误。配置与 unit 同时备份；全程没有停止共享 Codex daemon。

- 构建标记：`8fb72d6627a5+attachments-dev-20261015`；包含当前未提交实现，不是该 Git commit 的干净发布包。
- 二进制：`~/.local/lib/codex-feishu/attachments-20261015-0500/codex-feishu`。
- SHA-256：`8861bbb185a8c2babf2fee7a1a0f39e2b67193fdfab203a3a2b847045c5f7b70`。
- 配置：`~/.local/share/codex-feishu/p1-candidate-20261012-b26f1988/config.toml`，显式 `enabled=true`、`receive_standalone=true`。
- 私有备份：同目录下 `backup-attachments-20261015-0500/`，目录 0700，数据库、配置、unit 和旧数据比对清单均为 0600；不进入 Git。
- 使用既有用户服务安装并启动，运行进程二进制摘要与候选一致，Codex / 飞书连接及两个附件必需权限检查通过。

doctor 总状态仍为 `diagnostics-pending`：原有 `instance.bindings_reconciled`、`instance.operations_known` 及 P1/UI 验收仍待完成；不能把连接成功写成全部验收通过。回滚旧二进制必须先停桥并恢复成套旧配置、数据库和 unit，不能让 schema 25 二进制直接读取 schema 26 数据库；发生新附件输入后应保留升级后的数据库，先评估回滚会丢失的新记录。

## 真实验收矩阵

按每项完整场景记录状态；部分细项已有用户确认，剩余细项仍需实际验收，自动测试或权限检查不能代替它们。

| 状态 | 场景 | 必须观察的结果 |
| --- | --- | --- |
| 部分确认 | @ 多图、视觉和 App 同线程不回声 | 单图已到达 App，并有用户确认及实际识图回复；同条多图和整批不回声完整验收仍 pending。 |
| 部分确认 | 独立图片/文件草稿合批 | 独立 ZIP 加说明已实际提交，用户确认文字加 EML 修正正常；多发送者/多附件草稿完整客户端验收仍 pending。 |
| pending | 随机 marker 文件工具读取 | 文件保存后，原会话工具能读取预置随机 marker；文件引用、权限和失败提示符合预期。 |
| 部分确认 | 忙时 queue/steer/stopcontinue/reactionquote | 实际忙时图片排队、序号和操作卡已有截图；完整 steer/stop/continue 及新版单卡客户端验收仍 pending。 |
| pending | draft/queue 重启 | 草稿、collecting 批次、排队资源和恢复输入经桥重启后不丢、不重发，unknown 保持核验。 |
| pending | 大图、手机和内存 | 接近 hard cap 的图片/批次在真实 App、手机和运行内存中可控；超限明确失败，不以 tiny PNG 探针替代。 |

参考：[实施方案](feishu-inbound-attachments-plan-2026-10-15.md)、[接口调研](feishu-inbound-resources-2026-10-15.md)、[02 存储与恢复](../codex-feishu/02-store-and-recovery.md)、[03 飞书网关](../codex-feishu/03-feishu-gateway.md)、[06 输入队列与引导](../codex-feishu/06-queue-and-steering.md)。
