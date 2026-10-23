package sync

import (
	"encoding/json"
	"path"
	"strings"
	"unicode"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
)

const maxProgressToolDetails = 6
const omittedToolDetails = "另有其他工具类型"

// progressToolLabels uses only native classifications and descriptor names.
// Shell commands, arguments, output and file/search paths never become labels.
func progressToolLabels(item codexclient.Item) ([]string, bool) {
	switch item.Type {
	case "commandExecution":
		labels := make([]string, 0, maxProgressToolDetails)
		for _, action := range item.CommandActions {
			label := "执行命令"
			switch action.Type {
			case "read":
				label = "读取文件"
			case "search":
				label = "搜索内容"
			case "listFiles":
				label = "列出目录"
			}
			labels = appendToolDetail(labels, label)
		}
		if len(labels) == 0 {
			labels = []string{"执行命令"}
		}
		return labels, true
	case "mcpToolCall":
		server, tool := progressToolName(item.Server), progressToolName(item.Tool)
		if server != "" && tool != "" {
			return []string{"MCP · " + server + " / " + tool}, true
		}
		return []string{"外部工具"}, true
	case "dynamicToolCall":
		tool := progressToolName(item.Tool)
		if tool == "" {
			return []string{"动态工具"}, true
		}
		if namespace := progressToolName(item.Namespace); namespace != "" {
			tool = namespace + "." + tool
		}
		return []string{"工具 · " + tool}, true
	case "fileChange":
		return []string{"修改文件"}, true
	case "webSearch":
		return []string{"网页检索"}, true
	case "imageView":
		return []string{"查看图片"}, true
	case "imageGeneration":
		return []string{"生成图片"}, true
	case "collabAgentToolCall":
		// One native parent invocation, never a recursive count of its agents.
		return []string{"子代理调用"}, true
	default:
		// subAgentActivity is an activity marker; sleep is waiting, not a call.
		return nil, false
	}
}

func addProgressTool(tools *cards.ProgressTools, item codexclient.Item, labels []string) {
	summary := progressToolItem(item, labels, progressToolStatus(item))
	switch summary.Status {
	case "inProgress":
		tools.Running++
	case "completed":
		tools.Completed++
	case "failed":
		tools.Failed++
	case "declined":
		tools.Declined++
	default:
		// Missing, interrupted or future states never become successful calls.
		tools.Unknown++
	}
	for _, label := range labels {
		tools.Details = appendToolDetail(tools.Details, label)
	}
	tools.Items = append(tools.Items, summary)
	if extra := len(tools.Items) - cards.MaxProgressToolItems; extra > 0 {
		copy(tools.Items, tools.Items[extra:])
		clear(tools.Items[len(tools.Items)-extra:])
		tools.Items = tools.Items[:len(tools.Items)-extra]
		tools.OmittedItems += extra
	}
}

// ProgressToolSummary is the safe public projection shared by rendering and
// action-revision fingerprints. It never executes or persists raw shell text.
func ProgressToolSummary(item codexclient.Item) (cards.ProgressTool, bool) {
	labels, ok := progressToolLabels(item)
	if !ok {
		return cards.ProgressTool{}, false
	}
	return progressToolItem(item, labels, progressToolStatus(item)), true
}

func progressToolStatus(item codexclient.Item) string {
	if item.Type == "webSearch" || item.Type == "imageView" {
		return "unknown" // These native variants do not have a status field.
	}
	switch item.Status {
	case "inProgress", "completed", "failed":
		return item.Status
	case "declined":
		if item.Type == "commandExecution" || item.Type == "fileChange" {
			return item.Status
		}
	}
	return "unknown"
}

func progressToolItem(item codexclient.Item, labels []string, status string) cards.ProgressTool {
	tool := cards.ProgressTool{ItemID: item.ID, Status: status, Kind: "tool", Title: labels[0]}
	switch item.Type {
	case "commandExecution":
		tool.Kind, tool.Title, tool.Details = commandToolSummary(item)
		return tool
	case "fileChange":
		tool.Kind, tool.Title = "edit", "修改文件"
		for index, change := range item.Changes {
			var nativeKind struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(change.Kind, &nativeKind)
			kind, label := "edit", "修改文件"
			switch nativeKind.Type {
			case "add":
				kind, label = "write", "写入"
			case "update":
				kind, label = "edit", "编辑"
			case "delete":
				kind, label = "delete", "删除"
			}
			if index == 0 {
				tool.Kind = kind
			} else if tool.Kind != kind {
				tool.Kind = "edit"
			}
			if name := progressFileName(change.Path); name != "" {
				label += " " + name
			} else if nativeKind.Type == "add" || nativeKind.Type == "update" || nativeKind.Type == "delete" {
				label += "文件"
			}
			tool.Details = appendProgressDetail(tool.Details, label, "另有其他文件或动作")
		}
	case "mcpToolCall":
		tool.Kind = "mcp"
	case "dynamicToolCall":
		tool.Kind = "dynamic"
	case "webSearch":
		tool.Kind = "web"
	case "imageView", "imageGeneration":
		tool.Kind = "image"
	case "collabAgentToolCall":
		tool.Kind = "agent"
	}
	if len(tool.Details) > 0 {
		tool.Title = tool.Details[0]
		if len(tool.Details) > 1 {
			tool.Title += " 等"
		}
	} else {
		tool.Details = []string{tool.Title}
	}
	return tool
}

// progressFileName never reads files or interprets a URL/Markdown as a path.
// Metadata can contain native directories, but only a bounded basename leaves
// this projection. Suspicious or unavailable metadata falls back to the action.
func progressFileName(value string) string {
	if value == "" || strings.TrimSpace(value) != value || strings.Contains(value, "://") {
		return ""
	}
	if colon := strings.IndexByte(value, ':'); colon >= 0 && !(colon == 1 && len(value) > 2 && (value[2] == '/' || value[2] == '\\') && (value[0] >= 'A' && value[0] <= 'Z' || value[0] >= 'a' && value[0] <= 'z')) {
		return "" // Reject URL schemes; an explicit Windows drive is a path.
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ""
		}
	}
	name := path.Base(strings.ReplaceAll(value, `\`, "/"))
	if name == "." || name == ".." || name == "/" || strings.TrimSpace(name) != name {
		return ""
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsMark(r) && !strings.ContainsRune("_-. ", r) {
			return ""
		}
	}
	runes := []rune(name)
	if len(runes) > 40 {
		return string(runes[:39]) + "…"
	}
	return name
}

func appendToolDetail(details []string, label string) []string {
	return appendProgressDetail(details, label, omittedToolDetails)
}

func appendProgressDetail(details []string, label, omitted string) []string {
	for _, old := range details {
		if old == label || old == omitted {
			return details
		}
	}
	if len(details) < maxProgressToolDetails {
		return append(details, label)
	}
	// Preserve a bounded list and explicitly disclose that it is incomplete.
	details[len(details)-1] = omitted
	return details
}

func progressToolName(name string) string {
	if name == "" || strings.TrimSpace(name) != name {
		return ""
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '_' && r != '-' && r != '.' {
			return ""
		}
	}
	runes := []rune(name)
	if len(runes) > 24 {
		return string(runes[:24]) + "…"
	}
	return name
}
