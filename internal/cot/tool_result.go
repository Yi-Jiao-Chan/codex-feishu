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
		if item.ExitCode == nil {
			label += "，请在 Codex App 查看详情。"
		}
		content["status"], content["isError"] = "error", true
	case "declined":
		label = "已拒绝"
	}
	lines := []string{label}
	if status == "completed" || status == "failed" {
		if item.ExitCode != nil {
			content["exitCode"] = *item.ExitCode
			lines = append(lines, fmt.Sprintf("退出码：%d", *item.ExitCode))
		}
		if item.DurationMS != nil {
			content["durationMs"] = *item.DurationMS
			duration := fmt.Sprintf("%d 毫秒", *item.DurationMS)
			if *item.DurationMS >= 1000 {
				duration = fmt.Sprintf("%.2f 秒", float64(*item.DurationMS)/1000)
			}
			lines = append(lines, "耗时："+duration)
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
	if len(details) != 0 {
		lines = append(lines, "", "涉及内容：", strings.Join(details, "\n"))
	}
	p.fitToolResult(content, strings.Join(lines, "\n"), item.TimestampMS)
	return content
}

// Bound the actual twice-encoded event, including metadata and escaping. Keep
// status and outcome first; if details exceed the wire budget, disclose that.
func (p *planner) fitToolResult(content map[string]any, text string, observedMS int64) {
	fits := func(value string) bool {
		content["content"] = value
		encoded, _ := json.Marshal(content)
		wire, _ := json.Marshal(feishu.COTEvent{
			EventType: "TOOL_CALL_RESULT", Content: string(encoded),
			Timestamp: max(observedMS, p.State.LastTimestampMS+1),
		})
		return len(wire) <= feishu.MaxCOTEventBytes
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
