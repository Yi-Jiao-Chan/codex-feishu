# 会话模型设置与指令帮助本地部署记录

日期：2026-10-23。用户明确要求将本轮版本部署到本地服务用于测试。本次继续使用既有 `codex-feishu.service`、配置、数据库、应用和共享 Codex daemon。

## 构建与范围

- 集成基线：`348dd9935909d31488ead85c8e1c8cf4308f0f4a`，保留当前服务已有的创建卡同页布局与附件预览更新。
- 本地构建标识：`348dd9935909+session-settings-8212a54c826c`；会话设置和帮助改动为未提交增量。
- 构建源码清单 SHA-256：`8212a54c826cd0a5e741f81d696bcd640e45af656678015b84be583d8aa4612f`。
- 二进制 SHA-256：`58f9ffb39296192b21dc2707c6bdca89315375e82dca4aa67e04d047f6641742`。
- 原工作分支保留；另建独立工作树整合并构建。构建使用显式源码指纹和 `main.commit` 标识，自动 VCS 标注失败后使用 `-buildvcs=false`，不将本地增量标成已提交版本。

本轮包括 `/model`、`/reasoning`、`/fast` 会话设置，新 AI 回复底部的会话设置引用，以及列出全部 15 条指令的 `/help` 和 `/help 指令名` 详情。设置用于后续轮次。说明见 [08 卡片与动作](../codex-feishu/08-cards-and-actions.md)。

## 数据与服务切换

先在私有数据库副本上验证 schema 31→32，再通过旧 CLI 停止准确的原服务。核验 PID、运行二进制、unit、配置和稳定 CLI 均未被并行部署改变后，保存原配置、unit、二进制引用及 SQLite 一致备份。

停服备份保留原 56 张业务表、179006 行；`quick_check` 与外键检查通过。离线升级持有应用和数据库监听锁，确认没有未完成配置事务，调用仓库的 `Store.Open` 迁移，新增 `session_settings_requests` 和 `reply_settings_sources` 两表。升级前后原表定义及逐表行数一致，新表为空。迁移日志、原始诊断与备份均留在仓库外的私有目录。

使用新二进制执行候选 `service install/start`，并原子更新原稳定 CLI 链接。启动后 PID 557845，systemd active/running/enabled，自动重启次数为 0；运行进程和稳定 CLI 的构建标识及二进制摘要一致。配置字节保持，数据库为 schema 32，Codex 和飞书连接恢复；本轮未重启共享 daemon。

## 验证与未完成项

整合后的 `go test ./cmd/... ./internal/... ./compat` 全部通过；文档链接与 diff 检查通过。数据库副本演练和正式升级分别完成完整性、外键、表定义及行数检查。

部署前后 doctor/status 均退出 5，required 非通过项仍为 `instance.bindings_reconciled` 和 `instance.operations_known`，没有新增 required 检查状态类型。绑定恢复计数从切换前 65/104 变为重启后 61/104；现有绑定元数据中有 32 个目录已不存在，日志切换前后均有附着和历史核验待处理，但这些聚合证据不足以确定 4 个差异的原因。保留该差异为待排查，不能写成整体健康通过。

本轮尚未获得用户对会话设置、回复脚注和帮助卡的真实客户端确认，UI 保持 pending；`phase1_released=false`，完整一期矩阵状态不变。未发送合成测试消息或创建测试会话。
