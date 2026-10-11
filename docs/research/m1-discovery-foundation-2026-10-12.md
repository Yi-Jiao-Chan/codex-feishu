# M1 发现与建群意图：实现进展

更新：2026-10-12。本切片是 M1 基础实现，**不是 M1 完整交付或一期发布**。M0 已通过真实 App / 飞书验证；用户随后明确 fork 暂不处理，本期发现器将它记录为 `fork_deferred`，代码保留 TODO。普通 App 会话与正式登记的 Git 工作树继续在范围内。

## 已实现

- SQLite schema 2：保留 M0 收据和操作日志，增加部署基线、候选观察、稳定建群意图和唯一绑定。完成标志与历史忽略记录原子提交；首次基线的毫秒 fence 重启后不变。
- 首装连接先观察出生事件，快照不会覆盖已记录的新候选。普通、归档和全部 provider 的列表按原生 cursor 分页；loaded 列表补充未落盘草稿。
- 首条实际 `userMessage` 激活草稿；typed 首输入前错误可证明 blank，普通读取错误不能。首输入边界不明、历史时间缺失、目录暂不可读均保留明确 pending 原因。
- 正常本机 0.162.0 的 UUIDv7 出生时间用于漏事件/离线创建补偿，必须严格晚于持久 fence 且不在未来。来源、主/子线程、目录和实际用户输入仍独立校验；时间异常或自定义/非 v7 ID 保守等待。详见[准确版本研究](discovery-baseline-0.162.0.md)。
- metadata-only read 补齐薄列表可能缺失的 fork/parent 分类；内部验证/创建标签、子代理、临时线程不会自动建群。fork 保留 TODO，不读父历史或猜继承输入边界。
- 工作树授权同时验证 Git common-dir、正式登记和相对项目子目录；仅复制 `.git` 指向同一个 common-dir 不足以获得授权。mute 判断实际 cwd 与映射回原项目的 cwd，精确规则不扩大为递归。
- 建群参数和 UUID 在请求前持久化；并发只领取一次 dispatch。未知响应或 dispatch 中崩溃停止重发；已保存远程群 ID 后的绑定失败可只恢复本地绑定。

`internal/bindings.Manager` 要求基线完成、候选 eligible、附着同一线程和 dispatch 前再次授权。生产组合根仍须在建群期间持久记录事件/镜像，并提供完整同步、outbox 和恢复；仅调用 Manager 不代表这些能力已交付。

## 验证

`go test -race ./...` 与 `go vet ./...` 已通过。新增场景覆盖基线回滚/重启、历史续聊不激活、出生与快照竞争、草稿激活、漏事件补偿、归档暂停、读取故障隔离、子代理排除、fork TODO、worktree/子目录/mute、并发建群、明确拒绝、未知响应及成功后本地保存前崩溃。

开发探针 `cmd/m1-discovery-check` 已连接实际共享 daemon **0.162.0**，使用独立 probe DB，未读取凭据、未 resume、未建群、未发消息。实际首装扫描完成：324 条 ignored、432 条 pending、5 条 excluded；用户给出的真实 App fork 样例确认为 `fork_deferred`。432 条 pending 包含 386 条 cwd 暂不可访问、46 条归档候选，属于保守等待，不能写成已全部跟进。重复启动使用同一基线；该探针不启用生产自动建群。

```bash
go run ./cmd/m1-discovery-check \
  --config /absolute/config.toml \
  --database /absolute/separate-probe.db \
  --watch-seconds 0
```

Probe DB 必须与配置中的正式/M0 DB 不同；命令输出仅分类、计数与明确提供的样例 ID，不输出真实正文、群标题或凭据。

## 继续实施

接入生产组合根、镜像/outbox、每轮单过程卡和完整历史补齐后，才能验收 App 首条输入自动建唯一群且两端持续跟进。目录命令/通知持久化、停止恢复、重连核验和 Agent 自动安装仍在后续 M2/M3；目前不要求用户再做 UI 操作，完成完整链路后集中验收。
