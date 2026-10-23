# 执行卡布局、停止按钮与工具摘要：本地候选部署

日期：2026-10-22。适用：现有本机 `codex-feishu.service` 用户服务。状态：本地候选部署与后端连接 verified；旧负 margin 版桌面 UI fail；本人私聊全宽合成预览已获用户桌面确认，并已按预览更新本地服务；真实操作、连续刷新及手机 UI pending；`phase1_released=false` 保持。

## 构建身份

- 基于 `196586e6fd88d83f0cacdd78915e1d146fed64b6`，包含 `codex/compact-task-progress` 尚未提交的卡片、工具投影与动作校验改动。
- 构建标识：`196586e6fd88+task-progress-20261022-171740-3c992f76`。
- 二进制 SHA-256：`8f286a4639f71417fb239da9750d541578f7bdebff62d0ecd1bef5d592ef3687`。
- 源文件摘要清单的 SHA-256：`3c992f76687cbb1743a049fdb49f872d2520f06371de62d0b9b426ae633e2419`。构建前后逐项核对输入没有变化，差异与新增文件副本保存在私有版本目录。

构建继承已有 Go 缓存，使用 `-trimpath`。Go 自动 VCS 查询退出 128，改用 `-buildvcs=false`，通过显式构建标识、源文件摘要及二进制摘要记录身份，没有修改 Git 或 Go 全局配置。本轮功能与并发验证范围及限制见[集中清单](../codex-feishu/12-live-acceptance-handoff.md#2026-10-22-执行卡布局与工具摘要本地候选部署)。

## 切换与备份

用户授权本地部署试用。部署前核对实际运行二进制、稳定 CLI、原配置和检查结果；旧版本也是 `196586e`，部署前两侧连接与发现基线均通过。

沿原 CLI `service stop` → 新 CLI `service install --candidate-test --user` → `service start --candidate-test` 切换同一服务。没有重新应用 setup、改写配置或重启共享 Codex daemon。配置字节摘要前后一致；daemon version 只读结果一致。

停止桥后保存原 unit、配置、setup 状态、旧二进制 / CLI 链接引用和包含已提交 WAL 的 SQLite 一致备份。备份 schema 30、55 张业务表、159,493 条记录，`quick_check=ok`；此次没有数据库迁移。私有构建、不可覆盖的备份与脱敏聚合证据位于 `~/.local/lib/codex-feishu/task-progress-20261022-171740-omdeztn7/`，不进入 Git。旧二进制保留；回滚只切回二进制和原服务单元，不用旧数据库覆盖部署后的会话活动。

## 部署核对与待验收

| 项目 | 证据与状态 |
| --- | --- |
| 服务与构建 | verified：systemd active / running / enabled，实际进程和稳定 CLI 均匹配新二进制摘要 |
| 后端连接 | verified：运行实例、Codex 连接、飞书接收器与发现基线检查通过 |
| 原有恢复状态 | `bindings_reconciled`、`operations_known` 与部署前同为 pending，doctor/status 仍退出 5；无新增 required fail 或 pending |
| 卡片渲染 | pending：动作摘要入口、停止按钮点击区域、短记录直接阅读、长文与工具详情展开的真实客户端表现 |
| 连续更新与停止 | pending：展开后的更新是否保留展开状态、同卡停止、队列继续及终态移除停止按钮 |

没有向原会话注入测试输入、创建替代会话或主动发送合成卡片。正常服务沿原绑定继续同步，客户端观察由用户在原任务群试用确认。本次部署不提升 P1 或发布状态。

## 2026-10-22 客户端反馈后的第二次布局调整

首版已在真实桌面截图中观察到，但尚未通过整体验收：短标题重复出现在内层摘要和原文，工具详情再次折叠，状态行单独占高，时间包含秒，整卡采用紧凑宽度。按用户反馈调整为：

- 执行卡单独使用 `width_mode=fill`，标题采用首句/首行、最多 120 个字符兜底，折叠箭头跟随文字；不改变其他卡片宽度。
- 只保留一个外层折叠面板；展开后直接阅读全文与工具摘要，长文和块 Markdown 保持完整。公开记录时间仅显示 `HH:MM`，不改变接收时间的存储与时区。
- 普通运行用时并入标题；等待、停止处理中及未知状态继续显著显示。已全部完成的工具组只显示一次完成数量，其他状态仍分别列出；安全标签并排显示。
- 停止按钮缩成纯图标，复用原令牌、revision、已投递卡与原轮次校验。取消额外状态行后，请求标记可沿真实原生分栏进入正文；原有面板 ID 和全卡读回规则保持。

用户另要求悬停执行行才出现停止按钮。核对公开字段后，普通 button / 分栏 / 折叠面板没有已核实的此类显示规则；只有 checker 的 button_area 支持，且附带复选框及勾选行为，不能作为普通执行行使用。已说明这一限制，用户选择保留常驻纯停止图标、去掉文字；原 `/stop` 命令仍可用，没有将悬停效果标为已实现。字段依据见[当前卡片契约](../codex-feishu/08-cards-and-actions.md)。

第二次部署已完成，构建标识 `196586e6fd88+task-progress-20261022-173723-92dfd07c`，二进制 SHA-256 `cbdfa7f3e932a4263321168af31758826dd305759948fc000b61649aa23bdfa3`，构建输入清单 SHA-256 `92dfd07cc69f56f7093474ab442ca46767e6ee4d4d2db73c2f855920213a1929`。构建期间输入摘要保持一致；其后仅补充部署交付记录。私有版本目录 `~/.local/lib/codex-feishu/task-progress-20261022-173723-6oa6kkl3/` 保存旧版引用、源文件快照与备份。停止桥后的一致数据库备份 schema 30、55 张表、161,957 条记录，`quick_check=ok`。

验证：`go test ./internal/cards`、`go test ./internal/sync` 通过；`go test -race ./internal/cards ./internal/store ./internal/app -run '^Test(Task|CompletedTask|.*RequestMarker)' -timeout 120s` 通过，覆盖新版 renderer、嵌套请求标记及原动作领取校验。文档链接和 diff 检查通过。首次部署记录中的全仓既有失败与 store 全包 race 超时限制仍保留，本轮未重跑无关用例或记为通过。

同一 user service 已切换为新二进制，实际进程与稳定 CLI 摘要一致，systemd active / running / enabled；配置字节、daemon version 读回和数据库 schema 保持。运行实例、两侧连接与发现基线通过；既有两个 required pending 保持，没有新增 required fail / pending。未向会话注入测试输入或发送合成卡片。新版桌面、窄屏、手机与持续刷新后的展开状态仍待用户试用，不提升发布验收状态。

## 2026-10-22 再次按最初参考图修正展开内容

第二次真实客户端截图仍未达到最初参考图的结构：每条正文前独占一行的时间、工具调用后的名称行，以及标题用时增加了多余的信息层级。本次不再将这些字段作为参考图中的必要内容，改为正文与灰色计数行交错排列。

每条公开记录只渲染一个完整 normal Markdown；工具组仅一行箱形图标与“已调用 N 次工具”，不展示安全工具名称。进行中、失败、拒绝和未知仍作准确附注；拒绝和未知不计入已调用次数，避免把可见请求总数说成实际执行。接收时钟与原投影仍保留，界面不再逐条展示时间。普通执行标题不显示用时，原生 Markdown 标题保留 inline code；终态耗时和异常状态说明保持。

合成回归直接按最初参考图的正文甲、7 次工具、正文乙、3 次工具核对四个平铺 children、灰色箱形图标，以及没有时间、名称或内层折叠。测试不能替代真实飞书展示验收。

停止图标继续采用用户已确认的常驻纯图标。官方面板标题没有按钮槽，兄弟按钮与展开正文仍在同一分栏；未找到同时让停止按钮紧贴标题箭头、展开正文跨列满宽的公开原生布局。因此图标仍在右侧，这处已明确说明，不能声称完全复制参考图。没有添加未经核实的负 margin、悬停规则或假复选框。

第三次部署已完成，构建标识 `196586e6fd88+task-progress-20261022-174838-16a1f3f7`，二进制 SHA-256 `1672370e166da75fc39b1fa396c0af8cdd22e9481acf03c6cdddeb61b912182a`，构建输入清单 SHA-256 `16a1f3f7d0fbc15bc8e4fe0522225147f27b433bce85297a3ea5c847c7f03017`。构建期间输入保持一致，之后仅更新交付文档。私有版本目录 `~/.local/lib/codex-feishu/task-progress-20261022-174838-3wy5yevd/` 保留旧版引用、源文件和一致数据库备份；备份 schema 30、55 张表、163,983 条记录，`quick_check=ok`。

验证：`go test -count=1 ./internal/cards`、`go test ./internal/sync` 通过；cards / store / app 的 `^Test(Task|CompletedTask|.*RequestMarker)` 定向 race（120 秒上限）通过，文档链接和 diff 检查通过。全仓既有超时与 store 全包 race 未完成的限制沿用前文，没有改为通过。

部署核对：同一服务 active / running / enabled，实际进程和稳定 CLI 指向第三次构建；配置字节、共享 daemon version 读回和 schema 保持。两侧连接和发现基线通过，两个既有 required pending 保持，没有新增 required fail / pending。正常服务投递的只读聚合观察到新版箱形图标卡片 252 次 confirmed，214 张最新 revision 已投递；另有 14 次 rejected、1 次 unknown，观察时永久 rejected 出站为 0。unknown 仍沿原核验链路处理，没有手动重发，不把该聚合说成所有卡片成功。

没有向会话注入测试输入、主动发送合成卡片或重启共享 daemon。API 接受新结构与真实视觉验收分开：新客户端视觉验收仍 pending，前两次截图只证明各自旧版表现；停止图标位置差异仍保留，不提升发布验收状态。

## 2026-10-22 恢复工具组与逐调用展开，纠正字号

用户补充参考图和明确反馈：调用数量是可展开入口，组内每次调用需单独查看，前一次把它压成纯计数并将公开正文从 notation 改为 normal 都没有满足要求。本轮恢复工具组与逐调用的原生折叠，公开正文及工具详情统一 12px；14px 仅保留在外层动作入口。不是把公开长文再次逐条折叠。

投影新增按原生 ID 保留的逐调用项；重复工具名不合并。读取/写入/编辑/删除等标题只使用原生动作和安全 basename，展开显示状态与有限元信息，不展示命令、参数、输出、完整目录或 diff。组标题改为“调用 N 个工具”，拒绝与未知不冒充已执行；数量和原生顺序保持。

每组最多保留最新 32 项详情，组件与 JSON 字节预算再优先保留最新项，省略数如实披露且计数不变。外层、组与单项 ID 稳定；任务控制补充文件 path/kind 变化的版本失效，diff 不参与摘要。原停止、继续、同卡投递与 unknown 核验链路不变，无数据库迁移。

第四次部署已完成，构建标识 `196586e6fd88+task-progress-20261022-181124-01e3d3ea`，二进制 SHA-256 `fb7937690756ab6c21e496f1237f2eadfc75ae965dc95b857b86eb898fcabac4`，构建输入清单 SHA-256 `01e3d3ea5e37c1bda4557b58ff0c60a247384bbf17838f5758c958a53b71e763`。构建期间输入保持一致，其后仅补充交付文档。私有版本目录 `~/.local/lib/codex-feishu/task-progress-20261022-181124-gzfjgzs6/` 保留旧版、源文件快照与一致数据库备份；备份 schema 30、55 张表、165,436 条记录，`quick_check=ok`。

验证：

- `go test ./cmd/... ./internal/... ./compat -skip '^TestRuntimeGroupDeadlinePreservesFinalAndWakesWithoutNewInput$'` 通过，排除的是已在原 main 复现的既有超时，本轮未将该例记为通过。
- cards / codexclient / sync 的 `Task|CompletedTask|Progress|Tool|Clock|RequestMarker` 定向 race（120 秒上限）通过；app / store 的 `^TestTask` race 通过。
- 覆盖原生 name/path 解码、逐调用 ID/顺序、同名不合并、原生文件动作、安全 basename、状态区分、每组最新 32 项、组件上限、JSON 字节裁剪、嵌套深拷贝、12px、稳定 ID、请求标记及文件动作变更使旧 stop 失效。
- 文档链接、gofmt 和 diff 检查通过。此前 store 全包 race 超时的历史限制没有写成全包通过。

部署后 systemd active / running / enabled，进程和稳定 CLI 匹配新二进制；配置字节、共享 daemon version 读回、schema 保持，两侧连接和发现基线通过。两个既有 required pending 保持，没有新增 required fail / pending。正常服务投递的只读聚合观察到嵌套组及逐项面板卡片 220 次 confirmed，其中含停止按钮的执行中卡片 7 次 confirmed；215 张最新 revision 已投递。另有 1 次 rejected，观察时永久 rejected 出站为 0；本轮聚合未观察到新版嵌套卡 unknown。没有手动重发 unknown、注入测试输入或发送合成卡片。

API 接受嵌套结构不替代客户端验收。新版真实客户端的字号、组展开、单项展开及 PATCH 后展开状态保留仍 pending；先前回归仅证明当时的错误展示结构，不能替代本轮需求验收。

## 2026-10-22 执行入口标题统一小字号

用户标记的是执行过程卡外层入口标题，该处仍使用 normal（14px），此前只调整了展开后的正文与工具详情。本轮将入口标题也统一为 notation（12px），改用与工具面板一致、有第一方实现依据的 `plain_text` / notation / grey；没有将通用 Markdown 的字号能力当作面板标题保证。标题摘要去除 inline code 的反引号，完整正文 Markdown 保持。折叠与停止链路不变，不调整人类正文卡或附件布局。

第五次部署已完成，构建标识 `196586e6fd88+task-progress-20261022-193137-c92526ba`，二进制 SHA-256 `8cae3ca0a5e9549e671a4fe233a39b0148447fd45d508d86216529d854d6b8e6`，构建输入清单 SHA-256 `c92526ba8c3c55c7e00c6e5d95fa15dc01abfc9227f9710a1a97f4e994d18db7`。构建期间输入保持一致，后续仅补充交付文档。私有版本目录 `~/.local/lib/codex-feishu/task-progress-20261022-193137-6bb0697e/` 保存旧版本、源文件快照及一致数据库备份；备份 schema 30、55 张表、166,880 条记录，`quick_check=ok`。

`go test ./internal/cards ./internal/sync`、app / store 的 `^TestTask` 定向测试、文档链接与 diff 检查通过。本轮仅改标题样式，沿用前文全仓排除既有超时后的结果及相关 race 证据，不宣称重跑或通过未排除的全仓测试。

服务 active / running / enabled，实际进程及稳定 CLI 与新二进制一致；配置字节、daemon version 只读结果与 schema 保持。两侧连接与发现基线通过，原有两个 required pending 保持，没有新增 required fail / pending。正常服务投递的只读聚合确认 3 次带停止按钮、外层标题为 plain_text / notation 的执行卡已获 API confirmed。没有主动发送合成卡片或注入测试输入；API 接受样式字段不替代当前客户端实际字号验收，UI 仍 pending。


## 2026-10-22 工具单项平铺与 HH:MM 恢复

用户明确纠正：只有“调用 N 个工具”的组入口可以展开，单次调用应直接概括实际动作，没有自己的展开箭头；此前删除逐条时间也不符合要求。本轮移除叶子折叠及详情正文，改为一行灰色 notation（12px）Markdown 与原生动作图标，保留原生调用顺序、重复名称、状态和完整组计数。外层入口、工具组、纯停止图标及自适应宽度继续沿用，终态保留过程历史。

命令摘要复用现有 Item.Command 和 commandActions：本地有界纯词法子集识别常见 Git、Go 测试/构建、文件搜索与发布命令，单个调用最多联合三个用途，例如“查看 Git 状态；运行 Go 测试”。文件名优先沿用原生结构化元信息。只解析实际命令位置、受控 shell 包装及常见串联，不执行 shell，不新增模型/RPC，不转发参数、凭据、查询、完整路径或输出。行内脚本只说明脚本类型；复杂替换、重定向或未知程序保守标注用途未识别，不将引用中的命令文本当作执行动作。停止控制的 fingerprint 共享同一安全摘要投影，实际用途变化使旧卡失效，私有参数变化不会单独改变版本。

公开记录恢复既有接收时间的 HH:MM 展示，不显示秒；普通段落与时间同行，块 Markdown 的时间独立前置，保留完整正文和所有 notation 字号。时区沿用配置，未指定默认 Asia/Shanghai，无效值回退 UTC；零/缺失时间不编造，刷新和重启不重新计时。没有修改持久化时钟或数据库 schema。

验证完成：cards 与 sync 包回归、app/store 的 `^TestTask` 通过；全仓业务测试在排除已于未修改 origin/main 复现的 `^TestRuntimeGroupDeadlinePreservesFinalAndWakesWithoutNewInput$` 后通过。首轮全仓还遇到一个仍期待旧通用标签的工具摘要断言，已按具体联合用途更新并重新通过全仓，不把首轮失败记作通过。cards/codexclient/sync 的 `Task|CompletedTask|Progress|Tool|Clock|RequestMarker|Command` 定向 race，以及 app/store 的 `^TestTask` 定向 race 通过。覆盖时区、无秒、块 Markdown、无叶子折叠、字面转义、动作识别、引用/脚本保守降级、敏感参数隔离、同卡控制及实际字节/组件预算。文档链接及 diff 检查通过。

已将原本地服务更新为 `196586e6fd88+task-progress-20261022-193752-71749bdd`，二进制 SHA-256 `d8e745f27a28f03430612105a0dc556c72bb400a74678ab03379233c4fcb9cf6`，构建源快照 SHA-256 `71749bdd950a6dbfb7fa089cac946c16716cfd6aec74badf55d8317875257c87`。私有部署目录为 `~/.local/lib/codex-feishu/task-progress-20261022-193752-5e0c1538/`，保存源快照、构建、部署、核验和旧版回退材料。停桥后以 SQLite backup API 保存一致备份，quick_check=ok、schema 30、55 表、167137 行；配置保持。实际进程 PID 3980840 与稳定 CLI 都匹配新二进制，systemd active/running/enabled，两侧连接与发现基线通过；共享 daemon version 只读结果一致，未重启 daemon。原 bindings_reconciled、operations_known 两个 required pending 及 doctor/status 退出 5 保留，无新增 required fail/pending，phase1_released=false。

附加版本核对脚本首次将 schema 字段误读到 capabilities 下而报错，该次不计通过；随后按 data.database_schema/config_schema/phase1_released 与 capabilities.candidate_deployment 分别重新核验，均符合候选身份。部署脚本的运行二进制、配置、备份及启动核对通过。

部署后仅只读聚合正常服务投递，不注入输入或发送合成卡片：观察到 215 次新执行卡尝试，其中 207 次 API confirmed；215 次都含 HH:MM，214 次含工具组及平铺调用行，0 次含叶子折叠面板、0 次显示时钟秒、0 次处于确定拒绝且未恢复状态。147 次包含已识别的具体命令用途。API 接受新结构不替代真实客户端显示验收；本轮桌面/手机字号、时间、组展开和更新后展开状态仍 pending。


## 2026-10-22 时间后置、工具箱图标与左侧停止入口

用户继续试用后要求：时间放在正文末尾，工具组标题前加入参考图中的工具箱；停止图标移到最前面，避免展开后摘要与正文重复，并统一入口与正文的文字颜色。本轮普通单行公开进度改为“正文 · HH:MM”，多行、代码、列表、标题、引用、表格及含反引号的正文保持完整独立 Markdown，时间使用后置灰色 notation 元素，避免流式未闭合代码吞掉时间。时区、首次接收时钟及无秒规则不变。

外层入口改为“正在处理”等简短状态，不再复述最新公开动作；完整进度仅在展开区展示。终态、等待、停止领取与 unknown/verifying 语义保留。入口保留 notation（12px）并沿用正文默认文字色，工具元信息仍为灰色。纯停止图标改到第一列，状态入口与展开内容在相邻列，按钮令牌及原轮次停止链路保持。

工具组采用 16px 图标列与原生折叠面板的分栏组合，前置灰色 briefcase_outlined 工具箱，并保留 follow_text 展开箭头；不向 header.title 填入未公开的 icon 字段，也不将工具箱替换成会旋转的展开图标。单次工具仍是不可折叠的具体动作行。依据为官方分栏、Markdown 前缀图标与折叠面板字段，见当前模块手册。容器五层规则的精确计算方式未公开；官方分栏示例本身包含三组 column_set→column，不能简单把每个 column 当成独立布局层。本轮组合随后已由正常服务投递的卡片获得 API confirmed，真实客户端排版仍独立待验收。

新增图标分栏与后置时间计入实际组件预算；基础布局超限时从最老记录裁剪，明确披露省略数，再优先分配组件给最新调用。保留完整 DTO、组计数和最新记录，不突破 172 个历史 tag / 180 个整卡 tag 的预留上限。合成回归覆盖最大分组、裁剪披露、原生稳定 ID、嵌套请求标记、左侧停止、颜色、正文去重、时间后置及未闭合代码。

验证：cards 包及 sync/app/store 相关回归通过。首轮相关测试的预算用例依赖旧版长摘要撑大卡片；改成实际可见的单次工具组 fixture 后，继续核对“整组放不下时明确披露且保留停止”的原行为，回归通过。cards/sync 的 Task|CompletedTask|Progress|Tool|Clock|RequestMarker 定向 race，以及 app/store 的 ^TestTask 定向 race 通过。首次全仓并行回归在本机磁盘等待较高期间出现三条 creation 用例 context deadline exceeded；其他包通过。随后运行 `go test -p 1 ./cmd/... ./internal/... ./compat -skip '^TestRuntimeGroupDeadlinePreservesFinalAndWakesWithoutNewInput$'`，creation 包重跑 12.209 秒通过，其余成功包复用 Go 测试缓存，全仓命令退出 0。排除的建群重试用例仍是此前已在未修改 origin/main 独立复现的基线问题，不将其记为通过。没有修改 creation 的实现或延长测试 deadline。文档及 diff 检查通过。

本地服务已更新为 `196586e6fd88+task-progress-20261022-200236-2c23aad7`，二进制 SHA-256 `56ab4341418a7c84104c0a906f69824df937f7e6a9fae84b0c84f3ef9d5df4aa`，构建源快照 SHA-256 `2c23aad75914bc3913728077d8b0ad0ac83b5ea6c691568abe4fb59779a218b3`。私有材料位于 `~/.local/lib/codex-feishu/task-progress-20261022-200236-0a5fd31f/`。停桥后一致备份 quick_check=ok、schema 30、55 表、168249 行；实际进程 PID 4189397 与稳定 CLI 匹配新二进制，systemd active/running/enabled，两侧连接和发现基线通过。配置、共享 daemon version 只读结果、schema 及既有两个 required pending 保持；doctor/status 退出 5，没有新增 required fail/pending。未重启共享 daemon，phase1_released=false。

正常同步的只读聚合观察到 219 次新执行卡尝试、218 次 API confirmed：219 次入口均为默认文字色与简短状态，219 次含 HH:MM、0 次时间前置或显示秒；218 次有工具箱与原生箭头共存的工具组，0 次有叶子折叠；3 次带停止按钮的卡片均将停止放在第一列，0 次处于确定拒绝且未恢复状态。没有主动发送测试卡或注入输入。平台接受新分栏与结构不替代真实桌面/手机效果，字号、图标对齐、展开状态保留及完整 P1 仍 pending。


## 2026-10-22 普通进度时间同行与展开区左边缘补偿

用户真实客户端截图确认上一版仍存在三处问题：普通进度只因包含换行或行内代码就把时间单列；停止按钮的左侧列挤进了全部正文；工具箱的左侧列也挤进了工具明细。该截图优先于此前 API confirmed 的结构证据，前轮不能据此标为无缩进验收通过。

本轮放宽普通进度的时间同行条件：换行、多段、闭合行内代码及行内 pipeline 都可在最后一句后接 HH:MM。仅去掉展示副本尾部空白，不改变完整记录或接收时钟。真正的代码块、引用、末尾列表/标题、表格及未闭合代码仍将时钟留在独立安全位置，避免破坏 Markdown。包含/不包含 leading pipe 的表格分隔线均识别，不能把 pipeline 字符误判为表格。没有增加工具或模型调用。

缩进来自完整面板放在图标右列。当前保留原生分栏和折叠链路，以固定列宽计算展开正文的负 margin：停止列 28px、间距 4px，只有存在停止按钮时外层正文直接子项回拉 32px；工具箱列 16px、间距 4px，工具明细和省略提示回拉 20px。图标只占标题位置，标题及面板自身不回拉，终态不继承停止列的偏移。停止按钮使用合法 width=fill 填满预留列，未使用小于 100px 的自定义按钮宽度。28px 是布局预留值，不宣称是官方已核实的纯图标最小宽度；列/面板 padding 为 0。两层偏移分别计算，重复渲染不累积，也不修改原 DTO。

采用公开的 margin 范围 [-99,99]px，官方图片通栏示例也使用负 margin。跨嵌套列与折叠面板的裁剪和扩宽语义没有公开保证，本次只完成合法载荷与几何参数核对，真实对齐明确保持 pending；不能再次用 API 接受替代截图验收。

验证：cards 包通过，新增测试从实际 JSON 列宽、间距与 margin 求和验证正文及工具明细的左偏移为 0，覆盖 header/panel 不移动、无 stop 终态无回拉、重复渲染不累积、投影不变、嵌套投递标记、组件与字节预算，以及普通多行/行内代码尾部时间。`go test -p 1 ./cmd/... ./internal/... ./compat -skip '^TestRuntimeGroupDeadlinePreservesFinalAndWakesWithoutNewInput$'` 通过，本轮未出现前轮并行负载下的 creation deadline 失败；原已在未修改 origin/main 独立复现的基线超时例仍排除，未记为通过。cards/sync 的 Task|CompletedTask|Progress|Tool|Clock|RequestMarker 及 app/store 的 ^TestTask 定向 race 通过，均以 -p 1 执行。文档链接和 diff 检查通过。

已更新同一本地服务为 `196586e6fd88+task-progress-20261022-204009-40d7314a`，二进制 SHA-256 `b86a6d568311485bf1f321d875ce60185b30d2d4f58217f40cbda8bd951b3ed3`，源快照 SHA-256 `40d7314a62b8fce1981aa49863ef83436af63ed57d2210b1fb5464ad15aef783`。私有部署目录 `~/.local/lib/codex-feishu/task-progress-20261022-204009-62cd4cb9/` 保存构建、源快照、备份与核对材料。停桥后 SQLite 一致备份 quick_check=ok、schema 30、55 表、169383 行。实际 PID 142557 与稳定 CLI 匹配新二进制，systemd active/running/enabled，两侧连接及发现基线通过；配置、schema 与共享 daemon version 只读结果保持。既有 bindings_reconciled、operations_known 两个 required pending 和 doctor/status 退出 5 保持，无新增 required fail/pending；phase1_released=false，没有重启共享 daemon。

正常投递的只读聚合观察到 222 次新执行卡尝试、219 次 API confirmed。2 次带停止按钮的源卡均包含外层正文 -32px，221 次带工具组的源卡均包含明细 -20px，0 次含叶子折叠。222 次含同行时钟，其中 27 次包含多行正文、138 次包含行内代码；1 次仍包含格式块的独立时钟。0 次处于确定拒绝且未恢复状态。没有主动发送合成卡或注入输入。该证据仅证明新版载荷已正常投递，双端实际对齐、裁剪、换行和展开状态保留仍 pending，等待用户新版本截图。

## 2026-10-22 裁切反馈后的本人私聊预览

用户最新桌面截图确认负 margin 会裁掉文字左侧，前一轮几何参数与 API confirmed 不能证明真实布局正确。用户明确要求先通过机器人私聊查看修改效果，再替换日常执行卡。本轮没有继续部署，也没有将失败展示记为通过；运行服务仍为 `196586e6fd88+task-progress-20261022-204009-40d7314a`，systemd active / running，PID 142557。

单独制作并发送一张合成展开态卡片：停止图标和状态仅在顶行分栏，正文和工具组均为独立全宽组件，不使用负 margin；统一 notation，普通正文末尾接 HH:MM。两个原生工具组一开一闭，标题使用 `🧰 调用 N 个工具` 和原生箭头，不再为工具箱单设图标列；工具叶子为不可折叠的具体动作。emoji 是本次预览取舍，不宣称完全匹配参考图的灰色线框图标。停止图标 disabled 且无行为，不关联真实任务；整段历史折叠及真实停止行为不在本次视觉预览验收范围。

复用现有配置、密钥读取与 Feishu Gateway，仅向原配置的本人 open_id 发送。发送前持久化冻结载荷、UUID 与唯一标记，单次 dispatch 后保存 confirmed 及消息 ID；未使用生产数据库创建虚构操作，也未启动额外接收器或 Codex 连接。只读 GET 确认相同消息、app sender、interactive 和唯一标记，正文及工具内容未见差异。全 JSON 严格比对为 false：返回 JSON 新增默认字段与自动 element_id，省略字号、margin、间距等样式字段，body padding 从两项简写展开为等价的四项字符串；保留该结果，不放松校验、不重发，也不能据此判断客户端实际字号和对齐。

私有 `~/.local/lib/codex-feishu/card-preview-20261022-210501-7bf2de1e/` 保存参考配置摘要、预览、请求状态、只读证据及读回 JSON，目录 0700、材料 0600；收件人、消息 ID、凭据均不进入仓库或普通输出。一次性发送器位于 ignored 的 `dist/task-card-preview/main.go`，gofmt 与 `go build -buildvcs=false` 通过，Go 缓存继承原环境。生产代码未在本轮修改，不重复此前全仓回归；文档链接及 diff 检查通过。实际预览展示等待用户观察，不自动更新日常卡片。

## 2026-10-22 认可私聊预览后接入日常执行卡

用户通过真实桌面截图确认全宽合成卡效果，随后将同一布局接入正式 renderer 并更新现有本地服务。状态顶行仅包含 auto 停止图标列与 weighted 状态列，后续正文与工具组均为全宽根级兄弟；删除外层 `execution_history` 面板、工具箱专用图标列及全部负 margin。工具组标题为预览中的 `🧰 调用 N 个工具`，保持默认收起、原生箭头与稳定工具组 ID；调用行仍为不可折叠的具体动作。正文末尾 HH:MM、完整 Markdown、notation、终态耗时、过程历史及真实 stop/continue 的 token/revision、已投递消息授权和原轮次路由保持。

验证：`go test -p 1 ./internal/cards ./internal/sync` 通过（0.360 秒 / 10.040 秒）；app/store 的 `^TestTask` 定向回归通过。新回归核对根级正文/工具组、仅顶行分栏、无负 margin、重复渲染不改投影、工具明细平铺、整卡不超过 180 个 tag、计数及省略披露，以及三种终态唯一投递标记。首次完整 cards 测试的两条附件预算失败与本轮共享 `taskPanel` icon 新增颜色有关；将颜色限制到工具组后 cards 全包通过，未修改附件实现或预算测试。前轮全仓业务与定向 race 证据保留，本轮没有修改并发、存储或协议路径，未重复无关全仓及 race；不将旧证据说成本轮运行。

新构建为 `196586e6fd88+task-progress-20261022-211846-58930b18`，二进制 SHA-256 `9b3d67d79ba0b81a72ab7803d05c29438992e7dcd704fc238103c00c7bd7fccd`，构建源摘要 `58930b18418352cbfd6e4d31cf83e7877b46315f4c632b9d37654b4c14bfb79c`。私有目录 `~/.local/lib/codex-feishu/task-progress-20261022-211846-e5osljm8/` 保留构建、源快照、原版本引用、备份及核对材料。构建前后源摘要一致；构建后仅补充交付文档。停止桥后 SQLite 一致备份 quick_check=ok、schema 30、55 张表、170680 行。原配置、共享 daemon version 只读结果及 schema 不变；未重启共享 daemon、创建替代会话或注入输入。

同一服务 systemd active / running / enabled，实际 PID 346001 和稳定 CLI 匹配新二进制，两侧连接及发现基线通过。部署前后的 required nonpass 集合与状态完全一致：`instance.bindings_reconciled`、`instance.operations_known` 仍为 pending，doctor/status 退出 5；没有新增 required fail/pending，`phase1_released=false` 保持。

正常同步的只读聚合观察到 266 次新执行卡尝试、262 次 API confirmed、4 次 rejected、0 次 unknown，当前没有处于 rejected 且未恢复的出站。266 次均采用全宽根级历史，0 次外层历史面板或负 margin；3 次带停止按钮的卡片均只在顶行分栏；219 次工具组均带标题工具箱，0 次叶子折叠；220 次含行末时钟。没有另发合成卡或手动重试 unknown。用户已确认的是合成预览的桌面展示，正式卡的真实停止、持续更新后展开状态及手机表现仍待实际使用确认。

## 2026-10-22 恢复过程折叠与无彩色工具符号预览

用户最新桌面截图确认全宽版丢失整卡折叠，红色工具箱 emoji 也过于显眼。前一轮对全宽合成预览的认可不包含取消折叠，之前的实现取舍有误。本轮恢复根级原生 `execution_history` 面板，继续避免图标右列与负 margin；为让停止始终可用，保留顶行停止和状态，下方以“执行过程”为折叠入口。无可用停止时直接以状态作为面板标题；终态默认收起且保留耗时，非终态默认展开。没有过程或省略提示时不创建空折叠。工具组仍可独立收起，叶子不折叠；新面板、顶行和操作共同预留组件，历史预算从 172 调为 169，整卡仍不超过 180。

红色 `🧰` 改为 grey 普通文本齿轮 `⛭`（U+26ED）；它是无中心齿轮，不宣称为灰色工具箱。按 [Unicode 字符表](https://www.unicode.org/charts/PDF/U2600.pdf)和[Emoji 属性](https://www.unicode.org/Public/UCD/latest/ucd/emoji/emoji-data.txt)核对其非 emoji 属性，不依赖给彩色 emoji 添加变体选择符来去色；实际字形覆盖仍需客户端预览。保留原生 chevron，不向面板标题加入未公开的 icon 或按钮槽，也不把停止按钮放入折叠正文，原回调授权遍历无需改变。

预览使用 ignored 的 `dist/task-card-fold-preview/main.go` 直接调用当前 `cards.Task`，复用正式结构；仅将合成停止按钮禁用并去除 behaviors、将第一组工具置为展开以便观察，再添加预览脚注。使用当前配置的本人 open_id，经同一 Gateway 单次发送 confirmed；GET 确认 app sender、interactive、唯一 marker，公开 content、expanded 和 disabled 无差异。返回 JSON 与冻结请求有 78 处样式/默认字段差异，全 JSON 严格读回仍为 false；保留该限制，不放松校验、不重发。私有 `~/.local/lib/codex-feishu/card-fold-preview-20261022-215103-39owyzvh/` 保留参考配置摘要、发送器、冻结请求、消息 ID 与 GET 原始证据，凭据及收件人不进入仓库或普通输出。

验证：`go test ./internal/cards` 通过；`go test -p 1 ./internal/sync ./internal/app ./internal/store -run 'Task|Progress|Clock|RequestMarker' -timeout 120s` 通过（1.150 / 1.826 / 0.992 秒）。新增或更新回归覆盖终态折叠、运行态展开、面板外唯一停止、全宽无负 margin、独立工具组、灰色符号、完整正文与时钟、组件预算、投影不变和唯一投递标记。文档链接和 diff 检查通过。本轮未部署、未修改运行配置或数据库、未重启任何服务；原 PID 346001、运行版本 `196586e6fd88+task-progress-20261022-211846-58930b18` 保持。新预览桌面/手机显示、正式卡连续更新及真实停止验收仍 pending。

## 2026-10-22 同款工具箱与缩小明细图标预览

用户不接受灰色齿轮，提供放大的工具箱参考图，并指出展开后的明细图标偏大。已核对其轮廓与 [Lucide tool-case](https://lucide.dev/icons/tool-case) 一致，使用固定提交 `c31f22723f3fed5f3921eee3d514a3cd1bd5df72` 的原始 SVG，保留许可证。图片来自公开 SVG，不使用含水印和背景的真实截图作为资产。

[飞书 Markdown 官方字段](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/content-components/rich-text)只为 icon 公开 tag/token/color/img_key，不开放独立尺寸；正文 notation 不会同步缩小前缀图标。不能将折叠箭头的 size 字段复制到 Markdown.icon，也没有依据声称它在所有客户端固定为某个像素值。隔离预览将明细换成 custom_icon，通过 SVG viewBox 从 24 扩至 32 增加透明留白，使可见图形按资产画布缩小 25%；平台图标外框大小不变，最终像素仍待客户端确认。

同款工具箱 SVG 生成 16×16 灰色透明 PNG，标题采用官方公开的 Markdown 图片语法 `![工具箱](image_key)`，保留独立原生 chevron。明细代码与阅读图标是同源 Lucide code-xml / book-open 的灰色透明 PNG。源文件、许可证、可复现栅格化脚本和 SHA-256 元数据保存在 ignored 的 `dist/task-card-icon-preview/assets/`；大图轮廓核对、箱体内部透明和重复生成摘要验证通过。标题图片是否能同行及是否按预期大小显示，公开文档没有保证，必须真实 UI 验收；本轮不直接改正式渲染或部署。

三张图片先冻结在 0700 私有目录，并经原配置/app 的现有 UploadImage 链路各上传一次。每次调用前持久化 uploading 意图，成功的 image_key 仅保存私有 journal；三项均 confirmed，无 unknown 或盲重试。随后在正式 Task renderer 输出上仅覆盖预览标题与明细图标、禁用停止按钮、展开首工具组并增加预览脚注，经原本人私聊单次发卡 confirmed。GET 确认 app sender、interactive、唯一 marker，图片标题的 tag/content 保留，四个 custom_icon 节点保留；返回的图片引用字段改为 img_id，且存在样式/默认字段差异，全 JSON 严格读回仍 false，不记为全载荷核验通过。

私有 `~/.local/lib/codex-feishu/card-icon-preview-20261022-221105-oc3y372d/` 保存冻结图片、参考配置摘要、图片上传 journal、预览卡、发送请求与 GET 证据。凭据、图片 key、本人 ID 及消息 ID 未进入仓库或普通输出。一次性 uploader 与 preview renderer 位于 ignored dist，gofmt 与 Go build 通过，继承已有缓存。生产 Go 源码本轮未改，不重复此前相关回归；文档链接和 diff 检查通过。日常服务仍为 `196586e6fd88+task-progress-20261022-211846-58930b18`、PID 346001、active/running，未改配置、数据库或启动额外连接；新图片预览 UI pending。

## 2026-10-22 标题前缀图标能力验证

用户新截图证实 Markdown 图片标题被飞书显示为大图块，图片和计数分行，桌面 UI fail；不能继续作为行内图标方案。截图中的 custom_icon 明细则是小型前缀，用户要求标题也采用这种显示方式。本轮删除标题内容里的图片语法，在原来的完整 Markdown 标题上使用 `icon:{tag:custom_icon,img_key:...}`，保留 `header.icon` 中的原生 chevron；不改叶子的较小前缀图标、全宽过程面板、时钟或折叠布局。

官方标题字段简表只列 tag/content，但[第一方 openclaw-lark 标题](https://github.com/larksuite/openclaw-lark/blob/main/src/card/builder.ts#L735-L757)使用 text_size、text_color、i18n_content 等扩展字段，[Markdown 标题](https://github.com/larksuite/openclaw-lark/blob/main/src/card/builder.ts#L491-L510)也有扩展项。因此简表遗漏不能作为 icon 不受支持的证据。本轮只在明确授权的本人私聊合成卡中验证该能力，没有直接推广到日常卡。

一次性 renderer 位于 ignored `dist/task-card-prefix-preview/`，使用原 Task 输出并仅替换预览标题前缀、保留较小明细图标、禁用停止按钮和增加预览说明。结构检查确认 2 个标题前缀、4 个明细前缀、两个原生工具组箭头、0 处 Markdown 图片语法及负 margin。复用此前三张 confirmed 图标，新增图片上传为 0；新请求在发送前冻结并持久化，经原配置的本人私聊单次发送 confirmed。

只读 GET 确认 app sender、interactive、唯一 marker，两个标题的纯文字内容保持；两个 `header.title.icon` 均保留为 custom_icon，引用字段由 img_key 转换为 img_id，与明细图标的正规转换一致。服务端因此已识别该前缀结构，而不是原样保留未知字段；客户端是否显示为期望的同款小图标仍 pending。完整 JSON 继续严格比对为 false，不放松读回规则，也不把服务端接受当作 UI pass。

私有 `~/.local/lib/codex-feishu/card-prefix-preview-20261022-225037-y0dt1oju/` 保存参考配置摘要、复用素材引用、冻结请求、投递和读回证据，收件人、消息 ID、图片 key 和凭据不进入仓库或普通输出。新 renderer 的 gofmt/Go build、结构检查和文档链接/diff 检查通过；生产 Go、配置、数据库及服务本轮未改，不重复此前相关回归。日常运行版本与 PID 保持，新预览等待用户实际观察。

## 2026-10-22 最新预览客户端确认与待部署核验

用户在真实客户端确认最新私聊预览：标题前缀显示为灰色、小尺寸的 tool-case，展开后的叶子图标更小；执行卡为全宽布局，公开正文的接收时间在行末显示 `HH:MM`。按本轮建议，运行态外层过程折叠标题改为“执行中”，终态使用真实状态标题并保留既有耗时规则。真实停止、连续刷新后的展开状态和移动端表现仍 pending。

本轮补记的真实测试事实：

- `go test -p 1 ./cmd/... ./internal/... ./compat -skip '^TestRuntimeGroupDeadlinePreservesFinalAndWakesWithoutNewInput$'` 全部通过。
- `go test -race ./internal/app -run 'TaskIcons|TaskIcon' -timeout 120s` 通过；`foreign_owner` 子测试因 uid 1001 无法 `chown` 跳过。
- 完整 app 测试仍在已复现基线 `TestRuntimeGroupDeadlinePreservesFinalAndWakesWithoutNewInput` 超时；该基线不记为通过。

此时最新版本尚未完成部署核验，原 build 和服务状态保持；后续实际部署结果见下一节。manifest 合约沿用 [08 卡片手册](../codex-feishu/08-cards-and-actions.md)，不在此重复长篇说明。`phase1_released=false` 保持。

## 2026-10-22 前缀图标接入正式卡与执行中文案

正式 Task renderer 接入已确认的小型标题前缀与明细图标，运行态外层折叠入口改为“执行中”；保留全宽过程、面板外停止、工具组独立折叠和不可折叠明细。以真实 renderer、六角色私有图标配置及原合成内容生成对比卡，去掉预览脚注并仅归一化文案后，与用户认可的前缀预览布局逐项一致，无额外私聊发送或测试输入。

服务在单实例领取后、连接和 worker 启动前读取一次私有 manifest，向所有 generation 的 TaskControls 渲染注入不可变图标配置；纯 renderer 不读文件或上传。原三项 confirmed 图标复用，新增编辑、搜索与通用三项同源透明留白资产各上传一次，均 confirmed，无 unknown 或重试。六角色 manifest 的 SHA-256 为 `e83846f7ed495c0acd144afd2f97dfeac2dfb6ae17bfce6c50d9b1aeb28b1923`，实际 App ID 与图片 key 仅保存在原数据库旁的 0600 sidecar 及 0700 私有材料目录；配置 TOML 不变。公开 SVG、固定 Lucide commit 的许可证与 provenance 单独版本化，不包含真实图片引用。

本轮全仓业务串行命令排除上述基线超时后通过；cards/sync 的 `Task|Progress|Clock|RequestMarker` 定向 race 通过，app 图标/注入/重连定向 race 通过。文档链接与 diff 检查通过。没有把完整 app 的已知失败、无法执行 chown 的归属夹具或历史 store 全包 race 超时记为通过。

本地服务更新为 `196586e6fd88+task-progress-20261022-231324-ca78188a`，二进制 SHA-256 `dd4f7170d198a2148e391ebc5b021af0b9e089b727f0efa52c11d6e985e6da63`，构建源快照 SHA-256 `ca78188a5ea3c3bc65600ea08a22d7ffb4f751e8048a2e6b775b5692fc258605`。构建前后源摘要一致，构建后只补交付文档；私有 `~/.local/lib/codex-feishu/task-progress-20261022-231324-e7f6tr8c/` 保存构建、源快照、sidecar、部署与读回证据。停止原桥后的一致备份 quick_check=ok、schema 30、55 表、172528 行，并保存配置、用户服务及 sidecar 备份。实际 PID 836885 与稳定 CLI 匹配新二进制，systemd active/running/enabled，两侧连接及发现基线通过；原配置、manifest 摘要与共享 daemon version 只读结果保持，未重启共享 daemon或创建替代会话。

部署前后的 required nonpass ID 与状态完全一致：`instance.bindings_reconciled`、`instance.operations_known` 仍为 pending，doctor/status 退出 5，无新增 required fail/pending。首轮正常同步聚合观察到 254 次执行卡尝试、240 次 API confirmed、14 次 rejected、0 次 unknown；当前没有处于 rejected 且未恢复的出站，未手工重试。246 张含外层历史的卡均为全宽且无负 margin，6 张带停止的卡均仅顶行分栏、以“执行中”作为展开入口；240 张终态面板收起。245 张工具组卡均使用已配置的 custom_icon 标题与明细、原生 chevron，无叶子折叠或旧 emoji/齿轮/Markdown 图片标题；246 张有行末时钟。该聚合只证明新正式载荷已由正常链路投递，不替代真实停止、持续刷新和移动端验收；`phase1_released=false` 保持。

## 2026-10-22 宽度修正与单行折叠交互待选择

用户反馈执行卡收起时仍过宽，要求根据内容收窄，同时将任务创建卡加宽；随后指出“正在处理”下方再加“执行中”重复，希望状态行自身就是折叠入口。源码先完成独立宽度修正：执行卡 `fill` 改为 `default`；主创建卡、只读草稿和结果摘要 `compact` 改为 `default`，目录选择、搜索、Busy 与其他模板保持原策略。PC 宽版/iPad 的默认上限为 600px，compact 为 400px；官方没有承诺整卡随内容精确测量或随原生折叠状态自动收缩，不将标题 `auto_when_fold` 的语义扩大为卡片边界。

已核对[官方折叠面板](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/collapsible-panel)、[分栏](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/containers/column-set)与[全局结构](https://open.feishu.cn/document/feishu-cards/card-json-v2-structure)：面板 header 没有独立按钮槽或折叠事件回调；图标右侧分栏会约束整个正文子树，不能解决跨列全宽。要将停止、状态和箭头放在同一行，同时保持正文全宽，需要回调式整体折叠和共享消息重绘；这会让群内展示状态共享，与原生每客户端独立展开不同。已向用户询问该交互取舍；尚未实现回调式折叠或变更数据库，未将共享状态默认为已获选择。

先发送独立的任务创建卡加宽预览。ignored `dist/task-card-width-preview/` 直接调用正式 `cards.Creation`，仅使用合成任务和 example.com 链接、无 token 或动作，并增加预览脚注；没有创建真实任务。私有 `~/.local/lib/codex-feishu/card-width-preview-20261022-232826-hy6ib073/` 保存原版本引用、渲染器、单次发送 journal 和只读读回。经原配置的本人私聊一次发送 confirmed；GET 确认 app sender、interactive、唯一 marker 及 `width_mode=default`。全 JSON 严格比对仍 false，已保留结果：除样式/默认字段外，平台将裸 URL 转为等价 Markdown 链接，将跨行灰色 font 拆为逐行 font；未放松严格校验或重发。实际创建卡宽度和换行等待客户端观察。

`go test ./internal/cards` 及 `go test -p 1 ./internal/creation ./internal/sync -run 'Creation|Task|Progress|Clock|RequestMarker' -timeout 120s` 通过；gofmt、文档链接与 diff 检查通过。本轮只调整纯渲染宽度，未改并发、持久化或原生协议，不重复前轮全仓/race 并冒充本轮证据。运行服务、配置与数据库仍保持前节 `231324-ca78188a` 构建，未部署本轮宽度修改，`phase1_released=false` 保持。

## 2026-10-22 原生同排与回调布局的对比预览

用户要求将两种折叠方案各发本人私聊，并质疑同排是否一定要改为回调。已澄清：同排本身可以由原生分栏实现；代价是整个原生面板放在停止按钮右列，正文随该列缩进。回调式重绘是用于同时保留同排停止、单一状态入口和全宽正文，不应表述为任何同排布局都必须回调。原生标题目前已核实的字段没有独立停止按钮槽；若停止入口放在别处，单行原生状态折叠不需要改回调。

新增 ignored `dist/task-fold-comparison/main.go`，从当前正式 Task renderer 的同一组合成正文与 3/1 次工具记录生成三张对比卡。原生同排版仅左列放禁用停止按钮、右列放标题为“正在处理”的真实原生折叠面板，用户可直接展开/收起；不使用负 margin。回调方案分别展示展开态与收起态，前者正文和工具组直接作为顶行的根级兄弟，后者隐藏全部历史；宽度分别为 default/compact，状态仅一行。回调方案是静态布局样例，折叠箭头禁用、无 behaviors，卡内脚注明确整体折叠尚未接入；没有把它说成可点击的真实回调。工具组保持真正的原生折叠，组和明细图标复用既有六角色素材，无新上传。

三张卡均经原配置的本人私聊各发送一次 confirmed，无 rejected/unknown 或重试；没有创建真实任务、修改业务数据库或新增接收器。私有 `~/.local/lib/codex-feishu/fold-comparison-20261022-234920-ypviqtd2/` 保存渲染器源码/二进制、冻结载荷、独立发送 journal 及 GET 证据。结构检查确认展开历史逐项一致、各卡没有回调行为或负 margin、停止按钮禁用，原生/回调展开态各含两组工具。GET 确认三张卡 app sender、interactive、唯一 marker 与各自宽度模式；原生面板位于右列，回调样例没有外层原生面板，展开态各保留六个 custom_icon 节点。完整 JSON 严格比对均为 false，保留规范化差异和原始读回，不将结构接受记为客户端 UI pass。

gofmt、预览工具 Go build、私有载荷与读回结构检查、文档链接和 diff 检查通过。生产 Go 没有在本轮继续修改，未重复此前业务测试。运行进程摘要、配置字节、原本地服务 active/running 状态保持，未部署或重启；当前运行 build 仍为 `231324-ca78188a`，`phase1_released=false`。用户实际观察及折叠方案选择 pending。

## 停止按钮改放面板右侧的原生预览

用户建议停止按钮紧跟折叠箭头右侧，而不是占用整行最右端。新增 `native-after` 预览模式，将原生面板放在第一列、停止按钮放在第二列，两列均为 `auto`；标题只保留“正在处理”，继续使用 `auto_when_fold` 和 `follow_text`，正文因此不再位于停止按钮右侧的列中。该方案没有回调式折叠，也没有负 margin。按钮跟随的是整个面板列的右边界，并不锚定标题或箭头；收起时可能靠近箭头，展开后正文撑宽面板时可能右移。没有将标题自适应宽度当成外部按钮可随标题定位的保证，具体两态位置等待客户端观察。

沿用同一合成正文、工具与已上传图标，停止禁用、无 behaviors，用户可实际点击原生状态标题展开或收起。经原配置的本人私聊单次发送 confirmed，无新图片、真实任务、未知写或重试。私有 `~/.local/lib/codex-feishu/stop-after-fold-20261023-002835-28jpz98s/` 保存冻结请求、源码/二进制和独立投递 journal。GET 确认 app sender、interactive、唯一 marker，面板第一 auto 列、停止第二 auto 列、标题、expanded 和 follow_text 保留；请求中的 header.width 没有返回，不宣称该字段读回一致。全 JSON 严格比对仍 false，保留原始差异，不将服务端结构确认当作真实位置验收。

预览工具 gofmt/Go build、载荷和读回结构检查、文档链接及 diff 检查通过。生产 Go、数据库、配置、日常服务与原共享 daemon 未改，运行二进制摘要及 active/running 状态再次核对保持；没有为纯预览重复此前业务测试或接入 schema 31。`phase1_released=false`，预览 UI 和最终方案选择仍 pending。

## 2026-10-23 message_cot 原生能力调研与既有结论校正

用户提供 Axon 和简介为 Hosted At Mew 的 TRAE CLI 2.0 Oncall 作为参考后，继续追查真实消息能力。前文“header 没有独立按钮槽”“需要回调式整体折叠”等判断只覆盖当时核实的普通 Card JSON 结构，将其扩大为飞书整体能力限制并不成立。同排本身、普通卡片标题可用字段和独立原生过程消息是三个不同问题；历史预览结果保留，能力边界以本节及[08 手册补充](../codex-feishu/08-cards-and-actions.md#2026-10-23-原生过程消息调研与能力边界校正)为准。本轮未实现回调式折叠或数据库迁移。

Mew 门户静态脚本确认实际存在两种 transport：个人设置默认解析为 `message_cot`，Labs 的“透明进度卡片”开关开启后为 `card_summary`，关闭恢复 `message_cot`；“需使用飞书桌面测试版”直接绑定该开关，不是根据翻译词条相邻位置推断。共享模块定义 `GetLarkAgentProgressPreference` / `UpdateLarkAgentProgressPreference`，请求包含 `Transport`、`RedactSensitiveDetails`、`ReasoningOnly`、`ProgressDisabled`，仅在后端返回字段时发送 `HideProcessWhenDone`；普通个人设置的“完成后隐藏进度”还要求 transport 为 `message_cot`。依据为[个人设置发布脚本](https://cdn-tos.bytedance.net/codebase/x/assets/settings-BH6_Dwal.js)和[共享模块](https://cdn-tos.bytedance.net/codebase/x/assets/lark-agent-progress-EUxImoms.js)。这些是前端绑定和 API 契约证据，没有调用设置更新接口或读取用户会话。

随后取得[飞书官方内部 COT 接入参考](https://bytedance.larkoffice.com/wiki/ZfyXwOQImiLURtkRPtWcf2B2nNh)和[AG-UI 事件说明](https://bytedance.larkoffice.com/wiki/UDxqwqxBnitkFHkYc1WcVC6InDg)：`message_cot` 是独立过程消息接口，基础能力已全量开放；`content.input.statusText` 自定义文案单独灰度。创建、追加和完成分别使用 `POST /open-apis/im/v1/message_cot`、`PUT` 同一路径和 `POST /open-apis/im/v1/message_cot/complete/{cot_id}`，关联创建返回的 `message_id` / `cot_id`；最终结果仍由正常消息回复链路发送。另一仓库在提交 `fdd93f0e260a552c9a7542f0cc836403a61ba54d` 的 [feishu-api.ts](https://code.byted.org/CIS/aifee-enterprise-agent/blob/fdd93f0e260a552c9a7542f0cc836403a61ba54d/apps/aifee-agent-server/src/channels/feishu/feishu-api.ts#L186)中有实际 `createCot` / `appendCot` HTTP 调用代码，区别于网页 React 展示组件。该证据不等于当前应用实测，也未定位到 Axon 或 TRAE 的精确后端；Mew CDN 路径 `codebase/x` 仅是线索，仓库读取返回 AccessDenied，不能据此确认仓库归属或不存在。

接入参考说明原生停止入口通过普通 `/stop` 消息分发：单聊 `/stop`，群聊 `@bot /stop`；7.73 起群聊默认隐藏，可按用户、群或租户加白。后续接入须在模型/队列输入前识别该命令，校验操作者和上下文并锁定正确运行轮次，执行原有停止逻辑，再追加 `RUN_FINISHED` / `status=interrupted` 及 CompleteCOT。不能将它视作已有卡片 token/revision 回调，或以单聊的显示推断群聊已可用。公开进度只使用合成或已脱敏的摘要，不上传真实隐藏推理和工具输入输出。

普通卡片方面，仅对 `collapsible_panel.header.button_area` 搭配 `pc_display_rule="always"` 的候选载荷进行了单次本人私聊发送尝试，返回 HTTP 400 / `code=230099`；没有产生 MessageID，未重试。没有将这一组合的拒绝扩大为其他字段组合、hover 能力或原生 COT 均不受支持。

私有调研材料保存在 `~/.local/lib/codex-feishu/axon-card-investigation-assd4lor/`，原始读取及源码证据均不进入 Git。入口 JS 通过 bytedcli 读取成功但达到 1 MiB 截断上限，随后只读获取同一静态 CDN 文件及其设置依赖补齐，未登录浏览器、POST 业务 API 或读取真实 conversation。普通候选卡发送以外，原生 COT 本人私聊运行态合成预览此时尚未发送；安全读回、客户端展开位置、停止入口、工具摘要、连续更新和手机验收均 pending，等待后续独立记录。生产代码、运行配置、业务数据库、服务及共享 daemon 未因本次调研变更，`phase1_released=false` 保持。


## 2026-10-23 原生 CoT 预览确认与正式链路接入

原生本人私聊合成预览经 Create、Update、RUN_FINISHED 和 Complete 各自持久记账、单次调用均 confirmed；运行态保留约 77 秒供客户端观察后收为完成态，普通 GET 仅确认同 app sender 与本人 DM，返回 post 兼容正文，不能核验内部工具事件。用户确认样式，授权改成 CoT 并要求自主 Review 与测试。未将此前 Card JSON 布局判为等价。首轮材料保存在私有 `native-cot-preview-*` 目录。

正式实现新增纯公开投影 `internal/cot`、飞书三个原生方法、schema 31 的 CoT ledger 与冻结 attempt 字段；继续由原 actor、原绑定、原 outbox、原授权租约与共享 daemon 处理。新活动轮次使用 CoT，既有卡片类型按首次镜像冻结；历史终态不补建 CoT。每一步只发一次，confirmed 才推进投影 ACK，unknown 不重发或创建替代消息，也不凭 post 占位确认。最终答复继续独立 Card JSON 投递，CoT 不影响完成 reaction 的最终结果条件。普通公开文字只发后缀；非前缀更正明确新记录；工具仅同步已有安全用途摘要与状态，空参数占位，不同步隐藏推理、原始命令、真实参数/输出或 diff。分批限制实际双层 JSON 编码后的 4096 字节与 50 事件，完整工具历史不沿用旧 32 项裁剪。

原生 `/stop` 接入原命令 actor，并在入账事务冻结唯一未收口活动 CoT 的 turn，防止等待 actor 时误停后继轮次；多条不明确过程继续 fail closed。明确创建失败且没有远程消息的旧轮不阻断后来停止。群聊入口默认隐藏是平台限制，仍支持对应任务群/话题的 `/stop`、`/continue`；单聊命令不路由到任务。裸命令没有来源 run token，不能宣称还原网络延迟前的点击身份。旧卡片 token/revision 回调保留。

独立高风险 Review 与主代理复核找出并修复四项：明确未创建的 rejected 旧轮阻断停止；terminal Append ACK 后借用 Complete 前更正的未发送内容/原因；缺失旧实体在批次边界漏记部分关闭状态；工具转态后留下未闭合公开 Note。已为每项补精确回归，并回读确认修复。终态 ACK 后即冻结公开进度，Complete 不借用新状态；最终正文可独立更正。公开消息保留已认可预览的 REASONING_START / MESSAGE / END wrapper 顺序，仅作为公开摘要载体。

再次用正式 Gateway 和纯 Planner 向已核实无绑定的本人 DM 发送一条合成验收：21 个初始事件、3 个结束事件，Create、Append、终态追加和 Complete 全部 confirmed，稳定同一 message_id/cot_id；普通 GET 身份一致。没有真实任务、命令执行、额外接收器或 Codex 连接。每一步事前 claim、冻结载荷与结果均在私有 `native-cot-production-preview-*` 目录保存，0700/0600；生产 API 层封装不返回 HTTP 成功状态码，因此该轮只记录接口成功分类，不编造 HTTP 值。此探针确认真实协议闭环，不替代任务群与手机 UI。

正式部署和最终全仓/race 结果在后续记录，本节此时不提前写通过。


正式验证完成：`go test -p 1 ./cmd/... ./internal/... ./compat -skip '^TestRuntimeGroupDeadlinePreservesFinalAndWakesWithoutNewInput$' -timeout 180s` 退出 0；排除项是此前已在原始分支独立复现的建群 deadline 基线问题，不记为通过。首轮边开发边全仓运行曾遇旧 Service fake 没有 CoT 能力导致两项等待超时，以及源码更新期间一次编译读取失败；正式组合根改为仅对支持 COTGateway 的适配器默认启用并新增跨代能力测试后，最终稳定源码全仓重跑通过。`go test -race -p 1 ./internal/cot ./internal/sync ./internal/store ./internal/app -run 'COT|Planner|Document|Task|Progress|Clock|RequestMarker' -timeout 240s` 通过；feishu 全包普通与 race 已由独立网关实现任务通过。精确覆盖每步响应丢失/本地确认失败、重启不重发、final 独立、话题和租约守卫、旧卡/终态历史、停止轮次与歧义、长 UTF-8 和工具状态、跨批次 wrapper 部分 ACK。文档链接和 diff 检查通过。

现网候选拒绝隐式迁移旧 schema，因此没有修改该保护：先停原桥并一致备份 schema 30，再用隔离的一次性显式 migrator 在 application/listener 双锁下验证配置摘要、当前库与备份的 quick_check、FK 与逐表逻辑摘要（包含同数内容变化和 schema/trigger 定义），同步迁移 intent 文件及父目录后调用既有标准增量迁移。独立 Review 提出的迁移前比对和目录 fsync 缺口均已修正；逻辑摘要对照、排序稳定与表名引用测试通过。升级后逐表计数、quick_check 和 FK 再次核验，55 张旧表与 174260 行全部保留，schema 31 新增 CoT ledger。未恢复旧数据库、未清理缓存或改配置；如升级后启动失败，私有脚本明确阻止用旧 schema 二进制回滚，保留新数据库等待兼容修复。

本地服务已更新为 `196586e6fd88+native-cot-20261023-012302-c34e2573`，二进制 SHA-256 `553858a1417b46078163baefbeff579f1cfb7faacf4b853657e1036388ab7bf2`，构建源快照 SHA-256 `c34e2573c2389caee1a9a23f68c6466f1973958c98376c46096d504766d2eb6b`。私有 `~/.local/lib/codex-feishu/native-cot-20261023-012302-jem2ew5s/` 保存冻结源码、构建、备份、迁移和部署读回；构建后的 Go/依赖/嵌入权限清单摘要再次逐项一致，之后只补兼容性与交付文档。实际 PID 1334183、稳定 CLI、systemd active/running/enabled 与新二进制一致；配置与图标 manifest 未变，共享 daemon version 只读结果未变，未重启 daemon或新增接收器。两侧连接、运行健康与发现基线通过。

部署后 doctor/status 均退出 5；required nonpass 与部署前逐项一致，仅原有 `instance.bindings_reconciled`、`instance.operations_known` 仍 pending，无新增 required fail/pending，`phase1_released=false`。首轮正常业务只读聚合尚未观察到新 CoT 镜像/ledger/attempt（均 0）；已存在过程继续旧卡，不用主动输入创建真实任务来制造通过证据。新活动轮次默认 CoT 的实际业务展示、任务群入口和手机/真实停止保持 pending。

另补本人私聊的机器人原消息引用验收：正式 Gateway 传 `origin_message_id` 引用此前已确认的机器人合成卡，21+3 事件、创建/追加/结束全部 confirmed，GET 确认 app/chat 身份及 `parent_id` 与冻结 origin 一致，未重试。材料在私有 `native-cot-origin-preview-*`，与前一无 origin 的协议探针分开记录。该结果覆盖机器人镜像可被 CoT 引用的协议形态，不扩大为任务群/话题完整客户端验收。

## 2026-10-23 摘要隐私与回滚修复复审

用户要求修复独立 Review 的两个 P2 并再审一轮。解释器原先跳过前置选项后，将某些选项值误认为脚本 basename；现仅允许明确不带值的少量选项，未知、可能带值及组合选项保守显示固定类型摘要，保留明确脚本、`--` 和已识别的 Shell 包装。44 个新增合成用例同时检查共享工具摘要、CoT 文档与实际任务卡，私有参数哨兵不得进入任何公开投影。

运行工具摘要 A→B→A 原先只和不可变的初始 TOOL 标题比较，导致最后一次回滚不发。现按已 ACK 的公开 Note 与更正代持续对账，不新增 schema 或重复工具调用。新增回归覆盖持久状态 JSON 重启、连续反复更正、50 事件边界恢复及重复快照幂等。复审又复现了后续更正代仅确认开始事件、正文仍为空时回到 A 的同类边界；先用新增测试确认失败，再保留更正代历史修复，覆盖第 1/2 更正代及五种批次截断位置。

两名独立 SubAgent 分别只读复审这两处修复，最终无未闭合发现。解释器审查额外用仓库外 overlay 验证 516 种选项/包装组合及 7 个白名单、嵌套包装和 `--` 边界；Planner 审查额外验证 400 组 unknown/declined、确认执行、更正批次及轮次终态组合，Note 闭合、保守执行事实和幂等均通过。没有发送真实任务或额外平台探针。

最终源码验证：`go test -p 1 ./cmd/... ./internal/... ./compat -skip '^TestRuntimeGroupDeadlinePreservesFinalAndWakesWithoutNewInput$' -timeout 180s` 退出 0，排除项仍是此前在未修改 origin/main 已独立复现的基线问题，不记为通过。`go test -race -p 1 ./internal/cot ./internal/sync -run 'Planner|Document|COT|Command|Summary' -timeout 180s` 通过；补修空更正代后，`go test ./internal/cot -count=1` 和 `go test -race ./internal/cot -count=1 -timeout 120s` 再次通过。

本地候选已更新为 `196586e6fd88+native-cot-reviewfix-20261023-105930-87223b46`，二进制 SHA-256 `2cc7168fca891212e031d80f11299194f58dc40fa23a5ec423025155a8e7d08f`，构建源快照 SHA-256 `87223b46ecfa590c1844382e378f1b637eb618eeae3560bce18dbcdd4068e545`。私有 `~/.local/lib/codex-feishu/native-cot-reviewfix-20261023-105930-oawl9id6/` 保存冻结源码、构建、复审/测试摘要、备份和读回。停止原桥后一致备份 schema 31 数据库（56 表/175331 行，quick_check 通过），未运行迁移或恢复数据库。实际 PID 2820508、稳定 CLI 和 systemd active/running/enabled 均与新二进制一致；配置、图标 manifest 摘要与共享 daemon 只读版本不变，两侧连接、运行健康和发现基线通过。构建后源清单逐项一致，之后仅补本记录与交接文档。

doctor/status 仍退出 5，仅保留部署前相同的 `instance.bindings_reconciled` / `instance.operations_known` 两个 required pending，没有新增 required fail/pending，`phase1_released=false`。本轮属于安全摘要和投影状态修复，未将本地测试或部署读回提升为真实客户端 UI 验收。

## 2026-10-23 停止反馈与新输入继续

用户提供的任务群截图显示了原生停止入口发送命令、CoT 显示中断，以及两张额外停止回执；停止后普通新输入仍需另发继续命令。此证据只覆盖截图场景，不扩大为所有群/话题或手机验收。

重新通过 bytedcli 只读取得[官方接入参考](https://bytedance.larkoffice.com/wiki/ZfyXwOQImiLURtkRPtWcf2B2nNh)和[官方 agent 接入文本](https://feishu-agui-playground.gf-boe.bytedance.net/llms.txt)：原生停止图标仍由客户端发送 `/stop` 或群内 `@bot /stop`，没有已公开的停止回调、自定义 action 或隐藏该命令字段；`cot_hidden` 只表示完成后隐藏 CoT。本轮不撤回用户命令，也不增加独立停止卡。后端收到有效 CoT 停止命令后，仅保存去重收据和原 actor 的停止意图，不再另发成功收据；空正文中断若已有确认的远程 CoT，也不再生成“本轮已停止”结果卡。目标不明/未决控制的说明、真实最终正文、旧卡和 CoT 创建未知时的兜底保持。

停止后新收到的普通消息在原接收事务内保存收据关联的继续意图，依旧先 queue/add，再以原目标轮和已确认接收证据核验 queue/start；不改成 steer，也不在聊天中发送 `/continue`。来源固定为先前 stop action，或本地已完整观察的 App 中断轮。新收到的后续停止优先于先前继续，未决写入核验原 attempt；新消息不出现时不自动执行旧队列。消息重放、旧收据接管、附件下载完成及重启不补授权，附件开关两种纯文本入口一致。未观察到 App 停止且没有本地 stop 意图时不事后猜测输入与停止的先后。

实现与独立 Review 补齐了若干边界：未知 add 被 App 消费后只剩历史证据时，继续意图不能挡住原 add 的核验；未提交输入仍需保留离线取消入口，取消与继续意图结束须同事务；空队列上的显式 continue 不得遮蔽较早 stop；首次普通入账前须先给已有非草稿收据补顺序，避免新消息越过旧输入。连续两个同轮停止中，后一个若仅因该轮已结束而本地拒绝、没有任何 RPC attempt，可保留它已冻结的目标继续核验；真实 RPC 拒绝/unknown、空目标或换轮不放宽。精确合成回归与独立 overlay 用于验证，不执行真实任务或新增平台写探针。

独立高风险 Review 最终无未解决发现。仓库外 overlay 的 unknown 历史证据、离线取消、空 continue 顺序、旧收据 FIFO、重复同目标停止及禁止换到更新中断轮均通过；Observe、来源查询和 FIFO 的修复前对照均已复现失败。取消仅豁免严格识别、ready/prepare 且零 attempt 的继续意图；同轮冗余停止仍保留固定目标、零 attempt、live 状态、后到 stop 与 unknown 门槛。首轮边补测试边运行的全仓拿到 Observe 修复前的状态，在 unknown start 用例失败；该轮不记通过，最终稳定源码完整重跑如下。

最终验证：`go test -p 1 ./cmd/... ./internal/... ./compat -skip '^TestRuntimeGroupDeadlinePreservesFinalAndWakesWithoutNewInput$' -timeout 180s` 退出 0；排除项仍为此前在未修改 origin/main 独立复现的基线问题，不计通过。`go test -race -p 1 ./internal/store ./internal/submissions ./internal/app ./internal/sync -run 'Ordinary|InputContinue|StopArriving|TaskStopClaim|EmptyManualContinue|LocalCancel|COTStop|COTInterrupted|^TestCommandsRetryReplyDoesNotDuplicateStopAction$' -timeout 180s` 退出 0。新增专项同时覆盖实际接收器两种附件开关、下载与封存顺序、两种本地取消入口/失败回滚、最终 start 领取时的新停止，以及开始/入队响应未知后的重启不重发。

本地部署已更新为 `196586e6fd88+native-cot-stop-resume-20261023-111711-be04447e`，二进制 SHA-256 `3d107ba93f335d73ed868526183f0531ca94f1fbf2b82519a7585328e0f2f0ff`，源清单 SHA-256 `be04447ed7e6f637425f517e10b20da5271688779aa332494c1be0d771d0ce4e`。私有 `~/.local/lib/codex-feishu/native-cot-stop-resume-20261023-111711-c0idp3rl/` 保存构建、冻结源码、复审与测试摘要、备份及前后读回。停桥一致备份 schema 31（56 表/175517 行），quick_check 通过；无迁移、无数据库恢复。PID 2974219、稳定 CLI、systemd active/running/enabled、实际二进制摘要一致，两侧连接、运行健康和发现基线通过，配置、图标 manifest 和共享 daemon 只读版本保持。构建后源清单逐项一致，之后仅补本记录与交接文档。

schema 相同也不表示旧代码可安全接管新意图：私有部署脚本已加未完成 `input-continue.%`（ready/running/unknown）回退门槛；若存在则保留现库，要求兼容版本前向恢复，不让旧二进制忽略新顺序约束。本轮没有实际触发回退。doctor/status 退出 5，仅原有 bindings_reconciled / operations_known 两项 required pending，未新增 required fail/pending，`phase1_released=false`。本轮真实客户端的减少回执、新消息恢复与手机效果仍保持待确认。
