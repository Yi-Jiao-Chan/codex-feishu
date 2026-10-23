package cards

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"
)

// Reserve space for the history panel, status row and controls below Feishu's
// 200-component limit. Counts include titles and icons, not just body elements.
const taskHistoryComponentLimit = 169

// TaskIcons contains app-scoped keys for previously uploaded display assets.
// Empty keys use native icons; rendering never uploads or reads local files.
type TaskIcons struct {
	Toolbox, Command, Read, Edit, Search, Generic string
}

type TaskView struct {
	Status, Body, Token, Kind, State, Reason string
	Revision                                 int64
	QueueCount                               int
	Waiting                                  bool
	WaitingInFeishu                          bool
	Progress                                 ProgressView
	Icons                                    TaskIcons
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
		if v.WaitingInFeishu {
			label += " · 等待你处理审批或问题"
		} else {
			label += " · 请回 Codex App 处理审批或问题"
		}
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
	showStatus := v.State == "pending" || v.State == "unknown" || v.Reason == "verifying" || v.Waiting && v.Status == "inProgress"
	if progress.ElapsedSeconds > 0 && (terminal || showStatus) {
		prefix := "已用"
		if terminal {
			prefix = "用时"
		}
		label += " · " + prefix + " " + taskDuration(progress.ElapsedSeconds)
	}
	history := progress.Records
	if len(history) == 0 && strings.TrimSpace(progress.Body) != "" {
		history = []ProgressRecord{{Text: progress.Body}}
	}
	heading := element{"tag": "markdown", "content": Safe(label), "text_size": "notation", "margin": "0px"}
	var control element
	if v.State == "available" && v.Reason != "verifying" && v.Token != "" && v.Revision > 0 &&
		(v.Kind == "stop" && v.Status == "inProgress" || v.Kind == "continue" && v.Status == "interrupted" && v.QueueCount > 0) {
		buttonLabel := "停止"
		if v.Kind == "continue" {
			buttonLabel = "继续执行队列"
		}
		control = Button(buttonLabel, v.Kind, v.Token)
		control["behaviors"] = []element{{"type": "callback", "value": element{"action": v.Kind, "token": v.Token, "revision": v.Revision}}}
		if v.Kind == "stop" {
			delete(control, "text")
			control["size"] = "tiny"
			control["margin"] = "0px"
			control["hover_tips"] = element{"tag": "plain_text", "content": "停止当前轮"}
			// stop_outlined is listed in Feishu's public icon enumeration.
			control["icon"] = element{"tag": "standard_icon", "token": "stop_outlined"}
		}
	}
	if control != nil && v.Kind == "stop" {
		// Keep only the status in the stop button's row. The history panel is a
		// root sibling so folding it never hides the stop button or clips text.
		heading = element{"tag": "column_set", "flex_mode": "none", "horizontal_spacing": "4px", "vertical_align": "top", "columns": []element{
			{"tag": "column", "width": "auto", "padding": "0px", "elements": []element{control}},
			{"tag": "column", "width": "weighted", "weight": 1, "padding": "0px", "elements": []element{heading}},
		}}
	}
	els := []element{heading}
	if children := taskHistory(history, progress.OmittedRecords, progress.TimeZone, v.Icons); len(children) > 0 {
		panel := taskPanel(label, children)
		panel["element_id"] = "execution_history"
		panel["expanded"] = !terminal
		header := panel["header"].(element)
		header["width"] = "auto_when_fold"
		header["icon_position"] = "follow_text"
		title := header["title"].(element)
		delete(title, "text_color")
		if control != nil && v.Kind == "stop" {
			title["content"] = "执行中"
			els = append(els, panel)
		} else {
			els = []element{panel}
		}
	}
	if control != nil && v.Kind == "continue" {
		els = append(els, control)
	}
	return renderWidth(els, "default")
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

func taskHistory(records []ProgressRecord, omitted int, zone string, icons TaskIcons) []element {
	children := make([]element, 0, len(records)+1)
	type toolGroup struct {
		index int
		key   string
		tools *ProgressTools
	}
	var groups []toolGroup
	if omitted > 0 {
		children = append(children, element{"tag": "markdown", "text_size": "notation", "margin": "0px", "content": fmt.Sprintf("此前 %d 条记录已省略，请回 Codex App 查看。", omitted)})
	}
	for n, record := range records {
		if record.Tools != nil {
			key := record.ItemID
			if key == "" {
				key = fmt.Sprint(n)
			}
			if row := taskToolRecord(record.Tools, key, nil, icons); row != nil {
				groups = append(groups, toolGroup{index: len(children), key: key, tools: record.Tools})
				children = append(children, row)
			}
			continue
		}
		if strings.TrimSpace(record.Text) == "" {
			continue
		}
		children = append(children, taskRecordElements(record, zone)...)
	}
	// Disclose older records omitted to fit the base layout before distributing
	// the remaining space to call rows.
	if taskComponentCount(children) > taskHistoryComponentLimit && len(records) > 1 {
		return taskHistory(records[1:], omitted+1, zone, icons)
	}
	// Reserve each group's count and omission notice first. Spend the remaining
	// components on the newest individual calls without changing factual counts.
	remaining := taskHistoryComponentLimit - taskComponentCount(children)
	for n := len(groups) - 1; n >= 0; n-- {
		group := groups[n]
		var rows []element
		first := max(0, len(group.tools.Items)-MaxProgressToolItems)
		for i := len(group.tools.Items) - 1; i >= first; i-- {
			row := taskToolItem(group.tools.Items[i], icons)
			cost := taskComponentCount(row)
			if cost > remaining {
				break
			}
			remaining -= cost
			rows = append(rows, row)
		}
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
		children[group.index] = taskToolRecord(group.tools, group.key, rows, icons)
	}
	return children
}

func taskRecordElements(record ProgressRecord, zone string) []element {
	body := SafeMarkdown(record.Text)
	row := element{"tag": "markdown", "text_size": "notation", "margin": "0px", "content": body}
	if record.ReceivedAtMS <= 0 {
		return []element{row}
	}
	if zone == "" {
		zone = "Asia/Shanghai"
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		location = time.UTC
	}
	clock := taskGreyText(time.UnixMilli(record.ReceivedAtMS).In(location).Format("15:04"))
	if !taskInlineRecordTime(record.Text) {
		// A separate element also keeps the clock out of an unfinished code
		// block while a public message is still streaming.
		return []element{row, {"tag": "markdown", "text_size": "notation", "margin": "0px", "content": clock}}
	}
	row["content"] = strings.TrimRight(body, " \t\r\n") + " · " + clock
	return []element{row}
}

// Wrapped paragraphs and balanced inline code can share their final line with
// the clock. Keep block syntax and unfinished code separate from metadata.
func taskInlineRecordTime(text string) bool {
	lines := strings.Split(strings.TrimRight(text, " \t\r\n"), "\n")
	inlineWidth := 0
	for _, line := range lines {
		_, content, depth := publicQuotePrefix(line, -1)
		if depth > 0 || publicIndented(content) || openingMessageFence(content) != nil || taskTableDivider(content) {
			return false
		}
		_ = publicCodeReferences(content, &inlineWidth)
	}
	last := strings.TrimSpace(lines[len(lines)-1])
	return inlineWidth == 0 && publicBlockKind(last) == "paragraph" &&
		!inlineBlockPrefix.MatchString(last) && !strings.HasPrefix(last, "|") &&
		(last == "" || strings.Trim(last, "-*_ =\t") != "")
}

func taskTableDivider(line string) bool {
	if !strings.Contains(line, "|") {
		return false
	}
	cells := 0
	for _, cell := range strings.Split(line, "|") {
		cell = strings.TrimSpace(cell)
		if cell == "" {
			continue
		}
		cell = strings.Trim(cell, ":")
		if cell == "" || strings.Trim(cell, "-") != "" {
			return false
		}
		cells++
	}
	return cells > 0
}

func taskGreyMarkdown(content string) element {
	return element{"tag": "markdown", "text_size": "notation", "margin": "0px", "content": taskGreyText(content)}
}

func taskGreyText(content string) string {
	return "<font color='grey'>" + content + "</font>"
}

func taskToolRecord(tools *ProgressTools, key string, rows []element, icons TaskIcons) element {
	called := tools.Running + tools.Completed + tools.Failed
	if called+tools.Declined+tools.Unknown <= 0 {
		return nil
	}
	summary := "工具调用"
	if called > 0 {
		summary = fmt.Sprintf("调用 %d 个工具", called)
	}
	parts := []string{summary}
	for _, state := range []struct {
		label string
		count int
	}{{"进行中", tools.Running}, {"失败", tools.Failed}, {"已拒绝", tools.Declined}, {"状态未知", tools.Unknown}} {
		if state.count > 0 {
			parts = append(parts, fmt.Sprintf("%d 项%s", state.count, state.label))
		}
	}
	summary = strings.Join(parts, " · ")
	if len(tools.Items) == 0 && tools.OmittedItems == 0 {
		row := taskGreyMarkdown(summary)
		row["icon"] = taskPrefixIcon(icons.Toolbox, "briefcase_outlined")
		return row
	}
	children := append([]element(nil), rows...)
	if omitted := tools.OmittedItems + len(tools.Items) - len(rows); omitted > 0 {
		children = append(children, taskGreyMarkdown(fmt.Sprintf("另有 %d 个工具详情已省略，请回 Codex App 查看。", omitted)))
	}
	panel := taskPanel(summary, children)
	panel["element_id"] = taskStableID("tools", key)
	header := panel["header"].(element)
	header["width"] = "auto_when_fold"
	header["icon_position"] = "follow_text"
	header["icon"].(element)["color"] = "grey"
	header["title"] = element{"tag": "markdown", "content": taskGreyText(summary), "text_size": "notation", "icon": taskPrefixIcon(icons.Toolbox, "briefcase_outlined")}
	return panel
}

func taskToolItem(tool ProgressTool, icons TaskIcons) element {
	title := taskMetadataLine(tool.Title, 120)
	if title == "" {
		title = "工具调用"
	}
	state := "状态未知"
	switch tool.Status {
	case "inProgress":
		state = "进行中"
	case "completed":
		state = "已完成"
	case "failed":
		state = "失败"
	case "declined":
		state = "已拒绝"
	}
	if tool.Status != "completed" {
		title += " · " + state
	}
	icon := "briefcase_outlined"
	key := icons.Generic
	switch tool.Kind {
	case "read":
		icon = "wiki-book_outlined"
		key = icons.Read
	case "write", "edit", "delete", "fileChange":
		icon = "edit_outlined"
		key = icons.Edit
	case "search", "web", "webSearch":
		icon = "search_outlined"
		key = icons.Search
	case "command", "commandExecution", "list", "git", "test", "build", "script":
		icon = "code_outlined"
		key = icons.Command
	}
	row := taskGreyMarkdown(humanMetadata(title))
	row["icon"] = taskPrefixIcon(key, icon)
	return row
}

func taskPrefixIcon(key, fallback string) element {
	if key != "" {
		return element{"tag": "custom_icon", "img_key": key}
	}
	return element{"tag": "standard_icon", "token": fallback, "color": "grey"}
}

func taskMetadataLine(text string, limit int) string {
	runes := []rune(strings.Join(strings.Fields(text), " "))
	if len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return string(runes)
}

func taskStableID(prefix, key string) string {
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%s_%x", prefix, sum[:6])
}

func taskComponentCount(value any) int {
	count := 0
	switch value := value.(type) {
	case map[string]any:
		if _, ok := value["tag"].(string); ok {
			count++
		}
		for _, child := range value {
			count += taskComponentCount(child)
		}
	case []element:
		for _, child := range value {
			count += taskComponentCount(child)
		}
	case []any:
		for _, child := range value {
			count += taskComponentCount(child)
		}
	}
	return count
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
