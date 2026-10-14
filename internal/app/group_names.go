package app

import (
	"strings"

	"codex-feishu/internal/bindings"
	"codex-feishu/internal/codexclient"
)

// threadGroupTitle keeps the App's title behind one recognizable bot marker.
func threadGroupTitle(meta codexclient.Thread, turns []codexclient.Turn) string {
	return bindings.WithRobotPrefix(threadDisplayTitle(meta, turns))
}

// Preview and the first actual request are draft fallbacks for the App name.
func threadDisplayTitle(meta codexclient.Thread, turns []codexclient.Turn) string {
	if meta.Name != nil && strings.TrimSpace(*meta.Name) != "" {
		return strings.TrimSpace(*meta.Name)
	}
	preview := strings.TrimSpace(meta.Preview)
	if preview != "" && !strings.HasPrefix(preview, "# Files mentioned by the user:") {
		return shortGroupText(preview, 60)
	}
	for _, turn := range turns {
		for _, item := range turn.Items {
			if item.Type != "userMessage" {
				continue
			}
			presentation := item.UserPresentation()
			if strings.TrimSpace(presentation.Text) != "" {
				return shortGroupText(presentation.Text, 60)
			}
			if len(presentation.Attachments) > 0 {
				for _, attachment := range presentation.Attachments {
					if attachment.Kind != "image" {
						return "附件任务"
					}
				}
				return "图片任务"
			}
		}
	}
	return "Codex"
}
