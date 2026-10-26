# 单用户任务群免 @ 输入与本地部署

## 行为与范围

已绑定任务群只有授权操作者和当前机器人时，文字、富文本和指令可直接输入，不再要求每条消息 @机器人。每条未 @ 消息先确认准确的群/话题绑定，再在同一个两秒超时内读取群信息、当前机器人在群状态和完整用户成员；只有一个用户、一个机器人，且唯一用户就是发送者时才接受。缺字段、人数变化、读取失败或超时均不放行。

接受后沿用原收据去重和原生 queue/add，不自动 steer。未绑定群、其他话题和未授权用户不获得新入口；独立图片/文件仍遵守原有附件开关。群信息与成员查询不是原子快照，不缓存判定结果。

免 @ 需要应用已发布 `im:message.group_msg`。权限清单新增可选能力说明，原有推荐权限清单不变。本次实际应用的该项授权检查通过；权限检查不代表已完成飞书客户端消息验收。

## 构建与切换

用户明确授权提交 PR 并替换本地部署。2026-10-26 12:14 CST 使用既有 `codex-feishu.service` 完成 stop → install → start，继续使用同一配置、数据库和共享 daemon。

| 项目 | 结果 |
| --- | --- |
| 源提交 | `7029ff92ec8a41fc1ae6eafbeaba5d1a6c9126ab` |
| 二进制 SHA-256 | `7c61c37d72a7c24378de108ab1b052f2d0503f8aa03a0544498f5665a4a25528` |
| 版本与 schema | `0.0.1-dev`；schema 34，无迁移 |
| 服务 | PID 2936034；active/running/enabled；NRestarts=0 |
| 身份核验 | 运行进程、稳定 CLI、管理通道 commit/摘要一致 |
| 配置与 daemon | 配置摘要及 daemon 发现结果与切换前一致 |
| 发布状态 | 候选部署；`phase1_released=false` |

构建使用继承的 Go 缓存。构建器自动读取 VCS 状态失败后，核验干净工作区与源提交，以 `-buildvcs=false -trimpath` 构建，并通过 `-X main.commit=7029ff92ec8a41fc1ae6eafbeaba5d1a6c9126ab` 写入已核验提交身份。运行前再次核验二进制摘要与 version。

停桥后保存一致数据库备份，源库与副本均为 schema 34；62 张业务表、181,514 行的逐表行数相同，quick_check 与 foreign_key_check 通过。原二进制、unit、配置和数据库备份保留在仓库外的私有目录；目录权限 0700，备份文件权限 0600，未将真实数据或运行日志提交到仓库。

## 验证结果

- `go test ./cmd/... ./internal/... ./compat`：通过。
- `go test -race ./internal/app ./internal/feishu -run 'Test(ReceiverOneToOneGroup|ServiceOneToOneGroup|OneToOneGroup|ReceiverPersistsOneReceiptAcrossReplaysWithoutNativeOrEcho)' -count=1`：通过。
- `python3 scripts/check_docs.py`、`git diff --check`：通过。
- 合成回归覆盖群人数与双方身份、授权与话题隔离、读取失败、成员变化、文字/富文本、指令、附件开关、事件重放，以及 Service 到原生队列的一次投递。
- 12:15 CST 运行核验：Codex 与飞书连接正常，新的免 @ 权限检查通过，进程无重启，历史 unknown 计数未增加。
- PR 未配置 CI 检查；上述结果为本地验证。

doctor/status 仍退出 5，仅有切换前已存在的 `instance.bindings_reconciled`、`instance.operations_known` 两项 required pending。普通绑定总数保持 110，恢复计数由切换前 70 降为 66；启动后约 98 秒复核仍为 66，差异尚未定位，不能记为整体健康通过。结构化运行提示仍包含 attach、workspace identity 和 history 待核验，不因本次切换消除这些历史对账边界。

## 用户确认与剩余验收

Agent 没有为本次部署创建测试群、线程或发送测试输入；部署核验时，真实飞书免 @ 输入、客户端队列展示及成员变化交互均保留 pending。

2026-10-26 部署后，用户确认本轮测试没有问题，并授权合并 PR。该确认不扩展为所有客户端和成员变化边界均已逐项验收。12:47 CST 只读复核时，恢复绑定仍为 66/110，两侧连接正常且没有新增 unknown，两项历史对账 pending 保持；用户测试确认不代表这些对账问题已恢复。完整一期矩阵和 `phase1_released=false` 保持。
