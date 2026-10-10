# Codex app-server 0.162.0：主会话、fork、子代理与 worktree 分类

核验日期：2026-10-10。范围仅为本地生成协议与官方准确版本源码，不涉及 UI 操作。未读取用户 rollout 正文、auth、env，也未创建、恢复、修改或发送输入到原生线程。

版本证据：本地协议目录 `/tmp/codex-feishu-research-20261010/protocol-0.162.0/`；官方 tag [`rust-v0.162.0`](https://github.com/openai/codex/tree/rust-v0.162.0) 的 peeled commit 为 [`c1382380de69521303b416720a52f42d51af6248`](https://github.com/openai/codex/commit/c1382380de69521303b416720a52f42d51af6248)。以下源码链接固定该提交。官方公开 app-server 文档用来核对概念，具体行为以准确版本源码为准。[官方 app-server 文档](https://learn.chatgpt.com/docs/app-server)

## 可执行结论

M0 可以可靠识别“明确子代理”和“属于允许项目的已有用户输入主线程候选”；现有字段无法证明线程来自 App UI，更无法仅凭字段证明输入是自然人手工产生。不要把 `source=vscode && originator=codex-tui && threadSource=null` 当成 App 专属条件。

分类顺序应是：**发现候选 → 排除明确子代理/内部来源 → 检查允许项目与 worktree → 等到真实 userMessage → 按 thread.id 建立独立绑定**。人类 fork 允许进入该流程；`forkedFromId` 不是子代理条件。空草稿保留为待激活候选，不建群，也不永久标成“已处理”。

## 字段事实

| 字段 | 0.162.0 实际语义 | 能做什么 / 不能做什么 |
| --- | --- | --- |
| `source` | v2 SessionSource：`cli`、`vscode`、`exec`、`appServer`、`{custom:string}`、`{subAgent:...}`、`unknown`。 | `subAgent` 是强排除证据；来源标签不等于 UI/作者身份。 |
| `threadSource` | 可选客户端分析标签，协议类型是 string。核心已知值 `user`、`subagent`、`guardian_review`、`memory_consolidation`；其他字符串按 Feature 接受。 | 可辅助排除明确非人类标签；null 不表示非人类，user 不表示 App。 |
| `originator` | 创建时记录的 originator，独立于当前客户端/执行器；可为 null。 | 保留诊断与已知自建桥接来源去重；不是可靠的 App/CLI 分类条件。 |
| `parentThreadId` | Thread 注释明确“只在子代理线程上设置”。 | 非空排除；为空仍可能是 review/compact/other 等子代理。 |
| `forkedFromId` | 此线程 fork 的源线程 ID。 | 描述 fork 血缘；不能排除人类 fork，也不能单独证明是人类 fork。先检查 parent/source。 |
| `agentNickname` / `agentRole` | AgentControl-spawned 子代理的可选昵称/角色。 | 有值可帮助诊断；空值不能证明根线程。 |
| `canAcceptDirectInput` | 当前已加载线程是否接受直接 turn 输入。未加载时可能 null。 | false 是输入能力限制；true 不证明主线程。实现只对 multi-agent v2 的 thread_spawn 子代理返回 false。 |
| `preview` | 通常取首个用户消息，但还有非用户消息生成路径。 | 非空只作快速候选提示，不能当作严格首条输入证明。 |
| `turns` | list/一般通知通常为空；read(includeTurns)/resume/fork 才可填充。 | `thread/list` 返回 turns=[] 不能证明是空草稿。 |
| `cwd` | 线程工作目录；加载时 environments 还提供当前环境 cwd 与 runtimeWorkspaceRoots。 | 路径校验的输入，不是会话类型。`path` 是不稳定的历史持久化路径，不是代码目录。 |
| `sessionId` | 声明为会话树关联 ID，具体存储/加载路径存在差异。 | 不用于把 fork 合并到原线程的绑定；以 thread.id 去重。 |

依据：[v2 SessionSource 转换，thread_data.rs 31–74 行](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server-protocol/src/protocol/v2/thread_data.rs#L31)、[Thread 字段，204–293 行](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server-protocol/src/protocol/v2/thread_data.rs#L204)、[核心 ThreadSource 及 SubAgentSource，protocol.rs 2872–2953 行](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/protocol/src/protocol.rs#L2872)、[直接输入能力实现](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/thread_input.rs#L12)。

### source 不能区分 App 和 CLI 的源码原因

- app-server 的 `run_main` 默认传入 `SessionSource::VSCode`；因此 vscode 不意味着线程来自 VS Code，更不能据此断言来自 App。[lib.rs 438–455 行](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/lib.rs#L438)
- CLI TUI 连接远程 app-server 时使用 `client_name="codex-tui"`。[tui/lib.rs 498–512 行](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/tui/src/lib.rs#L498)
- 官方说明 initialize.clientInfo.name 用作 originator；准确版本 initialize processor 也据此设置默认来源。核心有效 originator 还考虑 service name、持久值、继承值和运行配置，不能当成当前入口证明。[官方 initialize 说明](https://developers.openai.com/siwc/token-sharing-open-source/codex-app-server)、[initialize_processor.rs 130–177 行](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/initialize_processor.rs#L130)、[thread_manager.rs 385–396 行](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/core/src/thread_manager.rs#L385)
- 此版本 TUI 新建和 fork 请求均显式传 `threadSource=user`。当前观察到 null 仍符合可选协议，但不是该准确版本 TUI 新请求的必然值；可能是旧线程、客户端未传、其他创建路径，需实际记录创建版本/调用来源确认。[新建参数，app_server_session.rs 2158–2176 行](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/tui/src/app_server_session.rs#L2158)、[fork 参数，2272–2296 行](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/tui/src/app_server_session.rs#L2272)

主代理提供的真实 App / 误用 CLI 对比均为 vscode、codex-tui、null，本文仅把它作为既有观察，不冒充独立 UI 验收结论。

## 候选来源与子代理规则

建议保留 `thread/list` 默认交互来源发现。准确版本默认集合是 **cli、vscode、custom("atlas")、custom("chatgpt")**；并不是“所有非子代理”。`sourceKinds` 省略或空数组都使用该集合。`sourceKinds:["cli","vscode"]` 会漏掉 atlas/chatgpt，而 sourceKinds 枚举没有 custom 项。若将来需要跟进 appServer 或其他已知自建客户端，显式扩展该策略，不自动接受所有 unknown/exec/custom。

来源：[INTERACTIVE_SESSION_SOURCES，rollout/lib.rs 88–95 行](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/rollout/src/lib.rs#L88)、[默认过滤与 source kind 映射](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/filters.rs#L6)。local app-server 拒绝非空 originators 过滤，不能把它当成可用的服务端 App allowlist。[thread_processor.rs 2545–2551 行](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/thread_processor.rs#L2545)

每个候选均二次检查：

1. `parentThreadId` 非空，排除。
2. `source` 是 `{subAgent:...}`，无论是哪种变体均排除：review、compact、thread_spawn、memory_consolidation、other。thread_spawn 内还带 snake_case 的 parent_thread_id，不能把所有子代理只理解成 thread_spawn。
3. 如果接入的是底层 protocol/历史元数据而不是 v2 API，键名是 `{subagent:...}`，还可能出现 `{internal:...}`；应在入口统一规范化，避免大小写误判。v2 会把 Internal 映射为 unknown；故 unknown 不能默认放行。
4. `threadSource` 显式是 subagent / guardian_review / memory_consolidation，保守排除或记录与 source 冲突的诊断；user/null 不取消前三项的排除证据。
5. 已识别来源为交互主线程候选且具备用户输入，再做目录/新线程边界检查。status=idle、父 ID 空、能直接输入都不能替代这一步。

## 首条输入与 fork

状态库对 UserMessage / completed UserMessage 设置 first_user_message；但 delegated FunctionCallOutput 和 ThreadGoalUpdated 也能设置 preview，所以 **preview 非空不严格等价于已有真实用户输入**。[state/extract.rs 140–159、184–200 行](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/state/src/extract.rs#L140)

运行中可靠门槛是该 thread 的公开 userMessage 事件，判定只需 item.type、item/turn ID 与输入是否有效，不需要为了分类持久化正文。启动后补扫历史可通过 app-server 的结构化 turn/items API 确认用户项存在，不直接扫描 rollout 正文。`thread/list` 的 preview/name 仅适合候选预筛；API 没有暴露 first_user_message 字段，不能虚构它。

本地协议 `ThreadTurnsListParams.itemsView` 默认 summary；需尊重返回 Turn.itemsView。notLoaded 表示未加载而非“没有用户输入”，必要时再请求该 turn 的 items；空分页需继续到 nextCursor=null。对旧 legacy 与 paginated 历史都应走 server API，不把一页缺少用户项判成永久空草稿。

fork 默认是新 thread.id，保留 forkedFromId，能够直接继承 preview 和用户历史。官方测试在 fork 响应中断言继承 preview、一个 UserMessage turn。[thread_fork.rs 211–238 行](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/tests/suite/v2/thread_fork.rs#L211)

按“新的人类主会话包括 fork”的需求，**明确人类主线程 fork 且继承有效人类输入，可以纳入独立绑定；不因继承内容或 forkedFromId 去重到原线程**。若产品另要求“fork 后新发输入才建群”，必须保存/确认 fork 边界，区分继承 turn 与 fork 后新增 userMessage；preview、turn 数量、createdAt 对比不足以可靠完成该判断。当前协议没有独立的 forkBoundary 字段；已启动监听可用新事件，历史恢复需要额外 lineage/turn ID 证据。

发现空草稿时保留 pending 候选，等待 userMessage 或后续扫描更新，不把它加入永久 handled 集合。若只按 createdAt 水位扫描，创建时为空的线程以后激活会漏掉；需要 pending 重查或输入事件触发。ephemeral 默认没有持久历史，不自动创建持久飞书绑定，除非产品专门支持这一生命周期。

## allowed_root 与 worktree

本版本没有 `source=worktree` 分类，也没有 Thread 的 worktree 布尔字段。worktree 是执行目录/仓库关系，既可承载主线程，也可承载子代理；来源和目录须分别校验。不要以 `.codex/worktrees` 前缀直接放行，更不要以 rollout `path` 代替 `cwd`。

推荐规则：

1. 对 allowed_root 与候选 cwd 做真实路径解析，使用路径分段的包含关系；`/repo-a` 不能匹配 `/repo-ab`，符号链接解析后越界不能放行。目录不存在/不可解析时标记待核验。
2. 严格“目录白名单”语义下，外部 worktree 需要本来就在 allowed_roots 中。若本产品明确允许“同一允许仓库的注册 worktree”，可按下一条扩展；不能无说明把物理白名单变成同 remote URL 的全部 clone。
3. 从允许仓库读取 `git worktree list --porcelain -z`，得到注册工作树集合；候选 cwd 必须在注册工作树根或其子目录中，再校验 candidate 与允许仓库的 `git rev-parse --path-format=absolute --git-common-dir` 一致。Git 元数据调用使用结构化 argv，不拼 shell；隔离影响仓库定位的 GIT_DIR/GIT_WORK_TREE/GIT_COMMON_DIR 等变量。这里只做 read，不调用 add/prune/repair。
4. 若 allowed_root 只允许仓库内某子目录，保留其相对仓库根的路径限制，例如 `/repo/services/mail` 仅对应 `/worktree/services/mail`，不能借 worktree 规则扩成整个 `/worktree`。
5. 每个 fork 使用它自己的 cwd 重新检查；不能因为 forkedFromId 指向已绑定父线程就自动放行新 cwd。Git branch/remote URL 相同不是相同仓库身份。绑定和 card 页脚记录实际 cwd，不能把源仓库目录误写为 worktree 目录。
6. 对 loaded environments 记录 primary-first 的 cwd/runtimeWorkspaceRoots；这些字段描述当前执行环境，不代表线程来源，也不能自动扩大允许范围。远端环境路径必须在相应主机校验，不用本机 realpath 误判。M0 未支持多个远程环境时只认已配置主机与对应目录。

Git 官方依据：[worktree 的注册关系与 porcelain 格式](https://git-scm.com/docs/git-worktree)、[rev-parse canonical path 与 git-common-dir](https://git-scm.com/docs/git-rev-parse)。Codex 环境字段：[v2/environment.rs](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server-protocol/src/protocol/v2/environment.rs#L20)。本调研未检查用户实际 worktree 注册表，以上是可执行校验方案。

## 尚需真实验收的 M0 分类矩阵

| 场景 | 预期 |
| --- | --- |
| 用户真正通过 App 新建，未发送输入 | pending，不建群；只记录元数据。 |
| 上述草稿发出首条文本/图片输入 | 触发激活，独立跟进；确认 title/preview 不影响门槛。 |
| 真正通过 App fork 一个人类主线程 | 新 thread.id 独立跟进；排除条件不误伤 forkedFromId；不重复镜像继承历史。 |
| fork 到已允许仓库的 worktree | 实际 cwd 正确，注册/仓库/子目录范围校验通过。 |
| 不允许项目或另一 clone/fork 跨目录 | 排除或待核验，不借父线程自动放行。 |
| 人类主线程派生 thread_spawn / review 等子代理 | 不创建独立群，source 变体与 parent 任一明确证据均有效。 |
| CLI 新建或恢复同线程 | 可以记录元数据差异；不得宣称凭当前 source/originator 已辨明 App UI。 |
| 重启时已有 draft 后激活、长历史分页、legacy/paginated fork | 不漏发现，不因 turns=[] 或第一页无 userMessage 误判，不重发继承内容。 |

没有 App UI 创建/首输入/fork/worktree/子代理的真实观察，就不能把这些测试标为通过。若业务要求仅跟进 App UI 而明确排除 CLI，需要额外可验证的客户端归属或 UI 集成来源；本协议字段与当前观察无法兑现该保证。人类主线程规则可以先实现，但应把“协议可验证候选”与“真实 App UI 已验收”分别报告。
