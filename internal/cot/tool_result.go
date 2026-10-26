package cot

import (
	"encoding/json"
	"fmt"
	"strings"

	"codex-feishu/internal/feishu"
)

// detailedToolResult renders only the safe, typed projection. The result stays
// attached to its native tool call, so each tool can reveal its own details.
func (p *planner) detailedToolResult(item Item, status, originalTitle string) map[string]any {
	content := map[string]any{
		"messageId":  StableID(p.doc, "result", item.ID, 0),
		"toolCallId": StableID(p.doc, "tool", item.ID, 0),
		"role":       "tool",
	}
	label := "结束状态未确认"
	switch status {
	case "completed":
		label = "已完成"
		if item.ResultSummary != "" {
			label = item.ResultSummary
		}
		content["status"], content["isError"] = "completed", false
	case "failed":
		label = "执行失败"
		if item.ExitCode == nil && item.OutputPreview == "" {
			label += "，请在 Codex App 查看详情。"
		}
		content["status"], content["isError"] = "error", true
	case "declined":
		label = "已拒绝"
	}
	var lines []string
	if status != "completed" || item.ResultSummary != "" || item.InputPreview == "" && item.OutputPreview == "" && len(item.Details) == 0 {
		lines = append(lines, label)
	}
	if status == "completed" || status == "failed" {
		if item.ExitCode != nil {
			content["exitCode"] = *item.ExitCode
			if *item.ExitCode != 0 {
				lines = append(lines, fmt.Sprintf("退出码：%d", *item.ExitCode))
			}
		}
		if item.DurationMS != nil {
			content["durationMs"] = *item.DurationMS
		}
	}
	// START headings cannot be replaced. If native metadata genuinely changed,
	// retain the latest action in the detail instead of silently losing it.
	if item.Title != originalTitle {
		lines = append(lines, "当前动作："+item.Title)
	}
	var details []string
	seen := map[string]bool{item.Title: true, label: true}
	for _, detail := range item.Details {
		if !seen[detail] {
			seen[detail] = true
			details = append(details, "• "+detail)
		}
	}
	if len(details) != 0 && item.InputPreview == "" && item.OutputPreview == "" {
		lines = append(lines, "", "涉及内容：", strings.Join(details, "\n"))
	}
	if item.InputPreview != "" || item.OutputPreview != "" {
		p.fitToolPreviews(content, strings.Join(lines, "\n"), item.InputPreview, item.OutputPreview, item.TimestampMS)
		return content
	}
	p.fitToolResult(content, strings.Join(lines, "\n"), item.TimestampMS)
	return content
}

// Budget the complete rendering, including closed code fences. Sharing the
// available space keeps a long script from displacing its actual output.
func (p *planner) fitToolPreviews(content map[string]any, prefix, input, output string, observedMS int64) {
	in, out := []rune(input), []rune(output)
	build := func(budget int) string {
		inCount, outCount := min(len(in), budget), 0
		if len(out) > 0 {
			inCount = min(len(in), budget/3)
			outCount = min(len(out), budget-inCount)
			inCount = min(len(in), budget-outCount)
		}
		parts := []string{}
		if prefix != "" {
			parts = append(parts, prefix)
		}
		if len(in) != 0 {
			parts = append(parts, "执行内容：\n"+toolCodeBlock(toolPreviewClip(in, inCount)))
		}
		if len(out) != 0 {
			parts = append(parts, "结果：\n"+toolCodeBlock(toolPreviewClip(out, outCount)))
		}
		return strings.Join(parts, "\n\n")
	}
	lo, hi := 0, len(in)+len(out)
	if text := build(hi); p.toolResultFits(content, text, observedMS) {
		return
	}
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if p.toolResultFits(content, build(mid), observedMS) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	content["content"] = build(lo)
}

func toolPreviewClip(runes []rune, limit int) string {
	if len(runes) <= limit {
		return string(runes)
	}
	const omitted = "…（详情已省略）"
	budget := limit - len([]rune(omitted)) - 2
	if budget <= 0 {
		return omitted
	}
	head := budget * 2 / 3
	return strings.Trim(string(runes[:head]), "\n") + "\n" + omitted + "\n" + strings.Trim(string(runes[len(runes)-(budget-head):]), "\n")
}

func toolCodeBlock(text string) string {
	longest, run := 2, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	return fence + "text\n" + text + "\n" + fence
}

func (p *planner) toolResultFits(content map[string]any, text string, observedMS int64) bool {
	content["content"] = text
	encoded, _ := json.Marshal(content)
	wire, _ := json.Marshal(feishu.COTEvent{
		EventType: "TOOL_CALL_RESULT", Content: string(encoded),
		Timestamp: max(observedMS, p.State.LastTimestampMS+1),
	})
	return len(wire) <= feishu.MaxCOTEventBytes
}

// Bound the actual twice-encoded event, including metadata and escaping. Keep
// status and outcome first; if details exceed the wire budget, disclose that.
func (p *planner) fitToolResult(content map[string]any, text string, observedMS int64) {
	fits := func(value string) bool {
		return p.toolResultFits(content, value, observedMS)
	}
	if fits(text) {
		return
	}
	const omitted = "\n…（详情已省略）"
	runes := []rune(text)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if fits(string(runes[:mid]) + omitted) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	content["content"] = string(runes[:lo]) + omitted
}
