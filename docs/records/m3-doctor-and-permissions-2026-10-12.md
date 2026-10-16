# M3：只读诊断与权限清单

日期：2026-10-12。M0 已通过；正式一期服务、配置迁移、配对、完整 verify 与发布矩阵仍未通过。本轮未发送飞书消息、未新开事件监听、未提交 Codex 输入、未部署正式服务。

`doctor --config` 已改为安装前诊断：严格配置与密钥检查，使用配置中的 CLI 查询现有 daemon 实际版本，通过既有同用户 Unix 管理端点查询桥状态，再以 GET 查询机器人身份及 tenant 权限授权。没有管理端点时只读数据库摘要，不打开另一条 native 连接。默认没有运行实例可返回前置条件就绪；启动后的 `--require-running` 将运行实例列为必需，旧 M0 没有管理通道，因此退出 5 / pending。已存在但未恢复的实例，不论是否加标志，都不能报告健康。独立 `--codex-only` 保留短暂 handshake / thread list 诊断，不含会话写入。

健康增加初次发现基线事实：自动建群启用而历史基线未完成时，零绑定与两个已连接 socket 不能让 status / doctor 报告 ready。事实来自当前部署的 `baseline_completed_at`，不借用其他 host 的基线、不写入或迁移状态。现有数据库身份不符、版本不支持或不可读时诊断失败，不能将它当作新用户初始化。

新增 `compat/feishu-permissions.json` 与嵌入 accessor，version JSON 返回整份清单。14 项能力带 API、来源、SDK v3.12.0、OR 替代权限、非 scope 条件和实测方法；新部署推荐的 7 项权限按既有官方研究固化。自身群消息读回保留 optional / documentation_gap，未扩大读取授权。权限满足与真实行为严格分开：scope-free 卡片回调不被误写为已经实际回调；每项 `runtime_observed` 保持 unknown。

授权接口 [官方导出](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/application-v6/scope/list.md) 本轮重新核验：`grant_status` 的 1 表示已授权，2 表示未授权；不存在可依赖的 pending 枚举。固定 SDK 的 `Scope.ScopeType` 说明 tenant / user。只接受明确 granted tenant；缺少身份、未知状态、冲突或不完整响应保留待核验。远端正文和凭据不进入错误输出。

实际只读检查使用现有 M0 配置与当前应用：daemon 0.162.0、机器人身份、授权 GET 及所有必需能力的文档权限检查通过；旧实例管理端点缺失，`--require-running` 正确返回 `diagnostics-pending` / 5。自身群消息读回的文档缺口仍为非必需 pending。检查前后 M0 数据库均为 schema 1、0600，没有迁移；不把此结果当作正式一期验收。

验证覆盖配置错误与泄漏防护、私有密钥、配置 CLI、安装前与运行后状态、管理身份冲突、当前代际断线/恢复、初次基线、unknown 写入、缺权限/授权歧义、tenant/user/拒绝授权过滤、OR 替代、二进制不依赖 cwd、数据库读取及历史 schema。相关包 race 测试和整仓 vet 通过；真实 UI 和完整发布仍待集中验收。
