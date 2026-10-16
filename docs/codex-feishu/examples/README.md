# 配置与消息样例

这里是合成样例及模板，不是运行配置或真实验收报告。配置步骤见 [AGENT-SETUP](../AGENT-SETUP.md)，字段和命令约束见 [CLI 手册](../10-cli-and-setup.md)。

## 配置与交付

| 文件 | 用途 |
| --- | --- |
| [config.example.toml](config.example.toml) | 正式配置结构，只引用私有 secret 文件 |
| [setup-input.example.json](setup-input.example.json) | 向导输入结构，与正式 TOML 不同 |
| [codex-feishu.service.template](codex-feishu.service.template) | systemd 用户服务模板 |
| [doctor-output.example.json](doctor-output.example.json) | 诊断输出格式，不代表实际健康状态 |
| [acceptance-report.template.json](acceptance-report.template.json) | 需填写并核验证据的一期验收报告模板 |

## 卡片与原生请求

| 文件 | 用途 |
| --- | --- |
| [queue-card.example.json](queue-card.example.json) | 队列操作卡格式 |
| [interaction-command.example.json](interaction-command.example.json) | 命令审批 |
| [interaction-file.example.json](interaction-file.example.json) | 文件审批 |
| [interaction-network.example.json](interaction-network.example.json) | 网络审批 |
| [interaction-permissions.example.json](interaction-permissions.example.json) | 额外权限请求 |
| [interaction-question.example.json](interaction-question.example.json) | 同步问答 |
| [interaction-async-question.example.json](interaction-async-question.example.json) | 异步问答 |

配置、向导输入、服务模板与验收报告模板中的交付文件由打包器白名单逐项引用。新增样例不会自动进入开发包；修改路径或交付范围时同步更新打包器及其测试。
