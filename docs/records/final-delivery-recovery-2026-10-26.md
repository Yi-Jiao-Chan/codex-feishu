# 最终回复投递纠正与失败卡

- 日期：2026-10-26。
- 范围：最终回复中的公开 Markdown 图片、明确拒绝后的纠正投递、失败可见性、schema 35 迁移及本地候选替换。
- 状态：implemented；业务测试、相关并发测试、迁移、候选部署、目标回复重发及平台 API 读回 verified；真实桌面/手机展示与完整一期验收 pending。
- 模块：[02 存储与恢复](../codex-feishu/02-store-and-recovery.md)、[03 飞书网关](../codex-feishu/03-feishu-gateway.md)、[05 消息同步](../codex-feishu/05-message-sync.md)、[08 卡片与动作](../codex-feishu/08-cards-and-actions.md)。

## 故障与修复边界

本次故障样本的最终回复含普通 Markdown 图片目标。原渲染路径会把该语法直接交给飞书 Markdown 组件，但普通 URL 或本地路径不是已上传的飞书图片 key，整张卡被平台明确拒绝。已有镜像会冻结这次 rejected 结果；后来即使完整原生历史可生成安全的新展示副本，也没有受限的恢复入口。用户既看不到最终回复，也收不到一张可读的投递失败说明。

公开正文现在先经 CommonMark 解析并只修改确实解析为图片的节点：HTTP(S) 图片降级为普通链接；本地绝对路径、相对路径、`file:`、`data:` 和其他非网页目标改为不含地址的“图片请回 Codex 查看”占位；不安全引用定义一并从展示副本移除。嵌套链接不产生嵌套 Markdown 链接，代码围栏、行内代码、转义及未解析为图片的示例保持原样。桥不读取本地文件、不抓取远端图片，也不改写原生历史或分片 Raw。

只有最终回复首次创建被明确拒绝、没有远端消息、没有 dispatching / unknown 或限流等待，且后续完整快照生成不同的安全卡片时，原镜像才允许一次纠正投递。新 attempt 引用原 rejected attempt，重新冻结当前卡片并重算提醒；原 attempt 和原卡片保持不可变。相同正文、update、unknown 和未决 attempt 不会打开该路径。

纠正投递或明确限流后的重试若再次被平台明确拒绝，同一事务创建一张独立失败卡。失败卡只含固定提示、受限 code / HTTP status，以及经过字符白名单的 request ID；没有安全 request ID 时使用本地 attempt ID。它不复用原回复、平台错误正文、mention 或按钮，自身没有 retry 来源，因而被拒绝时不会递归生成新卡。schema 35 为 outbound 和 attempt 增加 retry 来源，并为 attempt 保存受限拒绝字段；从 schema 34 升级不会自动重发旧记录或补造失败卡。

## 自动验证

新增回归覆盖图片直链、引用式图片、嵌套图片、代码与转义、长消息分片和 Raw 拼接，以及相同 rejected 冻结、修正后成功、修正后失败、限流后永久失败、失败卡不递归、提醒重算和 schema 34 迁移保留。所有样例均为合成内容。

以下检查通过：

```sh
go test ./cmd/... ./internal/... ./compat
go test -race ./internal/store ./internal/sync
python3 scripts/check_docs.py
git diff --check
```

并发检查后仅调整纯卡片与分片渲染，最终改动上重新运行了全仓业务测试。Go 命令继承原 `GOCACHE` 和 `GOMODCACHE`，没有清理或改写缓存。

## Rebase、迁移与本地候选部署

部署前获取 `main`，目标提交为 `72950694c6a9daf79e512922f5f7fc0802dbeb2b`，功能分支已基于该提交，无需改写。候选源码提交为 `e8aab9f2fde134e9ead1b0062d0b1543e633cfb5`，二进制 SHA-256 为 `b65ad24b01887a8743f2bdd25fa39f381d1759b3317bf4445dd4e540e0341b1c`。开发打包器读取工作树 VCS 状态时退出 128；核验干净工作区和提交身份后，继承原 Go 缓存，以 `-buildvcs=false -trimpath` 构建并通过 `main.commit` 写入上述源码身份。随后新增的本记录仅是文档，不改变已部署代码。

复用原用户服务执行 stop → install → start，继续使用同一配置、数据库和共享 Codex daemon；没有启动替代 app-server、创建替代会话或修改全局配置。停桥后在独占锁内保存一致数据库副本，再由正式 `store.Open` 将源库从 schema 34 升至 35。迁移前源库与副本均为 62 张业务表、185288 行，逐表行数一致，`quick_check=ok`、`foreign_key_check=0`；副本保持 schema 34，源库升级为 schema 35。原二进制、unit、配置和数据库备份保存在仓库外私有目录，目录与文件分别使用 0700 / 0600 权限。

切换后运行 PID 为 2120857，systemd active/running/enabled，`NRestarts=0`；实际进程、稳定 CLI 和管理通道均匹配上述提交与二进制摘要。原配置摘要不变，Codex 与飞书连接正常。doctor/status 仍退出 5，仅有部署前已存在的 `instance.bindings_reconciled`、`instance.operations_known` 两项 required pending；不能据此记录为整体健康通过。`phase1_released=false` 保持。

## 目标回复纠正投递

迁移与启动本身没有改写旧 rejected attempt。启动后的历史协调观察到同一最终回复已有不同的安全快照，按新规则创建一条带来源的 revision 2 attempt；原 revision 1 attempt 继续保持 rejected 且冻结内容不变。新 attempt 由正常 worker 单次投递并 confirmed，远端消息已保存，没有创建失败卡，也没有人工重复触发第二次发送。

平台 GET 读回确认消息由当前应用发送、回复到原父消息，唯一投递 marker 保留；可见正文不含故障样本中的本地路径或图片语法，使用无路径占位。平台会规范化卡片 JSON 的默认字段和样式，因此返回 JSON 与冻结请求的逐字节严格相等检查为 false；本轮只把发送者、回复关系、marker 和安全可见正文记为 verified，不把完整载荷记为严格一致。

部署前后历史 unknown attempt / outbound 聚合均为 15，没有新增 unknown；观察结束时该目标的 revision 2 已 confirmed。计数只描述当时的只读聚合，不能替代逐项一期验收。

## 剩余项

- 纠正后的卡片尚未取得真实桌面或手机截图；平台 API 读回不替代客户端 UI 验收。
- 独立失败卡的代码和合成回归已验证，但本次目标纠正投递成功，没有为了预览而制造第二次真实拒绝；其真实客户端样式仍 pending。
- 两项历史对账 pending、完整一期矩阵和 `phase1_released=false` 均保持，不因本次单卡恢复而提升发布状态。
