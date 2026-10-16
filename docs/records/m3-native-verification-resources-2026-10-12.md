# 受控原生验收的本地资源（2026-10-12）

本切片将独立私有目录和固定本地模拟模型接入上一切片的持久创建步骤，供后续管理验收工作流使用。当前仍是内部开发组件：未接入控制通道或 `verify --allow-test-thread`，不绑定飞书群、不提交输入、不执行 archive，不改变一期发布标志。运行中的 M0 未切换。

## 固定模型与执行时序

`StartLocalModel` 只监听 `127.0.0.1:0`，返回本机 `/v1` 地址；调用者不能选择监听地址、生产模型或凭据。只接受 `/v1/responses` 的 POST、至多 1 MiB 的 JSON object、`model=fixture-model` 和 `stream=true`；正文中的 input 被忽略，不回显、记录或转发。其他路由、方法、格式和超限请求不消耗有效请求序号。

输出沿用已在仓库原生 opt-in 探针使用的五个 Responses SSE 事件，文本固定为 `fixture reply N`，无工具调用。第一条有效请求在首个 `response.created` 实际 Flush 成功后发出 Started 信号，并等待本地 Release；后续有效请求可以直接完成，以控制原生队列 / 引导实验时序。Started 只代表本地服务器 Flush 成功，不证明 Codex 已收到事件或原生 turn 已开始；实际工作流仍须读取原生状态。

请求取消和服务器关闭会终止响应，不能生成用于验收的正常 completed。关闭先停止接收并关闭连接，再等待 handler / serve 退出；启动 context 取消、Close 和 Release 支持并发及重复调用。Flush 通过 `ResponseController.Flush` 读回错误；缓冲 Write 成功而 Flush 失败不会虚假关闭 Started。

## 私有资源与创建恢复

`NativeSessions` 只接收 request ID，不能传入任意模型 URL、原生参数或 prompt。构造器仅验证调用者提供的专用物理私有根目录，不创建目录或启动连接。授权 Begin 才独占创建 `root/ID` 和 `cwd`（0700），先持久保存 `owner.json`（0600，host/app/owner、构建与 ID），再启动模型和调用 `NativeManager`。后者在实际 RPC 前冻结准确请求、目录、模型端点和连接身份。

并发调用共用同一个活跃资源条目，创建仍由持久 CAS 单次领取。模型生命周期限于 5 分钟及服务 lifetime，工作目录不再私有 / 不存在、模型已关闭或到期时不使用旧端点执行原 ready。创建 RPC 在资源锁外执行，State 可以读取在途 unknown；Close 先取消，再等待在途已知 / 未知结果完成有界落盘，之后调用者才能关闭 store。

进程重启后，已有原生日志只读展示原出生历史及 `resource_recovery_required=true`；不重新绑定旧监听端口、不替换模型 URL、不继续原 ready 创建。部分目录即使尚无原生日志，也不覆盖或删除以重新执行。只有同一活跃实例中确定尚未准备原生日志的条目可继续使用其原目录 / 端点进行准备。

Close 只关闭本地资源，保留原生日志和目录所有权记录，不猜测远程创建失败，不 archive 未知或其他线程。公开 DTO 不输出私有路径、端点或请求；`model_requests` 是本实例的本地请求计数，不是持久验收证据。完整绑定 / 输入 / 附着 / 远端清理与真实 UI 仍待后续实现和验收。

## 验证范围

默认测试使用本地 HTTP / Unix fixture、临时私有目录与数据库、内存 native creator。覆盖实际 SSE 顺序、第一响应等待、后续响应并发、非法请求不消费等待、正文上限、取消 / 关闭无正常 completed、Flush 失败无 Started、Release / Close 并发；以及发送前所有权与准确请求已持久保存、并发只创建一次、ready / confirmed / unknown / 部分目录重启不替换、模型到期 / 关闭 / 工作目录缺失、关闭中 RPC 取消仍保存 unknown、查询不创建资源或发请求。

真实共享 daemon 的 opt-in 测试未启用；这证明本地组件，不证明新的真实原生创建入口、App 页面或完整一期部署。CLI / 管理通道能力保持原状态，全部 P1 场景仍须逐项验收。

本切片通过 `go test -race ./internal/verification`、`go test ./...`、`go vet ./...` 和 `git diff --check`。独立只读审阅发现首事件 Flush 错误被忽略，已修正并加入确定性回归测试；资源锁外 RPC、Close 加入在途结果和重启不替换资源未发现新增具体问题。
