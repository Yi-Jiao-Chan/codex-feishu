# 10 CLI、配置与安装契约

负责 `cmd/codex-feishu/`、`internal/config/`、`internal/setup/`、用户服务模板和发布元数据。依赖各模块健康接口；与 11 一起交付一期可安装版本。

**下表是开发验收契约，不是全部命令均已交付。** 开发版本已有 version、只读 doctor、默认只读 verify、M0 m0-listen、serve、setup plan / apply / pair / migrate、service 和 status。新版 serve 已接入仅同用户可访问的健康通道，status 优先查运行实例，缺失时退回只读快照。doctor 已读取运行实例、实际 daemon 版本及 tenant 权限授权，version 内嵌权限能力清单。新版尚未替换 M0 实例；`phase1_released=false` 时只允许配置预览，拒绝正式 apply、service install / start。配对、管理测试卡和管理测试群已通过自动测试，真实私聊验收仍待完成；通知规则事务及迁移命令已通过隔离测试；完整 verify、真实迁移与发布验收仍待完成。详见[配置与服务管理进展](../research/m3-setup-and-request-reminders-2026-10-12.md)、[管理通道与离线取消进展](../research/m3-control-and-offline-cancel-2026-10-12.md)、[只读诊断与权限清单](../research/m3-doctor-and-permissions-2026-10-12.md)、[私聊配对进展](../research/m3-pairing-2026-10-12.md)及[验证命令与数据库保护](../research/m3-verify-safety-2026-10-12.md)。Agent 配置指南仅在通过完整联调的桥版本存在时进入应用阶段。

## 命令表

| 命令 | 责任与副作用 |
| --- | --- |
| version --json | 版本、构建 commit、支持的命令、配置 schema、兼容清单摘要 |
| doctor --config PATH --json | 检查环境/配置/连接；只读，不创建群或输入 |
| doctor --config PATH --require-running --json | 启动后必需的连接、绑定恢复及发现基线检查；复用现有管理通道 |
| setup --input PATH --plan --json | 检查部分输入、生成配置差异和稳定 plan_id；不写正式配置、不启动服务 |
| setup --input PATH --apply --plan-id ID --json | 应用已知计划；备份旧配置、原子写入，不自行启动 |
| setup pair --input PATH [--config PATH] --timeout 300 --json | 限时私聊配对获取 owner open_id；复用已有接收器或只监听配对的临时接收器，不改变同步配置 |
| setup migrate --config PATH --plan --json | 核验静止的 M0 与原生历史、群成员，保存私有摘要计划；不备份、不迁移、不发送 |
| setup migrate --config PATH --apply --plan-id ID --json | 停止旧桥后持锁、一致备份、重新核验并原子接管；不启动服务，开发版本拒绝执行 |
| service install --config PATH --user --json | 创建/更新本工具的 systemd 用户服务，不操作 Codex 服务 |
| service start / stop --config PATH --json | 启停本工具用户服务 |
| service uninstall --config PATH --keep-data --json | 删除本工具服务，默认保留配置、密钥与数据库 |
| serve --config PATH | 前台常驻组合根；单部署锁、重连、优雅退出 |
| status --config PATH --json | 两侧连接、服务、绑定数、队列、outbox 和 unknown 操作摘要 |
| verify --config PATH --target self --json | 依授权范围运行本人测试卡/私有测试群/独立 Codex 测试线程，产出验收记录 |

所有路径用绝对路径。setup/verify 的文件输入可走受控 stdin，但密钥正文不通过命令行参数。帮助与 --json 不打印长期密钥或全量对话。version 输出必须提供权限与兼容清单的内容或实际可读位置，不能要求配置 Agent 猜发布包内路径。

verify 接收 `--allow-test-card`、`--allow-test-group`、`--allow-test-thread` 三个显式标志；无标志只复用配置 doctor 的 GET 与管理健康 / 只读快照，不新开 native 客户端或接收器、不创建 / chmod / 迁移数据库。完整 P1 场景证据尚未整合，必需项 pending、退出 5；连接与权限通过不表示验收通过。CLI 不从“用户愿意配置”自动推断能给任意人发消息。

当前开发版已接入正式实例的本人测试卡组件：只传 `--allow-test-card --request-id ID`，由已有实例持久领取并发送；`ID` 为调用前生成并保留的 32 位小写十六进制随机值。卡片按钮和私聊 `/verify TOKEN` 经过唯一接收器记录，不进入任务或原生队列。使用相同 `--request-id`、不带任何 allow 标志时只读取原记录；重复授权相同 ID 也不重发 dispatching / unknown / rejected / confirmed。schema 18 保存原 build commit、运行二进制 SHA-256、UUID、卡正文与 5 分钟到期；回调须匹配 owner 和实际 confirmed message ID。未发布状态不变，完整 P1 仍 pending。

管理测试群组件已接入：`--allow-test-group --request-id ID` 只创建 owner 与 bot 的验证群，冻结名称、成员与 UUID，不继承额外生产成员，不发卡 / 消息或绑定线程。创建后用 GET 核验 bot、owner 和完整名单，再记录本人在原群 @ 机器人发送 `/verify-group ID`。成员检查失败时，相同授权 ID 只重做 GET；unknown / rejected / confirmed / 遗留 dispatching 均不重复建群。schema 19 保存私有证据，状态查询不做成员 API 或其他外部写入。

三个 allow 标志独立授权；所有请求能力在第一项 mutation 前检查。无 allow 且有 ID 时查询运行实例支持的卡 / 群 / 线程创建日志，缺日志返回 skipped / not-found，不生成测试。旧实例缺少受控创建能力时，组合 thread 标志不部分发送卡片或建群。已有管理实例保留唯一接收器。只有单独授权 card / group、没有管理实例、同时取得应用与数据库监听锁，并且已有私有 schema 1 日志时，才保留旧 M0 准备能力；线程不退回独立连接。锁后重新核验诊断所用配置与密钥；变更返回冲突。可写打开只接受所属用户的 0700 物理目录 / 私有文件与准确 schema，不创建或迁移。UUID 在 API 前落盘，unknown / rejected 不重发；这不是正式一期 verify。

受控线程创建已接入开发实例管理通道：`--allow-test-thread --request-id ID` 只使用既有 initialized 原生连接，创建固定本机模型 / 只读策略的隔离线程，不启动 turn、绑定群、提交输入或 archive。私有资源在 5 分钟窗口内，断开 / unknown / 重启不替换原端点或重发；查询只读原记录。`verification-managed-thread-creation-observed` 仍 `ok=false` / 退出 5、12 项 P1 pending，创建不证明群绑定、排队 / 引导或 App UI。不得由配置 Agent 绕过完整验收安装未发布服务，见[管理创建组件](../research/m3-managed-thread-creation-2026-10-12.md)。

旧 M0 准备后只能使用保留的历史 M0 二进制处理其回调。当前开发版 m0-listen 的 Store.Open 会拒绝未接管的 schema 1，不能拿它替换运行中的旧实例；正式迁移须另行完成验收。

## JSON 与退出码

统一 envelope：schema_version、command、ok、state、checks、next_steps；按命令补 plan_id、artifact、paths、service、verification 等字段。checks 包含 id、status、required、detail、remediation、evidence_ref。status 只取 pass / fail / pending / skipped。

| 退出码 | 含义 |
| --- | --- |
| 0 | 所请求步骤成功；doctor 必需项全部通过 |
| 2 | 输入、schema 或命令参数错误 |
| 3 | 缺二进制、共享 daemon、身份一致性等前置条件 |
| 4 | 飞书凭据、权限、发布或可见范围故障 |
| 5 | 操作结果未知 / 需用户完成真实 UI 验证 |
| 6 | 计划已过期、实例锁冲突或配置变更冲突 |
| 7 | 内部错误；输出脱敏诊断位置 |

服务运行与完整验收分开：status 显示 running 不等于 verify pass。verify 必需项 pending 时不返回“配置完成”。示例输出见 [doctor 示例](examples/doctor-output.example.json)，它是格式样例，不是实际健康报告。

安装前 doctor 默认允许桥尚未运行：仅在配置、私有密钥、实际共享 server 版本、tenant 机器人身份及必需权限全部确认后返回 `prerequisites-ready-bridge-not-observed`，未观察到的实例仍以非必需 pending 列出。启动后必须加 `--require-running`；连接、绑定恢复、初次历史基线或远端操作待核验时退出 5。若已存在运行实例，即使没有该标志，其未恢复状态也不能忽略。配置摘要或 socket 身份冲突退出 6。已有数据库不可读、版本不支持或部署身份不符退出 3；从未初始化的数据库只作安装前说明，不创建或迁移。

配置 doctor 只查询既有管理端点、daemon version、机器人身份与权限 GET，不新建原生客户端或飞书长连接。独立的 `doctor --codex-only` 才执行短暂只读 handshake / thread list。权限按 API 文档认可的 OR 替代关系检查，只计算 `grant_status=1` 且 `scope_type=tenant`；缺字段、未知状态或授权响应不明确保留 pending。scope 满足不证明消息订阅、卡片回调、建群、更新、reaction 或 App UI 已验收：每项能力的 `runtime_observed` 仍为 unknown，实际 verify 单独提供证据。

## 配置与输入文件

正式配置默认 `~/.config/codex-feishu/config.toml`；状态默认 `~/.local/share/codex-feishu/bridge.db`，尊重已有 XDG 路径。不得覆盖 HOME 或 CODEX_HOME。安装工具展开这些路径后传给服务，运行时可见路径全为绝对路径。

- [正式配置样例](examples/config.example.toml)：配置文件不含 secret 正文，只引用 app_secret_file。
- [向导输入样例](examples/setup-input.example.json)：输入身份、目录、偏好和验证授权；允许缺 owner 后配对。

正式 TOML 与向导 JSON 是不同 schema。setup 负责转换，拒绝未知键、占位值、不可访问目录、错误 open_id 类型与 schema 版本。通知/操作者为空时向导按已配对 owner 补齐；正式配置不得含 null 身份。identity.owner_open_id 写入 access.owner_open_id，identity.group_member_open_ids 写入 groups.additional_member_open_ids；owner 和机器人始终是默认成员。host_id 首次生成后沿用，配置重跑不换 ID、不重建数据库。setup pair 使用专用部分输入校验，不要求已经存在 owner。

配置中的 notifications 是新用户的初始化默认值，数据库的用户设置优先；重跑安装不重置用户通过命令关闭的 @。向导读取现有有效规则；用户明确要求修改当前 quiet 时，将该用户规则差异单列进 plan 并应用，不将默认空值当“清掉当前规则”。

codex.cli_path=auto 只用于初次发现；setup 应将实际 CLI 启动器绝对路径写进生成配置或服务环境，并核验 daemon 实际版本。socket 每次运行动态发现；不能固定版本目录中的临时 socket。

## 向导事务与幂等

plan 展示：使用的二进制与兼容版本、身份与目录、自动建群默认值、通知默认值、配置/密钥引用路径、服务变更及允许的测试副作用。计划摘要脱敏；plan_id 绑定输入、现有配置版本及将使用的二进制。输入或文件在应用前发生变化时重新 plan，不忽略冲突。

apply 用部署配置锁，备份旧配置，原子写临时文件并 rename；配置/secret 文件 0600、目录 0700。数据库迁移采用可检查版本，迁移失败保留旧配置与数据库，不能清空重装。

同一有效输入再次 apply 为 no-op；重复 install 不添加第二份服务；start 已运行返回现状。重新配置只更新本工具配置，不能自动开启二期、升级 Codex 或修改原会话权限。

当前开发实现：计划实际核验所选二进制、现有 daemon、密钥文件权限、目录和现有部署；输入、二进制、密钥或配置变化使旧 plan_id 失效，兼容版本 daemon 的临时 socket 变化不影响恢复。已有 host_id 和数据库路径保留。通知 off / snooze 保持原值；向导仅修改本人明确提供的 quiet / timezone，不改变其他接收者。省略 quiet 保留现有规则，显式空字符串清除 quiet。

apply 同时取得应用与数据库监听锁。已有规则的更新要求私有、准确 schema 21 数据库；完整状态比较后在同一 SQLite 事务提交规则与审计，重复执行不增加 revision 或审计。数据库旁的部署标记与配置 prepared 日志在配置替换前持久化，完整更新完成后保存 applied 并清理部署标记。中断返回 `setup-recovery-pending` / 退出 5；保持原输入文件、二进制、密钥、原 plan_id、配置备份和事务文件，再重跑原 apply。不要用新计划覆盖未完成事务，也不要删除日志。serve 在连接或迁移前拒绝未完成事务；改用备份/复制配置或数据库 symlink 别名也不能绕过同一数据库的恢复检查。当前 apply 不创建或迁移数据库，旧 M0 schema 1 仍须单独迁移与绑定接管验收。自动测试证据见[通知配置事务进展](../research/m3-policy-apply-2026-10-12.md)。

改变运行中配置前须停止本工具实例，再重新 plan / apply；应用锁或数据库锁被占用时拒绝替换配置。service install 更新正在使用的 unit 后仅报告 restart pending，须明确 stop / start；start 对已运行服务是 no-op。配置 Agent 不应将这两种情况写成更新已经生效。

## M0 迁移事务

`setup migrate --plan` 只接受现有私有 schema 1 数据库与配置；通过既有 daemon 的只读 metadata / history / queue 方法和飞书 GET，确认 App 主线程、完整静止历史、空队列、允许目录、机器人及 owner 在原群。fork 暂缓。旧日志未知或进行中结果不能自动接管。计划和公有 JSON 不含原文或密钥；私有数据库旁保存摘要与原 plan_id，预览可能创建 SQLite 读取 sidecars。计划默认 15 分钟有效，重复相同预览沿用有效 ID。

`--apply` 要求已验收发布版本，配置、二进制、密钥、原生历史和群成员与计划一致。先由部署者核验并停止准确的旧 bridge 进程，保留 Codex daemon；命令取得配置锁、按物理数据库命名的共享计划锁、App 锁及数据库监听锁。不能靠复制配置绕过锁。使用 SQLite 一致备份包含已提交 WAL，0600 私有备份禁止覆盖；备份后重读全部证据，再在一个事务内登记原群、原消息 ID、输入关联和 schema。不会建群、发消息、提交输入或安装服务。

prepared marker 在写数据库之前持久化，中断阻止 serve 和配置 apply。保留同一配置、二进制、密钥、计划、备份，重跑原 `setup migrate --apply --plan-id ID`。如果数据库已提交，通过不可变接管记录恢复 marker，不重新生成 seeds 或覆盖后来活动。备份前后数据变化使计划失效；已确定未提交时可重新预览，旧备份保留。不要删除 marker、改 user_version、清空日志或直接复制活跃 WAL 的主文件。迁移成功只证明本地接管成功，实际启动、消息更新和 App UI 必须另验。见[M0 接管进展](../research/m3-legacy-adoption-2026-10-12.md)。

## 配对与凭据

用户没有 open_id 时，setup pair 创建 128 bit 随机单次口令，默认有效期 5 分钟，`--timeout` 可取 1–300 秒；只接受同应用、用户私聊、精确文本 `/pair <口令>`。每个实例只允许一个活动请求；口令过期或确认后不能再认领。已有 owner 时只接受原 owner，配对不提供身份重置。

新部署先持有应用锁，机器人身份及长连接 ready 核验通过后才输出口令；临时接收器忽略其他消息、不建群、不执行 Codex 输入，所有退出路径均关闭并等待接收器结束。已有部署须用 `--config` 指定实际配置（默认用户配置路径），严格核验应用、物理密钥引用与 owner；优先复用现有实例的管理通道。旧 M0 无管理通道时还必须取得其数据库监听锁，否则退出冲突，不抢占事件连接。运行中正式实例继续处理正常同步，配对消息在普通入站处理之前被拦截。

`--json` 输出逐行事件：首先是 `pairing-waiting`，其中 `data.pair_command` 仅输出一次；终态 `paired` 含 `data.owner_open_id`，不含口令。超时、取消、连接中断或状态未知返回非成功结果。Ctrl+C 和开始请求的响应丢失均按原 request_id 尝试取消，不重试创建新口令；同 request_id 的底层重试不延长原到期时间。配对状态只在内存中，口令不写数据库、配置或普通日志；CLI 不自动写配置。

输入 app_secret 推荐受控 TTY 或已有 0600 文件；无安全输入渠道时请用户在本机创建凭据文件，仅提供路径。API token 在内存或受限缓存中保存；不进入 chat、argv、git 或普通日志。配对口令可以为指导展示一次，禁止长期记录。

## 用户服务与生命周期

Linux 首选 systemd --user，ExecStart 使用绝对二进制路径和配置路径，WorkingDirectory 不影响 Codex 会话 cwd。设置合理 Restart=on-failure，保留日志脱敏和本工具部署锁。安装器只管理 codex-feishu.service，不管理 App 的 daemon。

运行中 status/verify/pair 复用现有实例，不另开竞争正式事件的 ws 连接。可以通过仅同用户可访问的本机 Unix 控制通道查询健康与提交测试/配对请求，限制为这些管理用例，不提供额外网络执行 API。

当前新版 serve 提供 Linux Unix socket 管理通道，私有目录 0700、socket 0600，客户端和服务端均核验 SO_PEERCRED 的系统用户；只允许 health、限时 pair.begin / pair.state / pair.cancel，以及固定 owner 的 verify.card.begin / verify.card.state、verify.group.begin / verify.group.state 方法，不提供任务输入或执行 API。请求绑定物理配置路径与当前完整配置摘要，配置已改变而服务未重启时拒绝报告生效。消息大小、并发连接和等待均有上限；退出关闭全部管理连接，清理只删除自己的 socket。

status 优先从匹配的运行实例读取真实 receiver ready、原生 reader 生命周期、当前代际已核验绑定数及聚合日志；未决 / 未知变更不报告全部健康。前台运行不要求已经安装 systemd unit；用户服务及注销常驻另列事实，安装向导仍须验证其生命周期。旧 M0 没有管理通道，退回只读检查用户服务、daemon 和数据库计数，可读 schema 1 而不迁移；实时连接 unknown、必需项 pending、退出码 5。systemd active、daemon running 或持久记录可读均不能代替实时桥健康证明。

检测 user manager 与注销后的常驻能力；服务在当前 SSH 中 active 不能证明退出 SSH 后仍在。若 linger/现有 supervisor 条件不足，输出具体系统阻塞，不能用一次后台 nohup 宣称完整常驻。工具不自行申请 sudo、修改系统级服务或暴露 Codex 网络端口。

## 本模块验收

1. 从全新用户配置到可用服务，Agent 只需业务信息；重复执行没有第二份服务或绑定。
2. 命令不存在、版本不兼容、计划过期、secret 文件权限错误均返回明确机器可读结果。
3. 配对获得 open_id，非匹配消息不能认领；临时配对连接退出后进入正式运行。
4. 备份/原子更新/迁移故障不损坏旧状态；卸载默认保留数据。
5. 非交互调用、SSH 退出与重启后的运行符合实际服务管理器条件。
6. 将 [Agent 配置指南](AGENT-SETUP.md) 单独交给一个没有本项目历史的 Agent，也能完成检测、收集、配置与验证，或报告具体前置阻塞。

## 本地开发验收包

代码和文档提交且 Git 干净后，在源码根目录执行：

```sh
go run ./cmd/dev-package --output /absolute/private/dist --json
```

当前构建本机 Linux amd64 / arm64 的 development 包；不是跨平台正式发布器，也不安装或启动实例。源码身份、二进制 `version --json` 的 commit、开发状态和兼容版本必须一致，打包后再次核验 Git 与白名单文件摘要。源文件不是普通文件、输出路径不私有 / 含 symlink、产物已存在但内容不同，均拒绝。构建继承 Go 缓存，stage 在输出私有目录，失败清理暂存。

包包含 `bin/codex-feishu`、`artifact.json`、`SHA256SUMS`、`compat/codex.json` 与精确飞书权限清单、Agent 指南、配置 / 向导样例、服务示例和完整 P1 pending 验收模板。全包按显式白名单组装，不遍历复制源码、`.env`、真实配置、数据库或研究日志。时间和成员顺序固定，相同输入可复现；重复相同归档 no-op，冲突文件不覆盖，归档和外部 SHA256 文件为 0600。

`artifact.json` 的 `channel=development`、二进制的 `phase1_released=false` 和 pending 验收模板均保留。校验和证明收到的字节与期望一致，不证明一期已通过；未知来源的 archive/checksum 同时替换也不能建立来源可信度。须从已知来源取得外部摘要，先核验归档，安全解包，再核验内部 `SHA256SUMS`，最后执行二进制 version 并读取能力。正式发布仍要求 11 的完整矩阵，不能通过修改 JSON 或编译标志自称验收通过。
