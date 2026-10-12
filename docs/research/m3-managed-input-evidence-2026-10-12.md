# 受控绑定的只读输入证据

原管理绑定查询此前只能证明登记和本地 actor 存在，不能说明 owner 的群输入是否已走默认排队或是否进入原生历史。本变更直接读取正式服务已有的收据、输入 attempt 和历史关联，不另造执行队列或测试输入入口。

## 查询及证明范围

`verify --config PATH --target self --request-id ID --json` 复用原 `verify.binding.state`。管理器在一个只读事务内重新比较完整私有登记，校验部署 app、原路由 / intent / owner / chat，随后只统计原 host / app / thread / chat 下已有 submission_inputs 的匹配记录，不读取正文、令牌或完整请求。没有新迁移或远端查询。

可选 `input_evidence_status=pass` 是快照读成功，`input_evidence` 包含：

| 字段 | 精确含义 |
| --- | --- |
| staged_inputs | 已正式登记的输入，不包含尚未 stage 的入站消息 |
| add_confirmed | 原无 action 的默认 add 已 confirmed，并有远程 queue ID 或完整相同 user history 关联 |
| queued_snapshot | 保存状态为 queued，queue ID 非空、原生序号正数；不证明当前队列或曾处于忙时 |
| history_linked | 原生 user item 与输入及收据的 turn / item 完整一致 |
| steer_confirmed | 同一显式 steer 动作，删除 attempt confirmed / deleted=true，steer attempt confirmed，receipt / client / queue / 原目标轮全部一致 |
| steer_history_linked | 上一项输入还在捕获的目标轮真实历史中 |
| unresolved_attempts | 原输入的 dispatching / unknown attempt，不从消失或等待时间推导成功 |

计数只表示已有的局部事实；例如 steer 响应确认与用户 item 落入目标轮分别报告。旧 provider 缺字段或查询失败保持 pending / 无快照，不伪造全零成功。控制客户端拒绝非法状态、负数、超出登记输入数及相互矛盾的计数。其他 build 的计数可保留为历史记录，但 CLI 不将其组件检查标为当前 build pass。

## 验证

隔离 Store 的真实 API 建立默认提交、队列观察、用户历史及引导阶段，反例验证未决 / 缺删除 / 错目标轮 / 外部 scope 不提升成功计数。管理器测试覆盖状态查询无写、无成员 GET / 原生附着、正文和 token 不出 DTO、数据库读失败不修复 schema。

实际 Service.Run、原唯一接收器、线程 actor 和本地管理 socket 的测试将同一 owner 消息重放两次，只生成一次 add；查询先看到已确认默认提交和队列快照，再看到匹配 user item，未出现无依据的 steer 计数。CLI 和控制协议测试保留 12 项 P1 pending。

验证通过：`go test -race ./...`（app 60.107s、store 119.964s、verification 45.593s）、`go vet ./...` 和 `git diff --check`。Store 的独立输入证据 race 测试已先通过 17.367s，管理器 / CLI / 控制协议及 Service.Run 的聚焦测试也通过。

所有原生 / 飞书网关均为测试替身；本轮没有真实远端发送、输入、部署或 M0 修改。局部历史关联不能代替 App 页面、忙时顺序、无重复执行、最终卡送达或一期完整验收，完整 `verify_managed_tests` 和发布状态保持 false。
