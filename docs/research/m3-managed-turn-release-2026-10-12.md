# 受控忙时测试的本轮释放

受控本机模型的第一条响应停在 `response.created` 之后，以便在同一忙时轮次验证原生排队和追加引导。已有绑定管理入口可以启用 owner 群跟进，但缺少结束这条模型响应的管理入口，无法继续观察最终投递。本变更补齐该入口。

## 行为

`verify --config PATH --target self --release-test-turn --request-id ID --json` 是独立显式操作，不能与 card / group / thread / binding 授权标志组合。它只开放原请求的本机模型回复门。要求已有 confirmed 组件与原登记，第一条模型请求已开始，原构建 / 原连接 / 私有资源 / 五分钟窗口有效，并通过当前 owner-and-bot 成员检查与无覆盖原生附着检查。复用原服务、原管理 socket 和资源操作上下文，不新建组件、不激活 actor、不提交输入、不引导或归档。

重复释放是同一 `sync.Once` 门；不增加模型请求，不续期。查询不开放门、不执行成员 GET / 原生附着，也不重建资源。释放之前没有模型请求时拒绝，不自行启动输入以满足前置条件。

`BindingResult.model_released` 和原线程资源查询只报告本机门的当前状态，资源关闭后不补造历史完成证据。管理方法为 `verify.binding.release`，只接收 request ID；客户端拒绝混杂响应和不成立的原绑定 / 资源事实。旧 provider 不支持时返回 unsupported，不退回 begin / state。运行实例能力为 `verification_turn_release_supported`，版本能力为 `verify_managed_turn_release`。

## 证据边界与验证

本轮只使用隔离数据库、假 native / Feishu 网关、实际本地 Unix socket 和 loopback HTTP 模型，未发送真实飞书消息或原生输入，未变更 M0。覆盖：原模型响应真正保持等待并在授权释放后返回完成 SSE；未登记 / 未开始拒绝；原群成员、连接、构建、窗口、附着及路由变化拒绝；查询无副作用；重复释放；控制协议 payload / response 隔离；CLI 缺 capability / ID、组合标志及失败不降级。

正式 Service.Run 整合测试覆盖现有唯一接收器和原连接上的创建、绑定、actor 激活，以及通过管理 socket 释放同一模型响应；创建 / 输入计数保持不变。它没有证明真实 App 页面或飞书投递。

验证通过：`go test -race ./...`（app 47.628s、verification 29.578s）、`go vet ./...` 和 `git diff --check`。相关聚焦测试也已通过：BindingRelease、VerifyRelease、管理 socket 与 Service.Run 的原模型释放路径。

`verification-managed-turn-released` 仍 `ok=false` / 退出 5，12 项 P1 及 UI 保持 pending。下一步需以实际 native 历史、队列 / 引导映射、最终投递和用户 UI 观察补齐验收；本机释放不等于原生轮次完成或完整一期交付。
