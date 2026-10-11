# 07 目录规则与通知策略

负责 `internal/rules/`、命令解析与规则存储。依赖 00、02；规则是领域函数，目录和时间边界适合参数化测试。

当前实现边界：目录物理匹配 / 注册工作树规则、严格目录命令解析和 host/app 隔离持久化、通知 `Policy` 计算、严格 `/notify at` 解析与纯更新函数、通知规则 revision 持久化已实现。默认初始化不会覆盖用户已有的 off / snooze / quiet，deadline 使用 UTC RFC3339Nano，不因重启延长。completed / failed 的首次提醒资格、历史策略审计、发送前策略与群成员核验、载荷冻结、送达去重和不确定结果恢复已接入多会话运行时，并通过自动测试。目录与自己的通知命令已接入新版 serve 的原子命令 / 回复日志；needs_user 也已有持久提醒日志，但新事件资格存在下述原生时间证据限制。现有 M0 listener 尚不能使用本页新增命令，新版尚未部署。详见[实现与只读证据](../research/m1-history-sync-and-rules-2026-10-12.md)、[运行时整合进展](../research/m1-runtime-and-m2-inputs-2026-10-12.md)、[接收器与通知出站进展](../research/m2-receiver-and-notification-delivery-2026-10-12.md)、[正式服务整合进展](../research/m2-service-and-task-controls-2026-10-12.md)和[待处理提醒进展](../research/m3-setup-and-request-reminders-2026-10-12.md)。

## 目录命令与匹配

```text
/mute-dir /absolute/project
/mute-dir /absolute/project --recursive
/unmute-dir /absolute/project
/unmute-dir /absolute/project --recursive
/mute-dir list
```

默认 mute/unmute 只增删**精确规则**；带 --recursive 只增删该路径的递归规则。两条可以共存。删完仍被其他祖先规则覆盖时，反馈卡列出实际命中的规则，不能写“已恢复自动建群”。

目录须位于本远程主机、使用绝对路径且存在。clean + realpath 解析符号链接后持久化；比较使用 path relative / 路径段边界。递归命中规则路径本身及所有子孙，`/repo/a` 不命中 `/repo/abc`。路径作为数据处理，不拼到 shell 字符串执行。

worktree 候选包含 actual cwd 与可核验的 original project root。allowed_roots 允许其实际目录或项目根；mute 命中任一可信路径时禁止自动建群。不能因为 worktree 放在另一个临时根就绕过项目 mute。

规则只作用于**未来自动建群**：已绑定会话继续同步，显式 /bind 和二期主动创建不受 mute 阻止。解除规则不重新给静默期的历史会话建群。反馈明确显示远程主机、精确/递归、作用范围和当前匹配原因。

## 通知命令

```text
/notify at off
/notify at on
/notify at snooze 2h
/notify at until 2026-10-11 09:00
/notify at quiet 23:00-08:00
/notify at quiet off
/notify at status
```

规则按 `(app_id, open_id)` 保存，跨本部署全部任务群生效。接收用户只能修改自己的设置；操作者维护通知目标列表。默认 enabled=true、没有 snooze、每日 quiet 关闭，时区 Asia/Shanghai；安装向导可推荐 23:00–08:00，须明确写入用户选择。

| 操作 | 写入行为 |
| --- | --- |
| off | enabled=false；保留其他规则 |
| on | enabled=true、清除当前 snooze；保留每日 quiet |
| snooze 2h | 记录绝对恢复时间；不自动解除长期 off |
| until 时间 | 使用用户时区解析未来时间，保存 UTC；过去/歧义时间报错 |
| quiet 时间段 | 保存本地 HH:mm 和时区；支持跨午夜 |
| quiet off | 仅删除每日 quiet，不清除 off / snooze |
| status | 显示全部规则、当前抑制原因、适用范围与恢复时间 |

例如长期 off 时再 snooze，反馈明确“仍长期关闭，两小时后不会自动恢复”。quiet 采用左闭右开区间：23:00 生效，08:00 结束；起止相等拒绝并建议 off 表示长期关闭。时间使用 IANA 时区，规则持久化后重启不延长 snooze。

## 提醒决策与送达

默认只有 completed、failed、needs_user 三类新事件具备提醒资格；人类镜像、token 增量、普通进度不 @。同一事件对同一用户的通知键唯一，状态变化与内容更新不能重复提醒。

任一长期 off、snooze、每日 quiet 命中就抑制。事件产生并安排通知时记录每个目标是否具备资格；quiet 期间被抑制的通知标 suppressed，不放进“稍后提醒”队列。真正发送前重新判断当前策略和群成员可达性，任何新抑制规则立即生效。

重连与旧历史绑定的补齐不新建 @；首次自动绑定的新任务依 05 的 completedAt 规则单独判定。已持久安排但尚未发送的新通知，发送时若命中免打扰则终止该次提醒，不能等待休息时间结束再补发。正文继续投递。这样关闭 @ 不丢上下文，也不会在早上集中补提醒。

所有 @ 由 03 的结构化 mention 生成；AI 文本里带类似 mention 语法不能自行触发提醒。关闭机器人的 @ 不改变飞书客户端对普通群消息的通知设置，卡片反馈应准确说明。

开发版 needs_user 只保存审批 / 问答的路由身份、方法、时间和状态，不保存命令、问题或答案，不在飞书代答。每个符合资格的请求至多发送一张引用该轮人类输入的固定提醒卡；成员不可核验或任一时间策略抑制时终止该次提醒，不留到早上补发。重连先清除当前代际确认状态，等待真实请求重放和完整历史核验；未知发送结果冻结。

0.162.0 请求通知不含原始发生时间，不能仅凭重连后的接收时间区分旧请求与新请求。当前只承认有本代际真实新会话出生证明的首轮，或 startedAt 严格晚于首次历史核验秒的后续新轮。此前已执行的轮、同秒边界和证据不明请求仍显示“回 App 处理”，但保守抑制新的 @；此限制与真实审批 UI 验收仍需解决 / 明确验证，不能宣称全部 needs_user 场景已经覆盖。

## 验收

1. 精确/递归、前缀碰撞、符号链接、worktree、叠加规则与解除后的反馈正确。
2. 非操作者不能修改目录；接收者可关闭自己的 @，不能替别人开关。
3. 23:00、00:00、07:59、08:00 四个边界与重启后的 snooze 正确。
4. off + snooze + quiet 组合，以及 on 仅清 snooze、保留 quiet 的结果正确。
5. 多群同时完成只提醒允许的用户；限流待发期间用户 off 后不再 @。
6. quiet 期间正文同步，结束后没有积压 @；历史补齐无提醒。
