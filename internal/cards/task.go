package cards

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"
	"unicode"
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
	if omitted := len(progress.Records) - MaxProgressRecords; omitted > 0 {
		progress.Records = progress.Records[omitted:]
		progress.OmittedRecords += omitted
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
	if !terminal && strings.TrimSpace(progress.Body) != "" {
		history = progressHistoryWithoutCurrent(progress.Body, progress.CurrentItemID, history)
	}
	if terminal && len(history) == 0 && strings.TrimSpace(progress.Body) != "" {
		history = []ProgressRecord{{Text: progress.Body}}
	}
	if terminal && (len(history) > 0 || progress.OmittedRecords > 0) {
		els = append(els, taskHistory(label, history, progress.OmittedRecords, progress.TimeZone))
	} else {
		els = append(els, element{"tag": "markdown", "content": Safe(label), "text_size": "notation"})
		if !terminal && strings.TrimSpace(progress.Body) != "" {
			els = append(els, element{"tag": "markdown", "content": Safe(progress.Body), "text_size": "normal"})
		}
		if len(history) > 0 || progress.OmittedRecords > 0 {
			title := fmt.Sprintf("执行过程 · %d 条记录", len(history))
			if len(history) == 0 {
				title = "执行过程"
			}
			els = append(els, taskHistory(title, history, progress.OmittedRecords, progress.TimeZone))
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

// progressHistoryWithoutCurrent only changes the slice used for display. The
// complete public projection remains available to the caller for persistence,
// byte budgeting and later reconciliations.
func progressHistoryWithoutCurrent(body, currentID string, records []ProgressRecord) []ProgressRecord {
	if len(records) == 0 {
		return records
	}
	remove := -1
	if currentID != "" {
		for n, record := range records {
			if record.ItemID == currentID {
				remove = n
				break
			}
		}
	} else if records[len(records)-1].Text == body {
		remove = len(records) - 1
	}
	if remove < 0 {
		return records
	}
	history := make([]ProgressRecord, 0, len(records)-1)
	history = append(history, records[:remove]...)
	history = append(history, records[remove+1:]...)
	return history
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

func taskHistory(title string, records []ProgressRecord, omitted int, zone string) element {
	children := make([]element, 0, len(records)+1)
	if omitted > 0 {
		children = append(children, element{"tag": "markdown", "text_size": "notation", "margin": "0px", "content": fmt.Sprintf("此前 %d 条记录已省略，请回 Codex App 查看。", omitted)})
	}
	for n, record := range records {
		if strings.TrimSpace(record.Text) != "" {
			key := record.ItemID
			if key == "" {
				key = fmt.Sprint(n)
			}
			sum := sha256.Sum256([]byte(key))
			row := taskPanel(taskRecordTime(record.ReceivedAtMS, zone)+" · "+taskRecordSummary(record.Text),
				[]element{{"tag": "markdown", "text_size": "notation", "margin": "0px", "content": Safe(record.Text)}})
			row["element_id"] = fmt.Sprintf("history_record_%x", sum[:10])
			children = append(children, row)
		}
	}
	panel := taskPanel(title, children)
	panel["element_id"] = "execution_history"
	return panel
}

func taskPanel(title string, children []element) element {
	return element{
		"tag": "collapsible_panel", "expanded": false,
		"padding": "0px", "vertical_spacing": "4px", "margin": "0px",
		"header": element{
			"title": element{"tag": "plain_text", "content": Safe(title), "text_size": "notation", "text_color": "grey"}, "padding": "0px",
			"icon":          element{"tag": "standard_icon", "token": "down-small-ccm_outlined", "size": "16px 16px"},
			"icon_position": "right",
		},
		"elements": children,
	}
}

func taskRecordTime(ms int64, zone string) string {
	if ms <= 0 {
		return "时间未记录"
	}
	if zone == "" {
		zone = "Asia/Shanghai"
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		location = time.UTC
	}
	return time.UnixMilli(ms).In(location).Format("15:04:05")
}

// Keep plain-text previews short enough for the compact mobile card. Newlines
// and Markdown structure belong only in the expanded full-text body.
func taskRecordSummary(text string) string {
	flat := strings.Join(strings.Fields(text), " ")
	width := 0
	for offset, r := range flat {
		step := 1
		if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
			step = 0
		} else if r >= 0x2e80 {
			step = 2
		}
		if width+step > 28 {
			return flat[:offset] + "…"
		}
		width += step
	}
	return flat
}
