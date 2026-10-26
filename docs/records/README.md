# 实施与验证记录

按日期保留当时的实现、构建、实验和人工反馈，便于追溯方案落地与故障修复。旧记录中的版本、状态及命令有当时的适用边界，不能直接作为当前部署步骤。

当前使用看[安装指南](../codex-feishu/AGENT-SETUP.md)，模块约束看[模块手册](../codex-feishu/README.md)，当前发布结论看[集中验收清单](../codex-feishu/12-live-acceptance-handoff.md)。自动测试、平台 API 核验和真实 UI 观察分别记录，未确认的项目保持 pending。

## 分功能与版本核验

| 文档 | 日期 / 版本 |
| --- | --- |
| [最终回复投递纠正与失败卡](final-delivery-recovery-2026-10-26.md) | 2026-10-26 |
| [原生 CoT 短标题与工具详情](cot-tool-details-2026-10-26.md) | 2026-10-26 |
| [单用户任务群免 @ 输入与本地部署](one-to-one-group-deployment-2026-10-26.md) | 2026-10-26 |
| [飞书卡片邮箱脱敏与受控重发](feishu-email-redaction-2026-10-23.md) | 2026-10-23 |
| [飞书输入触发原工作树快照恢复](feishu-worktree-auto-restore-2026-10-23.md) | 2026-10-23 |
| [托管工作树归属登记、清理竞态与存量修复](managed-worktree-ownership-2026-10-23.md) | 2026-10-23 |
| [Goal 续跑同步与工作树生命周期修复](goal-sync-and-worktree-lifecycle-2026-10-23.md) | 2026-10-23 |
| [会话模型设置卡片：保存与取消终态部署](model-settings-card-deployment-2026-10-23.md) | 2026-10-23 |
| [会话模型设置与指令帮助本地部署](session-settings-help-deployment-2026-10-23.md) | 2026-10-23 |
| [创建卡同页布局与附件预览部署](creation-card-deployment-2026-10-23.md) | 2026-10-23 |
| [执行卡布局、停止按钮与工具摘要：本地候选部署](task-progress-deployment-2026-10-22.md) | 2026-10-22 |
| [异步问答联调与默认配置](feishu-async-question-defaults-2026-10-21.md) | 2026-10-21 |
| [Codex 0.162.1 候选部署兼容性](codex-0.162.1-compatibility-2026-10-13.md) | 2026-10-13 |
| [置顶群名、近期发现窗口与空闲降频](pinned-group-names-and-windowed-discovery-2026-10-16.md) | 2026-10-16 |
| [飞书创建 Codex 会话：实施与验收记录](feishu-create-codex-session-implementation-2026-10-15.md) | 2026-10-15；2026-10-19 目录预检增量 |
| [飞书入站图片、文件与多附件：实施记录](feishu-inbound-attachments-implementation-2026-10-15.md) | 2026-10-15 |
| [本地文件网页预览：实施与本机验证](feishu-local-file-preview-implementation-2026-10-15.md) | 2026-10-15；2026-10-19 全目录、代码高亮与 HTML 阅读增量 |
| [Controller 原生 Q2/Q3/Q9/Q10 opt-in 验证](native-controller-q2-q3-q9-q10-2026-10-13.md) | 2026-10-13 |
| [Controller 原生 Q4–Q8 opt-in 验证](native-controller-q4-q8-2026-10-13.md) | 2026-10-13 |

## M0–M2 基础实施

| 文档 | 日期 / 版本 |
| --- | --- |
| [M0 真实联调进展](m0-live-integration-2026-10-10.md) | 2026-10-10 |
| [M1 发现与建群意图：实现进展](m1-discovery-foundation-2026-10-12.md) | 2026-10-12 |
| [M1 完整历史同步与通知规则进展](m1-history-sync-and-rules-2026-10-12.md) | 2026-10-12 |
| [M1 镜像与投递基础切片](m1-mirror-outbox-foundation-2026-10-12.md) | 2026-10-12 |
| [正式运行时、持久输入与目录规则](m1-runtime-and-m2-inputs-2026-10-12.md) | 2026-10-12 |
| [M2 接收器、卡片回调与通知出站](m2-receiver-and-notification-delivery-2026-10-12.md) | 2026-10-12 |
| [M2 正式服务组合根、命令与任务控制](m2-service-and-task-controls-2026-10-12.md) | 2026-10-12 |

## M3 与受控验证

| 文档 | 日期 / 版本 |
| --- | --- |
| [管理通道、状态核验与离线取消进展](m3-control-and-offline-cancel-2026-10-12.md) | 2026-10-12 |
| [M3：只读诊断与权限清单](m3-doctor-and-permissions-2026-10-12.md) | 2026-10-12 |
| [M0 数据接管的存储与历史映射基础](m3-legacy-adoption-2026-10-12.md) | 2026-10-12 |
| [受控绑定的管理入口（2026-10-12）](m3-managed-binding-2026-10-12.md) | 2026-10-12 |
| [M3：现有实例内的本人测试卡验证](m3-managed-card-verification-2026-10-12.md) | 2026-10-12 |
| [受控绑定的只读投递证据](m3-managed-delivery-evidence-2026-10-12.md) | 2026-10-12 |
| [M3：现有实例内的本人测试群验证](m3-managed-group-verification-2026-10-12.md) | 2026-10-12 |
| [受控绑定的只读输入证据](m3-managed-input-evidence-2026-10-12.md) | 2026-10-12 |
| [正式 Service 的受控消息闭环](m3-managed-roundtrip-2026-10-12.md) | 2026-10-12 |
| [管理实例的受控线程创建（2026-10-12）](m3-managed-thread-creation-2026-10-12.md) | 2026-10-12 |
| [受控忙时测试的本轮释放](m3-managed-turn-release-2026-10-12.md) | 2026-10-12 |
| [M3：隔离原生线程创建适配器](m3-native-verification-adapter-2026-10-12.md) | 2026-10-12 |
| [隔离原生验证创建日志（2026-10-12）](m3-native-verification-journal-2026-10-12.md) | 2026-10-12 |
| [受控原生验收的本地资源（2026-10-12）](m3-native-verification-resources-2026-10-12.md) | 2026-10-12 |
| [M3：私聊配对与 Agent 配置指引](m3-pairing-2026-10-12.md) | 2026-10-12 |
| [M3 通知配置事务与恢复（2026-10-12）](m3-policy-apply-2026-10-12.md) | 2026-10-12 |
| [正式消息 outbox 的限流恢复（2026-10-12）](m3-rate-limit-delivery-2026-10-12.md) | 2026-10-12 |
| [配置向导、用户服务管理与待处理提醒进展](m3-setup-and-request-reminders-2026-10-12.md) | 2026-10-12 |
| [受控验收群与线程的绑定基础（2026-10-12）](m3-verification-binding-foundation-2026-10-12.md) | 2026-10-12 |
| [验收绑定的飞书投递上下文（2026-10-12）](m3-verification-delivery-guards-2026-10-12.md) | 2026-10-12 |
| [验收绑定的运行时授权与逐次原生写入检查（2026-10-12）](m3-verification-operation-guards-2026-10-12.md) | 2026-10-12 |
| [M3：验证命令与数据库保护](m3-verify-safety-2026-10-12.md) | 2026-10-12 |

## 配套材料

- [结构化证据](evidence/README.md)：当时的已脱敏 JSON 观察结果。
- [历史原生探针](probes/README.md)：可执行脚本和对应结果；不是普通单元测试或一键验收入口。

新增记录使用 `topic-YYYY-MM-DD.md`，注明所用版本、验证方式、结果及剩余项。后续更正保留日期与原因；当前能力变化同时更新模块手册，当前验收变化同步集中清单。
