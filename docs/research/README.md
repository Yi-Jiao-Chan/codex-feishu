# 接口与交互调研

记录核验日期、准确版本、官方来源与能力边界。这里的结论是设计依据；技术方案见 [plans](../plans/README.md)，运行与验收证据见 [records](../records/README.md)。旧版本研究不自动代表当前版本兼容。

## Codex 原生协议

| 文档 | 日期 / 版本 |
| --- | --- |
| [Codex 0.162.0 审批与原生问答路由](approval-routing-0.162.0.md) | 0.162.0 |
| [Codex 0.162.0：首装发现基线与草稿激活](discovery-baseline-0.162.0.md) | 0.162.0 |
| [Codex app-server 0.162.0：主会话、fork、子代理与 worktree 分类](thread-classification-0.162.0.md) | 0.162.0 |

## 飞书平台与资源能力

| 文档 | 日期 / 版本 |
| --- | --- |
| [群描述中的会话工作目录（2026-10-13）](chat-working-directory-2026-10-13.md) | 2026-10-13 |
| [飞书附件展示：接口与实现边界](feishu-attachments-2026-10-13.md) | 2026-10-13 |
| [Card JSON 2.0 原值读回与 unknown 确认边界](feishu-card-readback-normalization-2026-10-13.md) | 2026-10-13 |
| [飞书图片、文件与图文混排入站资源核验](feishu-inbound-resources-2026-10-15.md) | 2026-10-15 |
| [飞书卡片承载审批与用户问答](feishu-interactive-requests-2026-10-14.md) | 2026-10-14 |
| [飞书打开服务器本地文件：浏览器链接方案](feishu-local-file-browser-links-options-2026-10-15.md) | 2026-10-15 |
| [飞书自建机器人 M0 最小权限核验](feishu-m0-permissions-2026-10-10.md) | 2026-10-10 |
| [飞书 unknown 请求的只读核验边界](feishu-unknown-readback-2026-10-12.md) | 2026-10-12 |

## 产品与交互参考

| 文档 | 日期 / 版本 |
| --- | --- |
| [飞书与 Codex 双向会话连接调研](codex-feishu-bridge-2026-10-10.md) | 2026-10-10 |
| [飞书 Codex 桥接卡片交互参考](codex-feishu-card-ux-2026-10-10.md) | 2026-10-10 |
| [飞书创建会话：远程目录选择的开源交互研究](feishu-create-directory-ux-research-2026-10-15.md) | 2026-10-15 |
| [飞书原生目录选择卡：平台能力与 UX 边界](feishu-directory-card-platform-ux-2026-10-15.md) | 2026-10-15 |
| [飞书目录选择：交互模式对标与方案判断](feishu-directory-picker-patterns-2026-10-15.md) | 2026-10-15 |
| [执行过程折叠标题与记录间距](feishu-progress-typography-2026-10-13.md) | 2026-10-13 |
| [同一张过程卡与原消息状态 reaction 调研](progress-cards-reactions-2026-10-10.md) | 2026-10-10 |

新增研究沿用 `topic-version.md` 或 `topic-YYYY-MM-DD.md`，标明范围、来源、已核实事实及推断。实施进度和候选部署日志写入 records，不继续追加到本目录。
