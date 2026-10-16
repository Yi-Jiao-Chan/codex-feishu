# M1 完整历史同步与通知规则进展

日期：2026-10-12。此切片新增完整历史到持久镜像的转换与免打扰规则计算 / 存储，尚未接入正式 `serve`，不是一期发布。

## 实现

`sync.Engine` 只读取完整、已补齐 items 的原生历史；事件用于触发重读，不直接转发公开增量。只发布原生人类文本和 AI 公开消息，工具仅显示状态计数，不发布 reasoning、工具全文或命令输出。人类来源标为“Codex 端”，不把 vscode / cli 元数据冒充实际桌面来源或作者。

飞书输入按原生 clientId 与本部署、本 thread 的收据精确关联，原正文不再镜像；实际 receipt / turn / item 关系与镜像、outbox、扫描水位在 schema v4 的同一事务保存。一个逻辑收据冲突映射到两个原生输入时不推进水位，不自行再次输入。

第一条人类输入确定本轮真实回复父消息，后续追加引导不改变它。运行中的每轮只有一张过程卡，终态压缩原卡，并单独聚合 phase=final_answer；无 phase 时仅兼容最后一条无 phase 公开回复，明确 commentary 不充当最终正文。已完成历史不会补建过程卡。最终结果全部确认送达后，`FinalDelivered` 才返回 true，可用于 reaction 完成门槛。

长正文纠正后继续更新原分片。减少分片时，未尝试投递的尾片本地抑制；已存在的尾片 PATCH 成合并提示，不留下旧正文。未知投递不被清理操作绕过，核验后才可更新原卡。

首个最终分片保存原始 completed / failed 事件类型和可核验的 completedAt（原生 0.162.0 的 Unix 秒）。缺失、负数或未来时间不冒充已知事件时间；重连与正文纠正保留首次 replay / 时间证据，不创造新提醒。正式通知安排与送达时策略重查仍待接入。

`rules.Policy` 实现 off / on / snooze / until / quiet / quiet off / status。quiet 左闭右开并支持跨午夜；until 拒绝不存在或歧义的本地时间。长期 off 不会被 snooze 自动解除；on 清 snooze、保留 quiet；quiet off 只删除 quiet。通知规则按 app/user 持久保存，默认初始化不重置已有设置，revision CAS 防止并发丢失，损坏规则不会回退成允许 @。

## 真实 daemon 只读探针

执行开发命令：

```sh
go run ./cmd/m1-sync-check \
  --config /home/chenxiaoyu.lemon/.config/codex-feishu/m0.toml \
  --database /home/chenxiaoyu.lemon/.local/share/codex-feishu/m1-sync-check.db \
  --thread 01a12524-92d1-7c10-bcc3-9e3d58976116 \
  --import-m0-receipts
```

探针只连接现有 daemon，不 resume、不发原生输入、不回答原生请求、不实例化飞书连接或投递 worker。明确指定样本的 eligibility 只是隔离数据库里的测试 fixture，**不证明自动发现或自动建群**。配置中的实际 M0 数据库以 mode=ro / query_only 读取关联身份字段，不复制收据正文，不迁移它；同路径、硬链接、符号链接、父目录别名都禁止作为探针目标库。

真实 0.162.0 样本读取 6 轮历史：1 张原生人类卡、6 张最终结果卡、6 条精确飞书输入关联，最大序列化卡片 355 字节，第二次完整扫描文档及版本未变，groups_created=0、messages_sent=0。没有把每条飞书输入当成新的人类镜像。[脱敏输出](evidence/m1-sync-read-only-2026-10-12.json)只含计数与状态，不包含正文、标题、密钥或用户身份。

随后只读数据库元数据检查确认：探针库 schema=4，6 条终态事件均保存了可核验的原生秒级时间；实际 M0 数据库仍是 schema=1，6 条关联均保存在隔离探针库。M0 listener 的原 session 仍可轮询且没有退出，本轮没有重启它。

## 本地验证与尚未完成

本地测试覆盖完整多轮补齐（80 轮、跨 turn 的重复 legacy item ID）、精确重复去重与冲突快照拒绝、Feishu 收据去重 / 首输入锚点、终态 / final phase、长代码完整拆片和原卡纠正、未知尾片恢复、原始通知时间及 replay 不被改写、v1/v2/v3 迁移保留原生或投递未知日志。规则测试覆盖 quiet 四个边界、组合、DST、绝对 deadline 重启、revision 并发冲突及损坏数据。

最终执行 `go test -race ./...`、`go vet ./...`、`git diff --check`。本地测试及真实只读快照不能代替正式服务端到端 UI 验收。正式统一事件路由、自动建群与同步组合、原生输入 / 动作恢复、reaction、回 App 处理等待状态、命令授权与卡片反馈、通知投递、限流恢复、CLI / setup / service 仍须继续实现。现有 M0 listener 保持原实现，本轮未切换真实服务，也未要求用户新增操作。
