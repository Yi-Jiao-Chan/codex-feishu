# 贡献与代码提交指南

本文供开发者和代码 Agent 使用，约定从开始修改到提交 PR 的工作流程。项目能力见 [README](README.md)，Agent 约束见 [AGENTS.md](AGENTS.md)，模块与文档入口见 [docs](docs/README.md)。

## 环境与工程入口

- 使用 [go.mod](go.mod) 声明的 Go 版本，当前为 Go 1.25.6；工作树功能还需要 Git。
- 构建入口为 `cmd/codex-feishu`，业务代码位于 `internal`，协议与权限清单位于 `compat`。
- 常规单元测试使用 fixture；真实 Codex / 飞书联调另行授权并使用独立资源。不要为了测试启动替代 daemon 或改动既有会话策略。
- Go 命令沿用现有构建和模块缓存，不设置临时 `GOCACHE` / `GOMODCACHE`，不运行 cache clean。
- 文档链接检查使用 Python 3 标准库，无额外依赖。

## 开始一个改动

先检查工作区，保留已有修改。新任务从最新 `origin/main` 创建 `codex/` 前缀的主题分支；不要将无关任务带入同一次提交。

```sh
git status --short --branch
git fetch origin main
git switch -c codex/queue-recovery origin/main
```

`queue-recovery` 是示例，按本次改动命名。已有主题分支则继续使用，不重新创建。已有未提交修改时，先确认归属再切换分支，不用 reset、clean 或自动 stash 掩盖它们。

读取源代码前先读 [公共契约](docs/codex-feishu/00-contracts-and-layout.md) 和对应模块。实现以当前接口和需求为限，不顺带重构、更新依赖或调整生产配置。

## 修改代码与文档

| 改动 | 同步更新 |
| --- | --- |
| 用户行为、配置项、CLI | 对应模块手册；影响使用入口时更新根 README / 安装指南 |
| Codex / 飞书协议或权限 | 客户端或网关手册、compat 清单、合成 fixture 与核验证据 |
| 数据库结构、收据、恢复 | 存储手册、增量迁移、旧数据保留和 unknown 恢复用例 |
| 卡片与交互 | 卡片手册；真实客户端与平台 API 证据分别记录 |
| 新技术方案或调研 | plans / research 的日期文档及分类索引 |
| 实施或验收结果 | records；当前验收状态同时更新集中验收清单 |
| 文档重命名或移动 | 所有入站和出站链接、分类索引、引用它的代码或打包清单 |

完整存放和命名规则见 [文档维护规则](docs/README.md#维护规则)。安装包固定引用的 `docs/codex-feishu/AGENT-SETUP.md` 与 `examples/` 是交付路径，移动前必须同时调整打包器及其测试。

样例和测试使用合成数据。App Secret、口令、token、真实消息正文、数据库、日志与私有探针输出留在仓库外；`.gitignore` 不能代替暂存前检查。

## 验证要求

按改动范围验证，在 PR 中写明实际命令与结果，明确区分未运行、失败和通过。已有检查通过且代码未再变化时复用结果；文档修改不要求重复全部 Go 测试。

| 范围 | 最低检查 |
| --- | --- |
| 仅 Markdown、目录、导航 | `python3 scripts/check_docs.py`、`git diff --check` |
| Go 代码 | 对改动文件运行 gofmt，运行相关包测试 |
| 跨模块、协议、迁移或恢复 | 相关回归及全仓业务测试；并发路径补充对应包的 `-race` |
| 安装包路径或模板 | 文档检查及 `go test ./internal/devpackage` |
| 真实交互或发布声明 | 按验收矩阵逐项获取对应证据，自动测试不替代 UI |

存储包的普通业务 fixture 复用进程内已迁移、已关闭的空库快照，每个用例复制到自己的 `t.TempDir`，再通过正常 `Open` 配置连接。首次空库初始化、旧 schema 升级和重启恢复用例中的待测 `Open` 仍须真实执行；快照只用于准备初始状态，不能覆盖待升级文件或替代迁移断言。调整 fixture 时同时核验副本隔离、连接设置和迁移回归。

```sh
# 仅格式化本次改动的 Go 文件，以下路径是示例。
gofmt -w internal/submissions/controller.go internal/submissions/controller_test.go

# 仓库代码测试；不包含本机 dist 下的临时工具。
go test ./cmd/... ./internal/... ./compat

# 仅对受影响的并发模块执行；以下以输入控制器为例。
go test -race ./internal/submissions

python3 scripts/check_docs.py
git diff --check
```

普通测试不启用 opt-in 环境变量，不运行历史探针脚本。[原生探针](docs/records/probes/README.md)可能创建线程、输入或工作树；必须先核对原日志、资源归属和授权，不得因超时重新执行 unknown 操作。真实服务与发布验收使用 [集中清单](docs/codex-feishu/12-live-acceptance-handoff.md)。

## 提交与 PR

### 1. 整理提交

一个 commit 表达一个可解释的改动，相关测试和文档一起提交。只暂存明确属于本任务的文件，再检查暂存 diff；不要用 `git add .` 把本机产物或其他人的修改一并带入。

```sh
git status --short
git diff --stat
git add internal/submissions/controller.go internal/submissions/controller_test.go
git diff --cached --check
git diff --cached
git commit -m "fix(submissions): reconcile queued inputs after reconnect"
```

提交标题使用 `type(scope): 简要说明`，scope 可省略；中文或英文均可，描述实际结果。

| type | 用途 |
| --- | --- |
| feat | 新能力或行为 |
| fix | 修复已有问题 |
| docs | 文档、导航、说明 |
| test | 单独补充测试或 fixture |
| refactor | 保持行为的结构调整 |
| build / chore | 构建、依赖或工程维护 |

### 2. 同步并推送

推送前重新获取目标分支，检查差异。若 main 已有新提交，合并并处理冲突，再验证受影响部分；不要覆盖他人的远端提交。

```sh
git fetch origin main
git log --oneline --left-right HEAD...origin/main
git merge origin/main
git push -u origin HEAD
```

发布前检查当前分支是否已有 PR：未合并的继续更新；已关闭或合并的改动以新的主题分支和 PR 继续。避免向 main 直接推送，常规更新不使用强制推送。

### 3. 创建与审核 PR

目标分支为 main，使用仓库的 [PR 模板](.github/PULL_REQUEST_TEMPLATE.md)。说明问题与最终行为、实际验证、迁移或兼容影响，以及仍待确认的项目；不要把对话过程或本机运行日志贴进 PR。

```sh
gh pr create --base main
gh pr checks --watch
```

有 CI 时等待所需检查完成；没有配置检查时，明确记录本地验证，不能把“无检查”写成“CI 通过”。审核时核对提交身份、范围、真实验收边界和未解决问题。

### 4. 合并与交付

仅在此次任务已授权合并、且审核和必要检查满足时执行；授权推送不自动等于授权合并或部署。合并时绑定已审核的源提交，保留 main 上其他人的改动。

```sh
gh pr merge --merge --match-head-commit "$(git rev-parse HEAD)"
git fetch origin main
```

完成后报告 PR 链接、合并提交、验证结果和本地工作区状态。合入 main 与创建发布、部署服务、修改全局配置是不同动作；发布仍遵守 [集成与验收门槛](docs/codex-feishu/11-integration-and-acceptance.md)。
