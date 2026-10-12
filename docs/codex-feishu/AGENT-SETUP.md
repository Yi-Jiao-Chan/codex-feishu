# Agent 一键配置指南：Codex × 飞书

版本：2026-10-12 / 配置 schema v1。对象：用户和负责配置的 Agent。

本指南自包含，目标是在用户提供少量信息后，由 Agent 检测环境、生成配置、安装用户服务并验证。**M0 真实联调已通过，当前整合 M1–M3；development 版已有 serve、setup plan / apply、service 和实时 / 只读 status，但一期发布仍待验收。本文安装流程是正式发布契约。** Agent 必须检查 version 的 `phase1_released` 与具体 capabilities：开发版可做 plan，正式 apply / install / start 会拒绝，setup pair 已有开发实现但真实私聊 UI 尚待验收（`setup_pair_ui_verified=false`）。status 实时健康只对提供管理通道的新版运行实例有效，旧 M0 返回只读快照和 pending。命令存在不代表可正式安装；缺少已验收版本时完成只读环境检查并报告前置条件，不能假装配置完成。开发任务另见开发手册，不在一次配置任务里默认编写整个产品。

## 用户直接交给 Agent 的提示词

```text
请按本文为我配置并启动 Codex × 飞书桥接工具，先检测环境，只向我询问缺失的信息。
目标是 Codex App 已连接的远程机器，复用现有共享 App Server，不新起替代服务。
先配置一期：App 发起任务自动建群、双向同步、默认排队、按钮追加引导、目录 mute 和 @ 控制。

2026-10-12 用户已将 fork 跟进暂缓；不为 fork 自动建群。普通 App 主会话和注册工作树继续在本期范围内。
允许写入本工具的用户配置、安装/启动本工具用户服务。
允许向我本人发送测试卡、创建只有我和机器人的测试群，以及创建独立的 Codex 测试会话。
允许将该测试群与该测试会话绑定，并启用我在群内的跟进。
不要给其他用户发送测试消息。沿用原 Codex 权限，不改变现有工作会话。
长期密钥通过本机文件或安全输入保存，不要让我贴到聊天里。
请实际验证收发、回调和同会话回显；未能验证的项目明确列出，不要仅凭进程启动报成功。
```

如果用户删掉测试授权，只做配置和可做的只读检查，保留外部测试为 pending。用户只要求看配置计划时，只执行 plan。用户要求“按本文配置并启动”且保留上述授权时，Agent 可完成本地配置、服务安装和指定本人测试，不再逐步询问同一授权。

## A. 自动检测，先不问用户能检测到的内容

在目标远程执行主机运行。若 Agent 当前不在 App 连接的机器，先使用用户已有 SSH 连接；连接地址未知时才询问地址/用户，不索要私钥正文。

真实可用的基础检查命令：

```sh
id -un
uname -sm
command -v codex
codex --version
codex app-server daemon version
command -v codex-feishu
systemctl --user is-system-running
```

Agent 分别检查退出码和 JSON，不能把 shell 输出当一条成功链。某个检查失败不阻止其余独立只读检查。不要改 HOME、CODEX_HOME，也不要为了检查而读取/打印 Codex auth.json 或 app_secret 文件正文；SDK 验证通过受限文件引用加载凭据。

自动检测并记录：

1. App 所用远程主机、系统用户和实际共享 daemon；daemon 返回的 socketPath / appServerVersion 为准。本机先前验证版本为 0.162.0，但目标机必须重新检查。
2. codex-feishu 可执行文件、version --json 的命令能力与已验收兼容清单。若没有，检查用户提供的发布包/二进制；不要猜下载地址、版本或校验和。
3. 现有 codex-feishu 配置、host_id、服务和数据库中的有效通知规则；沿用已有部署，优先做差异更新，不重置用户命令设置。
4. 可访问项目/目录及 realpath，作为待选择建议，不默认镜像整台机器。
5. 用户级服务管理器与 SSH 注销后的常驻条件。无 user manager/linger 时具体报告，不自行 sudo 或改系统级服务。
6. 目标主机可出站连接飞书；长连接不需要公网回调 URL，不开放 Codex 端口。

若要读取源码仓库或构建已提供的源码，先遵循该仓库 AGENTS.md 和 CodeGraph 就绪检查；普通安装配置无需扫描源码。

### 收到归档包时

先取得来自已知来源的归档 SHA-256，再核验归档。单独收到 archive 和它旁边的 checksum，只能检查二者字节一致，不能证明来源可信。未核验前不执行包内二进制或脚本。

检查归档条目只含一个预期顶层目录；拒绝绝对路径、`..` 路径段、重复成员、软硬链接、设备 / FIFO 和超出合理总大小的内容。安全解包到本人受限、长期保留的版本目录，避免把服务执行路径留在会被清理的临时目录。包内 `SHA256SUMS` 须全部验证，`artifact.json` 的二进制摘要、成员摘要、source_commit、OS/架构须与实际内容一致；它没有资格替代本页的能力与真实验收检查。

二进制位于包内 `bin/codex-feishu`，指南和样例位于 `docs/codex-feishu/`，权限位于 `compat/feishu-permissions.json`。版本输出的 commit 必须等于 manifest 的 source_commit；任一文件缺失、摘要不同或架构不匹配则停止使用这个包，不顺手重建 / 覆盖现有安装。仓库研究链接是补充背景，未打入包也不影响本指南的自包含步骤。

development 包的 `phase1_released=false` 和 pending 验收模板不能改成 pass；只继续只读检查和收集缺项，不执行正式 apply / install / start。`docs/codex-feishu/examples/codex-feishu.service.template` 仅用于说明生成结构，禁止直接复制占位 unit，真实单位由已验收 CLI 的 service install 生成。

**前置故障处理：**没有桥版本、共享 daemon 未运行、不是同一执行用户、版本未验收，均不进入启用同步。仍可整理目录建议、检查服务环境和收集缺失项。daemon 未启动时提示用户先用 App 连接这台机器；不私自创建一个新执行器代替它。

## B. 收集最少信息

通常三组信息足够。能从现有配置读取的不要重复问；独立环境准备可在等待答案期间继续。

| 信息 | 提问方式 | 自动化处理 |
| --- | --- | --- |
| 飞书应用 | 企业自建应用的 App ID；App Secret 已保存在哪个本机文件？ | 检查文件权限；API/长连接验证应用 |
| 使用者 | 你知道此应用下自己的 open_id 吗？不知道可单聊配对 | 有 ID 直接验证；无 ID 走 D |
| 目录与提醒 | 哪些项目允许自动建群？是否设置每日免打扰，例如 23:00–08:00？ | 建议现有 App 项目；规范化路径，生成明确规则 |

默认 operator 与通知目标都是本人；通知只在最终结果、失败或需处理时 @；时区 Asia/Shanghai；每日 quiet 默认关闭，可采用用户选择。默认排队、一期自动专属群、二期关闭，这些已确定行为不要再当必答问题。

用户主动提出其他通知接收者时，收集该应用下的 open_id 与是否加入任务群，加入名单放 identity.group_member_open_ids；不能仅靠姓名猜 ID，也不为配对顺手给其他人发消息。第二期已经发布且用户明确启用时，才补问工作群 chat_id，默认创建仍为专属群。

长期 secret **不进入聊天或 argv**。首选用户已有的受限文件；如需要录入，用配置工具安全 TTY 提示。没有安全输入通道时，引导用户在目标机器保存文件并只回复绝对路径。Agent 仅核验可读、所有者、0600，不显示内容。

## C. 飞书后台一次性准备

若应用已可用，按实际检查结果跳过已完成项。未完成时给用户一张具体操作清单：

1. 在飞书开放平台创建企业自建应用，启用机器人，取得 App ID 和 Secret。
2. 配置长连接接收事件，添加 `im.message.receive_v1` 和 `card.action.trigger`。
3. 按发布包 `compat/feishu-permissions.json` 的精确权限 ID 申请：私聊收消息、群内 @ 收消息、发送/回复/更新、创建群和所需成员管理、读取核验所需群/成员/自身投递记录及原消息 reaction 写权限 `im:message.reactions:write_only`；二期另含话题能力。
4. 发布版本，完成组织需要的审批，设置应用可见范围包含本人及计划使用的群成员。
5. 在飞书找到机器人并打开单聊。需要群内收发时，机器人须在目标群中。

Agent 可以检测 token、API 错误、长连接和实际事件；仅持有应用凭据不能保证有权自动创建应用、申请组织权限或发布。没有已经授权的控制台操作能力时，上述平台步骤由用户/管理员完成，Agent继续其他准备并提示具体缺项。不要索要整套管理员凭据。

参考：[飞书官方 SDK](https://github.com/larksuite/oapi-sdk-go/blob/v3_main/doc/channel.zh.md)、[自动群管理](https://open.feishu.cn/solutions/detail/groups?lang=zh-CN)、[卡片回调](https://open.feishu.cn/document/feishu-cards/card-callback-communication)。权限精确 ID 以已验收发布清单为准，不能由 Agent 根据名称编造。

## D. 不知道 open_id：单聊配对

此步骤要求已验收版本声明 `capabilities.setup_pair=true`，机器人已发布且能收私聊。工具会优先复用已有正式实例；新部署才使用临时配对监听。已有部署要指定实际配置，不能另起连接绕过旧实例的锁。身份冲突、旧实例不支持或连接未恢复时先报告具体前置条件。

Agent 准备受限的部分向导 JSON：`schema_version: 1`、`feishu.app_id`、`feishu.app_secret_file`；无须先有 owner、目录或发布包路径。调用：

```sh
codex-feishu setup pair --input /absolute/path/to/pair-input.json --timeout 300 --json
```

已有配置不在默认位置时，加 `--config /absolute/path/to/config.toml`。保持这个 CLI 进程运行，逐行读取 JSON；收到 `state=pairing-waiting` 后，只把 `data.pair_command` 展示一次，引导用户在机器人单聊发送该命令。不要将完整输出落盘或保存口令，也不要在用户回复“已发”后重启 CLI 创建第二个口令。

```text
/pair <一次性口令>
```

继续读取同一进程的终态；仅当 `state=paired`、`ok=true` 且退出码为 0 时，将 `data.owner_open_id` 写入待审核的向导输入，并默认用于 operator 与通知目标。CLI 本身不写配置、不重置已有 owner；已有 owner 时只有本人能确认。临时模式只处理配对；已有实例继续正常同步，但 `/pair` 不变成任务或输入。

一次性口令有 128 bit 随机性，最长 5 分钟有效；群消息、其他应用、错误发送者、过期口令不能认领。终态不含口令。取消时终止并等待原 CLI 退出，它会按原请求尝试清理，临时监听也会关闭。

超时或结果未知保持 needs_input，提示检查发布、私聊订阅和可见范围；不能把发送者姓名、union_id 或另一个应用的 open_id 代替。用户未回答/未配对时继续保持缺项，等待时间不能代替其选择。

## E. 生成向导输入与计划

Agent 用安全序列化生成受限 JSON 文件，替换全部占位值。以下是完整字段示例，**不是可直接运行的用户配置**：

```json
{
  "schema_version": 1,
  "artifact": {"binary_path": "/absolute/path/to/codex-feishu"},
  "feishu": {"app_id": "cli_replace_me", "app_secret_file": "/absolute/path/to/app-secret"},
  "identity": {"owner_open_id": "ou_replace_owner", "pair_owner": false, "operator_open_ids": ["ou_replace_owner"], "notification_open_ids": ["ou_replace_owner"], "group_member_open_ids": []},
  "discovery": {"allowed_roots": ["/absolute/path/to/projects"], "auto_group": true},
  "notifications": {"timezone": "Asia/Shanghai", "quiet_hours": ""},
  "features": {"phase": "phase1", "creation": false},
  "service": {"mode": "systemd-user"},
  "verification": {"send_test_card_to_owner": true, "create_private_test_group": true, "create_codex_test_thread": true, "bind_test_group_and_thread": true}
}
```

verification 的三个 true **仅来自用户明确授权的范围**；没有对应授权填写 false。其他用户不能因在 notification 列表中自动成为操作者。

Agent 将以下变量设为验证过的绝对路径/计划 ID；它们是任务变量，不是系统环境变量：cfb_cli、cfb_intake、cfb_config、cfb_plan_id。不得对包含用户输入的路径做未转义 shell 拼接。

```sh
"$cfb_cli" version --json
"$cfb_cli" setup --input "$cfb_intake" --plan --json
```

计划应列明：安装版本与兼容性、实际共享端点、本人身份、允许目录、自动建群、提醒/quiet、配置/密钥引用/数据库位置、将管理的用户服务和允许的测试。

向用户简短展示计划后，若任务已有上述配置授权继续执行；若用户只要求计划，到此交付。占位值、缺身份、未验收版本、权限 pending 不可被勾成通过。

## F. 应用并启动

CLI 已通过能力检查且 plan 无必需缺项时：

```sh
"$cfb_cli" setup --input "$cfb_intake" --apply --plan-id "$cfb_plan_id" --json
"$cfb_cli" doctor --config "$cfb_config" --json
"$cfb_cli" service install --config "$cfb_config" --user --json
"$cfb_cli" service start --config "$cfb_config" --json
"$cfb_cli" doctor --config "$cfb_config" --require-running --json
"$cfb_cli" status --config "$cfb_config" --json
```

逐步检查退出码，不用 shell 链把后续步骤在失败后继续跑。setup 生成正式 TOML，备份现有配置并原子写入；目录 0700，配置/secret 0600。保留原 host_id、数据库和绑定。重复配置差异为空时 no-op，不换身份、不清空历史、不重建群。

重配已有部署时，先读 plan 的现有通知规则与独立差异。省略 quiet / timezone 保留有效规则；只有用户明确要求清除免打扰时才传 `quiet_hours: ""`。更新仅影响 owner 的明确字段，保留关闭 @ 与 snooze，其他接收者保持原值。已有规则更新需当前 schema 21；旧 M0 数据库不能由配置任务顺手迁移。普通启动也会拒绝未接管的 schema 1。迁移命令、备份和中断恢复已有隔离测试，但开发版拒绝真实迁移；不要改 user_version、清空日志或启动新实例绕过该边界，见[接管进展](../research/m3-legacy-adoption-2026-10-12.md)。

**检测到旧 M0 schema 1 时：**先检查 `capabilities.setup_migrate` 和 `phase1_released`。开发版最多做迁移预览，报告 `migration-plan-needs-release`；不能为配置任务切换现有桥。发布版且用户已授权迁移时，使用原配置绝对路径：

```sh
codex-feishu setup migrate --config /absolute/existing/config.toml --plan --json
codex-feishu setup migrate --config /absolute/existing/config.toml --apply --plan-id REVIEWED_ID --json
```

plan 将私有摘要保存到数据库旁，不包含原文或密钥，不改 schema。审核群绑定、卡片更新数量和备份路径后，apply 前核验并停止准确的本工具旧进程；不要停止 Codex daemon。旧二进制保留。命令会持共享计划锁及 App / 数据库锁，生成包含 WAL 的私有一致备份，再重读历史、队列、目录及群成员。成功不自动启动服务或发送测试消息。

中断时保留原配置、密钥、二进制、计划文件和备份，用同一 ID 重跑；不要删除日志、复制活跃数据库主文件或启动第二个接收器。已经提交的恢复只修复 marker，不回退后续活动。计划过期或证据变化退出 6，重新读取结果；退出 5 表示仍需恢复或排除未决操作，不报告迁移完成。迁移后再按 F / G 的服务与 UI 验收流程验证，详见[迁移契约](10-cli-and-setup.md#m0-迁移事务)。

若 apply 返回 `setup-recovery-pending` / 退出 5，暂停启动步骤，保留原向导输入、原 plan_id、二进制、密钥、备份与事务文件，重跑原 apply 恢复。不要创建新计划覆盖未完成事务、删除日志或改用备份配置启动。服务会检查同一物理数据库的未完成部署事务，在建立连接前拒绝启动。兼容版本 daemon 重启后的 socket 变化允许重新发现。身份、二进制、密钥或实际规则另有变化时报告具体冲突，不能强行覆盖用户后续命令。

doctor 是只读检查，不自动发送消息。基础失败立即停止对应后续动作；状态未知保存诊断而不重复 apply / 建群 / 发消息。service install 只操作本工具的用户服务，不重启 Codex，不修改原权限，不开放公网端口。

安装前的 `prerequisites-ready-bridge-not-observed` 仅表示前置条件通过，允许继续安装；它不表示服务已运行。启动后必须使用 `--require-running` 并检查必需项：连接、绑定恢复、初始历史基线和远端结果待核验都不能略过。`version` 的 `data.feishu_permissions_manifest` 已内嵌 API、准确权限、OR 替代关系、官方来源和实测方法，直接读取即可，不依赖当前目录下存在仓库。`documented_permission_satisfied=pass` 只表示文档要求的授权满足；`runtime_observed=unknown` 和真实 UI 待验收仍须进入 G 步骤。

若服务环境无法保证 SSH 注销后常驻，报告 systemd/linger 的具体情况，由适用管理员或已有 supervisor 解决；不能以 nohup 成功代替完整验收。配置文件已生成而服务阻塞时保留可复用成果。

## G. 真实验证

先检查 `version` 的 `capabilities.verify_managed_tests`。当前开发版为 false，完整受控验收尚未接入；默认 verify 可以做只读前置检查，但必需 UI / P1 证据仍 pending。不要把旧 M0 测试准备当作正式验收，也不要为了绕过实例锁停止用户现有桥或用开发版 m0-listen 迁移旧数据库。

当前 `capabilities.verify_managed_card=true` 只表示本人测试卡组件已开发，不表示完整受控验收通过。仅在已准备的测试实例和明确本人测试卡授权下使用，不为了此能力安装 / 替换未发布服务。运行实例还须报告 `verification_card_supported=true`。

先用安全随机源生成并保留 32 位小写十六进制 ID，例如 Python 的 `secrets.token_hex(16)`；将它赋给普通变量 `cfb_verify_id`（不是秘密凭据）。发送与后续查询分别为：

```sh
"$cfb_cli" verify --config "$cfb_config" --target self --allow-test-card --request-id "$cfb_verify_id" --json
"$cfb_cli" verify --config "$cfb_config" --target self --request-id "$cfb_verify_id" --json
```

本人在私聊卡片点一次按钮，再发送卡中 `/verify` 命令；5 分钟内的实际 owner 回调 / 私聊入站由原接收器记录。第二条命令不发送，重复第一条也不重发原未知或终态请求；unknown 不换 ID 盲重试，过期保留证据，新探针仍需明确测试授权。正常诊断失败时不启动探针；匹配实例的只读查询可返回现有证据，并保留诊断失败的退出码。`verification-managed-card-observed` 仅代表这三个组件，仍 `ok=false` / 退出 5、全部 P1 pending。命令会返回组件记录的原 `bridge_commit` 和 `binary_sha256`，后者由运行实例从 `/proc/self/exe` 计算；`build_changed=true` 时为历史组件证据，不能当作当前版本验证。

当前 `capabilities.verify_managed_group=true` 只提供测试群组件，仍需已经准备的测试实例和明确本人测试群授权，不据此安装未发布服务。实例须报告 `verification_group_supported=true`。单独的 group 标志只建 owner 与机器人的验证群，不继承生产额外成员，不发卡 / 消息或创建 Codex 线程；需要私聊卡组件时另加 card 标志。

```sh
"$cfb_cli" verify --config "$cfb_config" --target self --allow-test-group --request-id "$cfb_verify_id" --json
"$cfb_cli" verify --config "$cfb_config" --target self --request-id "$cfb_verify_id" --json
```

查看返回的 `verification_group.chat_id` 对应验证群，5 分钟内在该群 @ 机器人发送 `/verify-group <原 request ID>`，再执行只读查询。成员证明来自完整 GET；missing has_more、未识别成员、受限分页或额外人员不会报告通过。GET 失败时相同授权 ID 可以只重做成员检查，不重建群；查询自身只读日志，不发 GET 或建群。群入站可能先于成员 GET 成功，两份观察都取得才显示组件 pass，不据此证明发消息瞬间的完整成员名单。验证群无 thread binding，不证明 App 自动建群或输入回显。

无 allow 的原 ID 查询会检查该实例支持的卡 / 群 / 线程创建 / 绑定日志。不存在的组件为 skipped，全部无日志为 `verification-managed-probe-not-found`；不新开探针、不注册或激活、不当作拒绝证据。组件 observed 状态仍 `ok=false` / 退出 5、12 项 P1 pending。unknown 群不按名字找、自动换 ID 或再建；过期保留原群和证据。请求任一未支持的组件时，组合 allow 不部分执行。

当前 `capabilities.verify_managed_thread_creation=true` 只表示隔离出生组件已开发；`verify_managed_thread_ui_verified=false`，完整 `verify_managed_tests` 仍 false。只在已准备的测试实例、本人独立线程测试授权和实例 `verification_thread_creation_supported=true` 时使用；不为此能力部署未发布服务。

```sh
"$cfb_cli" verify --config "$cfb_config" --target self --allow-test-thread --request-id "$cfb_verify_id" --json
"$cfb_cli" verify --config "$cfb_config" --target self --request-id "$cfb_verify_id" --json
```

它在专用私有目录使用固定本机模型和只读策略，只复用当前已初始化原生连接。不启动 turn、不发飞书消息、绑定测试群、提交输入或 archive；group 标志也不会替它完成 binding。出生 pass、原 creator 连接仍当前和本机资源可用分别检查，`verification-managed-thread-creation-observed` 仍非完整验收。`resource_recovery_required=true`、连接变更、unknown 或到期时保留原 ID / 目录 / 创建日志，不绑定旧端口、更换 URL 或重建线程。不能用创建确认声称 App 页面、排队 / 引导或配置完成。

当前 `capabilities.verify_managed_binding=true` 提供已有群与线程的绑定组件；实例须报告 `verification_binding_supported=true`。它须有独立的绑定授权（输入 `verification.bind_test_group_and_thread=true`），不能从创建群 / 线程授权推导。只在同一 ID 的群和线程均 confirmed、owner 已在原群发送验证命令、原构建 / 连接 / 资源 / 窗口有效时执行；再次 GET 完整成员并核验无覆盖附着策略。它不创建缺失组件、不自动提交测试输入，也不 archive。

```sh
"$cfb_cli" verify --config "$cfb_config" --target self --allow-test-binding --request-id "$cfb_verify_id" --json
"$cfb_cli" verify --config "$cfb_config" --target self --request-id "$cfb_verify_id" --json
```

第一条保存原群 / 线程关系并请求现有 runtime 跟进；启用后仅 owner 的新群消息可进入该测试线程。结果 `verification_binding.binding_status=pass` 仅为本地登记，`activation_requested` 为本次入队，`actor_started` 为本地 actor 存在，均不证明 UI、输入或送达。第二条不请求激活；历史登记不会自动续期或重建模型。`verification-managed-binding-observed` 仍 `ok=false` / 退出 5、12 项 P1 pending。创建授权与绑定授权四项独立；用户未允许的字段保持 false。不要为组件能力安装未发布版本，也不要把 owner 群跟进当作自动化 queue / steer 场景已验收。

原绑定的只读查询可返回 `verification_binding.input_evidence_status` 和 `input_evidence`。前者 pass 只说明本地快照成功，所有计数为零时仍没有输入证据。`add_confirmed` 表示默认提交已确认；`history_linked` 表示收据与原生 user item 的真实关联；`steer_confirmed` 要求同一显式动作的 delete 成功及 steer 确认，`steer_history_linked` 还要求进入原目标轮。`queued_snapshot` 仅为已保存的队列观察，`unresolved_attempts` 是尚待核验的输入尝试。缺字段 / pending 时保留证据缺失，不推导无输入或失败，不重发输入。计数不证明 App 页面、忙时顺序、无重复执行或最终卡送达；仍按下表完成实际验收。

当前开发版也提供受控测试模型的本轮释放入口。只有用户要求结束这次受控忙时测试、原绑定仍有效、第一条模型请求已经开始，且 CLI / 实例分别报告 `verify_managed_turn_release=true` / `verification_turn_release_supported=true` 时，单独执行：

```sh
"$cfb_cli" verify --config "$cfb_config" --target self --release-test-turn --request-id "$cfb_verify_id" --json
"$cfb_cli" verify --config "$cfb_config" --target self --request-id "$cfb_verify_id" --json
```

不要与任何 allow 标志组合，也不要从创建 / 绑定授权推导自动释放。第一条只放行原本机模型的回复门，不新建或激活、不提交输入、steer 或归档；未开始 / 过期 / 原资源缺失时拒绝。第二条只读原记录。`model_released=true` 不是原生轮次完成或飞书结果送达证据；继续核验实际历史、最终卡和 App 页面，P1 仍 pending。原五分钟窗口不会因释放延长；未发布版本的安装门槛不变。

已验收版本具备该能力且有提示词中的本人测试授权时，运行：

```sh
"$cfb_cli" verify --config "$cfb_config" --target self --allow-test-card --allow-test-group --allow-test-thread --request-id "$cfb_verify_id" --json
```

删除未经授权的对应 allow 标志。运行实例负责验证事件，verify 不另开一个竞争收消息的正式长连接。测试会话在独立临时目录，使用已发布的受控测试流程；不向用户正在工作的线程注入消息，不改变原会话权限。

| 检查 | 自动证据 | 用户需要做的最少动作 |
| --- | --- | --- |
| 飞书发送与回调 | 消息 ID、同一卡更新、有效 callback 收据 | 在本人测试卡点一次按钮 |
| 建群与首轮同步 | 一次创建意图、chat ID、绑定、首条人类与 AI item 映射 | 查看测试群内容 |
| App 发起自动建群 | 新 App thread 事件、一个绑定、真实群 | 在 App 远程测试项目发一条“只回复 READY”的新任务 |
| 飞书输入与 App 回显 | 收据、原 thread ID、user item.clientId、队列状态 | 测试群 @ 机器人，确认同一 App 页面出现输入 |
| 忙时排队与显式引导 | 受控活跃轮、原生队列、delete/steer 确认、无重复 item | 测试卡点击“追加引导”，确认队列/聊天记录 |
| @ 与 quiet | 结构化 mention、策略命中、被抑制记录 | 确认本人收到 @；测试关闭后不再 @ |
| 恢复与持久性 | 服务重启后原绑定/规则/水位，SSH 注销后检查 | 仅在需要时重连 App/SSH 验证 |

配置 Agent 能读取线程内容但看不到 App 界面时，必须请用户确认 UI；可用文本确认，无需用户提交截图。用户尚未操作则该项 pending，不能用后台历史代替 UI 验收。忙时测试使用隔离测试流程；临时目录不在允许范围时为其设置专用测试范围，结束后移除，不能扩大生产监听范围。

测试结束恢复用户原通知规则，归档测试 Codex 会话，停止临时配对/模拟组件；测试群保留明确测试标签，不自动解散。不要删除生产配置、日志证据或绑定。

## H. 故障处理与完成报告

| 情况 | Agent 应做的事 |
| --- | --- |
| 没有桥二进制 / 命令不存在 | 指明软件尚未提供；保留环境报告，不执行虚构命令 |
| CLI 版本正确但 daemon 不兼容 | 报实际 appServerVersion 和所缺能力，暂停写入；不自动换服务 |
| token 可用但收不到消息 | 检查发布、事件订阅、应用范围、群中机器人与 @，不盲目重建应用 |
| 群/消息创建响应未知 | 使用原意图核验；不重复创建、不改 UUID |
| 回显历史存在但 UI 未确认 | 保持 UI pending，给一条具体确认动作 |
| 重跑已有部署 | 差异更新，沿用 host_id、DB 与 bindings，不重复建群 |
| 更改运行中配置 / 更新正在使用的 unit | 明确停止本工具，重新 plan / apply，再 install / start；restart pending 或 start no-op 不表示新配置生效 |
| 运行正常但待审批 | 回 App 处理；不扩大权限绕过 |

最终报告应包含实际安装版本、远程主机/用户、配置与状态路径、服务状态、允许目录、通知规则、测试群/线程标识，以及通过/未通过的验证项。不包含密钥、token、配对口令或用户全量对话。

全部必需验证通过才说“配置完成，双向同步已验证”；仅进程运行就说“服务已启动，以下验收待完成”。若缺外部条件，说明**具体阻塞、已完成部分、用户下一步**，不反复请求已有授权。

## 配好后常用指令

```text
/mute-dir /project --recursive          禁该目录及子孙未来自动建群
/unmute-dir /project --recursive        解除该递归规则
/mute-dir list                          查看目录规则
/notify at off                          长期关闭对自己的 @
/notify at on                           恢复并清临时暂停，仍遵守 quiet
/notify at snooze 2h                    暂停两小时
/notify at until 2026-10-11 09:00        暂停到指定时间（用户时区）
/notify at quiet 23:00-08:00             每日休息时间
/notify at quiet off                    清每日时间段
/notify at status                       查看当前实际状态
```

普通群消息要 @ 机器人；忙时默认排队，卡片点击“追加引导”才改变当前轮。关闭 @ 保留内容同步。第二期启用后才提供 /new、直接消息创建与话题选择。
