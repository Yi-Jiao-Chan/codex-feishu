# Agent 一键配置指南：Codex × 飞书

版本：2026-10-15 / 配置 schema v1。对象：用户和负责配置的 Agent。

本指南自包含，目标是在用户提供少量信息后，由 Agent 检测环境、生成配置、安装用户服务并验证。**一期候选服务已实际部署，独立配置 Agent 已验证首次安装、重跑和卸载保留数据；私聊配对也已通过，完整一期发布仍待验收。本文安装流程是正式发布契约。** Agent 必须检查实际 version 的 `phase1_released` 与具体 capabilities：开发版可做 plan，正式 apply / install / start 会拒绝。2026-10-13 的配对观察属于指定候选构建，当前包仍保留 `setup_pair_ui_verified=false`，不能凭本页把另一个包提升为已验收发布。status 实时健康只对提供管理通道的新版运行实例有效，旧 M0 返回只读快照和 pending。命令存在不代表可正式安装；缺少已验收版本时完成只读环境检查并报告前置条件，不能假装配置完成。开发任务另见开发手册，不在一次配置任务里默认编写整个产品。

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

若要读取源码仓库或构建已提供的源码，先遵循该仓库 AGENTS.md 和 CONTRIBUTING.md；普通安装配置无需扫描源码。

### 收到归档包时

先取得来自已知来源的归档 SHA-256，再核验归档。单独收到 archive 和它旁边的 checksum，只能检查二者字节一致，不能证明来源可信。未核验前不执行包内二进制或脚本。

检查归档条目只含一个预期顶层目录；拒绝绝对路径、`..` 路径段、重复成员、软硬链接、设备 / FIFO 和超出合理总大小的内容。安全解包到本人受限、长期保留的版本目录，避免把服务执行路径留在会被清理的临时目录。包内 `SHA256SUMS` 须全部验证，`artifact.json` 的二进制摘要、成员摘要、source_commit、OS/架构须与实际内容一致；它没有资格替代本页的能力与真实验收检查。

二进制位于包内 `bin/codex-feishu`，指南和样例位于 `docs/codex-feishu/`，权限位于 `compat/feishu-permissions.json`。版本输出的 commit 必须等于 manifest 的 source_commit；任一文件缺失、摘要不同或架构不匹配则停止使用这个包，不顺手重建 / 覆盖现有安装。仓库研究链接是补充背景，未打入包也不影响本指南的自包含步骤。

development 包的 `phase1_released=false` 和 pending 验收模板不能改成 pass；默认只继续只读检查和收集缺项，不执行正式 apply / install / start。若用户明确要求候选版本的安装联调，并且实际二进制声明 `capabilities.candidate_deployment=true`，可以按下一段进入候选路径。`docs/codex-feishu/examples/codex-feishu.service.template` 仅用于说明生成结构，禁止直接复制占位 unit，真实单位由 CLI 的 service install 生成。

### 显式候选安装联调

此路径用于发布前验证配置与用户服务命令，仍是未发布版本。缺候选能力的旧二进制不能使用它；不能改发布标志、伪造验收结果或绕过原配置事务。继续完成 B–E 的身份、应用、目录与通知收集；配对和测试消息分别按已有授权处理，候选安装不授权创建测试群或发送输入。

先只读确认本应用是否有运行中的桥。候选模式不接管接收器、不自动停止旧桥；同一应用不能并行运行两个实例。用户尚未授权服务切换时保留现有桥，仅准备计划，真实安装联调稍后进行。指定受限的候选配置和数据库绝对路径（目录 0700）；数据库只能不存在或为私有且与所选二进制 `database_schema` 一致（当前 28）。已有配置的数据库必须与显式路径一致；候选 CLI 不自动迁移，旧候选需先按已审查方案备份迁移，不能手改 user_version 或复用旧 M0/schema 1。

将 `cfb_database` 设为已经核验的数据库绝对路径，和后文的 `cfb_cli`、`cfb_intake`、`cfb_config` 一起使用。替换 E 的计划命令和 F 中对应三条写命令为：

```sh
"$cfb_cli" setup --input "$cfb_intake" --config "$cfb_config" --database "$cfb_database" --candidate-test --plan --json
"$cfb_cli" setup --input "$cfb_intake" --config "$cfb_config" --database "$cfb_database" --candidate-test --apply --plan-id "$cfb_plan_id" --json
"$cfb_cli" service install --config "$cfb_config" --candidate-test --user --json
"$cfb_cli" service start --config "$cfb_config" --candidate-test --json
```

每条命令独立执行、检查退出码；`cfb_plan_id` 必须来自本次候选计划。普通计划与候选计划不能混用，恢复必须沿用原模式、二进制、输入和 plan_id。`candidate_test_deployment=true` 仅标记此次安装性质，`release.acceptance=pending` 仍保留；不因配置应用成功把 12 项 P1 验收改成 pass。

仍使用固定 `codex-feishu.service`，不会新增第二份接收服务。手工 unit、其他部署身份或运行中需要替换的 unit 均拒绝；原 unit 只在同一候选二进制、配置、身份和已加载状态完全一致时允许重复安装为 no-op。unit 记录候选二进制摘要并使用 `serve --candidate-test`；启动须用原版本目录中的同一二进制，变更版本时先明确停止，再重新安装。候选运行时持实际应用和数据库锁，并禁止既有数据库迁移。

随后执行 F 的 doctor/status 和 G 的授权验证，记录真实 App / 飞书证据；`systemctl` 报 active 只证明进程状态，仍须 doctor `--require-running` 核验实际连接和基线。退出测试时使用普通 `service stop` 与 `service uninstall --keep-data`，带原候选 config，保留凭据、数据库和备份。状态 unknown 先读回，不自动回滚配置或抢占原服务。`setup migrate --apply` 仍要求正式发布版本，候选标志不适用于迁移。

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

### 可选：本地文件网页预览

仅在用户要求时启用 `file_preview`；这是桥自身的只读 HTTP 服务，无需云盘上传权限。预览目录必须单独授权，不从 discovery 的广泛范围推断；用户明确要求所有本地目录时可追加 `allowed_roots` 的 `/` 根。扩大范围时保留已有根，并另列隐藏目录下的工作树根；认证和敏感文件过滤仍生效，详见 [14 文件预览](14-file-preview.md)。先用 `file-preview init-secret --output /absolute/private/file-preview.key --json` 在已有私有目录创建口令；不要在配置聊天、argv 或普通诊断中输出口令。用户可自行运行 `file-preview show-secret --config PATH` 显式打印口令，该命令拒绝 JSON 模式；也可由部署所有者私聊机器人发送 `/preview` 取得口令，群内只提示转私聊。该敏感回复保存在私有命令 outbox 和飞书私聊中。

setup 输入可提供 `file_preview` 对象：`enabled=true`、`base_url`（用户设备可访问的 HTTP/HTTPS origin）、`listen_address`（IP 加端口）、`allowed_roots`、`secret_file`；`directory` 默认数据库旁 `file-preview`，单文件上限默认 1 MiB。IP 直连需显式监听可达网卡，默认 `127.0.0.1:8787` 只可本机访问。HTTP 由所用网络承载口令与内容，需要传输加密时接 HTTPS。

沿用既有 stop→plan/apply→install/start 规则，不并行启动第二个桥。首次浏览器打开需输入私有口令；`file-preview link --config PATH --file /absolute/project/report.md --json` 可登记指定文件并返回 URL，不发送消息。doctor 的本地检查不能代替真实飞书点击、手机网络和浏览器登录验收。

若应用已可用，按实际检查结果跳过已完成项。未完成时给用户一张具体操作清单：

1. 在飞书开放平台创建企业自建应用，启用机器人，取得 App ID 和 Secret。
2. 配置长连接接收事件，添加 `im.message.receive_v1` 和 `card.action.trigger`。
3. 按发布包 `compat/feishu-permissions.json` 的精确权限 ID 申请：私聊收消息、群内 @ 收消息、发送/回复/更新、创建群和所需成员管理、读取核验所需群/成员/自身投递记录及原消息 reaction 写权限 `im:message.reactions:write_only`；二期另含话题能力。

   图片预览需要 tenant 权限 `im:resource:upload`（或更宽的 `im:resource`）。缺失时正文和附件清单仍同步，图片提示回 App 查看；不将本地路径当下载 URL。新群在创建请求中写入工作目录，无需另加更新权限；给既有群补目录则需要 `im:chat:update`（或 `im:chat`），并需核验群编辑权限。只申请上述功能需要的权限，发布版本后用 doctor 检查当前已授予项。

   群名与 App 标题同步需要 `im:chat:update`（或已授权的 `im:chat`），并受群内编辑权限限制。当前工具仅更新已绑定群的 name，工作目录描述、群主和成员保持原值；缺少该可选能力不会阻断消息同步。

4. 发布版本，完成组织需要的审批，设置应用可见范围包含本人及计划使用的群成员。
5. 在飞书找到机器人并打开单聊。需要群内收发时，机器人须在目标群中。

Agent 可以检测 token、API 错误、长连接和实际事件；仅持有应用凭据不能保证有权自动创建应用、申请组织权限或发布。没有已经授权的控制台操作能力时，上述平台步骤由用户/管理员完成，Agent继续其他准备并提示具体缺项。不要索要整套管理员凭据。

参考：[飞书官方 SDK](https://github.com/larksuite/oapi-sdk-go/blob/v3_main/doc/channel.zh.md)、[自动群管理](https://open.feishu.cn/solutions/detail/groups?lang=zh-CN)、[卡片回调](https://open.feishu.cn/document/feishu-cards/card-callback-communication)。权限精确 ID 以已验收发布清单为准，不能由 Agent 根据名称编造。

## D. 不知道 open_id：单聊配对

此步骤要求已验收版本，或上文已明确授权的候选联调版本声明 `capabilities.setup_pair=true`，机器人已发布且能收私聊。工具会优先复用已有正式实例；新部署才使用临时配对监听。已有部署要指定实际配置，不能另起连接绕过旧实例的锁。身份冲突、旧实例不支持或连接未恢复时先报告具体前置条件。

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

用户可直接粘贴命令，也可在机器人单聊中先 @ 本机器人再发送。当前实现仅移除经消息 mentions 字段和实际 bot 身份核验的开头占位符，并容许命令首尾空白；内部命令与口令仍须按展示内容发送。不要让用户因使用 @ 或复制带入换行而重复配对；真实终态是成功依据。旧候选不具备此修复时，应先核验构建，不能在 Agent 中伪造一次配对成功。

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

verification 的四个 true **仅来自用户明确授权的范围**；没有对应授权填写 false。其他用户不能因在 notification 列表中自动成为操作者。

Agent 将以下变量设为验证过的绝对路径/计划 ID；它们是任务变量，不是系统环境变量：cfb_cli、cfb_intake、cfb_config、cfb_plan_id。不得对包含用户输入的路径做未转义 shell 拼接。

```sh
"$cfb_cli" version --json
"$cfb_cli" setup --input "$cfb_intake" --plan --json
```

计划应列明：安装版本与兼容性、实际共享端点、本人身份、允许目录、自动建群、提醒/quiet、配置/密钥引用/数据库位置、将管理的用户服务和允许的测试。

向用户简短展示计划后，若任务已有上述配置授权继续执行；若用户只要求计划，到此交付。占位值、缺身份、未验收版本、权限 pending 不可被勾成通过。

### 飞书内新建任务的配置增量

创建功能已接入候选实现，默认关闭，真实飞书 / App 完整验收仍见 [09 创建任务](09-feishu-task-creation.md)；不能因 `features.phase="phase2"` 自动开启。用户已要求启用时，先核对二进制 `capabilities.feishu_task_creation=true`，当前原生创建只验证共享 daemon **0.162.1**。不启动另一套 daemon，不修改全局 Codex 模型、审批或沙箱配置。

目录范围与 discovery 独立收集。询问允许在哪些远程目录新建任务；用户明确选择整台主机时可使用 `allowed_roots=["/"]`。范围根和浏览父级只需可读、可进入，最终执行目录仍须存在、可读写且可进入。省略可靠默认目录时，创建卡要求用户选择；不取服务进程 cwd。入口群与可承载话题的工作群分别配置，只在需要相应入口时收集 chat_id；专属群仍是默认跟进方式。

在上述向导 JSON 中增补明确的 `creation` 对象，例如：

```json
{
  "features": {"phase": "phase2", "creation": true},
  "creation": {
    "enabled": true,
    "default_follow_mode": "group",
    "entry_chat_ids": [],
    "workgroup_chat_ids": [],
    "allowed_roots": ["/absolute/allowed/project-root"],
    "common_directories": [],
    "topics_enabled": false,
    "draft_ttl_seconds": 1800
  }
}
```

这是原输入的增量，保留其余身份、凭据引用、数据库和规则。省略整个 `creation` 对象会保留原配置；显式对象为完整替换。开启 topic 时必须有已授权 `workgroup_chat_ids`，并分别验证同群两个话题及根消息撤回后的停止投递。私聊 `/new 任务内容` 会产生预填草稿，选择目录后默认创建专属群；绑定任务群普通 @ 继续原线程，`/new` 才建新任务。

创建卡组合最近 / 项目速选、项目名 / 绝对路径搜索与原生目录面板。所有入口复用既有飞书长连接，面板包含路径导航、当前层筛选与分页，不需要额外域名、HTTP 服务或 OAuth 配置。选择只回填原草稿；原创建者在主卡确认后才创建任务。搜索结果页先选用目录再回主卡，避免误用旧目录。

候选升级到 schema 28 前，停止准确的桥进程并保存配置、原 unit、旧二进制及包含 WAL 的一致数据库备份；不停止共享 daemon，不运行两个同应用接收器。已有附件及绑定必须保留。升级与真实客户端验证都遵循既有候选授权和 F/G 流程，不能把开发能力标志当成正式发布或 UI 通过。

## F. 应用并启动

CLI 已通过能力检查且 plan 无必需缺项时：

首次安装没有运行中的桥，可按下列步骤执行。已有部署先检查计划差异；若确实没有差异且没有待恢复事务，可以保留服务，只核验 doctor/status。需要执行 apply 时，即使 `changed=false`、`can_apply=true`，也必须先停止这个应用的桥，再生成新计划并 apply。实际候选联调已确认：活动桥下无差异 apply 仍退出 3，不能把预览结果当作取得了写锁。不要因此重新创建配置、数据库或第二份服务。

```sh
"$cfb_cli" setup --input "$cfb_intake" --apply --plan-id "$cfb_plan_id" --json
"$cfb_cli" doctor --config "$cfb_config" --json
"$cfb_cli" service install --config "$cfb_config" --user --json
"$cfb_cli" service start --config "$cfb_config" --json
"$cfb_cli" doctor --config "$cfb_config" --require-running --json
"$cfb_cli" status --config "$cfb_config" --json
```

逐步检查退出码，不用 shell 链把后续步骤在失败后继续跑。setup 生成正式 TOML，备份现有配置并原子写入；目录 0700，配置/secret 0600。保留原 host_id、数据库和绑定。重复配置差异为空时 no-op，不换身份、不清空历史、不重建群。

start 成功后，管理端点和历史基线可能仍在恢复；实际首次安装与重复配置都曾短暂返回 doctor/status 退出 5。先每隔几秒只读重查，直到 `doctor --require-running` 必需项通过且 status 为 running；不要重复 apply、另开接收器或反复重启。明确失败、构建/配置冲突或 unknown 操作按 H 处理，不能将它们当作普通启动等待。

已有配置的 unit 若处于 inactive/disabled，先核验它的部署身份、二进制摘要和配置与本次目标一致，再沿用同一 CLI 的 install/start 恢复 enabled/running。此路径已实际验证，原配置、绑定和规则保持一致；它不需要重新 apply 或更换数据库。活动 unit 的身份不符仍按冲突处理。

重配已有部署时，先读 plan 的现有通知规则与独立差异。省略 quiet / timezone 保留有效规则；只有用户明确要求清除免打扰时才传 `quiet_hours: ""`。更新仅影响 owner 的明确字段，保留关闭 @ 与 snooze，其他接收者保持原值。已有规则更新要求当前二进制的数据库 schema，以 `version --json` 中的 `database_schema` 为准；旧 M0 数据库不能由配置任务顺手迁移。普通启动也会拒绝未接管的 schema 1。迁移命令、备份和中断恢复已有隔离测试，但开发版拒绝真实迁移；不要改 user_version、清空日志或启动新实例绕过该边界，见[接管进展](../records/m3-legacy-adoption-2026-10-12.md)。

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

当前 0.162.0 有一个已本地验证的限制：新建但尚无输入的线程，即使由原 creator 连接执行无覆盖的 typed resume，也返回 `-32600/no_rollout`。因此当前候选的这个 pre-input 受控绑定路径不能作为完整测试入口。保留为 pending，不自动补发输入或放宽策略检查；本次配置改走下表的 App 新任务、自动建群和用户群内跟进流程。创建组件成功不代表这条绑定路径可用；已有输入的普通 App 会话已另行验证。

原绑定的只读查询可返回 `verification_binding.input_evidence_status` 和 `input_evidence`。前者 pass 只说明本地快照成功，所有计数为零时仍没有输入证据。`add_confirmed` 表示默认提交已确认；`history_linked` 表示收据与原生 user item 的真实关联；`steer_confirmed` 要求同一显式动作的 delete 成功及 steer 确认，`steer_history_linked` 还要求进入原目标轮。`queued_snapshot` 仅为已保存的队列观察，`unresolved_attempts` 是尚待核验的输入尝试。缺字段 / pending 时保留证据缺失，不推导无输入或失败，不重发输入。计数不证明 App 页面、忙时顺序、无重复执行或最终卡送达；仍按下表完成实际验收。

同一次查询可返回 `delivery_evidence_status` / `delivery_evidence`。快照 pass 只说明本地读取成功；`progress_confirmed`、`final_confirmed` 表示当前正文版本已有确认及原 reply 证明，`final_turns_delivered` 要求该轮全部最终分片和已发尾片清理都确认。`referenced_cards` 是已确认原 reply 指向匹配输入锚点的当前 AI 文档数量，`unresolved_attempts` 保留卡片未决请求（含队列操作卡）；普通发送、只有旧正文确认或只有部分长回复分片不算整轮完成。旧实例缺字段或读取失败时不补造零计数成功，也不重发卡片。原生输入证据与投递证据分别来自各自只读快照，不将它们当同一个原子完成记录；真实客户端、reaction、@、自动建群和完整 P1 仍按下表验收。

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

### 汇总真实证据并结束验证

若实际版本声明 `capabilities.verify_acceptance_report=true`，可以用案例报告表达已完成的真实验收，避免 CLI 永远只能返回占位 pending。先从默认 verify 的只读输出取得 configuration_identity、host_id、codex_daemon 和 instance；后者包含运行桥的 bridge_commit/binary_sha256。它们必须与实际 CLI 的 version、归档二进制摘要及当前配置一致。协议摘要取 version 的 protocol_schema_digest（固定 0.162.0 实验 schema 语料兼容摘要），SDK 版本取内嵌权限清单 sdk_version；缺字段、旧实例或不匹配时不能填写通过。

`configuration_identity` 必须取实际管理诊断的同名字段；unit 注释中的 Deployment identity 绑定服务安装，是不同的摘要，不能代填。实际报告已验证：身份不符退出 6，修正为当前实例身份后，仍含 pending 的报告正常退出 5；缺项不会被健康检查或成功配对覆盖。

使用包内 `docs/codex-feishu/examples/acceptance-report.template.json` 生成本人 0600 私有报告，保留 12 个必需案例，补全实际构建/版本/部署字段，将 template 改为 false。每条 evidence 写成：

```json
{"kind":"manual","path":"/absolute/private/owner-confirmation.json","sha256":"实际文件的64位小写SHA256","observed_by":"此应用配置的本人open_id"}
```

automated 条目不写 observed_by。证据文件必须为本人 0600 常规文件；可以保存脱敏自动测试/运行记录、截图或用户已给出的文本确认，记录实际案例、时间、对象与观察来源。**仅在已经取得当前案例所需真实证据并审核覆盖范围后填写 pass。** 不自行生成用户确认、不把协议/mock 当 UI、不仅修改旧 M0 报告的元数据套用到新版。用户未回答、案例未执行、证据仅覆盖局部时保留 pending。CLI 检查摘要和记录身份，不会替代验收者理解截图或判断案例覆盖。

将 cfb_acceptance_report 设为上述私有报告绝对路径后，独立执行：

```sh
"$cfb_cli" verify --config "$cfb_config" --target self --acceptance-report "$cfb_acceptance_report" --json
```

此命令不运行测试，不能带 allow、release-test-turn 或 request-id。当前服务/权限失败不能由报告覆盖；数据或构建不符退出 6，仍缺证据退出 5，非法输入退出 2。只有 `acceptance-report-validated`、ok=true、退出 0 才表示报告与当前部署核验通过；输出明确 UI 来源是人工记录，phase1_released 不因报告变化。候选验收即使通过，也应写“候选部署与记录已验证”，保留正式发布状态和剩余发布步骤。报告和证据保留在私有目录，不进入 Git 或普通日志。

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
