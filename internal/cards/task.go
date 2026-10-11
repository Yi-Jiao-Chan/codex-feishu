package cards

import "fmt"

type TaskView struct {
	Status, Body, Token, Kind, State, Reason string
	Revision                                 int64
	QueueCount                               int
	Waiting                                  bool
}

// Task renders the same compact per-turn progress card. Tokens identify server
// records; callback values never carry a trusted thread, turn or input body.
func Task(v TaskView) string {
	label := "执行中"
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
	els := []element{{"tag": "markdown", "content": Safe(label), "text_size": "notation"}}
	if v.Body != "" {
		els = append(els, element{"tag": "markdown", "content": Safe(v.Body), "text_size": "notation"})
	}
	if v.State == "available" && v.Reason != "verifying" && v.Token != "" && v.Revision > 0 &&
		(v.Kind == "stop" && v.Status == "inProgress" || v.Kind == "continue" && v.Status == "interrupted" && v.QueueCount > 0) {
		label := "停止"
		if v.Kind == "continue" {
			label = "继续执行队列"
		}
		b := Button(label, v.Kind, v.Token)
		b["behaviors"] = []element{{"type": "callback", "value": element{"action": v.Kind, "token": v.Token, "revision": v.Revision}}}
		els = append(els, b)
	}
	return render(els)
}
