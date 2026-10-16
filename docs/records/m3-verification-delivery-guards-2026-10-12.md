# 验收绑定的飞书投递上下文（2026-10-12）

本切片将已有专属操作授权贯穿正式卡片、reaction、命令确认与待处理提醒的远程写入。仍为隔离测试通过的开发实现；未开放 CLI 绑定授权或启动受控输入编排，真实 App / 飞书 UI、远端清理与一期发布继续 pending。没有发送真实消息、迁移 M0 或切换运行服务。

## 卡片与命令

Outbound 每个候选在领取 attempt 前读取原绑定并获取 BeginWrite，上下文贯穿一次 Send / Reply / PATCH 与 Finish / Defer。拒绝或取消时不领取，领取后取消时不调用 gateway，已持久 attempt 保持保守恢复状态；成功、未领取及错误都释放。旧 unknown 的 GET 读回仍是只读外部操作，不借此新发送。授权检查的网络延迟可能跨免打扰边界，领取前重新采样通知时间再冻结载荷；测试证明正文发送而 @ 被 suppressed。

命令使用只读候选快照、按原 ID 与完整快照的精确 CAS；授权一个候选不会因并发竞争改为领取另一条。过滤与旧领取方法一致，包括限流等待和未知尝试。最多查看 64 个候选，被拒绝的保留 ready / 零 attempt，继续查看本批后续候选；全部拒绝返回固定错误，不输出原卡或错误正文。scope 持有至 ReplyCard 结果保存，unknown 不重发。

Service 独立按 chat 查找验证标记，命令 ThreadID 为空或普通 route 已删除均不能将原测试群视为普通位置。另一条普通 route 重新使用被标记的 chat 也被拒绝。Runtime actor、离线 outbox 与接收反馈循环使用同一 Service 操作检查；普通线程保留原权限和通知语义。

## Reaction 与提醒

Reaction 在领取 transition 前获取操作上下文。已有 reaction 先删除；确认删除后，下一次添加须重新取得授权，再保存其独立发送日志。取消后的已 journal 操作保持未决，不伪造远端明确拒绝，不影响原生输入去重。

授权或日志写入失败时，只有数据库证明下一 add 未 journal、且 delete 未 journal 或已确认同一原 ID，才释放本地 changing。已确认删除的空 emoji / ID 保留，后续新 revision 不再次删除旧 ID；任何 add 日志、不明 delete 或不同操作 kind 均拒绝释放。这是本地未发出证明，不把“查不到远程 reaction”作为可重试证据。隔离触发器失败测试覆盖首次添加、删除及已删后的添加日志失败。

待处理提醒的 GET 读回与策略准备保持原流程，在领取新 attempt 前取得操作上下文，贯穿引用回复与结果保存；取消后保持保守 dispatching。scope 不改变原生审批 / 问答归属，飞书仍只提示回 App。

## 验证与剩余工作

隔离测试覆盖三种卡片写方法、拒绝零 attempt、取消和准确释放、授权跨 quiet、命令快照竞争与跳过拒绝候选、reaction 两阶段与已知未发送恢复、提醒 scope；实际 Service / Outbound 组合证明资源 Close 取消 gateway 上下文并等待 release，原 attempt 不被误标为安全重试。

整仓 `go test -race ./...`、`go vet ./...` 与 diff 检查通过；最后追加的 route 重新使用原 chat 回归场景也单独通过 race。独立审阅发现 transition 领取后日志插入失败可能留下 changing，已将两处日志错误接入未发送证明清理，并以触发器失败测试重验；复核确认消除原问题。所有证据均来自临时数据库、mock 两端或本地 HTTP 资源，不提升真实 P1。

后续需将已有创建、绑定、激活与输入编排组合到显式管理入口，记录真实 queue / steer 与投递结果，再验收 App / 飞书 UI、清理与部署。配置 Agent 仍不能根据单个组件的 pass 报告安装成功，完整 P1 不由这些 fixture 推导。
