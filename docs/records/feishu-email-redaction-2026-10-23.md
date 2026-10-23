# 飞书卡片邮箱脱敏与受控重发

日期：2026-10-23。状态：实现、自动测试、本地部署和一次真实重发完成；原失败消息脱敏后投递 confirmed，平台读回通过。客户端 UI 与完整一期验收保持 pending，`phase1_released=false`。

## 问题与改动

同一最终回复卡此前两次发送均被飞书拒绝，最近一次返回 HTTP 400 / `230028`。冻结载荷中含两个明文邮箱，平台没有返回具体命中字段。

卡片公共文本出口现在将常见 ASCII 邮箱替换为字面量 `<A>`，再执行原有安全转义。最终回复、历史镜像、过程卡及原生 CoT 文本共用该处理；邮箱自动链接与 mailto 目标一并脱敏。长文本分片不在匹配到的邮箱或链接内部切开，预算按实际渲染后的 JSON 字节计算。原生历史、输入与身份标识保持原值。匹配规则和边界见[卡片手册](../codex-feishu/08-cards-and-actions.md)。

## 构建与部署

| 项目 | 结果 |
| --- | --- |
| 构建 commit | `0a98f90d932b6f8e34845809a7e0492e2fac010a` |
| 二进制 SHA-256 | `890f50011fd742b05b3de9a9592e552eaf67d28e8b5b7171145deeb23f78463b` |
| 数据库 | schema 34 保持；一致备份完整性检查通过 |
| 服务与配置 | 沿用原 `codex-feishu.service`、配置和共享 daemon；稳定 CLI 与运行进程身份一致 |
| 最终服务状态 | 22:00 CST 核验，PID 2017445，active/running，NRestarts=0 |
| 连接与待办 | Codex、飞书均已连接，65 个绑定已恢复；仍只有原来的两项 required pending |

两项 required pending 为 `instance.bindings_reconciled` 和 `instance.operations_known`，不能将本次投递成功写成全部健康通过。部署、备份和重发证据保存在仓库外私有目录，不纳入真实正文、邮箱或路由标识。

## 一次受控重发

部署后历史重新投影为 revision 2，原 rejected 状态保持，没有自动重试该卡。用户明确授权后，先只读核验原父消息、目标群、两个旧 rejected attempt 和当前脱敏投影，再以数据库副本演练持久登记及结果保存。

停桥并持有原实例锁后，在发送前冻结新 attempt 和操作日志。新载荷只替换原卡的两个邮箱并更新请求标记，保留原群、父消息和提醒身份；随后仅执行一次发送，按标准存储流程保存结果，再启动同一候选服务。

飞书返回 `code=0`，新 attempt 为 confirmed；只读 GET 确认新消息存在于原群并引用原父消息。镜像 revision/delivered_revision 均为 2，出站状态 confirmed，两个旧 rejected attempt 原样保留，新发送计数为 1，unknown 出站数量未增加。这证明该消息脱敏后未再被拒绝，不代表已覆盖所有敏感内容规则或客户端渲染。

## 验证

| 检查 | 结果 |
| --- | --- |
| 邮箱、自动链接、mailto、长文本分片及实际 JSON 大小回归 | 通过 |
| 同步到冻结发送载荷、原历史保留、回复锚点及原生 CoT 回归 | 通过 |
| `go test ./cmd/... ./internal/... ./compat` | 全部通过 |
| `python3 scripts/check_docs.py`、`git diff --check` | 通过 |
| 本次真实发送及平台消息身份读回 | 通过；一次新发送，confirmed |
| 飞书客户端最终展示、完整一期矩阵 | pending；`phase1_released=false` |
