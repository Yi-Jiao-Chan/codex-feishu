package sync

import (
	"fmt"
	"strings"
	"unicode"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/cot"
)

// cotToolDetails projects classified titles and redacted native text excerpts.
// Only the bounded display copy may enter the persistent public document.
// The legacy card summary remains unchanged for existing action revisions.
func cotToolDetails(item codexclient.Item, tool cards.ProgressTool) cot.Item {
	public := cot.Item{
		ID: item.ID, Kind: "tool", Status: tool.Status,
		Icon: cotToolIcon(tool.Kind), ToolName: cotNativeToolName(item),
	}
	title := cotCleanToolLabel(tool.Title)
	details := tool.Details
	switch item.Type {
	case "commandExecution":
		title, details = cotCommandSummary(item, tool)
	case "fileChange":
		paths := make(map[string]bool)
		for _, change := range item.Changes {
			if change.Path != "" {
				paths[change.Path] = true
			}
		}
		if len(paths) > 1 {
			title = "修改文件"
		}
		if tool.Status == "completed" && len(paths) > 0 {
			public.ResultSummary = fmt.Sprintf("已修改 %d 个文件", len(paths))
		}
	case "mcpToolCall", "dynamicToolCall":
		if name := cotDescriptor(item.Tool); name != "" {
			title = "调用 " + name
		}
	}
	public.Title = cotBoundedLabel(title, 32)
	for _, detail := range details {
		detail = cotBoundedLabel(cotCleanToolLabel(detail), 160)
		if detail == "" || detail == public.Title {
			continue
		}
		public.Details = appendProgressDetail(public.Details, detail, "另有其他文件或动作")
	}
	// Only these variants define execution metadata in the pinned native schema.
	if item.Type == "commandExecution" && item.ExitCode != nil &&
		int64(*item.ExitCode) >= -1<<31 && int64(*item.ExitCode) <= 1<<31-1 {
		value := *item.ExitCode
		public.ExitCode = &value
	}
	if (item.Type == "commandExecution" || item.Type == "mcpToolCall" || item.Type == "dynamicToolCall") &&
		item.DurationMS != nil && *item.DurationMS >= 0 {
		value := *item.DurationMS
		public.DurationMS = &value
	}
	cotToolPreviews(item, &public)
	return public
}

func cotCommandSummary(item codexclient.Item, tool cards.ProgressTool) (string, []string) {
	// Reuse the existing bounded shell classifier, never evaluate scripts or
	// inspect arbitrary operands. Native actions contribute only known kinds.
	kinds := make(map[string]bool)
	for _, action := range item.CommandActions {
		switch action.Type {
		case "read", "search":
			kinds[action.Type] = true
		case "listFiles":
			kinds["list"] = true
		}
	}
	hasNativeActions := len(kinds) > 0
	details := tool.Details
	purposes := commandPurposes(item.Command, 0)
	for _, purpose := range purposes {
		if purpose.label == unidentifiedCommand && hasNativeActions {
			if len(purposes) == 1 {
				continue // Missing or wholly unclassified commands use native facts.
			}
			// The classifier found a separate unknown action in a compound
			// command. The legacy summary omits it; preserve it only in V2.
			details = appendProgressDetail(append([]string(nil), details...), cotCleanToolLabel(purpose.label), "另有其他文件或动作")
		}
		kinds[purpose.kind] = true
	}
	if len(details) > 1 {
		if len(kinds) == 2 && kinds["read"] && kinds["search"] {
			return "读取与检索文件", details
		}
		if len(kinds) == 1 && kinds["read"] {
			return "读取文件", details
		}
		if len(kinds) == 1 && kinds["search"] {
			return "检索文件", details
		}
		return "执行复合命令", details
	}
	return cotCleanToolLabel(tool.Title), details
}

func cotCleanToolLabel(label string) string {
	return strings.ReplaceAll(label, "（用途未识别）", "")
}

func cotNativeToolName(item codexclient.Item) string {
	name := ""
	if item.Type == "mcpToolCall" {
		if server, tool := cotDescriptor(item.Server), cotDescriptor(item.Tool); server != "" && tool != "" {
			name = cotQualifiedToolName(server, tool)
		}
	} else if item.Type == "dynamicToolCall" {
		name = cotDescriptor(item.Tool)
		if namespace := cotDescriptor(item.Namespace); name != "" && namespace != "" {
			name = cotQualifiedToolName(namespace, name)
		}
	}
	if name == "" {
		name = item.Type
	}
	return cotBoundedName(name)
}

func cotQualifiedToolName(namespace, tool string) string {
	namespaceRunes, toolRunes := []rune(namespace), []rune(tool)
	if len(namespaceRunes)+len(toolRunes)+1 > 64 {
		// Keep both real descriptors: a long namespace must not hide the tool.
		if len(namespaceRunes) > 31 {
			namespaceRunes = namespaceRunes[:31]
		}
		if len(toolRunes) > 32 {
			toolRunes = toolRunes[:32]
		}
	}
	return string(namespaceRunes) + "." + string(toolRunes)
}

func cotDescriptor(value string) string {
	if value == "" || strings.TrimSpace(value) != value {
		return ""
	}
	for _, r := range value {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '_' && r != '-' && r != '.' {
			return ""
		}
	}
	return cotBoundedName(value)
}

func cotBoundedName(value string) string {
	if runes := []rune(value); len(runes) > 64 {
		return string(runes[:64])
	}
	return value
}

func cotBoundedLabel(value string, limit int) string {
	// All callers use classified labels or validated names, not raw payloads.
	value = cards.Safe(strings.Join(strings.Fields(value), " "))
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return value
}
