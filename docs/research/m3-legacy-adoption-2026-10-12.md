# M0 数据接管的存储与历史映射基础

日期：2026-10-12。当前开发 schema 为 11。本文记录隔离验证，不代表真实 M0 已切换，也不是一期发布验收。

## 已实现

`Store.Open` 在 chmod、修改 journal mode 和迁移前检查已有数据库。schema 1 返回 `ErrLegacyAdoptionRequired`，防止正式服务只升级表结构却丢失旧绑定、重新发送历史。schema 2–10 的正式开发数据库继续按既有路径升级；新数据库直接创建当前 schema。

`LegacyInventory` 在一个读事务里读取旧绑定、输入、操作、动作和 reaction。摘要涵盖原始正文，但公开 JSON 不序列化这些记录。`Quiescent` 要求 app 和绑定一致、输入及操作全部终态、没有 unknown/sending/changing 或孤立记录。它不核验原生队列，也不把 `m0_actions.done` 单独当作 RPC 成功证明。

`sync.PlanLegacyThread` 从完整、静止的原生历史生成接管数据，与正式 `Engine.Apply` 共用纯渲染逻辑。首条用户 item 必须匹配；consumed/steered 输入按 client ID 唯一关联实际 turn/item。App 输入继续镜像，飞书输入使用原消息做回复锚点，不生成重复人类卡。

旧 M0 按 3500 rune 偏移分段，正式服务按序列化卡片大小分段。映射器复用 confirmed 消息 ID，形成正式 canonical key。旧片段比新片段多时，多余卡片更新为合并提示；新片段需要的消息数超过已确认消息数时拒绝接管，不能补发历史。旧最终 item 镜像与后来按 turn 聚合的结果合并处理；保留的旧 commentary 只映射为过程。retired ID 不更新。

维护脚本曾撤回 consumed 队列卡并清空 `receipt.CardID`，却留下 confirmed 的 send journal。因此空 CardID 的 consumed 输入不从该 journal 恢复卡片。其他无法判定的情况保持 pending；非空 CardID 必须与确认的队列卡记录一致，接管后更新为无按钮的终态视图。

`Store.AdoptLegacy` 在同一事务内重查库存摘要、升级 schema、登记原群的 confirmed intent/binding、保存镜像和输入关联，并写入 `legacy_adoptions` 计划记录。卡片保持 `RemoteID`，以 ready/revision 1/delivered 0 等待 PATCH，不新建消息。原 M0 表和身份不重写。完全相同的已提交计划重复执行为 no-op，后续输入与卡片状态不回退。未绑定历史仍通过首次基线成为 ignored，不自动建群。

## 调用与证据边界

这是内部接管基础，还没有可用于真实切换的 CLI。存储 primitive 验证本地来源摘要、消息 ID、父关系、canonical key 形状和事务一致性；**不能仅凭 NativeDigest 证明原生历史完整或渲染覆盖完整**。调用方必须从完整原生快照重跑 `PlanLegacyThread`，禁止从 CLI 输入反序列化裸镜像 seeds。后续配置命令须核验计划 ID 和新读取的数据。

真实 apply 前仍需：核验并停止准确的旧 bridge 进程，持有 App 与规范数据库部署锁，保存私有 SQLite 一致备份，冻结配置及兼容版本，重新核验完整 history/空队列、App 来源、目录和群成员，再执行原子接管。不能停止 Codex daemon，不能用文件复制代替活跃 WAL 数据库的备份。

已有 schema 的只读检查使用 `mode=ro` 读取 live WAL。SQLite 可以创建读取所需的 WAL/SHM sidecars；守卫保证不改记录、schema、journal mode 或文件权限，不承诺文件集合完全不变。不得改用忽略 WAL 的 immutable 读取，也不得删除可能属于活跃进程的 sidecars。

## 隔离验证覆盖

- schema 1 的普通启动拒绝；DELETE 与活跃 WAL 模式下，原 schema、记录、权限和 journal mode 保持不变。
- 计划过期、unknown 操作、缺少原生关联、伪造/重复/撤回消息 ID、错误父关系和遗漏绑定均拒绝。
- 注入后期 SQL 约束失败，schema DDL、正式绑定和卡片全部回滚，旧日志保留。
- 精确计划提交后，修改输入并重启，再重放原计划，不恢复旧正文或重开已投递卡片。
- mapper → schema 1 `OpenExisting` → 原子接管 → 正式 Engine → Outbound 整链路：复用原群，只有 update，飞书原输入不重复，已撤回卡不恢复，后续完整历史扫描不重发。
- 流式过程、旧新分片数量差异、混合最终回复、维护脚本和歧义历史均有独立测试。

本轮 `go test -race ./... -count=1`、最终存储/同步包的 race 复验、`go vet ./...` 与 `git diff --check` 通过。测试和审查均使用合成数据与临时数据库；没有向真实群发送消息、提交原生输入或执行真实迁移。

实际切换、配置迁移计划/备份/锁的组合、异常远程读回、正式服务 UI 与部署验收仍待完成。当前运行的 M0 配置、数据库和二进制没有替换。
