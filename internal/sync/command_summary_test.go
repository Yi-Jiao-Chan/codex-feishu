package sync

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
)

func TestCommandPurposesDescribeCommonOperationsWithoutArguments(t *testing.T) {
	for _, tc := range []struct{ command, want string }{
		{"git status --short --branch", "查看 Git 状态"},
		{"git -C /private/project diff --stat", "查看 Git 差异"},
		{"git log -5 --format=PRIVATE_FORMAT", "查看 Git 历史"},
		{"git show PRIVATE_REF", "查看 Git 提交"},
		{"git branch -D PRIVATE_BRANCH", "管理 Git 分支"},
		{"git switch PRIVATE_BRANCH", "切换 Git 分支"},
		{"git checkout -- PRIVATE_PATH", "切换或恢复 Git 内容"},
		{"git fetch PRIVATE_REMOTE", "获取 Git 更新"},
		{"git pull --ff-only", "拉取 Git 更新"},
		{"git push PRIVATE_REMOTE PRIVATE_BRANCH", "推送 Git 更新"},
		{"git add PRIVATE_PATH", "暂存 Git 变更"},
		{"git commit -m PRIVATE_MESSAGE", "创建 Git 提交"},
		{"go -C /private/project test ./... -count=1", "运行 Go 测试"},
		{"go build -o /PRIVATE_OUTPUT ./cmd", "构建 Go 程序"},
		{"go vet ./...", "检查 Go 代码"},
		{"go fmt ./...", "格式化 Go 代码"},
		{"gofmt -w /PRIVATE_PATH/file.go", "格式化 Go 代码"},
		{"rg 'PRIVATE_QUERY' /PRIVATE_DIRECTORY", "搜索内容"},
		{"rg --files /PRIVATE_DIRECTORY", "查找文件"},
		{"rg -- '--files'", "搜索内容"},
		{"grep PRIVATE_QUERY PRIVATE_FILE", "搜索内容"},
		{"find /PRIVATE_DIRECTORY -name PRIVATE_PATTERN", "查找文件"},
		{"ls /PRIVATE_DIRECTORY", "列出目录"},
		{"cat /PRIVATE_DIRECTORY/file.go", "读取文件"},
		{"sed -n '1,10p' /PRIVATE_DIRECTORY/file.go", "读取文件"},
		{"sed -i 's/PRIVATE_BEFORE/PRIVATE_AFTER/' /PRIVATE_DIRECTORY/file.go", "编辑文件"},
		{"sed --in-place=.bak 's/PRIVATE_BEFORE/PRIVATE_AFTER/' file.go", "编辑文件"},
		{"sed -e '-i' file.go", "读取文件"},
		{"sed -- '-i' file.go", "读取文件"},
		{"gh pr checks PRIVATE_ID --json PRIVATE_FIELDS", "检查 GitHub PR"},
		{"gh pr merge PRIVATE_ID --match-head-commit PRIVATE_SHA", "合并 GitHub PR"},
		{"bytedcli codebase mr get PRIVATE_ID", "查看 Codebase MR"},
		{"bytedcli codebase mr UNKNOWN_PRIVATE_OPERATION --token PRIVATE_TOKEN", "运行 Codebase MR 操作"},
		{"python3 /PRIVATE_DIRECTORY/inspect_fixture.py --secret PRIVATE_TOKEN", "运行 Python 脚本 inspect_fixture.py"},
		{"python3 -c 'print(\"git push\")'", "运行 Python 内联脚本"},
		{"node /PRIVATE_DIRECTORY/inspect_fixture.js PRIVATE_TOKEN", "运行 Node 脚本 inspect_fixture.js"},
		{"node -e 'console.log(\"git push\")'", "运行 Node 内联脚本"},
		{"bash /PRIVATE_DIRECTORY/deploy_fixture.sh --token PRIVATE_TOKEN", "运行 Shell 脚本 deploy_fixture.sh"},
		{"./deploy_fixture.sh PRIVATE_TOKEN", "运行脚本 deploy_fixture.sh"},
		{"codex --version", "运行 codex（用途未识别）"},
		{"PRIVATE_UNRECOGNIZED_PROGRAM PRIVATE_TOKEN", unidentifiedCommand},
	} {
		t.Run(tc.command, func(t *testing.T) {
			tool, ok := ProgressToolSummary(codexclient.Item{ID: "tool", Type: "commandExecution", Status: "completed", Command: tc.command})
			if !ok || tool.Title != tc.want || tool.ItemID != "tool" || tool.Status != "completed" {
				t.Fatalf("title=%q, want %q", tool.Title, tc.want)
			}
			encoded, _ := json.Marshal(tool)
			if strings.Contains(string(encoded), "PRIVATE_") || strings.Contains(tool.Title, "执行命令") {
				t.Fatal("raw shell arguments reached the public projection")
			}
		})
	}
}

func TestCommandPurposeShellSubsetHandlesWrappersAndKeepsQuotedTextOpaque(t *testing.T) {
	for _, tc := range []struct{ command, want string }{
		{`/bin/zsh -lc 'git status --short && go test ./internal/sync'`, "查看 Git 状态；运行 Go 测试"},
		{`sh -c 'env FIXTURE_TOKEN=PRIVATE_TOKEN git -C /PRIVATE_DIRECTORY status --short'`, "查看 Git 状态"},
		{`FIXTURE_TOKEN=PRIVATE_TOKEN git status --porcelain`, "查看 Git 状态"},
		{`env -i -u PRIVATE_NAME FIXTURE_TOKEN='git push' go test ./...`, "运行 Go 测试"},
		{`cd '/PRIVATE_DIRECTORY/git push' && git status; go test ./... | grep PRIVATE_QUERY`, "查看 Git 状态；运行 Go 测试；搜索内容"},
		{`git status && git status && git diff && go test ./... && git push`, "查看 Git 状态；查看 Git 差异；运行 Go 测试 等"},
		{`echo 'git push; go test ./...'`, "输出文本"},
		{`printf '%s\n' "git push && go test"`, "输出文本"},
		{`echo foo\;git push`, "输出文本"},
		{`'git push'`, unidentifiedCommand},
		{"python3 - <<'PY'\nprint('git push')\nPY\ngit push", "运行 Python 内联脚本"},
		{"node <<'JS'\nconsole.log('git push')\nJS", "运行 Node 内联脚本"},
		{"cat <<'TXT'\ngit push\nTXT", "读取内联文本"},
		{`echo "$(git push)"`, "运行 echo（用途未识别）"},
		{"echo `git push`", "运行 echo（用途未识别）"},
		{`$PRIVATE_PROGRAM git push`, unidentifiedCommand},
		{`sh -c "$PRIVATE_SCRIPT"`, "运行 Shell 内联脚本"},
		{`sh -c "git\ status"`, unidentifiedCommand},
		{`if true; then git push; fi`, unidentifiedCommand},
		{`echo git # push; go test`, "输出文本"},
		{`git status > /PRIVATE_OUTPUT`, "运行 git（用途未识别）"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			tool, _ := ProgressToolSummary(codexclient.Item{Type: "commandExecution", Command: tc.command})
			if tool.Title != tc.want {
				t.Fatalf("title=%q, want %q", tool.Title, tc.want)
			}
			encoded, _ := json.Marshal(tool)
			if strings.Contains(string(encoded), "PRIVATE_") {
				t.Fatal("wrapper, script or substitution leaked private text")
			}
		})
	}
}

func TestCommandSummaryInterpreterOptionsKeepPrivateArgumentsOutOfPublicProjections(t *testing.T) {
	for _, tc := range []struct{ command, want string }{
		{`node --title PRIVATE_TOKEN fixture.js`, "运行 Node 脚本"},
		{`python3 -X PRIVATE_TOKEN fixture.py`, "运行 Python 脚本"},
		{`bash -O PRIVATE_TOKEN fixture.sh`, "运行 Shell 脚本"},
		{`nodejs -r PRIVATE_TOKEN fixture.js`, "运行 Node 脚本"},
		{`node --require PRIVATE_TOKEN fixture.js`, "运行 Node 脚本"},
		{`python -W PRIVATE_TOKEN fixture.py`, "运行 Python 脚本"},
		{`sh -o PRIVATE_TOKEN fixture.sh`, "运行 Shell 脚本"},
		{`zsh +o PRIVATE_TOKEN fixture.sh`, "运行 Shell 脚本"},
		{`node --title=PRIVATE_TOKEN fixture.js`, "运行 Node 脚本"},
		{`node -rPRIVATE_TOKEN fixture.js`, "运行 Node 脚本"},
		{`python3 -X=PRIVATE_TOKEN fixture.py`, "运行 Python 脚本"},
		{`python3 -XPRIVATE_TOKEN fixture.py`, "运行 Python 脚本"},
		{`bash --rcfile=PRIVATE_TOKEN fixture.sh`, "运行 Shell 脚本"},
		{`bash -OPRIVATE_TOKEN fixture.sh`, "运行 Shell 脚本"},
		{`node --no-warnings --title PRIVATE_TOKEN fixture.js`, "运行 Node 脚本"},
		{`python3 -u -B -X PRIVATE_TOKEN fixture.py`, "运行 Python 脚本"},
		{`bash -l -e -O PRIVATE_TOKEN fixture.sh`, "运行 Shell 脚本"},
		{`python3 -uB PRIVATE_TOKEN fixture.py`, "运行 Python 脚本"},
		{`bash -euO PRIVATE_TOKEN fixture.sh`, "运行 Shell 脚本"},
		{`node --unknown PRIVATE_TOKEN fixture.js`, "运行 Node 脚本"},
		{`python3 --unknown PRIVATE_TOKEN -- fixture.py`, "运行 Python 脚本"},
		{`bash --unknown PRIVATE_TOKEN -c 'git status'`, "运行 Shell 脚本"},
		{`node -u PRIVATE_TOKEN fixture.js`, "运行 Node 脚本"},
		{`python3 --no-warnings PRIVATE_TOKEN fixture.py`, "运行 Python 脚本"},
		{`node /private/fixture.js PRIVATE_TOKEN`, "运行 Node 脚本 fixture.js"},
		{`python3 /private/fixture.py PRIVATE_TOKEN`, "运行 Python 脚本 fixture.py"},
		{`bash /private/fixture.sh PRIVATE_TOKEN`, "运行 Shell 脚本 fixture.sh"},
		{`node --no-warnings --trace-warnings fixture.js PRIVATE_TOKEN`, "运行 Node 脚本 fixture.js"},
		{`python3 -u -B fixture.py PRIVATE_TOKEN`, "运行 Python 脚本 fixture.py"},
		{`bash -l -e -u fixture.sh PRIVATE_TOKEN`, "运行 Shell 脚本 fixture.sh"},
		{`node -- fixture.js --title PRIVATE_TOKEN`, "运行 Node 脚本 fixture.js"},
		{`python3 -u -- fixture.py -X PRIVATE_TOKEN`, "运行 Python 脚本 fixture.py"},
		{`bash -l -- fixture.sh -O PRIVATE_TOKEN`, "运行 Shell 脚本 fixture.sh"},
		{`node -- -e PRIVATE_TOKEN`, "运行 Node 脚本 -e"},
		{`python3 -- -m PRIVATE_TOKEN`, "运行 Python 脚本 -m"},
		{`bash -- -c PRIVATE_TOKEN`, "运行 Shell 脚本 -c"},
		{`python3 -c PRIVATE_TOKEN`, "运行 Python 内联脚本"},
		{`python3 -m PRIVATE_TOKEN`, "运行 Python 模块"},
		{`node -e PRIVATE_TOKEN`, "运行 Node 内联脚本"},
		{`node --eval PRIVATE_TOKEN`, "运行 Node 内联脚本"},
		{`node --print PRIVATE_TOKEN`, "运行 Node 内联脚本"},
		{`bash -lc 'git status --short' PRIVATE_TOKEN`, "查看 Git 状态"},
		{`sh -l -c 'git status --short' PRIVATE_TOKEN`, "查看 Git 状态"},
		{`zsh -cl 'git status --short' PRIVATE_TOKEN`, "查看 Git 状态"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			item := codexclient.Item{ID: "tool", Type: "commandExecution", Status: "inProgress", Command: tc.command}
			summary, ok := ProgressToolSummary(item)
			if !ok || summary.Title != tc.want {
				t.Fatalf("title=%q, want %q", summary.Title, tc.want)
			}
			encoded, err := json.Marshal(summary)
			if err != nil {
				t.Fatal(err)
			}
			turn := codexclient.Turn{ID: "turn", Status: "inProgress", ItemsView: "full", Items: []codexclient.Item{item}}
			raw, err := CotDocument("thread", turn, nil, 200000)
			doc := parseProjection(t, raw, err)
			if len(doc.Items) != 1 || doc.Items[0].Title != tc.want {
				t.Fatal("COT changed the safe interpreter summary")
			}
			card := cards.Task(cards.TaskView{Status: "inProgress", Progress: progressView(turn, 200000)})
			if !strings.Contains(card, tc.want) {
				t.Fatal("task card omitted the safe interpreter summary")
			}
			for _, public := range []string{string(encoded), raw, card} {
				if strings.Contains(public, "PRIVATE_TOKEN") {
					t.Fatal("interpreter option value reached a public projection")
				}
			}
		})
	}
}

func TestCommandSummaryCombinesNativeFilesWithGitAndTestsAndSharesProjection(t *testing.T) {
	item := codexclient.Item{ID: "one-native-call", Type: "commandExecution", Status: "failed", Command: "cat /private/SKILL.md && git status && go test ./...", CommandActions: []codexclient.CommandAction{{Type: "read", Path: "/private/SKILL.md"}, {Type: "read", Path: "/private/AGENTS.md"}, {Type: "unknown"}}}
	summary, ok := ProgressToolSummary(item)
	if !ok || summary.Title != "读取 SKILL.md 等；查看 Git 状态；运行 Go 测试" || summary.Status != "failed" {
		t.Fatalf("file metadata hid other actual purposes: %+v", summary)
	}
	view := progressView(codexclient.Turn{Status: "inProgress", Items: []codexclient.Item{item, {ID: "two-native-call", Type: "commandExecution", Status: "failed", Command: item.Command, CommandActions: item.CommandActions}}}, 200000)
	tools := view.Records[0].Tools
	if len(tools.Items) != 2 || tools.Failed != 2 || !reflect.DeepEqual(tools.Items[0], summary) || tools.Items[1].Title != summary.Title || tools.Items[1].ItemID == summary.ItemID {
		t.Fatal("shared summary changed identity, status or per-native-call counting")
	}
	card := cards.Task(cards.TaskView{Status: "inProgress", Progress: view})
	if strings.Contains(card, "/private/") || strings.Contains(card, "cat ") || strings.Contains(card, "./...") {
		t.Fatal("safe call summary leaked raw shell into its card")
	}
	if _, ok := ProgressToolSummary(codexclient.Item{Type: "reasoning", Text: "PRIVATE_REASONING"}); ok {
		t.Fatal("hidden item became a public command")
	}
}

func TestCommandSummaryPrivateArgumentsDoNotChangeVisibleProjection(t *testing.T) {
	item := codexclient.Item{ID: "tool", Type: "commandExecution", Status: "inProgress", Command: "FIXTURE_TOKEN=PRIVATE_BEFORE git -C /private/first status --short"}
	before, _ := ProgressToolSummary(item)
	item.Command = "FIXTURE_TOKEN=PRIVATE_AFTER git -C /private/second status --porcelain"
	after, _ := ProgressToolSummary(item)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("private parameter changes polluted the public action projection")
	}
	item.Command = "go test ./internal/sync -run PRIVATE_PATTERN"
	changed, _ := ProgressToolSummary(item)
	if before.Title == changed.Title {
		t.Fatal("a purpose change retained the old public projection")
	}
	item.Command = strings.Repeat("x", 32769)
	bounded, _ := ProgressToolSummary(item)
	if bounded.Title != unidentifiedCommand || len([]rune(bounded.Title)) > 120 {
		t.Fatal("oversized shell source was not conservatively bounded")
	}
}
