# 置顶群名、近期发现窗口与空闲降频

日期：2026-10-16。状态：deployed-development；当前 CLI / 共享 daemon 0.162.1。App 置顶状态与只读协议已 verified；独立审查后按用户授权更新现有本地候选服务，4 个绑定置顶群名已有平台读回确认，真实飞书 UI 与完整恢复验收仍 pending。沿用原共享 daemon、配置和数据库。

## 行为与恢复

- 专属任务群从 `🤖 会话标题` 变为 `📌 🤖 会话标题`；取消置顶恢复普通名称。原标题优先级、完整 Name、附件回退和机器人前缀沿用既有规则，原标题中的 emoji 不删除。topic 不改共享工作群名。
- 首次从完整分区目录定位唯一 `Pinned`，持久保存不透明分区 ID；已确认 ID 优先于可变展示名。同名歧义或首次查询失败保持未知，不触发猜测改名。
- 每主机约 15 秒分页查询该分区普通与归档集合，显式包含全部 sourceKinds，避免遗漏飞书创建的 appServer 会话。跨页失败不发布半份集合，仅比较两次完整成功快照，变化只唤醒已有绑定。单会话最新授权元数据仍决定实际群名。
- 常规发现按 updated_at 降序翻页，包含最近 24 小时及边界相等记录，跨过边界即停止。时间缺失或顺序异常保守继续；loaded 草稿与持久 pending 单独覆盖。初次基线、首次覆盖检查、长断线和每日补偿仍可全量检查，出生 fence 不重置。
- 所有未绑定候选缺少 active 列表正向证据时先回到 pending，出生事件、loaded ID 和可读历史只保存证据，自动建群通常等待下个扫描周期。归档候选即使移出时间窗口也继续 hold，metadata 和事件唤醒不把“未列出”解释为已解除归档。
- 老且已确认空闲的绑定会话约 5 分钟补原生历史，本地投递、unknown GET 核验及反馈/提醒约 15 秒继续。新输入、原生事件及置顶变化立即唤醒并重新授权；未知原生提交保持普通协调频率。
- 改名继续使用现有发送前日志、PUT/GET 确认、明确限流等待及 unknown 不重发规则。覆盖检查点与置顶快照复用已有 m0_operations，按 host/app 隔离，无 schema 迁移。

## 真实只读协议与 App 证据

用户在真实 App 对同一会话依次置顶、取消置顶，观察到 thread/read.section 从空变为名为 Pinned 的持久分区，再恢复为空；对应数据库 section 变化一致，is_pinned 全程为 0，没有专门的置顶通知。私有采样已停止，真实 ID、标题、数据库和原始日志未写入仓库。

重新导出本机 0.162.1 experimental schema 共 447 个 JSON，按既有算法计算的摘要仍为：

```text
4248c44d68df2d0d55be53b15fffbb24d8c8cf41cb9993a1f79d5d03d3d2b5da
```

另一个现有共享连接只执行 initialize、threadSection/list 与 thread/list：sectionId、section_position 排序、普通/归档分页及全部 sourceKinds 均接受，返回记录的 section 字段与查询一致。没有 resume、创建线程、输入或飞书写操作。原生数据库的分区索引可覆盖 archived + thread_section_id + section_position 查询；常规路径使用服务端 state-only RPC，不直接读取原生数据库。

## 验证

合成 fixture 覆盖置顶/取消、老会话无事件变化、标题变化、分区稳定 ID、首次身份未知、失败半页、重复快照不改名、不因置顶创建历史群、分页边界、重复 cursor、缺失/null data、appServer 来源、重启检查点、出生 fence、归档 hold、空闲唤醒和本地恢复节拍。

- `go test ./cmd/... ./internal/... ./compat`：完整业务回归通过。
- `go test -race ./internal/app ./internal/discovery ./internal/store ./internal/codexclient`：app、discovery 和 codexclient 通过；store 在现有 `TestVerificationNativeMigrationFromNineteenPreservesJournals` 迁移用例中达到默认 10 分钟总时限，未报告数据竞争，不能视为全包 race 通过。
- `go test -race ./internal/store -run '^TestPolling' -timeout 5m`：本次新增的检查点与置顶快照持久化用例通过。
- `python3 scripts/check_docs.py`、Go 格式及 `git diff --check`：通过。

## 发布前独立审查

按用户要求启动独立 SubAgent review，发现一项 P2：普通 pending 或既有 eligible 候选在窗口外首次归档后，仍可能凭可读历史自动建群。新增回归在修复前复现两个失败分支；修复后所有未绑定候选都必须取得当前 active 列表的正向证据才可 eligible，并在可能失败的元数据读取前撤销旧 eligibility。事件 Check 与窗口扫描使用同一门控，不依赖此前已记录 archived。

独立 SubAgent 针对修复复审，原问题已消除，未发现新的阻塞。修复后 `timeout 30s go test -race ./internal/discovery -timeout 25s` 通过（14.903 秒），`timeout 10s go test ./internal/app -run '^(TestRuntime(Persists|Routes|Mute)|TestDiscovery)' -count=1 -timeout 8s` 通过（0.532 秒）。上述完整业务回归及其他 race 结果取得于此次门控修复之前；修复后补跑受影响的发现与运行时回归。

## 本地部署核验

- 代码通过 [PR #4](https://github.com/Yi-Jiao-Chan/codex-feishu/pull/4) 合入 main，源提交 `be054a70d31453d56e9f3a8c35deb2c2f13ef338`，合并提交及部署构建身份为 `0c7f2e2373aa97b3ddb7fce5136fe60a0f232c01`。
- 从干净 main 构建 `cmd/codex-feishu`，继承 Go 缓存，耗时 1.617 秒；二进制 SHA-256 为 `7a88950351c2a7bd1d9e8fa3ff2498afe21ee5b0483b7dc6158a0a88ea81deea`。运行进程的可执行文件摘要与构建一致，version 与 live health 均报告该合并提交。
- 使用原 CLI 停止准确的既有服务，保存私有 unit、配置与 SQLite 一致备份，再由新 CLI 执行候选 install/start；两步均退出 0。配置字节保持一致、数据库仍为 schema 29，无迁移；原二进制与私有备份保留。
- systemd 用户服务 active/running、enabled，观察时 NRestarts=0；Codex 与飞书两侧连接正常。完整 P1 发布标志仍为 false。
- 置顶分区快照与发现覆盖检查点已确认落盘，随后扫描持续推进；观察到 6 个置顶会话，其中已有绑定的 4 个群名均由现有 PUT/GET 流程确认 `📌 🤖 ` 前缀，其他绑定群的已确认名称无多出前缀。这是平台证据，未代替客户端观察。
- 部署前整体 status/doctor 已返回 pending：46 个绑定中 40 个完成当前代际恢复、6 条出站 unknown。部署后观察为 39/46、unknown 仍为 6；旧进程已有 attach、authorization 和 history 待协调诊断。现有恢复记录保留，整体 status 仍为 reconciling / 退出 5，doctor 仍为 diagnostics-pending / 退出 5，不能记作全部健康。

## 尚待真实联调

仍需在飞书客户端确认图标添加/移除、置顶中 App 改标题、完整服务重启恢复、平台限流后的最终名称，以及 topic 工作群名保持。平台读回、本地服务更新和自动测试不提升这些 UI 项，也不提升 phase1_released。
