# 10 CLI、配置与安装契约

负责 `cmd/codex-feishu/`、`internal/config/`、`internal/setup/`、用户服务模板和发布元数据。依赖各模块健康接口；与 11 一起交付一期可安装版本。

**下表是开发验收契约，不是全部命令均已交付。** 开发版本已有 version、只读 doctor、默认只读 verify、M0 m0-listen、serve、setup plan / apply / pair / migrate、service、status 和附件状态/保守清理命令。新版 serve 已接入仅同用户可访问的健康通道，status 优先查运行实例，缺失时退回只读快照。doctor 已读取运行实例、实际 daemon 版本及 tenant 权限授权，version 内嵌权限能力清单。候选服务已实际部署到独立 schema 26 数据库，旧 M0 数据保留；`phase1_released=false` 时默认只允许配置预览，拒绝正式 apply、service install / start。显式候选模式已验证安装、重跑与保留数据卸载，保留未发布状态，禁止既有数据库迁移。配对已通过真实机器人私聊，并容许首尾空白和开头经核验的本机器人 mention；通知规则事务及迁移命令已通过隔离测试。完整自动测试编排、真实迁移与发布验收仍待完成；显式 acceptance-report 已提供只读报告核验与结束判定，不自行运行测试或替代人工 UI 观察。当前构建与真实证据范围见[集中验收记录](12-live-acceptance-handoff.md)。Agent 配置指南默认只在已验收版本进入应用阶段；明确授权的候选安装按指南中的独立路径执行，不能自动当作正式部署。

## 命令表

飞书中发送 `/help` 可查看分点列出的全部聊天指令；使用 `/help 指令名` 查看单条用法，例如 `/help model` 或 `/help /notify`。未启用功能会明确标注，群聊中通常须 @机器人；符合[单人任务群条件](03-feishu-gateway.md)的操作者可免 @。详见 [指令帮助](08-cards-and-actions.md#飞书指令帮助)。

飞书任务群或对应话题内发送不带参数的 `/model` 打开原生设置表单；仅操作者可用。表单显示当前模型、思考强度和 Fast，保存一次提交全部修改，取消不写原生设置；修改只用于后续轮次，不修改部署配置文件。选项取当前服务端模型目录，具体行为见 [08 卡片与动作](08-cards-and-actions.md#会话模型思考强度与-fast)。

| 命令 | 责任与副作用 |
| --- | --- |
| version --json | 版本、构建 commit、支持的命令、配置 schema、兼容清单摘要 |
| doctor --config PATH --json | 检查环境/配置/连接；只读，不创建群或输入 |
| doctor --config PATH --require-running --json | 启动后必需的连接、绑定恢复及发现基线检查；复用现有管理通道 |
| setup --input PATH --plan --json | 检查部分输入、生成配置差异和稳定 plan_id；不写正式配置、不启动服务 |
| setup --input PATH --apply --plan-id ID --json | 应用已知计划；备份旧配置、原子写入，不自行启动 |
| setup --input PATH --config PATH --database PATH --candidate-test --plan / --apply --plan-id ID --json | 显式候选安装计划/应用，数据库只接受不存在或私有当前 schema；计划绑定候选模式；仍非正式发布 |
| setup pair --input PATH [--config PATH] --timeout 300 --json | 限时私聊配对获取 owner open_id；复用已有接收器或只监听配对的临时接收器，不改变同步配置 |
| setup migrate --config PATH --plan --json | 核验静止的 M0 与原生历史、群成员，保存私有摘要计划；不备份、不迁移、不发送 |
| setup migrate --config PATH --apply --plan-id ID --json | 停止旧桥后持锁、一致备份、重新核验并原子接管；不启动服务，开发版本拒绝执行 |
| service install --config PATH --user --json | 创建/更新本工具的 systemd 用户服务，不操作 Codex 服务 |
| service start / stop --config PATH --json | 启停本工具用户服务 |
| service install / start --config PATH --candidate-test [--user] --json | 候选安装/启动；install 必须 --user；不替换活动 unit，不迁移旧数据库，绑定候选二进制摘要 |
| service uninstall --config PATH --keep-data --json | 删除本工具服务，默认保留配置、密钥与数据库 |
| serve --config PATH | 前台常驻组合根；单部署锁、重连、优雅退出 |
| status --config PATH --json | 两侧连接、服务、绑定数、队列、outbox 和 unknown 操作摘要 |
| attachments status / cleanup --config PATH --json | 读取附件开关、磁盘额度与聚合用量；cleanup 须停桥并取得双锁，只清理无活动引用的生成文件 |
| worktrees audit --config PATH --report PRIVATE_JSON --json | 只读核验桥创建记录、原生 metadata 与当前 Git 身份，排他写入 0600 私有审计计划；标准输出仅聚合计数 |
| worktrees repair --config PATH --report PRIVATE_JSON --json | 停桥取得应用/监听双锁，重新核对同一计划和物理代际，只补缺失 owner；冲突、未知或目录缺失保留待核验 |
| file-preview show-secret --config PATH | 显式向标准输出打印当前网页预览口令；拒绝 JSON 模式，仍核验私有口令文件 |
| verify --config PATH --target self --json | 依授权范围运行本人测试卡/私有测试群/独立 Codex 测试线程，产出验收记录 |
| verify --config PATH --acceptance-report PATH --json | 只读核验明确提供的当前构建案例报告及私有证据摘要；人工 UI 来源单列，不执行测试或自动发布 |

所有路径用绝对路径。setup/verify 的文件输入可走受控 stdin，但密钥正文不通过命令行参数。帮助与 --json 不打印长期密钥或全量对话。version 输出必须提供权限与兼容清单的内容或实际可读位置，不能要求配置 Agent 猜发布包内路径。

工作树维护复用已有 daemon，仅连接读取 metadata；不启动 app-server、不 resume、不新建工作树或聊天。兼容层限定 App Server 0.162.1 / 桌面 26.1016.62342，桌面版本无法由远程协议自动检测。`repair` 的独立步骤先落盘后写 owner，unknown 只核验原结果；不会恢复其他 creation lane 或改写已完成 workflow。服务未停止返回 `stop-required` / 退出 6，有冲突或 unknown 返回 `repair-pending` / 退出 5。私有报告不可覆盖，修复后使用新报告路径再次 audit。已排队的桌面删除不能靠补 owner 撤销；维护窗口、冲突处置和回归边界见[实施记录](../records/managed-worktree-ownership-2026-10-23.md)。

verify 接收 `--allow-test-card`、`--allow-test-group`、`--allow-test-thread`、`--allow-test-binding` 四个显式标志；无标志只复用配置 doctor 的 GET 与管理健康 / 只读快照，不新开 native 客户端或接收器、不创建 / chmod / 迁移数据库。完整 P1 场景证据尚未整合，必需项 pending、退出 5；连接与权限通过不表示验收通过。CLI 不从“用户愿意配置”自动推断能给任意人发消息。

当前开发版已接入正式实例的本人测试卡组件：只传 `--allow-test-card --request-id ID`，由已有实例持久领取并发送；`ID` 为调用前生成并保留的 32 位小写十六进制随机值。卡片按钮和私聊 `/verify TOKEN` 经过唯一接收器记录，不进入任务或原生队列。使用相同 `--request-id`、不带任何 allow 标志时只读取原记录；重复授权相同 ID 也不重发 dispatching / unknown / rejected / confirmed。schema 18 保存原 build commit、运行二进制 SHA-256、UUID、卡正文与 5 分钟到期；回调须匹配 owner 和实际 confirmed message ID。未发布状态不变，完整 P1 仍 pending。

管理测试群组件已接入：`--allow-test-group --request-id ID` 只创建 owner 与 bot 的验证群，冻结名称、成员与 UUID，不继承额外生产成员，不发卡 / 消息或绑定线程。创建后用 GET 核验 bot、owner 和完整名单，再记录本人在原群 @ 机器人发送 `/verify-group ID`。成员检查失败时，相同授权 ID 只重做 GET；unknown / rejected / confirmed / 遗留 dispatching 均不重复建群。schema 19 保存私有证据，状态查询不做成员 API 或其他外部写入。

四个 allow 标志独立授权；所有请求能力在第一项 mutation 前检查。无 allow 且有 ID 时查询运行实例支持的卡 / 群 / 线程创建 / 绑定日志，缺日志返回 skipped / not-found，不生成或激活测试。旧实例缺少受控创建能力时，组合 thread 标志不部分发送卡片或建群。已有管理实例保留唯一接收器。只有单独授权 card / group、没有管理实例、同时取得应用与数据库监听锁，并且已有私有 schema 1 日志时，才保留旧 M0 准备能力；线程不退回独立连接。锁后重新核验诊断所用配置与密钥；变更返回冲突。可写打开只接受所属用户的 0700 物理目录 / 私有文件与准确 schema，不创建或迁移。UUID 在 API 前落盘，unknown / rejected 不重发；这不是正式一期 verify。

受控线程创建已接入开发实例管理通道：`--allow-test-thread --request-id ID` 只使用既有 initialized 原生连接，创建固定本机模型 / 只读策略的隔离线程，不启动 turn、绑定群、提交输入或 archive。私有资源在 5 分钟窗口内，断开 / unknown / 重启不替换原端点或重发；查询只读原记录。`verification-managed-thread-creation-observed` 仍 `ok=false` / 退出 5、12 项 P1 pending，创建不证明群绑定、排队 / 引导或 App UI。不得由配置 Agent 绕过完整验收安装未发布服务，见[管理创建组件](../records/m3-managed-thread-creation-2026-10-12.md)。

`--allow-test-binding --request-id ID` 只绑定同一 ID 已 confirmed 的测试群和线程：要求 owner 群验证消息、原构建 / 连接 / 资源 / 窗口及新成员 GET / 附着检查。它不会创建缺失目标或自动提交输入；登记后向现有 runtime 请求跟进，owner 的新群输入走正式默认队列 / 显式引导路径。私有创建、绑定与查询不续期；State 不激活。`binding_status=pass` 是本地登记，`activation_requested` 仅为本次入队，`actor_started` 仅为本地 actor 存在。CLI 和版本能力明确保留 UI、完整 verify 与一期发布 pending，见[管理绑定组件](../records/m3-managed-binding-2026-10-12.md)。

绑定的原 ID 查询现在还返回可选 `input_evidence_status` 与 `input_evidence`：在一个只读事务中核验原登记 / 路由，再统计已登记输入、默认 add 确认、保存的队列快照、真实 user item 关联、同动作 delete + steer 确认及目标轮历史关联、未决输入尝试。未决或队列消失不作为成功证据；查询失败为 pending / 无快照，旧 provider 缺字段也不能当零计数 pass。结果不含正文、令牌或相关 ID，不执行新的 queue / history GET。`queued_snapshot` 是已保存的观察，不证明当前仍排队或忙时顺序；这些组件检查不提升 P1 或 App UI。详见[受控输入证据](../records/m3-managed-input-evidence-2026-10-12.md)。

原绑定查询还返回可选 `delivery_evidence_status` / `delivery_evidence`。它只读取本地 AI 镜像与不可变投递日志，区分过程 / 最终卡、当前 revision 确认、引用证明、整轮最终分片完成和未决卡片尝试。原 reply 的 parent、chat 与当前输入锚点不匹配，或只有普通 send 时，不算引用证据。已发 retired 尾片需清理确认；从未发送的 suppressed 尾片须无未决 / 已确认创建记录才能跳过。`ui:` 队列卡不冒充 AI 过程卡，但其未决尝试仍可报告。缺字段 / 读失败保持 pending，不做消息 GET、PATCH、发送或修复。组件 pass 不提升 P1 或 UI，见[受控投递证据](../records/m3-managed-delivery-evidence-2026-10-12.md)。

受控忙时测试模型的第一条响应会暂缓，供 owner 验证默认排队和点击追加引导。绑定后、第一条模型请求已经开始时，可单独使用 `verify --config PATH --target self --release-test-turn --request-id ID --json` 开放原模型的回复门。该标志不能与四个创建 / 绑定标志组合；要求版本能力 `verify_managed_turn_release=true`、运行实例 `verification_turn_release_supported=true`，并重新核验原绑定、群成员、构建、连接、资源与五分钟窗口。重复释放只打开同一门，不再提交输入，不延长窗口；无标志查询从不释放。`model_released=true` 仅表示本机回复门已打开，`verification-managed-turn-released` 仍 `ok=false` / 退出 5，不证明原生轮次完成、结果送达或 App UI。详见[受控本轮释放](../records/m3-managed-turn-release-2026-10-12.md)。

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

默认 verify 和管理组件 observed 保留 pending。显式 `--acceptance-report` 才评估已有案例报告，不能与 allow、release-test-turn 或 request-id 混用；输入校验先于任何运行时诊断。要求 owner 0600 常规文件、严格 schema、完整且不重复的 12 个案例、配置 identity、host/app/owner、当前 CLI 与运行桥的 commit/实际二进制 SHA-256、数据库 schema、CLI/daemon/SDK 版本及固定协议语料摘要。报告或实际版本缺失不算相等。协议摘要核验的是构建兼容清单，不在 verify 中重新生成或查询 daemon schema。

证据条目为 `{kind, path, sha256, observed_by?}`，kind 为 automated/manual；引用须是本人 0600 常规文件并逐个核验摘要。人工条目 observed_by 须为 owner；App/飞书 UI、配置 Agent、服务生命周期等案例必须有明确人工确认记录，自动计数或 mock 不能替代。CLI 只检查报告身份、完整性和文件一致性，案例覆盖与人工记录真实性仍由验收者审核；不解析截图来猜测通过，不输出文件正文或 blocking_reason。案例缺证据、文件变更或人工未确认退出 5；身份/构建冲突退出 6；输入不合法退出 2；当前诊断失败保留原退出码，不能被报告覆盖。

全部必需案例证据有效且当前诊断通过时返回 `acceptance-report-validated` / 退出 0，含 `ui_evidence_source=explicit-manual-report`、`recorded_cases_all_passed`，每个案例仍说明 `independently_observed_ui=false`。这仅表示明确记录的报告与当前部署核验通过，不自行切换 phase1_released、发布、迁移、创建目标或操作输入；ready_to_release 布尔值不是成功依据。

安装前 doctor 默认允许桥尚未运行：仅在配置、私有密钥、实际共享 server 版本、tenant 机器人身份及必需权限全部确认后返回 `prerequisites-ready-bridge-not-observed`，未观察到的实例仍以非必需 pending 列出。启动后必须加 `--require-running`；连接、绑定恢复、初次历史基线或远端操作待核验时退出 5。若已存在运行实例，即使没有该标志，其未恢复状态也不能忽略。配置摘要或 socket 身份冲突退出 6。已有数据库不可读、版本不支持或部署身份不符退出 3；从未初始化的数据库只作安装前说明，不创建或迁移。

配置 doctor 只查询既有管理端点、daemon version、机器人身份与权限 GET，不新建原生客户端或飞书长连接。独立的 `doctor --codex-only` 才执行短暂只读 handshake / thread list。权限按 API 文档认可的 OR 替代关系检查，只计算 `grant_status=1` 且 `scope_type=tenant`；缺字段、未知状态或授权响应不明确保留 pending。附件下载权限只在 `enabled=true` 时成为 required；独立附件的群接收权限还需 `receive_standalone=true`。关闭时不改变 baseline required/recommended scopes。单人任务群免 @ 的 `receive_one_to_one_group_without_mention` 独立报告 `im:message.group_msg`，保持可选，不受附件开关影响。scope 满足不证明消息订阅、卡片回调、建群、更新、reaction、附件收取、免 @ 或 App UI 已验收：每项能力的 `runtime_observed` 仍为 unknown，实际 verify 单独提供证据。

## 配置与输入文件

可选本地文件预览通过 `file_preview` 对象配置，默认关闭。setup 省略该对象时保留当前设置，显式提供时整体替换，启用时私有口令的变化会使旧 plan 失效。`file-preview init-secret` 只生成新的 0600 口令文件；`file-preview show-secret` 显式打印口令；`file-preview link` 本地登记指定文件并返回可点击地址。这些 CLI 均不发送消息或改变 Codex 会话。部署所有者也可私聊机器人发送 `/preview` 取得口令；群内只提示转到私聊，口令回复按现有私有 outbox 持久投递。监听、访问范围及真实浏览器验收见 [14 网页预览](14-file-preview.md)。

正式配置默认 `~/.config/codex-feishu/config.toml`；状态默认 `~/.local/share/codex-feishu/bridge.db`，尊重已有 XDG 路径。不得覆盖 HOME 或 CODEX_HOME。安装工具展开这些路径后传给服务，运行时可见路径全为绝对路径。

- [正式配置样例](examples/config.example.toml)：配置文件不含 secret 正文，只引用 app_secret_file。
- [向导输入样例](examples/setup-input.example.json)：输入身份、目录、偏好和验证授权；允许缺 owner 后配对。

正式 TOML 与向导 JSON 是不同 schema。setup 负责转换，拒绝未知键、占位值、不可访问目录、错误 open_id 类型与 schema 版本。通知/操作者为空时向导按已配对 owner 补齐；正式配置不得含 null 身份。identity.owner_open_id 写入 access.owner_open_id，identity.group_member_open_ids 写入 groups.additional_member_open_ids；owner 和机器人始终是默认成员。host_id 首次生成后沿用，配置重跑不换 ID、不重建数据库。setup pair 使用专用部分输入校验，不要求已经存在 owner。

新建配置以及加载 TOML 时省略的交互字段使用问答默认值：`interactions.enabled=true`、`enabled_types=["question", "async_question"]`。显式 `enabled=false` 关闭全部交互；显式类型清单整体覆盖默认清单，开启时的空清单仍被拒绝。setup 重跑保留已有选择，不向旧清单自动加入异步问答或审批类型。详见 [13 审批与问答](13-feishu-approvals-and-questions.md#41-配置与验收操作)。

向导 JSON 的 `creation` 对象是可选的；省略时保留已有 creation 配置，显式提供时整体替换。`features.creation=true` 仍要求显式 creation 范围，不能从 discovery.allowed_roots 推断；`features.phase` 可为 `phase1` 或 `phase2`，phase2 不会隐式开启创建。启用创建必须提供真实可读、可进入的 `creation.allowed_roots`，最终执行目录另需可写；入口群、工作群和 common directories 只按其自身范围校验。默认跟进位置为 group，topic 需要开启 topics 并配置工作群；draft TTL 为 0 时使用 1800 秒。计划只展示入口群和目录范围计数，topic 与创建 UI 仍需独立真实验收。

目录交互统一使用飞书原生卡片与既有长连接：最近 / 项目速选、显式项目名 / 绝对路径搜索、带路径导航 / 当前层筛选 / 分页的目录面板。无需额外 HTTP listener、域名、反向代理、H5 或 OAuth 配置。长连接传递卡片操作事件；目录读取和权限校验均在远程桥进程完成，结果更新原卡。平台 API 校验和模拟测试仍不等于真实手机布局或交互验收。

配置中的 notifications 是新用户的初始化默认值，数据库的用户设置优先；重跑安装不重置用户通过命令关闭的 @。向导读取现有有效规则；用户明确要求修改当前 quiet 时，将该用户规则差异单列进 plan 并应用，不将默认空值当“清掉当前规则”。

codex.cli_path=auto 只用于初次发现；setup 应将实际 CLI 启动器绝对路径写进生成配置或服务环境，并核验 daemon 实际版本。socket 每次运行动态发现；不能固定版本目录中的临时 socket。

## 向导事务与幂等

plan 展示：使用的二进制与兼容版本、身份与目录、自动建群默认值、通知默认值、配置/密钥引用路径、服务变更及允许的测试副作用。计划摘要脱敏；plan_id 绑定输入、现有配置版本及将使用的二进制。输入或文件在应用前发生变化时重新 plan，不忽略冲突。

apply 用部署配置锁，备份旧配置，原子写临时文件并 rename；配置/secret 文件 0600、目录 0700。数据库迁移采用可检查版本，迁移失败保留旧配置与数据库，不能清空重装。

同一有效输入再次 apply 为 no-op；重复 install 不添加第二份服务；start 已运行返回现状。重新配置只更新本工具配置，不能自动开启二期、升级 Codex 或修改原会话权限。

当前开发实现：计划实际核验所选二进制、现有 daemon、密钥文件权限、目录和现有部署；输入、二进制、密钥或配置变化使旧 plan_id 失效，兼容版本 daemon 的临时 socket 变化不影响恢复。已有 host_id 和数据库路径保留。通知 off / snooze 保持原值；向导仅修改本人明确提供的 quiet / timezone，不改变其他接收者。省略 quiet 保留现有规则，显式空字符串清除 quiet。

apply 同时取得应用与数据库监听锁。已有规则的更新要求私有、准确 schema 26 数据库；完整状态比较后在同一 SQLite 事务提交规则与审计，重复执行不增加 revision 或审计。数据库旁的部署标记与配置 prepared 日志在配置替换前持久化，完整更新完成后保存 applied 并清理部署标记。中断返回 `setup-recovery-pending` / 退出 5；保持原输入文件、二进制、密钥、原 plan_id、配置备份和事务文件，再重跑原 apply。不要用新计划覆盖未完成事务，也不要删除日志。serve 在连接或迁移前拒绝未完成事务；改用备份/复制配置或数据库 symlink 别名也不能绕过同一数据库的恢复检查。当前 apply 不创建或迁移数据库，旧 M0 schema 1 仍须单独迁移与绑定接管验收。自动测试证据见[通知配置事务进展](../records/m3-policy-apply-2026-10-12.md)。

附件设置通过 setup JSON 顶层可选 `attachments` 对象传入；省略时保留既有设置，显式提供时整体替换并按配置校验。`enabled` 和 `receive_standalone` 都是布尔开关，默认关闭；目录省略时为数据库旁的 `attachments`，预算为 10 资源、5 图片、单图 10 MiB、图片合计 20 MiB、单文件 20 MiB、批次 50 MiB、单图 40M 像素、1 GiB 磁盘和 2 个下载 worker。plan 在启用时列出本地私有存储与限额；只有 `receive_standalone=true` 才提示敏感 `im:message.group_msg` 和权限发布，设为 `false` 时关闭免 @ 的独立图片/文件收取，post 图文仍按结构化 @ 或单人任务群免 @ 规则接收。doctor 仅输出 enabled、根目录、限额聚合和 `ui_verified=false`，不输出正文、资源 key 或密钥。

改变运行中配置前须停止本工具实例，再重新 plan / apply；应用锁或数据库锁被占用时拒绝替换配置。service install 更新正在使用的 unit 后仅报告 restart pending，须明确 stop / start；start 对已运行服务是 no-op。配置 Agent 不应将这两种情况写成更新已经生效。

## M0 迁移事务

`setup migrate --plan` 只接受现有私有 schema 1 数据库与配置；通过既有 daemon 的只读 metadata / history / queue 方法和飞书 GET，确认 App 主线程、完整静止历史、空队列、允许目录、机器人及 owner 在原群。fork 暂缓。旧日志未知或进行中结果不能自动接管。计划和公有 JSON 不含原文或密钥；私有数据库旁保存摘要与原 plan_id，预览可能创建 SQLite 读取 sidecars。计划默认 15 分钟有效，重复相同预览沿用有效 ID。

`--apply` 要求已验收发布版本，配置、二进制、密钥、原生历史和群成员与计划一致。先由部署者核验并停止准确的旧 bridge 进程，保留 Codex daemon；命令取得配置锁、按物理数据库命名的共享计划锁、App 锁及数据库监听锁。不能靠复制配置绕过锁。使用 SQLite 一致备份包含已提交 WAL，0600 私有备份禁止覆盖；备份后重读全部证据，再在一个事务内登记原群、原消息 ID、输入关联和 schema。不会建群、发消息、提交输入或安装服务。

prepared marker 在写数据库之前持久化，中断阻止 serve 和配置 apply。保留同一配置、二进制、密钥、计划、备份，重跑原 `setup migrate --apply --plan-id ID`。如果数据库已提交，通过不可变接管记录恢复 marker，不重新生成 seeds 或覆盖后来活动。备份前后数据变化使计划失效；已确定未提交时可重新预览，旧备份保留。不要删除 marker、改 user_version、清空日志或直接复制活跃 WAL 的主文件。迁移成功只证明本地接管成功，实际启动、消息更新和 App UI 必须另验。见[M0 接管进展](../records/m3-legacy-adoption-2026-10-12.md)。

## 配对与凭据

用户没有 open_id 时，setup pair 创建 128 bit 随机单次口令，默认有效期 5 分钟，`--timeout` 可取 1–300 秒；只接受同应用、用户私聊、精确文本 `/pair <口令>`。每个实例只允许一个活动请求；口令过期或确认后不能再认领。已有 owner 时只接受原 owner，配对不提供身份重置。

新部署先持有应用锁，机器人身份及长连接 ready 核验通过后才输出口令；临时接收器忽略其他消息、不建群、不执行 Codex 输入，所有退出路径均关闭并等待接收器结束。已有部署须用 `--config` 指定实际配置（默认用户配置路径），严格核验应用、物理密钥引用与 owner；优先复用现有实例的管理通道。旧 M0 无管理通道时还必须取得其数据库监听锁，否则退出冲突，不抢占事件连接。运行中正式实例继续处理正常同步，配对消息在普通入站处理之前被拦截。

`--json` 输出逐行事件：首先是 `pairing-waiting`，其中 `data.pair_command` 仅输出一次；终态 `paired` 含 `data.owner_open_id`，不含口令。超时、取消、连接中断或状态未知返回非成功结果。Ctrl+C 和开始请求的响应丢失均按原 request_id 尝试取消，不重试创建新口令；同 request_id 的底层重试不延长原到期时间。配对状态只在内存中，口令不写数据库、配置或普通日志；CLI 不自动写配置。

输入 app_secret 推荐受控 TTY 或已有 0600 文件；无安全输入渠道时请用户在本机创建凭据文件，仅提供路径。API token 在内存或受限缓存中保存；不进入 chat、argv、git 或普通日志。配对口令可以为指导展示一次，禁止长期记录。

## 用户服务与生命周期

Linux 首选 systemd --user，ExecStart 使用绝对二进制路径和配置路径，WorkingDirectory 不影响 Codex 会话 cwd。设置合理 Restart=on-failure，保留日志脱敏和本工具部署锁。安装器只管理 codex-feishu.service，不管理 App 的 daemon。

运行中 status/verify/pair 复用现有实例，不另开竞争正式事件的 ws 连接。可以通过仅同用户可访问的本机 Unix 控制通道查询健康与提交测试/配对请求，限制为这些管理用例，不提供额外网络执行 API。

当前新版 serve 提供 Linux Unix socket 管理通道，私有目录 0700、socket 0600，客户端和服务端均核验 SO_PEERCRED 的系统用户；只允许 health、限时 pair.begin / pair.state / pair.cancel，以及固定 owner 的 verify.card.begin / verify.card.state、verify.group.begin / verify.group.state 方法，不提供任务输入或执行 API。请求绑定物理配置路径与当前完整配置摘要，配置已改变而服务未重启时拒绝报告生效。消息大小、并发连接和等待均有上限；退出关闭全部管理连接，清理只删除自己的 socket。

status 优先从匹配的运行实例读取真实 receiver ready、原生 reader 生命周期、当前代际绑定生命周期及聚合日志；未决 / 未知变更不报告全部健康。`binding_hydration_scope` 分别列出普通绑定与有完整原始私有标记的隔离测试绑定；支持自适应生命周期的实例还设置 `lifecycle_managed=true`，并报告互斥的 `active_bindings`、`dormant_bindings` 和 `retrying_bindings`。`hydrated_bindings` 只要求当前 active 的普通绑定已核验；dormant 不占 actor，也不阻止就绪，等待激活或退避重试的绑定计入 retrying 并保持 pending。三个生命周期计数必须恰好等于普通绑定数，持久 `bindings` 仍为包括隔离测试路由的总数。测试资源过期或服务重启后不再拥有原资源，不应阻止普通服务就绪；现有私有标记与路由/原始组件不匹配，或读取失败时整体核验失败，不能排除后报告就绪。旧实例未提供自适应字段时继续按原 scope 或总数核验；所有 unknown / dispatching 日志仍参与健康判定，分类不续期或授权测试资源。前台运行不要求已经安装 systemd unit；用户服务及注销常驻另列事实，安装向导仍须验证其生命周期。旧 M0 没有管理通道，退回只读检查用户服务、daemon 和数据库计数，可读 schema 1 而不迁移；实时连接 unknown、必需项 pending、退出码 5。systemd active、daemon running 或持久记录可读均不能代替实时桥健康证明。

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
