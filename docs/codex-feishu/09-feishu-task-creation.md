# 09 第二期：飞书内创建任务

负责 `internal/creation/`，扩展目录选择、topic binding 和相应卡片。依赖一期通过验收的 01–08；phase1 默认 creation.enabled=false。

## 入口路由表

| 位置与输入 | 行为 |
| --- | --- |
| 机器人私聊：/new 或普通文本 | 建草稿，正文预填 |
| 已授权、未绑定入口群：@机器人 /new 或普通文本 | 建草稿，当前群可作话题目标 |
| 已绑定任务群：普通 @ 文本 | 进入现有线程，不建任务 |
| 已绑定任务群：@机器人 /new | 建新草稿，默认新专属群 |
| 绑定话题内：普通 @ 文本 | 跟进该话题的线程 |
| 无效权限 / 未授权群 | 使用说明或拒绝，不创建 |

命令解析必须先识别 /new，其他普通文本按位置路由。已知管理命令的拼写错误提示纠正，不把 `/notify ...` 误当新任务内容。

## 草稿与目录

draft 保存 creator、入口 message ID、任务文本、host、selected_directory、可选 project_id、follow_mode、target_workgroup、revision 和期限。草稿修改不创建远程对象；同一输入事件只生成一个草稿。

默认取可核验的当前项目 / 最近可用目录；没有可靠默认时必须让用户选择，不用服务进程 cwd 代替项目目录。列表来自 App project/list 和配置 allowed_roots 下常用目录，分页而不递归扫描整台机器。

其他目录选择卡允许在配置范围内浏览目录或输入绝对路径，所有校验发生在远程执行主机：真实路径、目录存在、可读写、允许范围、projectId 是否与目录匹配。worktree 和项目原路径沿用 07 的规范化函数。项目关系无法验证时可以无 projectId 创建，但卡片明确“独立目录”，不伪造项目归属。

mute 只阻止自动建群，用户主动创建不因此被拒绝。selected_directory 始终只有一个值。

## 专属群与话题

默认 group：创建机器人与 owner 的任务群；卡片明确显示“新建专属群”。topic：在已授权工作群发送任务根卡，保存 root_message_id / 平台 thread ID。来自单聊时必须选工作群；没有可用群时不执行，保持草稿或切回 group。

同一群可有多个 topic binding，按原始 root/thread ID 定位。脱离已绑定话题的 @ 不猜最近会话：引导进入对应话题或建新草稿。所有回复、通知和错误都保持 topic，失效时不降级为主群。

## 可恢复创建流程

```text
草稿提交（领取唯一 token / revision）
  → 校验目录、项目、成员和位置
  → 持久 create intent
  → 创建目标群，或创建任务根卡
  → 保存目标平台 ID
  → thread/start(cwd, 可选 projectId, 桥自建 threadSource)
  → 保存原生 thread ID + binding
  → queue/add 首条输入（复用 input_receipt）
  → 核验首次执行 / 排队 + App 可见性
  → 更新入口创建卡与目标任务卡
```

threadSource 使用该版本支持的自建来源字符串；discovery 同时按本桥 binding / intent 排除，不能只依赖一个来源字段防重复群。初始权限采用 Codex 既有默认/所选项目策略，不启用 bypass，不复制当前 Agent 的特殊授权到新线程。

每个远程步骤都有 prepared / dispatching / confirmed / unknown 记录。重复提交返回原 intent 进度，不从头再建。群已建、线程明确创建失败时保留已有群，允许“继续创建会话”；线程已建、首条输入失败时继续该线程与原收据。

thread/start 响应丢失属于关键 unknown：本机尚未证明可通过唯一 client token 去重创建。需要先查询自建标记、目录、时间和可用元数据核验；不足以唯一定位时保持阻塞，不重复 thread/start。二期验收前若需要可靠自动恢复，应验证当前协议可用的持久创建标识；不可声称“多重条件相似”就是严格唯一。

首条输入前 rollout 可能不存在，另一个客户端 resume/read 会暂时失败。拿到 thread ID 后先在原客户端提交首条输入，等待持久化/原生事件，再验证 App；不要重新 thread/start。App 侧可见性的稳定实现是二期发布门槛。

## 验收

1. /new 与普通文本入口均有预填草稿；绑定任务群中的普通 @ 只跟进原线程。
2. group 默认值、topic 工作群选择、单聊无默认工作群的反馈正确。
3. 实际 cwd / projectId / 权限正确；主机无权访问的目录不能执行。
4. 重复 callback、群成功线程失败、线程成功输入失败、创建响应丢失均不生成第二套对象。
5. App 出现该线程，App 与飞书继续同一 thread ID；discovery 不再建群。
6. 两个话题并行及根消息撤回，内容不串话题也不落主群。
