# 正式 Service 的受控消息闭环

此前管理绑定整合测试验证了同一服务上的创建、绑定、owner 输入、默认 add 和历史关联。本轮沿同一 receiver / actor 继续验证消息投递与 reaction，复用正式 Engine、Outbound、Feedback 和持久记录，不用独立 M0 实现代替它们。

## 通过的本地行为

- 同一 owner 输入重放两次，只生成一个收据和一次默认 add；执行后关联原 user item。
- 两条不同的公开进度更新同一原生轮次的过程卡；结束时更新该卡为终态。队列操作卡在独立 `ui:queue:` 命名空间，不误计为该轮过程卡。
- 最终回答单独创建一张确认的结果卡，过程 / 结果 / 队列卡都引用原 owner message；没有退为无父消息的普通发送。
- 人为保持最终 reply 未返回时，原消息没有 CheckMark；放行发送确认后才出现 CheckMark，且持久最终卡已经确认当前 revision。所有 reaction 都指向原 owner message。
- 飞书人类正文未被机器人重复镜像，reasoning 与完整工具输出未公开；只有公开进度与最终回答进入卡片。
- 原 native 连接、飞书接收器、测试线程和群没有重建，输入没有重发。

## 验证范围

测试使用隔离 SQLite、实际 Service.Run 和本地管理 socket。Codex 与飞书 API 使用可控测试替身；已建立的 loopback 模型仍属于原请求。没有真实飞书消息、原生输入、部署或 M0 改动。

聚焦 race 测试 `TestManagedBindingActivatesOnExistingServiceAndStateIsReadOnly` 通过，11.022s。它提供正式服务组合行为的证据，不能证明真实 App 页面、飞书客户端布局、原生执行或完整一期发布。UI 和 12 项 P1 的验收状态保持 pending。

完整 `go test -race ./internal/app` 通过，59.439s；`go vet ./internal/app` 与 `git diff --check` 通过。本轮仅修改该包的测试和文档，复用前一提交已通过的全仓库 race / vet 结果，不重复未受影响的检查。
