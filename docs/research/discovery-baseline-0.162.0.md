# Codex 0.162.0：首装发现基线与草稿激活

核验日期：2026-10-12。固定官方 rust-v0.162.0 commit `c1382380de69521303b416720a52f42d51af6248`，本地生成协议 `/tmp/codex-feishu-research-20261010/protocol-0.162.0/`。仅阅读官方源码/测试，未运行测试、读取用户 rollout 正文或操作原生线程。主代理报告 Oct12 的真实 App fork/worktree/child 分类已通过；该事实属于集成证据，本研究不声称独立操作或验证 App UI。

## 事件契约

| 操作 | `thread/started` | 订阅与首输入前可见性 |
| --- | --- | --- |
| `thread/start` | 成功响应之后全局广播。 | 自动附 caller 的线程 listener；初始 preview/turns 为空，path 可存在于响应但文件尚未创建。其他已初始化连接能看 started，不因此自动订阅该根线程的全部后续事件。 |
| `thread/fork` | 成功响应与可选恢复 usage 之后全局广播。 | fork 是新 thread.id；通知主动清空 turns，不能用通知 turns=[] 断言无历史/空草稿。按 source/parent 排除子代理，forkedFromId 本身不排除人类主会话。 |
| `thread/read` | 不发。 | 不附 listener；includeTurns=false 可从 loaded snapshot 回答尚未落盘的草稿。 |
| cold `thread/resume` | 不发。 | 附 caller listener，返回已存线程及可选快照/usage/goal；不是全局出生通知。没有 rollout 的草稿会失败。 |
| warm `thread/resume` / `excludeTurns:true` | 不发。 | 将 caller 加入已有线程订阅，返回 metadata；重放该线程 pending requests。excludeTurns 只减历史负载，不取消订阅。草稿仍可能在前置持久化查找阶段被拒绝。 |
| 新观察连接 initialize | 不补发已有根线程 started，也不自动订阅所有已 loaded 根线程。 | 仅登记 live connection；用 list/loaded-list 发现，用可成功的 resume 订阅选定已激活线程。 |

依据：[start response/notification](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/thread_processor.rs#L1575-L1655)、[fork response/notification](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/thread_processor.rs#L5377-L5417)、[通知清空 turns](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/thread_summary.rs#L220-L223)、[read 分支](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/thread_processor.rs#L2824-L2910)、[cold resume](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/thread_processor.rs#L4067-L4236)、[warm 订阅/response/replay](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/thread_lifecycle.rs#L719-L838)、[initialize 仅登记连接](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/thread_state.rs#L384-L395)。

“全局”仅指同一 app-server runtime 的当前 initialized 连接，仍受 notification opt-out 等过滤，不是共享 CODEX_HOME 上不同进程之间的事件总线；不会为断线期间重放 started。Outgoing 的无指定连接 notification 转成 Broadcast，transport 选择 initialized 且未被过滤的连接。[Broadcast 生成](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/outgoing_message.rs#L741-L787)、[传输过滤](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/transport.rs#L218-L241)。

内部 `thread_created_rx` 不等价于上述 thread/started：该 receiver 自动给当时所有 initialized 连接附 listener，源码调用者是 AgentControl 的 spawn/reload/resume；普通根线程 cold/warm resume 不走这个全局附接入口。receiver lag 只告警，不自动补齐。因此不要把“存在 thread_created 监听”解释为“bridge 自动看到 App 恢复的所有旧根线程”。[自动附接与 lag](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/lib.rs#L1291-L1321)、[附接实现](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/thread_processor.rs#L3607-L3643)、[spawn 调用](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/core/src/agent/control/spawn.rs#L953-L959)、[resumed child 调用](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/core/src/agent/control/spawn.rs#L1506-L1513)。

## 列表的薄元数据与草稿

`thread/list(useStateDbOnly:true)` 只查询 state DB，不运行文件扫描/repair；失败是 error，不能当成空历史并完成基线。普通非归档、无具体 section/relation 的列表还过滤 `preview <> ''`，所以列表不能穷举 loaded blank drafts。省略 modelProviders 会过滤为当前 provider；需要覆盖多个 provider 时用 `modelProviders:[]`。分页 cursor 是继续查询边界，没有一个跨所有页的原子快照契约，基线期间插入到已翻过边界之前的新 ID 可能不在后续页。[state DB only](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/rollout/src/recorder.rs#L549-L575)、[preview 过滤](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/state/src/runtime/threads.rs#L1416-L1443)、[provider/filter/每页调用](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/thread_processor.rs#L5494-L5535)。

薄列表中的 `forkedFromId=null` 是实现信息丢失，不能解释成非 fork：list 转换 `stored_thread_from_rollout_item` 硬编码 forked_from_id=None；loaded enrichment 也不补 fork lineage。`thread/read(includeTurns:false)` 的详细存储读取会取 session meta 的 forked_from_id，官方 read-fork 测试有断言。对需要 lineage 的候选单独 metadata-only read；不用缺省 forkedFromId 排除/合并候选。主代理已报告实际 UI fork 在 state-only list 中缺该字段、详细 read 中存在，与源码一致。[list 丢 lineage](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/thread-store/src/local/helpers.rs#L136-L170)、[详细 read](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/thread-store/src/local/read_thread.rs#L326-L372)、[官方 read-fork 测试](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/tests/suite/v2/thread_read.rs#L1353-L1393)。

草稿探测边界：

- `thread/loaded/list` 从当前 ThreadManager 返回所有 loaded ID，包含首输入前的新根线程；随后 metadata-only read 可成功。`path!=null` 是预计算路径，不能证明文件已 materialized、也不能证明有 userMessage。
- `thread/turns/list` 对尚无持久化 rollout 的 loaded draft 返回明确的 before first user message 错误；把这个结果记录成 `pending_draft`，不是永久 ignored。unsupported、暂不可读或空 preview 只能记未知 pending，不能等同“已证明空”。
- `thread/read(includeTurns:true)` 的官方未落盘拒绝测试明确选 legacy；loaded paginated 分支会先 persist_thread，recorder.persist 会 materialize，不能把该请求当成纯只读草稿探针。优先 metadata-only read + turns-list。
- 默认 turns-list 的 itemsView 是 summary；判断首输入须拿到实际 `userMessage` 类型。需要 full 的候选可请求有界页并逐页查到 userMessage，或在 paginated 上使用 items-list；不能用 notLoaded 的 items=[]、preview 非空或 turn 数量替代类型证明。只保留 has_user_input/时间等状态，不把历史正文写入发现记录。
- `thread/resume` 首输入前可能直接报 no rollout，不能靠反复 resume 给 draft 提前订阅；先保留 pending，由 loaded/list/turns-list 的后续观察激活，再按已有 M0 路径 resume。

依据：[loaded list](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/thread_processor.rs#L2777-L2821)、[metadata-only draft 官方测试](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/tests/suite/v2/thread_read.rs#L1398-L1442)、[turns-list 首输入前测试](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/tests/suite/v2/thread_read.rs#L1652-L1698)、[legacy read 拒绝测试](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/tests/suite/v2/thread_read.rs#L1602-L1650)、[paginated read persist](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/thread_processor.rs#L3048-L3066)、[persist 行为](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/rollout/src/recorder.rs#L1063-L1082)、[draft resume 拒绝测试](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/tests/suite/v2/thread_resume.rs#L445-L483)、[turns-list 视图与无 persist 的 paginated 路径](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/thread_processor.rs#L3220-L3252)。

## 建议的首装基线规则

以下是工程建议，不是 Codex 提供的事务协议。核心不变量：**历史 snapshot 只写 ignored；正向新建证据和已保存候选不被 snapshot 覆盖；尚无首输入或时序不明确始终 pending；发现基线完成不产生历史建群动作。**

1. 在外部建群动作之前，持久化一个未完成的 baseline generation。先 initialize 观察连接并开启 durable/deduplicated `thread/started` 缓冲，再开始历史分页；收到的 start/fork 记独立 live birth 证据（按 thread.id），不等基线结束才接收。中途崩溃保留 generation 和这些记录，不能把下一次重试重新定义为新安装。
2. 分页收集既有历史 ID；先将具有下节 UUIDv7 出生证据的基线期间新 ID 写为候选，其余已证明既有历史只插入 ignored，保留此前已写入的新候选/已绑定/pending 状态。基线页中包含一个刚创建的 live birth 时，live candidate 优先。原子提交 baseline 完成标记及这一批历史忽略；DB/list 错误、重复 cursor 或未扫完页不能标成功。
3. 同时读取 loaded IDs 并进行 metadata-only root/scope 核验。明确已有实际历史的既有线程留 ignored；明确尚未 materialized 的 blank draft 留 pending_draft，即使 draft.id 已在首装观察范围内。空 preview、历史读取错误和时序不能证明的 loaded 线程留 pending_unknown，不以未知结果建群，也不永久丢弃。
4. live birth 候选、下节可证明的离线/漏事件新候选，或先有可靠 blank 证据、随后观察到真实 userMessage 的 draft activation，进入后续已有 root/child/worktree 分类与建群。排除子代理/内部来源、ephemeral、越界 cwd；人类 fork 的长期规则需区分继承输入与 fork 本地首输入；当前用户已将 fork 跟进延后，明确详细 read 的 forkedFromId 后保留 `fork_deferred`，不自动建群（见文末范围说明）。普通 App 主会话与已注册 worktree 仍在范围内。对空草稿没有一次性“发现成功”终态。
5. 基线完成后从 list 头部及 loaded IDs 做轻量 catch-up，按 thread.id 去重，继续检查 pending。后续启动复用已完成 generation，不再次把现存候选扫成历史。未见过的旧 ID（DB repair、未归档恢复、不同 provider 曾漏查等）不能因第一次见到或 updatedAt 变新就自动建群；缺正向证据仍 pending/历史忽略；补偿扫描的正向证据见下节，不能把所有未知 ID 一律卡在 pending。忽略的历史线程后续普通 resume/继续输入不自动改为新根线程；需要产品明确的人工接入路径。

`createdAt/updatedAt/recencyAt` 仅辅助顺序与排查，不是首输入时间或原子 fence。API stored-thread 转换为 Unix 秒；list 缺少可解析 created_at 时还回退 Utc::now。已有 loaded draft 可以在安装前创建、安装后第一次输入；反过来老历史继续输入会刷新 updatedAt。因此不要仅用 `createdAt>=startup_now` 或 `updatedAt>=startup_now` 决定建群，尤其不能把一次列表成功当成完整 snapshot。[秒级时间](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/thread_processor.rs#L6122-L6128)、[时间 fallback](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/thread-store/src/local/helpers.rs#L138-L148)。

## 完成基线后的漏事件/离线补偿

**对于这个版本正常生成的本机 native 线程，可用 UUIDv7 时间作独立的出生证据，避免 createdAt fallback。** 固定源码明确 Codex-generated thread IDs 是 UUIDv7，`ThreadId::new()` 调 `Uuid::now_v7()`；新会话/人类 fork 分配新 ID，resume 复用原 conversation_id。RFC9562 §5.7 的 UUIDv7 首 48 bit 是 Unix 毫秒值；验证 version=7 与 RFC variant 后再读取，不能对任意 UUID 字符串取前缀时间。[ID生成](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/protocol/src/thread_id.rs#L14-L30)、[默认generator](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/core/src/thread_manager.rs#L158-L160)、[new/fork 与 resume 分支](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/core/src/session/session.rs#L875-L888)、[RFC布局](https://www.rfc-editor.org/rfc/rfc9562.html#section-5.7)。

可执行升级条件：baseline generation 永久保留 **首次开始时**的毫秒 fence（不能用每次启动时间，也不能把完成时间替换进来）；未知 ID 没有已存 ignored/binding 冲突，正常 native UUIDv7 时间严格晚于 fence、不过分领先当前本机时间，再 metadata-read 核验 persistent、source/parent、允许 cwd/worktree，拿到真实 userMessage 后成为 new_candidate。对于已有 draft pending，继续使用“先 blank 后 input”的证据，即使 ID 时间早于 fence。UUID 的出生时间只证明新 ID，不证明自然人/主线程，所以 source/parent 与首输入检查仍必需。snapshot 中查到这些 post-fence 新 ID 也先建候选，不能先写 ignored 再等待下一轮补偿。

这是**在来源确为正常本机0.162 native、墙钟可比较**的条件下充分的新建证据：旧 root 普通 resume/unarchive、DB repair、切换 provider 都不会新生成该 root 的 ID；其出生时间不会因晚发现而变新。UUID版本不对、相等毫秒边界、明显未来时间、时钟倒退/机器间时钟不可信、导入/自定义 ID 保留 ambiguous pending。这不是抗伪造的安全身份凭据，也不是所有历史版本/远端 thread-store 的 API 保证；该版本允许自定义 ID generator 和解析其他 UUID，不能由 version7 单独证明实际生成来源。[自定义generator](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/core/src/thread_manager.rs#L612-L621)、[解析任意UUID](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/protocol/src/thread_id.rs#L33-L44)。

metadata 的时间 provenance：正常 Create 在 recorder 初始化时写 session_meta.timestamp（UTC毫秒字符串），state metadata builder 从该 timestamp 解析；read 的 SQLite 分支直接用 metadata.created_at。可是 v2 Thread 不返回“时间从哪一条分支取得”的旗标；rollout/session-meta-only read 在缺少可解析值时也会 fallback Utc::now。因此 metadata-read.createdAt 在正常完整原生数据上可交叉检查，不能**无条件**消除 fallback 风险。UUID时间提供的是独立证据，既不是 fallback，也不会被历史 continuation/新文件 mtime 替换。创建 ID 与 recorder 初始化之间可能有启动等待，不要求两者逐秒相等；明显冲突留 pending。[session timestamp 生成](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/rollout/src/recorder.rs#L938-L956)、[builder provenance](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/rollout/src/metadata.rs#L39-L48)、[DB read created_at](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/thread-store/src/local/read_thread.rs#L393-L414)、[session-meta-only fallback](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/thread-store/src/local/read_thread.rs#L484-L495)。

补偿扫描与覆盖范围：

- 保留 periodic/reconnect list + loaded/list，并对未知 IDs应用上述出生证据。`modelProviders:[]` 覆盖不同 provider；不凭 originator 筛 App。每页去重、每轮从头或完成可复用的 post-fence 窗口；不能永远只扫一次最新一页或把本轮高水位提前推进而遗漏晚到的 DB projection。createdAt排序只能优化正常数据扫描，遇到时序冲突不能据它过早断言所有新 ID 已覆盖。
- active 与 archived 是两类独立页；若要覆盖离线期间“创建后立刻归档”，需要 archived:true 的补偿 ID扫描，记录为候选但不向归档线程自动建群；以后 unarchive 仍核验原 ID 的 post-fence 出生证据。若产品刻意不跟进 archived，新 archived线程不建群是范围规则，但不能用 active页完成状态声称覆盖了它。旧 archived 在线前后发现都不因 unarchive 提升为新 ID。
- active DB列表隐藏空 preview，因此 loaded/list/pending重查不可删除。正常有内容的首输入会生成 preview：文本，或 image/file/local_image→`[Image]`，audio→`[Audio]`；首输入写入而 DB投影暂滞后可延后发现。仅goal preview也会让无普通userMessage线程被列出，故仍要检查实际类型。[preview生成](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/protocol/src/protocol.rs#L2643-L2664)、[goal preview路径](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/rollout/src/list.rs#L1138-L1152)。
- 全部未 materialized、又已卸载的草稿没有持久 footprint，也不在 loaded/list；它尚无输入，本来不建群。已 materialized但空 preview且卸载、异常投影或 unreadable source的 ID也可能不能从现有全局 active列表发现；现有协议无法保证枚举这些异常集合，不把“补偿扫描完成”写成无条件全覆盖。

补偿最小fixture：正常0.162 root和人类fork在bridge离线时创建并输入；漏掉其started后扫描仍升级；旧root离线续聊、旧archived unarchive、provider切换/repair均不升级；baseline翻页中post-fence IDs不被snapshot ignore；UUID等于fence/non-v7/未来异常pending；loaded draft后输入可激活；延迟投影ID在后续重复扫描可见，不能因上次scan高水位永久跳过。本研究未运行这些客户端集成场景。

## Typed 草稿探测接口

`thread/turns/list` 的明确 BeforeFirstInput 错误是 JSONRPC `error.code=-32600`，完整 message：

```text
thread {thread_id} is not materialized yet; thread/turns/list is unavailable before first user message
```

只有 code匹配且预期完整模板/`thread/turns/list is unavailable before first user message` 子串匹配，才归入该状态；通用 -32600、no rollout、thread not loaded、unsupported、其他I/O错误不能统统解释为已证明空草稿。不是一个独立data.kind enum，应明确本规则绑定0.162.0。[message映射](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/thread_processor.rs#L5945-L5962)、[read-view error](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/thread_processor.rs#L5846-L5859)、[RPC code](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/error_code.rs#L1-L16)。

`thread/loaded/list` params为 `{cursor?:string|null, limit?:number|null}`，response为 `{data:string[], nextCursor:string|null}`。data仅Thread IDs，不是Thread对象；不返回createdAt/preview/turns。limit省略默认无限制，源码将显式值至少设1；用有界limit并传returned cursor直到null。[实现与分页](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/thread_processor.rs#L2777-L2821)。

## 无法由此源码单独证明的竞态

若草稿第一条输入在“连接初始化 → 首次 blank 核验”之间发生，之后读到 userMessage 只能证明现在有输入；old createdAt 与空的初始薄列表都不能证明输入在安装前还是安装后。没有 blank 前态或可靠的首输入事件/时间证据时保留 pending_unknown，不自动建群、不永久标 historical。若产品要完全覆盖这个窗口，需要客户端集成提供有序创建/首输入证据，或在仅自有 fixture 验证可用的 turn/item 时间契约；本研究不声称现有协议提供跨请求快照与无丢失事件日志。

同样，断线、notification opt-out、receiver lag、不同 app-server 进程创建时，单靠本进程 started 缓冲无法保证全覆盖。UUIDv7条件式补偿可追回正常本机native离线创建；保守策略仍不能保证那些不明确/异常窗口内每个新根线程都立即建群。应暴露 pending 原因并保留可重查记录。

需集成验证的最小场景：baseline 翻页中创建 start/fork；snapshot 与已保存 live candidate 同 ID；baseline 开始前已 loaded blank draft 后续首输入；首输入恰好跑赢 blank 核验；冷/暖 resume 一个历史 root，验证全局 started 不产生且新 bridge 不自动收到该 root 的后续 item；基线中断后重启不重扫/不覆盖候选；state-only list 出错或 DB projection 延迟保持 generation 未完成。源码和官方测试覆盖的是上述端点行为，首装客户端算法仍需自身 fixture/集成验证。


## Fork 延后范围与后续边界参考（2026-10-12）

用户已明确 fork 较少，当前 M1 延后实现；主代理将移除 fork 输入时间 gate 逻辑/测试并留代码 TODO。**当前实现对详细读取确认的 fork 保留 `fork_deferred`，不自动建群；普通 App 主会话与已注册 worktree 继续在范围内。** 上文关于 fork 创建通知、持久 lineage 与离线发现的源码事实仍有效，以下仅为后续参考，不作为当前实现的激活要求或完成证明。本研究没有执行 native fork fixture。

后续不能直接将 fork 里任何 userMessage 当成本地首输入，也不能与“当前父线程的 user item ID 集合”做差：paginated fork 保留显式 turn/item IDs（官方测试保留 `active-turn` / `before-fork`），legacy reducer 的 user item ID 却由计数器合成 `item-N`；父线程 revert 会切换可见历史，而 fork 的 bounded lineage 可仍指向先前范围。因此父当前 ID 集合既不等同 fork 继承快照，也不是所有模式的稳定分支标识；第一次异步 read 的快照还可能已经包含 fork 本地新输入。[paginated fork ID 测试](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/tests/suite/v2/thread_fork.rs#L2037-L2046)、[legacy user ID](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server-protocol/src/protocol/thread_history.rs#L504-L520)、[计数器](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server-protocol/src/protocol/thread_history.rs#L1537-L1540)、[revert 保留旧 rollout](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/thread-store/src/local/revert_thread.rs#L15-L18)、[有界 lineage](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/thread-store/src/local/rollout_lineage.rs#L178-L202)。

正常本机 native 0.162 路径有一个可供后续验证的充分 gate：实际 userMessage 所在 turn 的有效 RFC UUIDv7 出生毫秒**严格大于** fork thread ID 的有效 v7 出生毫秒。`new_submission_id()` 使用 `Uuid::now_v7()`，显式 TurnStarted 持久化 submission ID；paginated projection 和 legacy reducer 都复制显式 turn_id。thread/fork 在固定继承历史后才分配 child reserved ID，正常继承 turn IDs 不会在 child 出生之后生成。这个 gate 仍以正常本机生成、墙钟可比较为条件；non-v7、相等毫秒、时间异常或不明确的历史路径需 pending，不能推断为本地输入。[submission ID](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/core/src/session/mod.rs#L1129-L1136)、[持久化 TurnStarted](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/core/src/session/mod.rs#L2279-L2287)、[paginated projection](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server-protocol/src/protocol/thread_history_projection.rs#L23-L30)、[legacy 显式 ID](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server-protocol/src/protocol/thread_history.rs#L1295-L1304)、[冻结历史后分配 child ID](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server/src/request_processors/thread_processor.rs#L5148-L5200)。

不得把上述结论扩大成“任意 reducer 返回的 v7 turn 都可靠”：legacy persisted rollout 先递增 rollout index，缺少显式 turn 时生成 `rollout-N`；只有 index=0 的直接 live implicit-event 路径会生成新的 v7 fallback。另一个时间字段 `Turn.startedAt` 是**可空的 Unix 秒**，不是毫秒；以它作保守 gate 只能用 `startedAt*1000 > forkBirthMs`，同秒与 null 均不能证明 fork 本地输入。后续启用前应验证继承输入不激活、fork 本地输入激活、父 rollback 不误判、同毫秒/non-v7/implicit 历史保守 pending，以及多次 read 的显式 turn ID 不变。[rollout index](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server-protocol/src/protocol/thread_history.rs#L423-L425)、[implicit ID fallback](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server-protocol/src/protocol/thread_history.rs#L1408-L1415)、[startedAt 单位与可空类型](https://github.com/openai/codex/blob/c1382380de69521303b416720a52f42d51af6248/codex-rs/app-server-protocol/src/protocol/v2/thread_data.rs#L400-L403)。
