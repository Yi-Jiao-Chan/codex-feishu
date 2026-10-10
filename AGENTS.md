# 开发约束

先读 docs/codex-feishu/README.md 和当前模块手册。

- 仓库代码读取前检查 CodeGraph 就绪状态；若存在索引，先查询 CodeGraph。不得主动索引、删锁或杀索引进程。缺少可用索引可用 rg / 直接读取。
- 保留其他开发者修改。Go 命令继承 GOCACHE / GOMODCACHE，不覆盖或清理。
- M0 真实 App 与飞书联调通过后才继续 M1–M3；协议模拟不代替 UI 验收。
- 复用现有 daemon 和原线程；不新起 app-server / exec resume，不修改全局 Codex 配置或原会话策略。
- 普通输入原生 queue/add；只有用户点追加引导才 steer。clientUserMessageId 不是原生去重键。
- 远程状态变更在发出前持久记录；未知响应不盲重试。
- 凭据和真实正文不得进 Git、普通日志或诊断输出。
- 发布必须逐项核验一期矩阵，pending 不得冒充 pass。
