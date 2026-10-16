# 受控验收群与线程的绑定基础（2026-10-12）

本切片只提供内部登记管理器、专用附着适配器和 schema 21 存储。它将同一授权请求中已经确认的群与线程保存为一个本地绑定；不创建群或线程、不提交输入、不投递消息，也未开放 CLI / control 绑定方法。普通运行时仍排除 validation 线程，完整一期验收继续 pending。

## 证据与登记

`BindingManager` 要求创建组件属于相同 host / app / owner、commit 与运行二进制 SHA-256，原资源仍在窗口内，当前连接的 generation 和随机身份与出生记录一致。群创建须已确认，且原接收器已记录本人在原群的验证消息。登记前重新 GET 机器人在群状态和完整成员名单，缺失 owner / bot、额外成员、读取失败均不登记。

附着只使用上述原连接。资源关闭、模型单独停止会取消进行中的操作，关闭过程等待它结束；网络请求不持有资源互斥锁。附着返回后再次核验连接身份、资源存活与取消状态，再登记。错误正文不作为公开组件结果。

schema 21 在一个事务中核对数据库原出生与群证据，登记原 observation、confirmed create intent、普通 binding 和私有验证标记。确定性意图标识、host / thread 与 host / chat 唯一约束防止覆盖其他路由。验证 observation 保持 excluded，不伪造人类输入；时间单位沿用 observation 的 Unix 秒。绑定期限取两组件原窗口交集，重放保持原时间，派生记录被删除时报告冲突而不修复。标记按 host / thread 可查询，供后续授权判断识别作用域不匹配，避免退回普通授权。

State 只读本地记录、派生绑定完整性与资源事实，不做群 GET 或附着。历史登记仍可保留，但资源已关闭、连接已替换或窗口已过期不证明当前写入权限。私有存储 DTO 不输出目录、模型端点、成员或原始请求。

## 准确版本的附着检查

本机 Codex 0.162.0 生成的 `ThreadResumeParams` 只要求 threadId；适配器准确发送 threadId、excludeTurns 两项，不传配置覆盖。`ThreadResumeResponse` 含根层 model / modelProvider / cwd / approvalPolicy / sandbox，而 `ThreadReadResponse` 只有 thread，不能用后者代替策略证明。

| 本地生成文件 | SHA-256 |
| --- | --- |
| v2/ThreadResumeParams.json | c69d0d08d046e48b2f9e631761e1127c347fac2f8c2776df458559eb3aa4bd83 |
| v2/ThreadResumeResponse.json | 3ea7c6d06bab091cea98431c72b644ac026492217842335fcc2105a472be4aa2 |
| v2/ThreadReadResponse.json | d8b6934897c959f589fdf33be4f750a81f46f075df77bedcaaa92e15ad80a2b1 |

`AttachLocalVerification` 核验原 thread ID、两层 cwd、固定 fixture-model 与 provider、显式非 ephemeral、验收标签、无父代理 / fork，以及根层 never、legacy readOnly / networkAccess=false。该版本 schema 将省略的 readOnly.networkAccess 默认为 false；显式 null / true 或错误类型则拒绝。出生要求空 preview / turns；附着允许运行后的历史。activePermissionProfile 是来源信息，不作为这里已验证的权限字段。真实 Unix WebSocket fixture 验证准确请求和畸形响应；本切片没有调用真实 daemon 附着或验证 App UI。

## 验证与后续接入

隔离测试覆盖并发及不可变重放、schema 20→21 保留旧日志、正常路由冲突回滚、时间与作用域边界、缺失派生记录、JSON 私密性；管理器覆盖新成员名单、旧连接、未知出生、构建 / 窗口变化、附着拒绝与身份变化、资源关闭和取消，以及 State 无网络行为。

整仓 `go test -race ./...`、`go vet ./...` 与 `git diff --check` 通过。独立审阅发现模型在附着期间单独停止可能仍登记，已增加模型 Done 取消与登记前资源复查；随后重跑受影响客户端 / 管理器的 race 测试，通过合法返回和等待取消两种停止场景，均无派生行落盘。按准确 schema 保留合法缺省 false，同时新增拒绝 null 的检查和测试。本轮全部为隔离 fixture，未迁移实际 M0 数据库或切换服务。

后续需要接入独立显式授权、runtime actor 激活、首条输入前的空历史处理，并在接收器、每个原生写操作和全部消息投递处检查专属绑定授权、当前资源 / 连接 / 群成员。只在这些路径完成后开放绑定入口并验证 queue / steer；还需真实 App / 飞书验收、远端清理与正式部署。不能把本地登记的 pass 提升为任何 P1 用例通过。
