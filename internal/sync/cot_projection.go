package sync

import (
	"errors"
	"strings"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/cot"
)

// cotDocument projects a complete already-normalized history snapshot. It does
// not reuse ProgressView's 32-item display window: COT has its own event budget.
// clocks records first local observation for both messages and tools; callers
// persist it together with the document, never as a native occurrence time.
func cotDocument(thread string, turn codexclient.Turn, clocks map[string]int64, observedAtMS int64) (string, error) {
	if observedAtMS <= 0 || turn.ItemsView != "" && turn.ItemsView != "full" {
		return "", errors.New("COT projection requires a complete observed history")
	}
	items, err := uniqueItems(turn.Items)
	if err != nil {
		return "", err
	}
	legacyFinal, explicitFinal := -1, false
	if turn.Status != "inProgress" {
		for n, item := range items {
			if item.Type != "agentMessage" || strings.TrimSpace(item.Text) == "" {
				continue
			}
			if item.Phase == nil {
				legacyFinal = n
			} else if *item.Phase == "final_answer" {
				explicitFinal = true
			}
		}
	}
	doc := cot.Document{Version: cot.Version, ThreadID: thread, TurnID: turn.ID, Status: turn.Status}
	for n, item := range items {
		public := cot.Item{ID: item.ID}
		if tool, ok := ProgressToolSummary(item); ok {
			public.Kind, public.Title, public.Status = "tool", cards.Safe(tool.Title), tool.Status
			public.Icon = cotToolIcon(tool.Kind)
		} else if item.Type == "agentMessage" && strings.TrimSpace(item.Text) != "" && !isAsyncQuestion(item) &&
			(item.Phase == nil || *item.Phase == "commentary" || *item.Phase == "partial_answer") &&
			!(n == legacyFinal && !explicitFinal) {
			text, _ := codexclient.RewriteVisualizations(item.Text, nil)
			public.Kind, public.Text = "message", cards.SafeMarkdown(text)
		} else {
			continue
		}
		public.TimestampMS = clocks[item.ID]
		if public.TimestampMS <= 0 {
			public.TimestampMS = observedAtMS
			if clocks != nil {
				clocks[item.ID] = observedAtMS
			}
		}
		doc.Items = append(doc.Items, public)
	}
	return cot.Encode(doc)
}

// CotDocument exposes the pure projection for composition roots that stage COT
// separately. Engine callers should use the same normalized snapshot as cards.
func CotDocument(thread string, turn codexclient.Turn, clocks map[string]int64, observedAtMS int64) (string, error) {
	return cotDocument(thread, turn, clocks, observedAtMS)
}

func cotToolIcon(kind string) string {
	switch kind {
	case "read":
		return "read"
	case "write", "edit", "delete", "fileChange":
		return "write"
	case "search", "web", "webSearch":
		return "search"
	case "command", "commandExecution", "list", "git", "test", "build", "script", "mcp", "dynamic":
		return "bash"
	case "agent":
		return "task"
	default:
		return "default"
	}
}
