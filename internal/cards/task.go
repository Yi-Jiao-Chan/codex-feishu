package cards

import (
	"fmt"
	"strings"
)

type TaskView struct {
	Status, Body, Token, Kind, State, Reason string
	Revision                                 int64
	QueueCount                               int
	Waiting                                  bool
	Progress                                 ProgressView
}

// Task renders the same compact per-turn progress card. Tokens identify server
// records; callback values never carry a trusted thread, turn or input body.
func Task(v TaskView) string {
	label := "正在处理"
	switch v.Status {
	case "completed":
		label = "已完成"
	case "failed":
		label = "执行失败 · 请查看 Codex App"
	case "interrupted":
		label = "已停止"
		if v.QueueCount > 0 {
			label += fmt.Sprintf(" · %d 条消息待执行", v.QueueCount)
		} else if v.QueueCount == 0 {
			label += " · 队列为空"
		} else {
			label += " · 队列状态待核验"
		}
	}
	if v.State == "pending" {
		if v.Kind == "continue" {
			label = "已收到 · 正在恢复队列"
		} else {
			label = "已收到 · 正在停止"
		}
	} else if v.State == "unknown" || v.Reason == "verifying" {
		label = "正在核验任务状态 · 暂不可操作"
	}
	if v.Waiting && v.Status == "inProgress" {
		label += " · 请回 Codex App 处理审批或问题"
	}
	progress := v.Progress
	if progress.Body == "" {
		progress.Body = v.Body
	}
	terminal := (v.Status == "completed" || v.Status == "failed" || v.Status == "interrupted") &&
		v.State != "pending" && v.State != "unknown" && v.Reason != "verifying"
	if progress.ElapsedSeconds > 0 {
		prefix := "已用"
		if terminal {
			prefix = "用时"
		}
		label += " · " + prefix + " " + taskDuration(progress.ElapsedSeconds)
	}
	var els []element
	history := progress.Records
	toolSummary := ""
	if progress.RunningTools+progress.FinishedTools > 0 {
		toolSummary = fmt.Sprintf("工具：%d 项执行中 · %d 项已结束", progress.RunningTools, progress.FinishedTools)
	}
	if terminal && len(history) == 0 && strings.TrimSpace(progress.Body) != "" {
		history = []string{progress.Body}
	}
	if terminal && (len(history) > 0 || progress.OmittedRecords > 0 || toolSummary != "") {
		els = append(els, taskHistory(label, history, progress.OmittedRecords, toolSummary))
	} else {
		els = append(els, element{"tag": "markdown", "content": Safe(label), "text_size": "notation"})
		if !terminal && strings.TrimSpace(progress.Body) != "" {
			els = append(els, element{"tag": "markdown", "content": Safe(progress.Body), "text_size": "normal"})
		}
		if len(history) > 0 || progress.OmittedRecords > 0 || toolSummary != "" {
			title := fmt.Sprintf("执行过程 · %d 条记录", len(history))
			if len(history) == 0 {
				title = "执行过程"
			}
			els = append(els, taskHistory(title, history, progress.OmittedRecords, toolSummary))
		}
	}
	if v.State == "available" && v.Reason != "verifying" && v.Token != "" && v.Revision > 0 &&
		(v.Kind == "stop" && v.Status == "inProgress" || v.Kind == "continue" && v.Status == "interrupted" && v.QueueCount > 0) {
		label := "停止当前轮"
		if v.Kind == "continue" {
			label = "继续执行队列"
		}
		b := Button(label, v.Kind, v.Token)
		b["behaviors"] = []element{{"type": "callback", "value": element{"action": v.Kind, "token": v.Token, "revision": v.Revision}}}
		els = append(els, b)
	}
	return render(els)
}

func taskDuration(seconds int64) string {
	if seconds < 60 {
		return fmt.Sprintf("%d 秒", seconds)
	}
	if seconds < 3600 {
		return fmt.Sprintf("%d 分 %d 秒", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%d 小时 %d 分", seconds/3600, seconds%3600/60)
}

func taskHistory(title string, records []string, omitted int, toolSummary string) element {
	parts := make([]string, 0, len(records)+1)
	if omitted > 0 {
		parts = append(parts, fmt.Sprintf("此前 %d 条记录已省略，请回 Codex App 查看。", omitted))
	}
	for _, record := range records {
		if strings.TrimSpace(record) != "" {
			parts = append(parts, Safe(record))
		}
	}
	if toolSummary != "" {
		parts = append(parts, toolSummary)
	}
	return element{
		"tag": "collapsible_panel", "element_id": "execution_history", "expanded": false,
		"padding": "0px", "vertical_spacing": "8px",
		"header": element{
			"title": element{"tag": "plain_text", "content": title}, "padding": "0px",
			"icon":          element{"tag": "standard_icon", "token": "down-small-ccm_outlined", "size": "16px 16px"},
			"icon_position": "right",
		},
		"elements": []element{{"tag": "markdown", "text_size": "notation", "content": strings.Join(parts, "\n\n")}},
	}
}
