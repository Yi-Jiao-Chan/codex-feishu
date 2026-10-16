# 2026-10-10 原生协议探针

适用历史 Codex 0.162.0 实验。下列结果按原内容保存，不据此推导当前版本或 App / 飞书 UI 已通过。背景见 [M0 联调记录](../../m0-live-integration-2026-10-10.md)。

| 材料 | 用途 |
| --- | --- |
| [队列探针](native-queue-probe.py) | 会话、队列及输入相关协议实验 |
| [并发探针](native-race-probe.py) | 队列与并发边界实验 |
| [队列结果](native-queue-probe-result.json) | 当时的主链路观察 |
| [并发结果](native-race-probe-result.json) | 当时的竞争与恢复观察 |
| [空闲输入结果](native-idle-queue-probe-result.json) | 空闲 queue/add 行为观察 |
| [App 线程元数据](app-thread-metadata-probe.json) | 当时线程分类的脱敏样本 |

脚本可能创建原生资源或提交输入；先阅读[探针执行边界](../README.md)，不直接把历史脚本当作当前验收命令。
