package sync

import (
	"strings"

	"codex-feishu/internal/codexclient"
)

const unidentifiedCommand = "运行自定义命令（用途未识别）"

type commandPurpose struct{ kind, label string }
type summaryWord struct {
	text    string
	literal bool
}

func commandToolSummary(item codexclient.Item) (string, string, []string) {
	var nativeOrder []string
	native := map[string]string{}
	multiple := map[string]bool{}
	var details []string
	for _, action := range item.CommandActions {
		kind, label := "", ""
		switch action.Type {
		case "read":
			kind, label = "read", "读取文件"
		case "search":
			kind, label = "search", "搜索内容"
		case "listFiles":
			kind, label = "list", "列出目录"
		default:
			continue
		}
		name := progressFileName(action.Path)
		if name == "" && kind == "read" {
			name = progressFileName(action.Name)
		}
		if name != "" {
			label = map[string]string{"read": "读取 ", "search": "搜索 ", "list": "列出目录 "}[kind] + name
		}
		if previous, ok := native[kind]; !ok {
			native[kind] = label
			nativeOrder = append(nativeOrder, kind)
		} else if previous != label {
			multiple[kind] = true
		}
		details = appendProgressDetail(details, label, "另有其他文件或动作")
	}
	var purposes []commandPurpose
	covered := map[string]bool{}
	for _, purpose := range commandPurposes(item.Command, 0) {
		if purpose.label == unidentifiedCommand && len(native) > 0 {
			continue
		}
		if label, ok := native[purpose.kind]; ok {
			purpose.label = label
			if multiple[purpose.kind] {
				purpose.label += " 等"
			}
			covered[purpose.kind] = true
		}
		purposes = appendCommandPurpose(purposes, purpose)
		if !covered[purpose.kind] {
			details = appendProgressDetail(details, purpose.label, "另有其他文件或动作")
		}
	}
	for _, kind := range nativeOrder {
		if !covered[kind] {
			label := native[kind]
			if multiple[kind] {
				label += " 等"
			}
			purposes = appendCommandPurpose(purposes, commandPurpose{kind, label})
		}
	}
	kind := purposes[0].kind
	labels := make([]string, 0, 3)
	for n, purpose := range purposes {
		if kind != purpose.kind {
			kind = "command"
		}
		if n < 3 {
			labels = append(labels, purpose.label)
		}
	}
	title := strings.Join(labels, "；")
	if len(purposes) > 3 {
		title += " 等"
	}
	if runes := []rune(title); len(runes) > 120 {
		title = string(runes[:119]) + "…"
	}
	return kind, title, details
}

// commandPurposes recognizes a bounded shell subset without evaluating any
// shell text. Only command positions, fixed verbs and safe script basenames
// become output. Unsupported syntax never turns embedded text into a command.
func commandPurposes(command string, depth int) []commandPurpose {
	if depth > 3 || len(command) > 32768 {
		return []commandPurpose{{"command", unidentifiedCommand}}
	}
	commands, heredoc, ok := summaryShellWords(command)
	if !ok {
		if len(commands) > 0 {
			return []commandPurpose{{"command", summaryProgramFallback(commands[0])}}
		}
		return []commandPurpose{{"command", unidentifiedCommand}}
	}
	var purposes []commandPurpose
	for n, words := range commands {
		words = summaryCommandWords(words)
		if len(words) == 0 {
			continue
		}
		if summaryReserved(words[0].text) {
			return []commandPurpose{{"command", unidentifiedCommand}}
		}
		var next []commandPurpose
		if heredoc && n == len(commands)-1 {
			next = []commandPurpose{{"command", summaryInlineProgram(words)}}
		} else {
			next = summarySimpleCommand(words, depth)
		}
		for _, purpose := range next {
			purposes = appendCommandPurpose(purposes, purpose)
		}
	}
	if len(purposes) == 0 {
		return []commandPurpose{{"command", unidentifiedCommand}}
	}
	return purposes
}

func appendCommandPurpose(purposes []commandPurpose, next commandPurpose) []commandPurpose {
	for _, old := range purposes {
		if old.label == next.label {
			return purposes
		}
	}
	// Keep one extra purpose to disclose that the three-label title is partial.
	if len(purposes) < 4 {
		purposes = append(purposes, next)
	}
	return purposes
}

// summaryShellWords supports quotes, escapes, comments and simple list/pipeline
// operators. Here-document content is never tokenized. Substitutions, grouping,
// redirections and background execution conservatively stop interpretation.
func summaryShellWords(command string) ([][]summaryWord, bool, bool) {
	var commands [][]summaryWord
	var words []summaryWord
	var word strings.Builder
	started, literal, quote := false, true, byte(0)
	flushWord := func() {
		if started {
			words = append(words, summaryWord{word.String(), literal})
			word.Reset()
			started, literal = false, true
		}
	}
	flushCommand := func() {
		flushWord()
		if len(words) > 0 {
			commands = append(commands, words)
			words = nil
		}
	}
	fail := func() ([][]summaryWord, bool, bool) {
		flushCommand()
		return commands, false, false
	}
	for n := 0; n < len(command); n++ {
		c := command[n]
		if len(words) > 256 || len(commands) > 32 {
			return fail()
		}
		if quote == '\'' {
			if c == '\'' {
				quote = 0
			} else {
				word.WriteByte(c)
			}
			continue
		}
		if c == '\\' {
			if n+1 == len(command) {
				return fail()
			}
			if quote == '"' && !strings.ContainsRune("$`\"\\\n", rune(command[n+1])) {
				word.WriteByte(c)
				continue
			}
			n++
			if command[n] != '\n' {
				started = true
				word.WriteByte(command[n])
			}
			continue
		}
		if c == '`' || c == '$' && n+1 < len(command) && command[n+1] == '(' {
			return fail()
		}
		if quote == '"' {
			if c == '"' {
				quote = 0
			} else {
				if c == '$' {
					literal = false
				}
				word.WriteByte(c)
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote, started = c, true
			continue
		}
		if c == '#' && !started {
			for n < len(command) && command[n] != '\n' {
				n++
			}
			flushCommand()
			continue
		}
		if c == '<' && n+1 < len(command) && command[n+1] == '<' {
			flushCommand()
			return commands, true, true
		}
		switch c {
		case ' ', '\t', '\r':
			flushWord()
		case '\n', ';':
			flushCommand()
		case '|', '&':
			if c == '&' && (n+1 == len(command) || command[n+1] != '&') || c == '|' && n+1 < len(command) && command[n+1] == '&' {
				return fail()
			}
			flushCommand()
			if n+1 < len(command) && command[n+1] == c {
				n++
			}
		case '(', ')', '{', '}', '<', '>':
			return fail()
		default:
			if c < 32 || c == 127 {
				return fail()
			}
			if c == '$' {
				literal = false
			}
			started = true
			word.WriteByte(c)
		}
	}
	if quote != 0 {
		return fail()
	}
	flushCommand()
	return commands, false, true
}

func summaryAssignment(word string) bool {
	name, _, ok := strings.Cut(word, "=")
	if !ok || name == "" {
		return false
	}
	for n, c := range name {
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || n > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func summaryCommandWords(words []summaryWord) []summaryWord {
	for len(words) > 0 && summaryAssignment(words[0].text) {
		words = words[1:]
	}
	if len(words) > 0 && summaryProgram(words[0]) == "env" {
		words = words[1:]
		for len(words) > 0 {
			if summaryAssignment(words[0].text) || words[0].text == "-i" || words[0].text == "--ignore-environment" || strings.HasPrefix(words[0].text, "--unset=") {
				words = words[1:]
			} else if words[0].text == "-u" || words[0].text == "--unset" || words[0].text == "-C" || words[0].text == "--chdir" {
				if len(words) < 2 {
					return nil
				}
				words = words[2:]
			} else if words[0].text == "--" {
				return words[1:]
			} else {
				break
			}
		}
	}
	if len(words) > 1 && words[0].literal && (words[0].text == "exec" || words[0].text == "command") && !strings.HasPrefix(words[1].text, "-") {
		return words[1:]
	}
	return words
}

func summaryReserved(word string) bool {
	switch word {
	case "if", "then", "else", "elif", "fi", "for", "do", "done", "while", "until", "case", "esac", "function", "select", "!":
		return true
	}
	return false
}

func summarySimpleCommand(words []summaryWord, depth int) []commandPurpose {
	if !words[0].literal {
		return []commandPurpose{{"command", unidentifiedCommand}}
	}
	program := summaryProgram(words[0])
	args := words[1:]
	one := func(kind, label string) []commandPurpose { return []commandPurpose{{kind, label}} }
	switch program {
	case "cd":
		return nil
	case "sh", "bash", "zsh":
		options := true
		for n, arg := range args {
			if !arg.literal {
				return one("command", "运行 Shell 内联脚本")
			}
			if options {
				switch arg.text {
				case "--":
					options = false
					continue
				case "-c", "-lc", "-cl":
					if n+1 < len(args) && args[n+1].literal {
						return commandPurposes(args[n+1].text, depth+1)
					}
					return one("command", "运行 Shell 内联脚本")
				case "-":
					return one("command", "运行 Shell 内联脚本")
				case "-l", "-e", "-u":
					continue
				}
				// Unknown options may consume the next word, including shell
				// options beginning with '+'. Never guess that word is a script.
				if strings.HasPrefix(arg.text, "-") || strings.HasPrefix(arg.text, "+") {
					return one("command", "运行 Shell 脚本")
				}
			}
			if name := progressFileName(arg.text); name != "" {
				return one("command", "运行 Shell 脚本 "+name)
			}
			break
		}
		return one("command", "运行 Shell 脚本")
	case "python", "python3", "node", "nodejs":
		language := "Python"
		if program == "node" || program == "nodejs" {
			language = "Node"
		}
		options := true
		for _, arg := range args {
			if !arg.literal {
				break
			}
			if options {
				switch arg.text {
				case "--":
					options = false
					continue
				case "-c", "-e", "--eval", "-p", "--print", "-":
					return one("command", "运行 "+language+" 内联脚本")
				case "-m":
					return one("command", "运行 Python 模块")
				case "-u", "-B":
					if language == "Python" {
						continue
					}
				case "--no-warnings", "--trace-warnings":
					if language == "Node" {
						continue
					}
				}
				// Only the small no-value allowlist above preserves script
				// position. Other options may carry private argument values.
				if strings.HasPrefix(arg.text, "-") {
					return one("command", "运行 "+language+" 脚本")
				}
			}
			if name := progressFileName(arg.text); name != "" {
				return one("command", "运行 "+language+" 脚本 "+name)
			}
			break
		}
		return one("command", "运行 "+language+" 脚本")
	case "git":
		args = summarySkipOptions(args, map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true})
		if len(args) > 0 && args[0].literal {
			if label := map[string]string{"status": "查看 Git 状态", "diff": "查看 Git 差异", "log": "查看 Git 历史", "show": "查看 Git 提交", "branch": "管理 Git 分支", "switch": "切换 Git 分支", "checkout": "切换或恢复 Git 内容", "fetch": "获取 Git 更新", "pull": "拉取 Git 更新", "push": "推送 Git 更新", "add": "暂存 Git 变更", "commit": "创建 Git 提交"}[args[0].text]; label != "" {
				return one("command", label)
			}
		}
	case "go":
		args = summarySkipOptions(args, map[string]bool{"-C": true})
		if len(args) > 0 && args[0].literal {
			if label := map[string]string{"test": "运行 Go 测试", "build": "构建 Go 程序", "vet": "检查 Go 代码", "fmt": "格式化 Go 代码"}[args[0].text]; label != "" {
				return one("command", label)
			}
		}
	case "gofmt":
		return one("command", "格式化 Go 代码")
	case "rg", "grep":
		for _, arg := range args {
			if arg.literal && arg.text == "--" {
				break
			}
			if arg.literal && arg.text == "--files" && program == "rg" {
				return one("search", "查找文件")
			}
		}
		return one("search", "搜索内容")
	case "find":
		return one("search", "查找文件")
	case "ls":
		return one("list", "列出目录")
	case "sed":
		for n := 0; n < len(args); n++ {
			arg := args[n]
			if arg.literal && arg.text == "--" {
				break
			}
			if arg.literal && (arg.text == "-e" || arg.text == "--expression" || arg.text == "-f" || arg.text == "--file") {
				n++ // A script argument resembling -i is not an option.
				continue
			}
			if arg.literal && (arg.text == "--in-place" || strings.HasPrefix(arg.text, "--in-place=") || strings.HasPrefix(arg.text, "-i")) {
				return one("edit", "编辑文件")
			}
		}
		return one("read", "读取文件")
	case "cat":
		return one("read", "读取文件")
	case "echo", "printf":
		return one("command", "输出文本")
	case "gh":
		if len(args) >= 2 && args[0].literal && args[0].text == "pr" && args[1].literal {
			if label := map[string]string{"view": "查看 GitHub PR", "list": "列出 GitHub PR", "checks": "检查 GitHub PR", "diff": "查看 GitHub PR 差异", "create": "创建 GitHub PR", "edit": "修改 GitHub PR", "comment": "评论 GitHub PR", "review": "审核 GitHub PR", "merge": "合并 GitHub PR", "close": "关闭 GitHub PR", "checkout": "检出 GitHub PR"}[args[1].text]; label != "" {
				return one("command", label)
			}
		}
	case "bytedcli":
		if len(args) >= 2 && args[0].literal && args[0].text == "codebase" && args[1].literal && (args[1].text == "mr" || args[1].text == "merge-request") {
			label := "运行 Codebase MR 操作"
			if len(args) > 2 && args[2].literal {
				if next := map[string]string{"get": "查看 Codebase MR", "view": "查看 Codebase MR", "list": "列出 Codebase MR", "diff": "查看 Codebase MR 差异", "create": "创建 Codebase MR", "update": "修改 Codebase MR", "review": "审核 Codebase MR", "comment": "评论 Codebase MR", "merge": "合并 Codebase MR"}[args[2].text]; next != "" {
					label = next
				}
			}
			return one("command", label)
		}
	}
	return one("command", summaryProgramFallback(words))
}

func summarySkipOptions(args []summaryWord, takesValue map[string]bool) []summaryWord {
	for len(args) > 0 && args[0].literal && strings.HasPrefix(args[0].text, "-") {
		if args[0].text == "--" {
			return args[1:]
		}
		if takesValue[args[0].text] {
			if len(args) < 2 {
				return nil
			}
			args = args[2:]
		} else if strings.Contains(args[0].text, "=") || args[0].text == "--no-pager" || args[0].text == "--no-optional-locks" {
			args = args[1:]
		} else {
			return nil
		}
	}
	return args
}

func summaryInlineProgram(words []summaryWord) string {
	if len(words) == 0 || !words[0].literal {
		return "运行内联脚本（用途未识别）"
	}
	switch summaryProgram(words[0]) {
	case "python", "python3":
		return "运行 Python 内联脚本"
	case "node", "nodejs":
		return "运行 Node 内联脚本"
	case "sh", "bash", "zsh":
		return "运行 Shell 内联脚本"
	case "cat":
		return "读取内联文本"
	}
	return "运行内联输入命令（用途未识别）"
}

func summaryProgramFallback(words []summaryWord) string {
	words = summaryCommandWords(words)
	if len(words) == 0 || !words[0].literal {
		return unidentifiedCommand
	}
	program := summaryProgram(words[0])
	switch program {
	case "git", "go", "gofmt", "rg", "grep", "find", "ls", "cat", "sed", "gh", "bytedcli", "codex", "curl", "wget", "make", "npm", "pnpm", "yarn", "cargo", "echo", "printf", "pwd", "tee", "head", "tail", "wc", "sort", "uniq", "awk", "date", "sleep", "mkdir", "chmod", "rm", "cp", "mv", "touch", "env":
		return "运行 " + program + "（用途未识别）"
	case "python", "python3", "node", "nodejs", "sh", "bash", "zsh":
		return summaryInlineProgram(words)
	}
	if strings.HasSuffix(program, ".sh") || strings.HasSuffix(program, ".py") || strings.HasSuffix(program, ".js") {
		if name := progressFileName(words[0].text); name != "" {
			return "运行脚本 " + name
		}
	}
	return unidentifiedCommand
}

func summaryProgram(word summaryWord) string {
	if !word.literal {
		return ""
	}
	return progressFileName(word.text)
}
