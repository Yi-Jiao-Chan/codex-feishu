# 创建卡同页布局与附件预览部署记录

日期：2026-10-23。用户确认本人私聊合成预览后授权实现，并明确要求部署到既有桥接服务。本次更新沿用原配置、数据库、应用和共享 Codex daemon。

## 构建与范围

- 源码基线：`f86c0422922c6b1bf5114593e757d6b9f5ea52ad`，保留已部署的独立压缩轮、Goal 续跑同步和桥工作树生命周期修复。
- 本地构建标识：`f86c0422922c+creation-card-796ba293d814`。部署构建时，创建卡改动为本地工作区增量，未将该构建标成已提交或已发布版本。
- 构建时源码快照 SHA-256：`796ba293d8146829457a6af92420aeb76a28cfb0310bc5b7f9e566a2d549fd2f`。
- 二进制 SHA-256：`588549e8c3d7cc139f4f84dc1d8dda1505307a9b5dbe3b885fcb348fb41d41f0`。
- 数据库保持 schema 31，无迁移；`phase1_released=false`。

新草稿默认勾选工作树；工作目录使用同一位置的最近/常用下拉与四层名称搜索。图片可点击放大，文本文件可在卡内展开，附件逐项删除与表单编辑一起持久化。未新增上传界面。具体边界见 [09 创建任务](../codex-feishu/09-feishu-task-creation.md) 与 [08 卡片](../codex-feishu/08-cards-and-actions.md#创建表单与确认)。

## 切换与核验

切换前识别到另一份更新刚部署到同一服务，未执行过时构建的停止或安装；先纳入该版本的两个新增提交，再重跑全仓测试。正式切换前再次核验运行二进制、PID、unit 和配置摘要。

通过原 CLI 停止准确的 `codex-feishu.service`，保存旧二进制引用、私有配置和 unit；SQLite 一致备份覆盖 56 表、178532 行，逐表行数一致，`quick_check=ok`、无外键错误。随后使用候选 `service install/start` 更新同一个用户服务，未重启共享 daemon。备份、原始健康报告和部署日志位于仓库外的私有目录。

启动后 PID 为 359051，systemd 为 active/running/enabled；运行进程、稳定 CLI 和新构建的身份及二进制摘要一致。配置字节未改变，Codex 与飞书连接均恢复。

部署前后 doctor/status 均退出 5，只保留原有 `instance.bindings_reconciled` 和 `instance.operations_known` 两项 required pending，无新增 required fail/pending。该结果不能写成整体健康或完整一期验收通过。

## 验证与未完成项

创建卡改动的相关包测试及 `go test -race ./internal/creation ./internal/store` 已通过；纳入当前运行版本后再次执行 `go test ./cmd/... ./internal/... ./compat`，全部通过。文档链接和 diff 检查通过。

合成原生卡的布局已由用户确认。2026-10-23 部署后，用户明确反馈本轮创建卡“测试通过”，并要求创建 PR、提交本轮修改及合入 main。该反馈作为本轮创建卡的客户端确认，不以 API 成功、进程状态或自动测试替代；手机布局、双话题和完整一期矩阵没有获得本轮逐项确认，仍保留原状态。
