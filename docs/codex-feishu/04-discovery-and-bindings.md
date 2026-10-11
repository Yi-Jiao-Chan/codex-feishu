# 04 会话发现、自动建群与绑定

负责 `internal/discovery/`、`internal/bindings/`。依赖 01、02、03 和 07 的目录判断接口。

## 可自动绑定的会话

处理允许项目范围内、持久化、有实际人类输入的交互主会话。排除 ephemeral、存在父代理关系的子线程、桥 validation/creation 标记的线程。**2026-10-12 用户调整：fork 暂不实现，发现后记录 `fork_deferred`，不自动建群；代码保留 TODO。** 普通 App 主会话和注册工作树仍在本期范围内；不能因为 sessionId 与 threadId 不同就判为子代理。

来源不能写死：本机真正 App 与误用 CLI 的测试主线程都出现过 source=vscode、originator=codex-tui、threadSource=null；这些字段不能区分两种入口。M0 保存真实 App 新建、fork、worktree 与子代理的脱敏样例，再落实分类器。准确版本规则与边界见[分类研究](../research/thread-classification-0.162.0.md)：parentThreadId 与所有 subAgent 变体排除；forkedFromId 不排除；preview 不作首条用户输入证明。至少保证 App 主会话可发现；共享 daemon 内其他符合条件的交互会话可复用入口，来源页脚在无法细分时写“Codex 端”。CLI 回显仍不验收。

只看到 thread/started 不代表已经有首条任务。对候选读取元数据和首条 userMessage，空白草稿保持等待。新 thread/start 尚未生成 rollout 时可短暂读不到，按候选状态重查，不因此建空群或判永久失败。

## 基线与补偿扫描

第一次启动先保存毫秒 fence、连接事件流并持久接收出生通知，建立 thread/list 基线。已在启动前存在的历史只记已见，不批量建群。基线期间的出生通知和正常本机 0.162.0 UUIDv7 的正向新建证据优先于快照忽略记录；不能单凭 createdAt/updatedAt 判定新任务。边界无法判清的会话保持待检查，不能因扫描竞争大量补建旧群。将基线完成标志与历史忽略记录原子保存；重启不重置 fence。首输入前草稿通过 loaded/list + metadata-only read + typed turns/list 探测保留 pending，详见[准确版本基线研究](../research/discovery-baseline-0.162.0.md)。

平时使用 thread/started 快速发现；约每 15 秒按 updated_at 排序分页扫描，并检查 loaded IDs 与持久 pending。thread/list 没有“updated_since”参数时不要虚构它；本切片完整扫描普通/归档 ID、显式 modelProviders=[]，避免投影延迟落在已推进水位之后。后续优化窗口必须保留覆盖证据，不能默认最新一页完整。归档候选暂停建群，解除归档再判断；乱序和重启通过已见表、首条输入及绑定唯一键去重。

静默期间创建但未绑定的候选保存最终判定原因。解除目录 mute 后不重新自动激活这些历史候选；用户仍可显式 /bind。

## 自动建群状态机

```text
candidate → eligible → create_intent_saved → creating_group
  → group_confirmed → binding_saved → backfilling → active

creating_group 响应未知 → reconciling_group
明确失败 → pending_retry（依具体可重试原因）
目录 mute / 非主会话 / 不在范围 → excluded
```

创建前在 SQLite 领取 `(host_id, thread_id)`，保存 stable intent 与预期成员，并先附着原生线程接收公开事件；建群在独立步骤执行，不阻塞事件存储。群名采用项目名 + 短任务标题，完整路径与线程 ID 放卡片详情。默认加入 owner 与机器人；其他用户只有列在 groups.additional_member_open_ids、且确认平台允许时才加入。notification target 不隐式扩大成员或操作权限。

群 API 的幂等字段由 03 提供。响应未知先核验；没有可靠核验通道时停止该意图并显示阻塞，绝不根据群名猜测并再次创建。拿到 chat ID 后保存绑定，再发送任务卡并由 05 补齐首轮全部人类输入/公开 AI 内容。

即使建群较慢、AI 已完成，也补齐内容而不丢首条人类消息。创建失败的线程保留同步意图；恢复到原意图继续。

当前开发版已实现建群的持久限流等待：只有网关明确通用频控拒绝才重开原意图，原请求参数和平台 UUID 不变，每次尝试有独立记录。建群步骤等待期间不重新 Attach 或调用建群；到期由现有线程 tick 唤醒，无需用户追加输入。重试先用实际线程元数据重新核验分类、目录、mute，再事务领取；已 excluded 的历史不会复活。等待期间继续保存最终回复，拿到群后补齐。unknown、无 chat ID、权限拒绝和额度耗尽不自动重发。未决 dispatch 的崩溃恢复为 unknown。真实服务尚未部署，证据见[限流恢复进展](../research/m3-rate-limit-delivery-2026-10-12.md)。

## 显式绑定

提供 `/bind <thread-id>`：在工作群绑定本群，或在机器人私聊请求为已有会话新建专属群。核验操作者、主机、线程存在及已有绑定；已有绑定时返回现有位置，不静默改绑或另建第二个群。是否把旧上下文带入由绑定卡明确显示；首版默认补最近一轮，不对旧事件 @。

绑定记录至少包含目标模式、chat/root ID、owner、最新原生轮次、状态、首次建群意图和镜像水位。机器人被移除或群不可访问时暂停投递并保留记录，不自动建替代群。

## 目录与 worktree

调用 07 的同一规范化函数处理 allowed_roots、mute 和二期目录校验。worktree 的实际 cwd 与可核验的原项目路径共同判断；项目 mute 命中任一项目路径即可禁止自动建群。没有可靠项目根时不得猜上级目录，保留 unknown_project_root，并在 M0 用真实 App worktree 解决该信息来源。

## 验收

1. 首次安装不会给旧历史建群；随后真实 App 提交首条输入只建一个群。
2. 草稿不建群，子代理不建群；fork 按用户本期调整保留 TODO，不自动跟进。
3. thread/started 丢失，补偿扫描仍发现；重放事件不重复创建。
4. 建群中断/超时、建群成功保存前崩溃，不出现第二个群。
5. mute / 递归 / worktree 生效；解 mute 不补建历史；显式 bind 仍可用。
6. 一条主会话只有一个有效目标，原群不可访问时不擅自替换。
