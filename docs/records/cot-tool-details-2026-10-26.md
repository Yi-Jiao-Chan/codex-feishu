# 原生 CoT 短标题与工具详情

- 日期：2026-10-26。
- 范围：新轮次的安全内容投影、工具展开结果、旧展示版本兼容及本地验证。
- 状态：implemented；全仓业务测试、相关包 race、文档检查及本地候选切换 verified；真实客户端验收 pending。
- 模块：[01 原生适配](../codex-feishu/01-codex-client.md)、[02 存储](../codex-feishu/02-store-and-recovery.md)、[05 同步](../codex-feishu/05-message-sync.md)、[08 展示](../codex-feishu/08-cards-and-actions.md)。
- 依据：[内容质量调研](../research/feishu-cot-content-quality-2026-10-26.md)、[飞书原生 CoT 契约](../../compat/feishu-native-cot.md)。

## 实现

用户确认后，第一阶段以及第二阶段中已有结构化来源的详情已实现。新 `codex_feishu_cot=2` 文档把工具身份、短标题、详情、结果摘要、退出码与耗时分别保存；工具标题最多 32 个 Unicode 字符，详情最多 6 条、每条最多 160 字符。复合命令按真实动作种类概括，未知命令采用中性名称；原命令摘要与旧卡片的动作版本计算保持原样。

每次真实工具调用继续使用同一 native ID 派生的 START / RESULT 身份。展开后的 RESULT 提供安全文件/动作列表，成功 fileChange 提供去重文件数；没有额外事实时只保留简短状态。命令的 nullable int32 退出码及命令/MCP/动态工具的 nullable int64 毫秒耗时已核对本地 0.162.1 schema 和[官方固定源码](https://github.com/openai/codex/blob/25f50de6ed95627e1ffe7f11ca30d3d62c6e20e6/codex-rs/app-server-protocol/src/protocol/v2/item.rs#L255-L317)。0 保留、缺失省略，结果不解析原始输出，也不推断复合命令各步是否执行成功。

ARGS 仍为 `{}`；详情在结果可用时进入 RESULT，不重复放到输入。结果按实际双重 JSON 编码的 4096 字节限制裁剪，超限明确标注省略。元数据后到只更正原 RESULT，不重发 START 或增加调用次数；真实标题更正仍保留已有纠正机制。没有状态且未开始的工具只显示中性活动说明，不伪造可计数的调用。

已有版本 1 文档和旧 ACK 继续原格式，包括活动轮次；新版本 ACK 明确保存展示版本，持久镜像禁止中途切换。`/stop` 同时识别两个版本，已完成消息保持不可变。没有 SQL 迁移、额外模型请求、全局配置或 daemon 变更。主要代码是 [详情投影](../../internal/sync/cot_tool_details.go)、[RESULT 渲染](../../internal/cot/tool_result.go)与[展示版本兼容](../../internal/store/mirrors.go)；模块手册和原生契约同步更新。

## 验证

新增回归覆盖每次调用与详情的独立关联、结果更正、退出码/耗时的 null 与 0、执行状态、fileChange 成功计数、隐私哨兵、双重编码预算、重启恢复、旧文档保持原格式和禁止展示版本切换。停止命令回归覆盖新旧展示版本并存时的目标歧义。测试全部使用合成内容，继承原 GOCACHE / GOMODCACHE。

已通过以下检查：

```sh
go test ./cmd/... ./internal/... ./compat
go test -race ./internal/store ./internal/sync
python3 scripts/check_docs.py
git diff --check
```

后补的仅耗时元数据更新断言单独通过 `TestPlannerToolDetailsRemainAttachedToOneCallAcrossRestartAndCorrection`。文档检查覆盖 101 个文件、1169 个本地链接；调研中的 4 段合成 JSON 均可解析。

## 独立代码审查

SubAgent 首轮审查发现 1 项 P2：已有 native 读取分类时，复合命令中的独立未知子命令被忽略，整次调用可能错误地只显示为读取。已仅在 V2 补充中性的“运行自定义命令”详情，并采用复合标题；空命令、整体无法分类的命令继续依赖 native 元信息，旧共享摘要与 V1 保持原样。补充读取/搜索、shell 包装及整体无法解析的对照用例后，专项回归与定向 race 通过。

同一独立审查者完成复审，无待修复发现；额外核验了详情超过 6 条的省略说明、旧摘要不被修改、V1 格式保持和隐私哨兵。修复后重新运行全仓业务测试，结果通过。

## 未完成项

未发送真实预览；新版桌面/手机、群/话题的展开效果、字段优先级和失败样式仍为 pending。本地协议与发送 fixture 通过不替代客户端 UI 验收，也不改变一期发布状态。已发送且已完成的旧 CoT 不重写。

## 2026-10-26 Rebase 与本地候选部署

按用户授权 rebase 到 `main` 的 `d8657aa89446e7cedfbf43cada44dd942d1ce4fa`，保留单用户任务群免 @ 输入。仅文档索引发生冲突，解决时保留两边记录，COT 代码补丁保持原样；整合后全仓业务测试和文档检查通过。

候选源提交为 `dda578099397b0717defb0c875624278c943282f`，二进制 SHA-256 为 `588741090478871db73218ff27eb259cae267a4a360b33af830f7a5a0877156b`。构建继承原 Go 缓存，以 `-trimpath -buildvcs=false` 和显式 `main.commit` 标注已核验的干净源码身份；后续仅补充本部署记录与集中清单。

复用原用户服务执行 stop → install → start，稳定 CLI 原子切换到新二进制。停桥后通过 SQLite backup API 保存包含已提交 WAL 的一致备份：schema 34、62 张业务表、181836 行，源库与副本逐表行数一致，quick_check 和 foreign_key_check 通过；没有迁移。旧二进制、原 unit/配置和数据库备份保留在仓库外私有目录，不用旧数据库覆盖后续活动，也不回退到只理解展示版本 1 的二进制。

运行 PID 3139060，systemd active/running/enabled，NRestarts=0；进程、稳定 CLI 和管理通道的 commit/摘要一致。Codex、飞书连接正常，发现基线完成，原配置摘要及共享 daemon 发现结果保持。doctor/status 仍退出 5，仅有原先的 bindings_reconciled / operations_known 两项 required pending；各类 unknown/dispatching 聚合计数未增加。恢复绑定从切换前 66/110 变为 65/110，复核仍为 65，差异尚未定位，不能记为整体健康通过。

本轮没有创建测试资源或发送测试输入。新展示用于新轮次，已有活动轮次继续原格式；真实客户端验收与 `phase1_released=false` 保持。
