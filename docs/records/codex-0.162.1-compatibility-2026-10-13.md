# Codex 0.162.1 候选部署兼容性

核验日期：2026-10-13。支持范围是本桥使用的原生协议和隔离测试行为，不提升 P1 发布或新卡片的 App / 飞书 UI 验收状态。

## Schema 核验

使用本机 `codex-cli 0.162.1` 执行 `codex app-server generate-json-schema --experimental`，未启动新的 App Server。输出 447 份 JSON，与之前核验的 0.162.0 corpus 逐文件 SHA-256 比较：新增、删除、改变均为 0。

按既有 corpus 算法计算的摘要保持：

```text
4248c44d68df2d0d55be53b15fffbb24d8c8cf41cb9993a1f79d5d03d3d2b5da
```

因此 `codex_protocol` 和 `compat.ProtocolSchemaDigest` 继续表示 0.162.0 schema 基线，不改写历史证据。新增 `codex_protocol_compatible_versions` 独立报告经过验证的运行版本。

## 真实隔离行为核验

复用本机已经运行的共享 0.162.1 daemon，仅新建本工具拥有的隔离线程、独立 SQLite 和固定 loopback 模型。没有启动、重启、升级 daemon，没有改变既有 App 会话、全局配置或飞书资源。

`CODEX_FEISHU_REAL_NATIVE_EXPECTED_VERSION=0.162.1` 只用于 opt-in 测试 helper；默认期望仍是 0.162.0，仅允许这两个精确值。入口、出生元数据、子进程证明与清理均核对同一 CLI / AppServer 版本及 socket / 线程所有权，不把未知结果自动重试成成功。

真实共享 daemon 下 `-race` 测试通过：

| 病例 | 结果 |
| --- | --- |
| Q2 / Q3 混合 App / 飞书队列顺序、中间项引导 | pass |
| Q4 其他客户端先移除队列项 | pass |
| Q5 原轮结束后不把引导重定向到新轮 | pass |
| Q7 add 已接收但返回丢失，busy / idle | pass |
| Q8 返回丢失及真实子进程退出边界 | pass |
| Q9 重复按钮、steer / remove 竞争 | pass |
| Q10 continue 与其他客户端 start 竞争 | pass |
| cold creator attach 诊断 | pass；具体出生状态结论以私有原始证据为准，不冒充 UI 通过 |

原始脱敏记录与 schema 摘要保存于部署私有 evidence 目录，权限 0600，不提交线程标识、真实正文或凭据到 Git。

## 运行门槛

`compat.VerifiedCodexVersion` 精确允许 `0.162.0` 和 `0.162.1`，serve 每次连接、doctor、M0、setup plan / migration 使用同一门槛。0.162.2、0.163.0、空值和未知版本仍拒绝，不使用 patch 通配规则。

本轮本地测试与 schema 相同均不替代新的卡片客户端验收；`phase1_released` 和完整 UI 状态保持 pending。候选安装继续使用 `--candidate-test`，不把这次部署标成正式一期发布。
