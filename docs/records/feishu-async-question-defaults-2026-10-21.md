# 异步问答联调与默认配置

日期：2026-10-21。范围：[审批与用户问答](../codex-feishu/13-feishu-approvals-and-questions.md)、配置加载与 setup 新建配置。状态：真实异步单题选择的飞书填写和同会话回传 verified；App 原问题控件的完成显示 pending。

## 真实联调

用户报告异步问题卡只有“请在 Codex App 中回答”。运行配置仅启用 `command` 和 `question`；请求实际为 `async`，因此未生成飞书回答控件。同步问答的 `question` 开关不包含 `async_question`。

经用户授权，备份私有配置、停止飞书桥后，向现有清单加入 `async_question`，再用同一二进制恢复服务。Codex 与飞书连接恢复；共享 daemon、数据库 schema 和原生会话策略保持原值。配置备份和操作记录保存在仓库外的私有目录。

联调使用当时运行的候选构建 `b613584f8932+html-interactive-20261021-162439-8850d9d4`，数据库 schema 30，二进制 SHA-256 为 `775f5a72a4e8bb419ad2edf4a7d379df45f4a1c86dd1c194c1b74a360cd89b58`。这是显式配置开启后的联调，不是下文默认值代码的部署证明。

用户指定在当前会话触发一次真实异步提问，随后从飞书提交单题选择。当前会话实际收到原生答案包装，用户确认成功。只读核验结果：

| 证据 | 结果 |
| --- | --- |
| 问题卡投递 | 可操作表单已投递确认 |
| 飞书动作收据 | 1 次 `submitted` |
| 原生队列提交 | `add` 总数 1，且 `confirmed` |
| 原生输入消费 | `consumed`，具备 turn / item 关联，历史关联 1 条 |
| 问题与实际答案 | 请求 `resolved`，实际答案收据 1 份 |
| 原飞书卡 | 显示实际答案、移除提交控件，更新投递已确认 |

私有验证记录于 Asia/Shanghai 16:38 保存。题文、答案、会话/群/请求标识和数据库不进入仓库；以上仅保留汇总事实。App 原控件的完成显示、多题异步填写、手机布局及跨端同时回答未在本次确认，继续保留相应验收边界。既有恢复诊断的 pending 与 `phase1_released=false` 不变。

## 默认配置

用户确认回传成功后要求默认开启。配置加载和 setup 新建配置共用问答默认值：`enabled=true`、`enabled_types=["question", "async_question"]`。省略的字段采用默认值，显式关闭或类型清单覆盖对应默认值；启用时显式空清单仍拒绝。旧的自选清单不会自动扩充，审批类型仍需另行配置。

该改动不调整请求发送、原生队列、表单回调和 unknown 核验流程，也不增加默认审批类型。当前已运行服务的异步开关由前面的显式配置变更保持启用。

## 代码验证

配置与 setup 定向测试通过：`go test ./internal/config ./internal/setup -count=1`。覆盖省略字段、显式关闭、关闭且空清单、自选类型、开启但空清单拒绝，以及 plan / apply / load / 重跑 no-op。修改的 Go 文件已执行 gofmt。

全仓命令 `go test ./cmd/... ./internal/... ./compat` 中，其他包通过，`internal/app` 的 `TestRuntimeGroupDeadlinePreservesFinalAndWakesWithoutNewInput` 在 `group_retry_test.go:55` 超时失败。另建干净的 `b613584f8932` 基线工作树，以相同缓存单独运行该测试，也在同一位置失败；该测试直接构造 Runtime，不经过本次配置默认值路径。本次未修改这一既有建群重试问题，不将全仓结果记为通过。

`python3 scripts/check_docs.py` 与 `git diff --check` 通过。配置默认值代码未在本次另行部署，真实联调证据仍对应上文准确候选构建。
