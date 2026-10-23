package cards

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func taskTestElements(t *testing.T, card string) []any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(card), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["header"]; ok {
		t.Fatal("progress gained a card header")
	}
	if doc["config"].(map[string]any)["width_mode"] != "default" {
		t.Fatal("task should use the bounded default width instead of filling the chat window")
	}
	body := doc["body"].(map[string]any)
	if body["padding"] != "12px 12px" || body["vertical_spacing"] != "8px" {
		t.Fatal("progress lost compact body spacing")
	}
	return body["elements"].([]any)
}

func taskTestFind(value any, field string, expected any) []map[string]any {
	var found []map[string]any
	switch value := value.(type) {
	case map[string]any:
		if value[field] == expected {
			found = append(found, value)
		}
		for _, child := range value {
			found = append(found, taskTestFind(child, field, expected)...)
		}
	case []any:
		for _, child := range value {
			found = append(found, taskTestFind(child, field, expected)...)
		}
	}
	return found
}

func taskTestHistory(t *testing.T, elements []any) []any {
	t.Helper()
	panel := taskTestHistoryPanel(t, elements)
	history, ok := panel["elements"].([]any)
	if !ok {
		t.Fatalf("execution history panel lost its native children: %+v", panel)
	}
	if len(taskTestFind(history, "tag", "column_set")) != 0 {
		t.Fatalf("execution history body gained a wrapper column set: %+v", panel)
	}
	taskTestNoNegativeMargins(t, panel)
	return history
}

func taskTestHistoryPanel(t *testing.T, elements []any) map[string]any {
	t.Helper()
	var panel map[string]any
	for _, value := range elements {
		child, ok := value.(map[string]any)
		if ok && child["element_id"] == "execution_history" {
			if panel != nil {
				t.Fatalf("task rendered duplicate execution history panels: %+v", elements)
			}
			panel = child
		}
	}
	if panel == nil {
		t.Fatalf("task lost its root execution history panel: %+v", elements)
	}
	if panel["tag"] != "collapsible_panel" || panel["padding"] != "0px" || panel["margin"] != "0px" || panel["vertical_spacing"] != "4px" {
		t.Fatalf("execution history panel lost its compact native shape: %+v", panel)
	}
	header, ok := panel["header"].(map[string]any)
	if !ok || header["width"] != "auto_when_fold" || header["icon_position"] != "follow_text" || header["padding"] != "0px" {
		t.Fatalf("execution history header lost its adaptive follow-text layout: %+v", panel)
	}
	title, ok := header["title"].(map[string]any)
	if !ok || title["tag"] != "plain_text" || title["text_size"] != "notation" || title["text_color"] != nil || title["content"] == "" {
		t.Fatalf("execution history header lost its default-color notation title: %+v", panel)
	}
	if icon, ok := header["icon"].(map[string]any); !ok || icon["tag"] != "standard_icon" || icon["token"] != "down-small-ccm_outlined" {
		t.Fatalf("execution history header lost its native chevron: %+v", panel)
	}
	return panel
}

func taskTestNoNegativeMargins(t *testing.T, value any) {
	t.Helper()
	switch value := value.(type) {
	case map[string]any:
		if margin, ok := value["margin"].(string); ok && strings.Contains(margin, "-") {
			t.Fatalf("element has a negative margin: %+v", value)
		}
		for _, child := range value {
			taskTestNoNegativeMargins(t, child)
		}
	case []any:
		for _, child := range value {
			taskTestNoNegativeMargins(t, child)
		}
	}
}

func taskTestTitle(value any) map[string]any {
	elements, ok := value.([]any)
	if !ok || len(elements) == 0 {
		return nil
	}
	heading, ok := elements[0].(map[string]any)
	if !ok {
		return nil
	}
	if heading["tag"] == "markdown" {
		return heading
	}
	if heading["tag"] == "collapsible_panel" && heading["element_id"] == "execution_history" {
		header, _ := heading["header"].(map[string]any)
		title, _ := header["title"].(map[string]any)
		return title
	}
	if heading["tag"] != "column_set" {
		return nil
	}
	columns, ok := heading["columns"].([]any)
	if !ok || len(columns) != 2 {
		return nil
	}
	right, ok := columns[1].(map[string]any)
	if !ok {
		return nil
	}
	rightElements, ok := right["elements"].([]any)
	if !ok || len(rightElements) != 1 {
		return nil
	}
	status, _ := rightElements[0].(map[string]any)
	return status
}

func taskTestDirectPanels(value any) []map[string]any {
	var panels []map[string]any
	children, _ := value.([]any)
	for _, value := range children {
		child, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if child["tag"] == "collapsible_panel" {
			panels = append(panels, child)
			continue
		}
	}
	return panels
}

func taskTestFoldedPanel(t *testing.T, panel map[string]any) []any {
	t.Helper()
	if panel["tag"] != "collapsible_panel" || panel["expanded"] != false {
		t.Fatalf("expected a default-collapsed native panel: %+v", panel)
	}
	header, ok := panel["header"].(map[string]any)
	if !ok || header["icon_position"] != "follow_text" {
		t.Fatalf("panel lost its follow-text chevron layout: %+v", panel)
	}
	icon, ok := header["icon"].(map[string]any)
	if !ok || icon["tag"] != "standard_icon" || icon["token"] != "down-small-ccm_outlined" {
		t.Fatalf("panel lost its native chevron: %+v", panel)
	}
	title, ok := header["title"].(map[string]any)
	if !ok || title["text_size"] != "notation" || title["content"] == "" {
		t.Fatalf("panel lost its grey notation title: %+v", panel)
	}
	if title["tag"] == "plain_text" {
		if title["text_color"] != "grey" {
			t.Fatalf("plain-text panel title lost its grey color: %+v", title)
		}
	} else if title["tag"] == "markdown" {
		if title["text_color"] != nil || !strings.HasPrefix(title["content"].(string), "<font color='grey'>") {
			t.Fatalf("Markdown panel title lost its grey rendering: %+v", title)
		}
	} else {
		t.Fatalf("panel title changed to an unsupported tag: %+v", title)
	}
	children, ok := panel["elements"].([]any)
	if !ok {
		t.Fatalf("panel lost its native children: %+v", panel)
	}
	return children
}

func taskTestPanelIDs(value any) []string {
	var ids []string
	for _, panel := range taskTestFind(value, "tag", "collapsible_panel") {
		if id, ok := panel["element_id"].(string); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

func taskTestTagCount(value any, tag string) int {
	count := 0
	switch value := value.(type) {
	case map[string]any:
		if (tag == "" && value["tag"] != nil) || value["tag"] == tag {
			count++
		}
		for _, child := range value {
			count += taskTestTagCount(child, tag)
		}
	case []any:
		for _, child := range value {
			count += taskTestTagCount(child, tag)
		}
	}
	return count
}

func taskTestCloneRecords(records []ProgressRecord) []ProgressRecord {
	clone := make([]ProgressRecord, len(records))
	for n, record := range records {
		clone[n] = record
		if record.Tools == nil {
			continue
		}
		tools := *record.Tools
		tools.Details = append([]string(nil), record.Tools.Details...)
		tools.Items = make([]ProgressTool, len(record.Tools.Items))
		for itemN, item := range record.Tools.Items {
			tools.Items[itemN] = item
			tools.Items[itemN].Details = append([]string(nil), item.Details...)
		}
		clone[n].Tools = &tools
	}
	return clone
}

func TestTaskUsesFullWidthHistoryWithSmallScopedStop(t *testing.T) {
	card := Task(TaskView{Status: "inProgress", Body: "public <at id=\"private\">", Token: "opaque", Revision: 7, Kind: "stop", State: "available", QueueCount: -1, Waiting: true})
	els := taskTestElements(t, card)
	if len(els) != 2 {
		t.Fatal("running card should have a stop/status row and one root history panel")
	}
	row := els[0].(map[string]any)
	if row["tag"] != "column_set" || row["flex_mode"] != "none" || row["horizontal_spacing"] != "4px" || row["vertical_align"] != "top" {
		t.Fatal("stop must stay beside the status at narrow widths")
	}
	columns := row["columns"].([]any)
	if len(columns) != 2 || columns[0].(map[string]any)["width"] != "auto" || columns[1].(map[string]any)["width"] != "weighted" || columns[1].(map[string]any)["weight"] != float64(1) || columns[0].(map[string]any)["padding"] != "0px" || columns[1].(map[string]any)["padding"] != "0px" {
		t.Fatal("stop and execution title lost auto/weighted left-to-right columns")
	}
	if len(columns[0].(map[string]any)["elements"].([]any)) != 1 || columns[0].(map[string]any)["elements"].([]any)[0].(map[string]any)["tag"] != "button" {
		t.Fatal("stop control moved out of the first column")
	}
	history := taskTestHistory(t, els)
	panel := taskTestHistoryPanel(t, els)
	if els[1].(map[string]any)["element_id"] != "execution_history" || panel["expanded"] != true {
		t.Fatalf("running stop card did not keep an expanded root history panel: %+v", els)
	}
	if panel["header"].(map[string]any)["title"].(map[string]any)["content"] != "执行中" {
		t.Fatalf("usable stop card lost its fixed history heading: %+v", panel)
	}
	title := taskTestTitle(els)
	if title["tag"] != "markdown" || title["text_size"] != "notation" || title["text_color"] != nil || title["margin"] != "0px" {
		t.Fatal("latest action title lost its small default-color Markdown style")
	}
	if !strings.Contains(title["content"].(string), "请回 Codex App 处理审批或问题") {
		t.Fatal("waiting status must remain prominent while details are folded")
	}
	buttons := taskTestFind(els, "tag", "button")
	if len(buttons) != 1 || len(taskTestFind(history, "tag", "button")) != 0 {
		t.Fatal("stop must remain visible while execution details are folded")
	}
	button := buttons[0]
	if button["type"] != "text" || button["size"] != "tiny" || button["width"] != nil || button["margin"] != "0px" || button["text"] != nil || button["hover_tips"].(map[string]any)["content"] != "停止当前轮" || button["icon"].(map[string]any)["token"] != "stop_outlined" || button["pc_display_rule"] != nil {
		t.Fatal("stop lost its icon-only styling or explanatory hover text")
	}
	if len(history) != 1 || history[0].(map[string]any)["tag"] != "markdown" || history[0].(map[string]any)["margin"] != "0px" {
		t.Fatalf("history did not remain directly inside the root execution panel: %+v", history)
	}
	value := button["behaviors"].([]any)[0].(map[string]any)["value"].(map[string]any)
	if len(value) != 3 || value["action"] != "stop" || value["token"] != "opaque" || value["revision"] != float64(7) {
		t.Fatal("callback includes untrusted routing/body values or wrong revision")
	}
	full := history[0].(map[string]any)["content"].(string)
	if strings.Contains(full, "<at") || !strings.Contains(full, "&lt;at") || strings.Contains(card, "approve") || strings.Contains(card, "answer") {
		t.Fatal("waiting text answered a native request or exposed a mention")
	}
	if _, ok, err := WithRequestMarker(card, "cabcdefghijklmnopqrs"); err != nil || !ok {
		t.Fatal("column layout prevented delivery marking", err)
	}
}

func TestTaskRecordsReadContinuouslyWithCompleteMarkdownInEveryState(t *testing.T) {
	text := "第一段很长的公开执行记录，应该在单行摘要中省略而且保留全文。\n\n第二段\n```go\nfmt.Println(\"public\")\n```"
	at := time.Date(2026, 10, 13, 14, 12, 3, 0, time.UTC).UnixMilli()
	for _, status := range []string{"inProgress", "completed", "interrupted", "failed"} {
		t.Run(status, func(t *testing.T) {
			records := []ProgressRecord{{ItemID: "public-1", Text: text, ReceivedAtMS: at}, {ItemID: "public-2", Text: "同一条摘要", ReceivedAtMS: at + 1000}}
			original := append([]ProgressRecord(nil), records...)
			card := Task(TaskView{Status: status, Progress: ProgressView{
				Records:  records,
				TimeZone: "Asia/Shanghai", RunningTools: 2, FinishedTools: 9,
			}})
			rows := taskTestHistory(t, taskTestElements(t, card))
			if len(rows) != 3 {
				t.Fatalf("ordered public history lost a body or clock element: %d", len(rows))
			}
			if len(taskTestFind(rows, "tag", "collapsible_panel")) != 0 {
				t.Fatal("public history gained per-record disclosure panels")
			}
			body := rows[0].(map[string]any)
			clock := rows[1].(map[string]any)
			second := rows[2].(map[string]any)
			if body["tag"] != "markdown" || body["content"] != SafeMarkdown(text) || body["text_size"] != "notation" || body["margin"] != "0px" {
				t.Fatalf("multiline public record was not kept as a complete Markdown element: %+v", body)
			}
			if clock["tag"] != "markdown" || clock["content"] != "<font color='grey'>22:12</font>" || clock["text_size"] != "notation" || clock["margin"] != "0px" {
				t.Fatalf("multiline public record lost its separate clock element: %+v", clock)
			}
			if second["tag"] != "markdown" || second["content"] != SafeMarkdown("同一条摘要")+" · <font color='grey'>22:12</font>" || second["text_size"] != "notation" || second["margin"] != "0px" {
				t.Fatalf("single-line public record did not keep its inline clock: %+v", second)
			}
			for n, value := range rows {
				content := value.(map[string]any)["content"].(string)
				if strings.Contains(content, "14:12:03") || strings.Contains(content, "22:12:03") || strings.Contains(content, "时间未记录") {
					t.Fatalf("public record element %d exposed seconds or fabricated time text: %+v", n, value)
				}
			}
			if strings.Contains(card, "工具调用") {
				t.Fatal("legacy aggregate counts fabricated an ordered tool group")
			}
			if !reflect.DeepEqual(records, original) {
				t.Fatal("rendering changed the public records projection")
			}
		})
	}
}

func TestTaskRecordTimeUsesZoneAndPreservesMarkdownBlocks(t *testing.T) {
	at := time.Date(2026, 10, 13, 14, 12, 3, 0, time.UTC).UnixMilli()
	for _, tc := range []struct {
		name, zone, text, clock string
		receivedAt              int64
		block                   bool
	}{
		{name: "default timezone", text: "普通公开正文", clock: "22:12", receivedAt: at},
		{name: "invalid timezone falls back to UTC", zone: "Mars/Phobos", text: "普通公开正文", clock: "14:12", receivedAt: at},
		{name: "zero timestamp", zone: "Asia/Shanghai", text: "没有记录时间的正文"},
		{name: "inline code", zone: "Asia/Shanghai", text: "运行 `go test`", clock: "22:12", receivedAt: at},
		{name: "multiline closed inline code with trailing whitespace", zone: "Asia/Shanghai", text: "第一行说明\n运行 `cat | rg foo`\r\n  \t\n", clock: "22:12", receivedAt: at},
		{name: "fenced code", zone: "Asia/Shanghai", text: "```go\nfmt.Println(\"public\")\n```", clock: "22:12", receivedAt: at, block: true},
		{name: "streaming unclosed fence", zone: "Asia/Shanghai", text: "先核对结果\n```go\nrun()", clock: "22:12", receivedAt: at, block: true},
		{name: "streaming unclosed inline code", zone: "Asia/Shanghai", text: "运行 `go test", clock: "22:12", receivedAt: at, block: true},
		{name: "list", zone: "Asia/Shanghai", text: "1. 第一项\n2. 第二项", clock: "22:12", receivedAt: at, block: true},
		{name: "heading followed by paragraph", zone: "Asia/Shanghai", text: "# 公开标题\n正文", clock: "22:12", receivedAt: at},
		{name: "quote", zone: "Asia/Shanghai", text: "> 公开引用\n> 第二行", clock: "22:12", receivedAt: at, block: true},
		{name: "table", zone: "Asia/Shanghai", text: "| 列一 | 列二 |\n| --- | --- |", clock: "22:12", receivedAt: at, block: true},
		{name: "table without leading pipe", zone: "Asia/Shanghai", text: "列一 | 列二\n--- | ---", clock: "22:12", receivedAt: at, block: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records := []ProgressRecord{{ItemID: "timed", Text: tc.text, ReceivedAtMS: tc.receivedAt}}
			original := append([]ProgressRecord(nil), records...)
			card := Task(TaskView{Status: "inProgress", Progress: ProgressView{Records: records, TimeZone: tc.zone}})
			rows := taskTestHistory(t, taskTestElements(t, card))
			body := SafeMarkdown(tc.text)
			if tc.receivedAt <= 0 {
				if len(rows) != 1 || rows[0].(map[string]any)["content"] != body {
					t.Fatalf("zero-time record fabricated a clock or changed body: %+v", rows)
				}
			} else if tc.block {
				if len(rows) != 2 || rows[0].(map[string]any)["content"] != body || rows[1].(map[string]any)["content"] != "<font color='grey'>"+tc.clock+"</font>" {
					t.Fatalf("block record did not keep body and clock in separate elements: %+v", rows)
				}
			} else {
				want := strings.TrimRight(body, " \t\r\n") + " · <font color='grey'>" + tc.clock + "</font>"
				if len(rows) != 1 || rows[0].(map[string]any)["content"] != want {
					t.Fatalf("single-line record did not keep its inline clock: got=%v want=%v", rows[0].(map[string]any)["content"], want)
				}
			}
			for n, value := range rows {
				content := value.(map[string]any)["content"].(string)
				if strings.Contains(content, ":12:03") || strings.Contains(content, "时间未记录") {
					t.Fatalf("timed record element %d exposed seconds or fabricated time text: %q", n, content)
				}
			}
			if !reflect.DeepEqual(records, original) {
				t.Fatal("rendering changed the timed record projection")
			}
		})
	}
}

func TestCompletedTaskRetainsPublicHistoryAndDeliveryMarker(t *testing.T) {
	at := time.Date(2026, 10, 13, 14, 12, 3, 0, time.UTC).UnixMilli()
	records := []ProgressRecord{{ItemID: "a", Text: "已检查模板", ReceivedAtMS: at}, {ItemID: "b", Text: "已验证布局", ReceivedAtMS: at + 1000}}
	original := append([]ProgressRecord(nil), records...)
	base := Task(TaskView{Status: "completed", Progress: ProgressView{Body: "已验证布局", CurrentItemID: "b", Records: records, TimeZone: "Asia/Shanghai", ElapsedSeconds: 102}})
	marked, ok, err := WithRequestMarker(base, "cabcdefghijklmnopqrs")
	if err != nil || !ok || len(marked)-len(base) != MaxRequestMarkerBytes {
		t.Fatal("task did not reserve its exact delivery marker budget", err)
	}
	els := taskTestElements(t, marked)
	children := taskTestHistory(t, els)
	panel := taskTestHistoryPanel(t, els)
	if len(els) != 1 || panel["expanded"] != false || taskTestTitle(els)["content"] != "已完成 · 用时 1 分 42 秒" {
		t.Fatal("completed execution did not retain its terminal history panel and full label")
	}
	if len(taskTestFind(children, "tag", "collapsible_panel")) != 0 {
		t.Fatal("terminal public history gained nested disclosure panels")
	}
	for n, child := range children {
		if got := child.(map[string]any)["margin"]; got != "0px" {
			t.Fatalf("terminal history child %d inherited a stop-column offset without a stop: %v", n, got)
		}
	}
	for n, expected := range []string{SafeMarkdown("已检查模板"), SafeMarkdown("已验证布局")} {
		text := children[n].(map[string]any)
		want := expected + " · <font color='grey'>22:12</font>"
		if text["tag"] != "markdown" || text["text_size"] != "notation" || text["content"] != want || strings.Contains(text["content"].(string), ":12:03") {
			t.Fatal("terminal card lost full history or its delivery marker")
		}
	}
	var markedDoc map[string]any
	if err := json.Unmarshal([]byte(marked), &markedDoc); err != nil {
		t.Fatal(err)
	}
	if got := len(taskTestFind(markedDoc, "element_id", "cabcdefghijklmnopqrs")); got != 1 {
		t.Fatalf("terminal marker was not unique: %d", got)
	}
	if !reflect.DeepEqual(records, original) {
		t.Fatal("terminal rendering changed the public records projection")
	}
}

func TestTaskTerminalStatesKeepOneMarkerAndRootHistory(t *testing.T) {
	for _, tc := range []struct {
		status, label string
	}{
		{status: "completed", label: "已完成"},
		{status: "failed", label: "执行失败 · 请查看 Codex App"},
		{status: "interrupted", label: "已停止 · 队列为空"},
	} {
		status := tc.status
		t.Run(status, func(t *testing.T) {
			base := Task(TaskView{Status: status, Body: "终态正文", Progress: ProgressView{Records: []ProgressRecord{{ItemID: "terminal", Text: "终态记录"}}}})
			marker := "cterminal" + status
			marked, ok, err := WithRequestMarker(base, marker)
			if err != nil || !ok {
				t.Fatalf("terminal %s could not reserve its marker: marked=%v err=%v", status, ok, err)
			}
			var doc map[string]any
			if err := json.Unmarshal([]byte(marked), &doc); err != nil {
				t.Fatal(err)
			}
			if got := len(taskTestFind(doc, "element_id", marker)); got != 1 {
				t.Fatalf("terminal %s marker count = %d, want 1", status, got)
			}
			els := taskTestElements(t, marked)
			panel := taskTestHistoryPanel(t, els)
			history := taskTestHistory(t, els)
			if len(els) != 1 || panel["expanded"] != false || taskTestTitle(els)["content"] != tc.label || len(history) != 1 || history[0].(map[string]any)["tag"] != "markdown" || len(taskTestFind(history, "tag", "collapsible_panel")) != 0 {
				t.Fatalf("terminal %s did not keep one collapsed root history panel: %+v", status, els)
			}
			if len(taskTestFind(els, "tag", "button")) != 0 {
				t.Fatalf("terminal %s unexpectedly exposed an action button", status)
			}
		})
	}
}

func TestTaskKeepsRepeatedCurrentRecordAndDoesNotMutateProjection(t *testing.T) {
	records := []ProgressRecord{{ItemID: "first", Text: "同一条公开动作"}, {ItemID: "latest", Text: "同一条公开动作"}}
	original := append([]ProgressRecord(nil), records...)
	card := Task(TaskView{Status: "inProgress", Progress: ProgressView{Body: "同一条公开动作", CurrentItemID: "latest", Records: records}})
	els := taskTestElements(t, card)
	history := taskTestHistory(t, els)
	if len(els) != 1 || len(history) != 2 || len(taskTestFind(history, "tag", "collapsible_panel")) != 0 || taskTestTitle(els)["content"] != "正在处理" {
		t.Fatal("a repeated current action removed a distinct native item or replaced the status title")
	}
	if !reflect.DeepEqual(records, original) {
		t.Fatal("rendering changed the complete records projection")
	}
	legacy := Task(TaskView{Status: "inProgress", Progress: ProgressView{Body: "相同正文", Records: []ProgressRecord{{Text: "相同正文"}, {Text: "相同正文"}}}})
	if len(taskTestHistory(t, taskTestElements(t, legacy))) != 2 {
		t.Fatal("legacy projection lost a repeated public record")
	}
}

func TestTaskLatestLongRecordSurvivesShortenedCurrentBody(t *testing.T) {
	full := strings.Repeat("公开动作", 300)
	body := string([]rune(full)[:800]) + "…（内容已截短）"
	card := Task(TaskView{Status: "inProgress", Progress: ProgressView{Body: body, CurrentItemID: "latest", Records: []ProgressRecord{{ItemID: "latest", Text: full}}}})
	els := taskTestElements(t, card)
	history := taskTestHistory(t, els)
	row := history[0].(map[string]any)
	if row["tag"] != "markdown" || row["text_size"] != "notation" || row["content"] != SafeMarkdown(full) || len(taskTestFind(history, "tag", "collapsible_panel")) != 0 || taskTestTitle(els)["content"] != "正在处理" || strings.Contains(taskTestTitle(els)["content"].(string), full) {
		t.Fatal("latest title or shortened Body replaced the complete public record")
	}
	if panel := taskTestHistoryPanel(t, els); panel["header"].(map[string]any)["title"].(map[string]any)["content"] != "正在处理" {
		t.Fatal("running card lost the real status label in its root history header")
	}
}

func TestTaskBodyFallbackAndShortRecordThreshold(t *testing.T) {
	for _, test := range []struct {
		name, text string
	}{{"single", "一条公开进度"}, {"limit", strings.Repeat("字", 120)}, {"long", strings.Repeat("字", 121)}, {"paragraphs", "第一段\n\n第二段"}, {"inline code", "运行 `go test`"}, {"code", "```go\nrun()\n```"}, {"list", "1. 保留列表"}, {"heading", "# 保留标题"}, {"quote", "> 保留引用"}, {"indented", "    保留代码"}, {"table", "| 列一 | 列二 |"}} {
		t.Run(test.name, func(t *testing.T) {
			card := Task(TaskView{Status: "inProgress", Body: test.text})
			rows := taskTestHistory(t, taskTestElements(t, card))
			if len(rows) != 1 {
				t.Fatal("Body-only call rendered more than one public record element")
			}
			row := rows[0].(map[string]any)
			if row["tag"] != "markdown" || row["text_size"] != "notation" || row["content"] != SafeMarkdown(test.text) || len(taskTestFind(rows, "tag", "collapsible_panel")) != 0 || strings.Contains(row["content"].(string), "时间未记录") {
				t.Fatal("Body-only call lost full content or fabricated a reception time")
			}
		})
	}
	for _, v := range []TaskView{{Status: "inProgress"}, {Status: "completed"}, {Status: "inProgress", Kind: "stop", State: "available", Token: "opaque", Revision: 1}} {
		card := Task(v)
		if len(taskTestFind(taskTestElements(t, card), "tag", "collapsible_panel")) != 0 {
			t.Fatal("empty task created an empty history disclosure")
		}
		if _, ok, err := WithRequestMarker(card, "cabcdefghijklmnopqrs"); err != nil || !ok {
			t.Fatal("empty task lost a delivery marker slot", err)
		}
	}
}

func TestTaskToolsKeepOrderSeparateOutcomesAndLiteralDetails(t *testing.T) {
	at := time.Date(2026, 10, 13, 14, 12, 3, 0, time.UTC).UnixMilli()
	records := []ProgressRecord{
		{ItemID: "start", Text: "先检查结构", ReceivedAtMS: at},
		{ItemID: "tools-1", Tools: &ProgressTools{Running: 1, Completed: 2, Failed: 3, Declined: 4, Unknown: 5, Details: []string{"<at id=\"bad\">", "[伪造链接](https://example.invalid)", "`工具`", "工具四", "工具五", "工具六", "不应展示第七项"}}},
		{ItemID: "next", Text: "再验证行为", ReceivedAtMS: at + 1000},
		{ItemID: "tools-2", Tools: &ProgressTools{Completed: 1}},
	}
	originalDetails := append([]string(nil), records[1].Tools.Details...)
	for _, status := range []string{"inProgress", "completed", "failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			card := Task(TaskView{Status: status, Progress: ProgressView{Records: records}})
			rows := taskTestHistory(t, taskTestElements(t, card))
			if len(rows) != 4 || !strings.Contains(rows[0].(map[string]any)["content"].(string), "先检查结构") || !strings.Contains(rows[2].(map[string]any)["content"].(string), "再验证行为") {
				t.Fatal("tool groups moved across native public messages")
			}
			tools := rows[1].(map[string]any)
			want := "<font color='grey'>调用 6 个工具 · 1 项进行中 · 3 项失败 · 4 项已拒绝 · 5 项状态未知</font>"
			if tools["tag"] != "markdown" || tools["content"] != want || tools["text_size"] != "notation" {
				t.Fatal("tool summary used the wrong called count or state wording")
			}
			if icon, ok := tools["icon"].(map[string]any); !ok || icon["tag"] != "standard_icon" || icon["token"] != "briefcase_outlined" || icon["color"] != "grey" {
				t.Fatalf("tool summary lost its native grey toolbox fallback: %+v", tools["icon"])
			}
			details := tools["content"].(string)
			if strings.Contains(details, "<at") || strings.Contains(card, "伪造链接") || strings.Contains(card, "工具四") || strings.Contains(card, "第七项") {
				t.Fatal("tool metadata was exposed in the rendered card")
			}
			if rows[3].(map[string]any)["content"] != "<font color='grey'>调用 1 个工具</font>" || strings.Contains(details, "时间") || strings.Contains(details, "\n") {
				t.Fatal("tool-only record used the old completion wording or fabricated details")
			}
			if !reflect.DeepEqual(records[1].Tools.Details, originalDetails) {
				t.Fatal("rendering mutated tool metadata in the projection")
			}
		})
	}
}

func TestTaskToolOnlyHistoryAndSingleStablePanel(t *testing.T) {
	for _, status := range []string{"inProgress", "completed", "failed", "interrupted"} {
		card := Task(TaskView{Status: status, Progress: ProgressView{Records: []ProgressRecord{{ItemID: "tools", Tools: &ProgressTools{Unknown: 1}}}}})
		rows := taskTestHistory(t, taskTestElements(t, card))
		if len(rows) != 1 || rows[0].(map[string]any)["content"] != "<font color='grey'>工具调用 · 1 项状态未知</font>" || strings.Contains(card, "时间未记录") {
			t.Fatal("tool-only task lost its history or invented a message reception time")
		}
		if icon, ok := rows[0].(map[string]any)["icon"].(map[string]any); !ok || icon["tag"] != "standard_icon" || icon["token"] != "briefcase_outlined" || icon["color"] != "grey" {
			t.Fatalf("tool-only task lost its native grey toolbox fallback: %+v", rows[0].(map[string]any)["icon"])
		}
		if _, ok, err := WithRequestMarker(card, "cabcdefghijklmnopqrs"); err != nil || !ok {
			t.Fatal("tool-only task lost its delivery marker slot", err)
		}
	}
	for _, tools := range []*ProgressTools{{Declined: 2}, {Declined: 2, Unknown: 1}} {
		rows := taskTestHistory(t, taskTestElements(t, Task(TaskView{Status: "completed", Progress: ProgressView{Records: []ProgressRecord{{ItemID: "tools", Tools: tools}}}})))
		content := rows[0].(map[string]any)["content"]
		want := "<font color='grey'>工具调用"
		if tools.Declined > 0 {
			want += " · 2 项已拒绝"
		}
		if tools.Unknown > 0 {
			want += " · 1 项状态未知"
		}
		if content != want+"</font>" {
			t.Fatalf("called=0 tool record fabricated a call count: got %v", content)
		}
	}
	records := []ProgressRecord{{ItemID: "one", Text: strings.Repeat("长", 121)}, {ItemID: "two", Text: strings.Repeat("长", 121)}, {ItemID: "tool", Tools: &ProgressTools{Completed: 1, Details: []string{"安全标签"}}}}
	for _, status := range []string{"inProgress", "completed"} {
		card := Task(TaskView{Status: status, Progress: ProgressView{Records: records}})
		rows := taskTestHistory(t, taskTestElements(t, card))
		if len(rows) != 3 || len(taskTestFind(rows, "tag", "collapsible_panel")) != 0 {
			t.Fatal("terminal update changed the root history body or complete records")
		}
		for n, value := range rows[:2] {
			row := value.(map[string]any)
			if row["tag"] != "markdown" || row["text_size"] != "notation" || row["content"] != SafeMarkdown(records[n].Text) {
				t.Fatal("public record was split or changed while rendering")
			}
		}
		if tool := rows[2].(map[string]any); tool["tag"] != "markdown" || tool["text_size"] != "notation" || tool["content"] != "<font color='grey'>调用 1 个工具</font>" {
			t.Fatal("tool record added a second disclosure layer or exposed details")
		}
		if strings.Contains(card, "⛭") || strings.Contains(card, "🧰") {
			t.Fatal("tool count row retained a legacy emoji prefix")
		}
		for _, value := range rows {
			if len(taskTestFind(value, "tag", "collapsible_panel")) != 0 {
				t.Fatal("record added a second disclosure layer")
			}
		}
	}
}

func TestTaskToolItemsRenderAsStableCollapsedGroupRows(t *testing.T) {
	titleText := "读取 `fixture.go` [原文] <at id=\"bad\">"
	records := []ProgressRecord{{ItemID: "tool-group", Tools: &ProgressTools{
		Completed: 1,
		Unknown:   1,
		Items: []ProgressTool{
			{ItemID: "read-a", Kind: "read", Status: "completed", Title: titleText, Details: []string{"完成 <at id=\"bad\">", "[恶意链接](https://example.invalid)", "`literal`"}},
			{ItemID: "read-b", Kind: "command", Status: "unknown", Title: titleText, Details: []string{"无法确认结果"}},
		},
	}}}
	original := taskTestCloneRecords(records)
	var previousIDs []string
	for _, status := range []string{"inProgress", "completed"} {
		t.Run(status, func(t *testing.T) {
			card := Task(TaskView{Status: status, Progress: ProgressView{Records: records}})
			els := taskTestElements(t, card)
			history := taskTestHistory(t, els)
			root := taskTestHistoryPanel(t, els)
			wantRootExpanded := status == "inProgress"
			if root["expanded"] != wantRootExpanded {
				t.Fatalf("root execution panel expansion changed with status %q: got=%v want=%v", status, root["expanded"], wantRootExpanded)
			}
			if strings.Contains(card, "⛭") || strings.Contains(card, "🧰") {
				t.Fatal("tool group retained a legacy emoji prefix")
			}
			groups := taskTestDirectPanels(history)
			if len(groups) != 1 {
				t.Fatalf("expected one root-level tool group panel, got %d", len(groups))
			}
			if len(taskTestFind(history, "tag", "collapsible_panel")) != 1 || len(taskTestFind(history, "tag", "column_set")) != 0 {
				t.Fatalf("tool group gained an outer wrapper or nested panel: %+v", history)
			}
			groupChildren := taskTestFoldedPanel(t, groups[0])
			if len(groupChildren) != 2 {
				t.Fatalf("same-title tools were merged or not rendered as individual rows: %d", len(groupChildren))
			}
			if groups[0]["margin"] != "0px" {
				t.Fatalf("tool group panel itself was shifted with its body: %+v", groups[0])
			}
			ids := taskTestPanelIDs(history)
			if len(ids) != 1 || len(taskTestFind(history, "tag", "collapsible_panel")) != 1 || !strings.HasPrefix(ids[0], "tools_") {
				t.Fatalf("expected one stable tools disclosure ID, got ids=%v", ids)
			}
			title := groups[0]["header"].(map[string]any)["title"].(map[string]any)
			if got := title["content"]; got != "<font color='grey'>调用 1 个工具 · 1 项状态未知</font>" {
				t.Fatalf("tool group title lost its aggregate state counts: %v", got)
			}
			if icon, ok := title["icon"].(map[string]any); !ok || icon["tag"] != "standard_icon" || icon["token"] != "briefcase_outlined" || icon["color"] != "grey" {
				t.Fatalf("tool group title lost its native grey toolbox fallback: %+v", title["icon"])
			}
			seen := map[string]bool{}
			for _, id := range ids {
				if id == "" || len(id) > 20 || seen[id] {
					t.Fatalf("panel IDs must be short and unique: %q in %v", id, ids)
				}
				seen[id] = true
			}
			if previousIDs == nil {
				previousIDs = append([]string(nil), ids...)
			} else if !reflect.DeepEqual(previousIDs, ids) {
				t.Fatalf("panel IDs changed across status update: before=%v after=%v", previousIDs, ids)
			}
			for n := range groupChildren {
				row := groupChildren[n].(map[string]any)
				if row["tag"] != "markdown" || row["text_size"] != "notation" || row["margin"] != "0px" {
					t.Fatalf("tool item lost its compact Markdown row: %+v", row)
				}
				if _, ok := row["element_id"]; ok || row["icon_position"] != nil || len(taskTestFind(row, "tag", "collapsible_panel")) != 0 {
					t.Fatalf("tool leaf gained an ID, disclosure arrow, or nested panel: %+v", row)
				}
				icon, ok := row["icon"].(map[string]any)
				if !ok || icon["tag"] != "standard_icon" || icon["color"] != "grey" {
					t.Fatalf("tool kind icon is missing or not grey: %+v", row)
				}
				wantIcon := "wiki-book_outlined"
				if n == 1 {
					wantIcon = "code_outlined"
				}
				if icon["token"] != wantIcon {
					t.Fatalf("tool kind icon lost its native token: got=%v want=%v", icon["token"], wantIcon)
				}
				wantTitle := "<font color='grey'>读取 \\`fixture.go\\` \\[原文\\] &lt;at id=\"bad\"&gt;"
				if n == 1 {
					wantTitle += " · 状态未知"
				}
				wantTitle += "</font>"
				if row["content"] != wantTitle {
					t.Fatalf("same-title tool %d lost its literal escaped title: got=%v want=%v", n, row["content"], wantTitle)
				}
				if strings.Contains(row["content"].(string), "状态：") || strings.Contains(card, "完成 <at id=\"bad\">") || strings.Contains(card, "[恶意链接](https://example.invalid)") || strings.Contains(card, "`literal`") || strings.Contains(card, "无法确认结果") {
					t.Fatalf("tool details or state body leaked into the leaf row: %q", row["content"])
				}
			}
			if !reflect.DeepEqual(records, original) {
				t.Fatal("nested tool rendering mutated the source projection")
			}
			marked, ok, err := WithRequestMarker(card, "cnestedtoolmarker")
			if err != nil || !ok {
				t.Fatalf("marker could not find a free Markdown element in nested tool card: marked=%v err=%v", ok, err)
			}
			var markedDoc map[string]any
			if err := json.Unmarshal([]byte(marked), &markedDoc); err != nil {
				t.Fatal(err)
			}
			if got := len(taskTestFind(markedDoc, "element_id", "cnestedtoolmarker")); got != 1 {
				t.Fatalf("nested marker was not unique after insertion: %d", got)
			}
		})
	}
}

func TestTaskToolIconsUseSyntheticKeysAndNativeFallback(t *testing.T) {
	tools := &ProgressTools{
		Completed: 5,
		Items: []ProgressTool{
			{ItemID: "read", Kind: "read", Status: "completed", Title: "读取 fixture.go"},
			{ItemID: "edit", Kind: "edit", Status: "completed", Title: "编辑 fixture.go"},
			{ItemID: "search", Kind: "search", Status: "completed", Title: "搜索 fixture.go"},
			{ItemID: "command", Kind: "command", Status: "completed", Title: "运行 Go 测试"},
			{ItemID: "generic", Kind: "other", Status: "completed", Title: "调用工具"},
		},
	}
	records := []ProgressRecord{{ItemID: "synthetic-group", Tools: tools}}
	original := taskTestCloneRecords(records)
	icons := TaskIcons{
		Toolbox: "synthetic-toolbox",
		Command: "synthetic-command",
		Read:    "synthetic-read",
		Edit:    "synthetic-edit",
		Search:  "synthetic-search",
		Generic: "synthetic-generic",
	}
	view := TaskView{Status: "completed", Icons: icons, Progress: ProgressView{Records: records}}
	card := Task(view)
	if strings.Contains(card, "![") || strings.ContainsAny(card, "⛭🧰") {
		t.Fatal("custom tool icons reintroduced Markdown image syntax or a legacy emoji prefix")
	}
	history := taskTestHistory(t, taskTestElements(t, card))
	groups := taskTestDirectPanels(history)
	if len(groups) != 1 {
		t.Fatalf("expected one synthetic tool group, got %d", len(groups))
	}
	group := groups[0]
	children := taskTestFoldedPanel(t, group)
	if len(children) != len(tools.Items) {
		t.Fatalf("synthetic tool group lost leaf rows: got %d want %d", len(children), len(tools.Items))
	}
	header := group["header"].(map[string]any)
	if icon := header["icon"].(map[string]any); icon["tag"] != "standard_icon" || icon["token"] != "down-small-ccm_outlined" {
		t.Fatalf("tool group lost its native chevron: %+v", header["icon"])
	}
	title := header["title"].(map[string]any)
	if title["tag"] != "markdown" || title["content"] != "<font color='grey'>调用 5 个工具</font>" || title["text_size"] != "notation" || title["text_color"] != nil {
		t.Fatalf("tool group did not use the grey Markdown title contract: %+v", title)
	}
	if icon := title["icon"].(map[string]any); icon["tag"] != "custom_icon" || icon["img_key"] != icons.Toolbox {
		t.Fatalf("tool group used the wrong synthetic toolbox key: %+v", title["icon"])
	}
	if _, ok := title["icon"].(map[string]any)["size"]; ok {
		t.Fatal("custom toolbox icon gained an unsupported size field")
	}
	wantKeys := []string{icons.Read, icons.Edit, icons.Search, icons.Command, icons.Generic}
	for n, value := range children {
		row := value.(map[string]any)
		icon, ok := row["icon"].(map[string]any)
		if !ok || icon["tag"] != "custom_icon" || icon["img_key"] != wantKeys[n] {
			t.Fatalf("synthetic leaf %d used the wrong custom icon: %+v", n, row["icon"])
		}
		if _, ok := icon["size"]; ok {
			t.Fatalf("synthetic leaf %d custom icon gained an unsupported size field", n)
		}
		if strings.Contains(row["content"].(string), "![") {
			t.Fatalf("synthetic leaf %d used Markdown image syntax", n)
		}
	}
	if len(taskTestFind(history, "tag", "column_set")) != 0 {
		t.Fatal("synthetic tool history gained a layout column")
	}
	if !reflect.DeepEqual(records, original) || view.Icons != icons {
		t.Fatal("tool icon rendering mutated the source progress or icon keys")
	}
	marked, ok, err := WithRequestMarker(card, "csyntheticicons")
	if err != nil || !ok {
		t.Fatalf("synthetic icon card lost its marker slot: marked=%v err=%v", ok, err)
	}
	var markedDoc map[string]any
	if err := json.Unmarshal([]byte(marked), &markedDoc); err != nil {
		t.Fatal(err)
	}
	if got := len(taskTestFind(markedDoc, "element_id", "csyntheticicons")); got != 1 {
		t.Fatalf("synthetic icon marker was not unique: %d", got)
	}

	fallbackCard := Task(TaskView{Status: "completed", Progress: ProgressView{Records: taskTestCloneRecords(records)}})
	fallbackHistory := taskTestHistory(t, taskTestElements(t, fallbackCard))
	fallbackGroups := taskTestDirectPanels(fallbackHistory)
	if len(fallbackGroups) != 1 {
		t.Fatalf("expected one native fallback tool group, got %d", len(fallbackGroups))
	}
	fallbackTitle := fallbackGroups[0]["header"].(map[string]any)["title"].(map[string]any)
	if icon := fallbackTitle["icon"].(map[string]any); icon["tag"] != "standard_icon" || icon["token"] != "briefcase_outlined" || icon["color"] != "grey" {
		t.Fatalf("tool group did not use the native grey toolbox fallback: %+v", fallbackTitle["icon"])
	}
	fallbackChildren := taskTestFoldedPanel(t, fallbackGroups[0])
	wantFallback := []string{"wiki-book_outlined", "edit_outlined", "search_outlined", "code_outlined", "briefcase_outlined"}
	if len(fallbackChildren) != len(wantFallback) {
		t.Fatalf("native fallback tool group lost leaf rows: got %d want %d", len(fallbackChildren), len(wantFallback))
	}
	for n, value := range fallbackChildren {
		icon, ok := value.(map[string]any)["icon"].(map[string]any)
		if !ok || icon["tag"] != "standard_icon" || icon["token"] != wantFallback[n] || icon["color"] != "grey" {
			t.Fatalf("native fallback leaf %d used the wrong icon: %+v", n, value.(map[string]any)["icon"])
		}
	}
	if strings.Contains(fallbackCard, "![") || strings.ContainsAny(fallbackCard, "⛭🧰") {
		t.Fatal("native fallback tool icons reintroduced Markdown image syntax or a legacy emoji prefix")
	}
}

func TestTaskStopAndToolHistoryAreRootSiblings(t *testing.T) {
	view := TaskView{
		Status: "inProgress", Kind: "stop", State: "available", Token: "opaque", Revision: 9,
		Progress: ProgressView{Records: []ProgressRecord{
			{ItemID: "public", Text: "公开正文"},
			{ItemID: "tools", Tools: &ProgressTools{Completed: 1, OmittedItems: 1, Items: []ProgressTool{{ItemID: "tool-1", Kind: "read", Status: "completed", Title: "读取 fixture.go"}}}},
		}},
	}
	original := taskTestCloneRecords(view.Progress.Records)
	var previousIDs []string
	for pass := 0; pass < 2; pass++ {
		card := Task(view)
		els := taskTestElements(t, card)
		if len(els) != 2 {
			t.Fatalf("stop card should have one top status row and one root history panel: %d", len(els))
		}
		stopRow := els[0].(map[string]any)
		if stopRow["tag"] != "column_set" || stopRow["flex_mode"] != "none" || stopRow["horizontal_spacing"] != "4px" || stopRow["vertical_align"] != "top" {
			t.Fatalf("stop control lost its leading status row: %+v", stopRow)
		}
		stopColumns := stopRow["columns"].([]any)
		if len(stopColumns) != 2 || stopColumns[0].(map[string]any)["width"] != "auto" || stopColumns[1].(map[string]any)["width"] != "weighted" || stopColumns[1].(map[string]any)["weight"] != float64(1) {
			t.Fatalf("stop control columns changed: %+v", stopRow)
		}
		stopButton := taskTestFind(stopRow, "tag", "button")
		if len(stopButton) != 1 || stopButton[0]["width"] != nil || stopButton[0]["margin"] != "0px" {
			t.Fatalf("stop button no longer fits its unpadded auto column: %+v", stopButton)
		}

		history := taskTestHistory(t, els)
		panel := taskTestHistoryPanel(t, els)
		if panel["expanded"] != true || panel["header"].(map[string]any)["title"].(map[string]any)["content"] != "执行中" {
			t.Fatalf("usable stop card did not keep an expanded execution panel: %+v", panel)
		}
		if len(history) != 2 || len(taskTestFind(history, "tag", "column_set")) != 0 {
			t.Fatalf("history is not made of direct public/tool panel children: %+v", history)
		}
		taskTestNoNegativeMargins(t, els)
		if len(taskTestFind(panel, "tag", "button")) != 0 || len(taskTestFind(stopRow, "element_id", "execution_history")) != 0 {
			t.Fatal("stop control leaked into the folded execution panel")
		}
		groups := taskTestDirectPanels(history)
		if len(groups) != 1 || groups[0]["margin"] != "0px" {
			t.Fatalf("tool group is not a direct root-level panel: %+v", groups)
		}
		leaves := taskTestFoldedPanel(t, groups[0])
		if len(leaves) != 2 {
			t.Fatalf("expected one tool leaf and one omission notice: %d", len(leaves))
		}
		groupTitle := groups[0]["header"].(map[string]any)["title"].(map[string]any)
		if groupTitle["content"] != "<font color='grey'>调用 1 个工具</font>" {
			t.Fatalf("tool group title lost its stable count: %+v", groupTitle)
		}
		if icon, ok := groupTitle["icon"].(map[string]any); !ok || icon["tag"] != "standard_icon" || icon["token"] != "briefcase_outlined" || icon["color"] != "grey" {
			t.Fatalf("tool group title lost its native grey toolbox fallback: %+v", groupTitle["icon"])
		}
		if strings.Contains(card, "⛭") || strings.Contains(card, "🧰") {
			t.Fatal("tool history retained a legacy emoji prefix")
		}
		ids := taskTestPanelIDs(history)
		if len(ids) != 1 || !strings.HasPrefix(ids[0], "tools_") {
			t.Fatalf("tool group lost its stable disclosure ID: %v", ids)
		}
		if pass == 0 {
			previousIDs = append([]string(nil), ids...)
		} else if !reflect.DeepEqual(previousIDs, ids) {
			t.Fatalf("re-render changed the tool group ID: before=%v after=%v", previousIDs, ids)
		}
		for n, value := range leaves {
			leaf := value.(map[string]any)
			if leaf["margin"] != "0px" {
				t.Fatalf("tool body row %d inherited a column offset: %v", n, leaf["margin"])
			}
		}
	}
	marked, ok, err := WithRequestMarker(Task(view), "cstopfoldmarker")
	if err != nil || !ok {
		t.Fatalf("folded execution panel prevented marker insertion: marked=%v err=%v", ok, err)
	}
	var markedDoc map[string]any
	if err := json.Unmarshal([]byte(marked), &markedDoc); err != nil {
		t.Fatal(err)
	}
	if got := len(taskTestFind(markedDoc, "element_id", "cstopfoldmarker")); got != 1 || len(taskTestFind(markedDoc, "tag", "button")) != 1 {
		t.Fatalf("marker or stop control was not unique after folding: marker=%d buttons=%d", got, len(taskTestFind(markedDoc, "tag", "button")))
	}
	if !reflect.DeepEqual(view.Progress.Records, original) {
		t.Fatal("root-level layout rendering mutated the source projection")
	}
}

func TestTaskToolComponentBudgetPreservesLatestDetailsAndStop(t *testing.T) {
	records := make([]ProgressRecord, MaxProgressRecords)
	for groupN := range records {
		items := make([]ProgressTool, MaxProgressToolItems)
		for itemN := range items {
			items[itemN] = ProgressTool{
				ItemID:  fmt.Sprintf("g%02di%02d", groupN, itemN),
				Kind:    "read",
				Status:  "completed",
				Title:   fmt.Sprintf("工具 %02d-%02d", groupN, itemN),
				Details: []string{fmt.Sprintf("详情 %02d-%02d", groupN, itemN)},
			}
		}
		records[groupN] = ProgressRecord{ItemID: fmt.Sprintf("group-%02d", groupN), Tools: &ProgressTools{Completed: MaxProgressToolItems, Items: items}}
	}
	original := taskTestCloneRecords(records)
	card := Task(TaskView{Status: "inProgress", Body: "最新正文", Kind: "stop", State: "available", Token: "opaque", Revision: 11, Progress: ProgressView{Records: records}})
	var doc map[string]any
	if err := json.Unmarshal([]byte(card), &doc); err != nil {
		t.Fatal(err)
	}
	if got := taskTestTagCount(doc, ""); got > 180 {
		t.Fatalf("worst-case tool card exceeded platform component budget: %d", got)
	}
	panel := taskTestHistoryPanel(t, taskTestElements(t, card))
	if got := taskComponentCount(panel["elements"]); got > taskHistoryComponentLimit {
		t.Fatalf("history panel exceeded its reserved component budget: %d", got)
	}
	if !strings.Contains(card, "另有") || !strings.Contains(card, "工具详情已省略") {
		t.Fatal("component truncation did not disclose omitted tool details")
	}
	if !strings.Contains(card, "工具 31-31") {
		t.Fatal("component truncation did not preserve the latest tool detail")
	}
	if len(taskTestFind(doc, "tag", "button")) != 1 {
		t.Fatal("component truncation dropped the stop control")
	}
	if !reflect.DeepEqual(records, original) {
		t.Fatal("component-budget rendering mutated the source projection")
	}
}

func TestTaskWidthAndAdaptiveHeaderDoNotChangeOtherCards(t *testing.T) {
	for _, card := range []string{Queue("queued", "opaque"), QueueInput("queued", "opaque", "queue", 1), Human("Codex", "public", []AttachmentView{{Kind: "file", Name: "fixture.txt"}})} {
		var doc map[string]any
		if json.Unmarshal([]byte(card), &doc) != nil {
			t.Fatal("invalid non-task card")
		}
		if doc["config"].(map[string]any)["width_mode"] == "fill" {
			t.Fatal("task fill width escaped into another template")
		}
		for _, panel := range taskTestFind(doc, "tag", "collapsible_panel") {
			header := panel["header"].(map[string]any)
			if header["width"] == "auto_when_fold" || header["icon_position"] == "follow_text" {
				t.Fatal("task header settings changed a shared disclosure")
			}
		}
	}
}

func TestTaskTitleUsesDefaultColorStatusWithoutRepeatingPublicBody(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"我先检查两个关键入口，再核对回调和真实卡片之间的绑定关系。后面这句话只留正文。", "我先检查两个关键入口，再核对回调和真实卡片之间的绑定关系。"},
		{"保留这段足够完整的第一行\n第二行仍在展开正文", "保留这段足够完整的第一行"},
		{"Read `fixture.go` first. The rest stays visible when expanded.", "Read `fixture.go` first."},
		{strings.Repeat("长", 121), strings.Repeat("长", 119) + "…"},
	} {
		card := Task(TaskView{Status: "inProgress", Progress: ProgressView{Body: "已缩短的兜底", CurrentItemID: "current", Records: []ProgressRecord{{ItemID: "current", Text: tc.text}}, ElapsedSeconds: 48}})
		els := taskTestElements(t, card)
		history := taskTestHistory(t, els)
		title := taskTestTitle(els)
		if len(els) != 1 || title["tag"] != "plain_text" || title["text_size"] != "notation" || title["text_color"] != nil || title["content"] != "正在处理" || strings.Contains(title["content"].(string), tc.want) || strings.Contains(title["content"].(string), "48") || strings.Contains(card, "正在处理 · 已用") {
			t.Fatal("running title did not stay a small default-color status label without a duplicate summary or duration")
		}
		row := history[0].(map[string]any)
		if row["content"] != SafeMarkdown(tc.text) || row["tag"] != "markdown" || row["text_size"] != "notation" {
			t.Fatal("full public Markdown did not remain in the expanded body")
		}
	}
}

func TestTaskLimitsRecordsWithoutChangingSourceAndReportsOmissions(t *testing.T) {
	records := make([]ProgressRecord, MaxProgressRecords+3)
	for n := range records {
		records[n] = ProgressRecord{ItemID: fmt.Sprint(n), Text: fmt.Sprintf("public %02d", n)}
	}
	original := append([]ProgressRecord(nil), records...)
	card := Task(TaskView{Status: "completed", Progress: ProgressView{Records: records, OmittedRecords: 2}})
	rows := taskTestHistory(t, taskTestElements(t, card))
	if len(rows) != MaxProgressRecords+1 || rows[0].(map[string]any)["content"] != "此前 5 条记录已省略，请回 Codex App 查看。" || rows[1].(map[string]any)["tag"] != "markdown" || rows[1].(map[string]any)["text_size"] != "notation" || rows[1].(map[string]any)["content"] != SafeMarkdown("public 03") {
		t.Fatal("record cap lost newest entries or its explicit omission notice")
	}
	if len(card)+MaxRequestMarkerBytes > DefaultMaxCardBytes || !reflect.DeepEqual(records, original) {
		t.Fatal("compact bounded history exceeded the byte budget or mutated its source")
	}
}

func TestTaskPendingUnknownAndUnprovenQueueHaveNoButtons(t *testing.T) {
	for _, v := range []TaskView{
		{Status: "inProgress", Kind: "stop", State: "pending", Token: "opaque", Revision: 2},
		{Status: "inProgress", Kind: "stop", State: "unknown", Token: "opaque", Revision: 2},
		{Status: "inProgress", Kind: "stop", State: "available", Reason: "verifying", Token: "opaque", Revision: 2},
		{Status: "interrupted", Kind: "continue", State: "available", QueueCount: -1, Token: "opaque", Revision: 2},
		{Status: "interrupted", Kind: "continue", State: "available", QueueCount: 0, Token: "opaque", Revision: 2},
		{Status: "completed", Kind: "stop", State: "available", Token: "opaque", Revision: 2},
		{Status: "failed", Kind: "stop", State: "available", Token: "opaque", Revision: 2},
		{Status: "inProgress", Kind: "continue", State: "available", QueueCount: 2, Token: "opaque", Revision: 2},
	} {
		v.Body = "公开进度"
		card := Task(v)
		if len(taskTestFind(taskTestElements(t, card), "tag", "button")) != 0 {
			t.Fatalf("unavailable native state has a button: %+v", v)
		}
		if v.State == "pending" && !strings.Contains(card, "正在停止") || v.State == "unknown" && !strings.Contains(card, "正在核验") || v.Status == "interrupted" && v.QueueCount < 0 && !strings.Contains(card, "队列状态待核验") {
			t.Fatal("card claimed a more certain outcome than its state")
		}
	}
	card := Task(TaskView{Status: "interrupted", Kind: "continue", State: "available", QueueCount: 3, Token: "opaque", Revision: 8, Body: "公开进度", Progress: ProgressView{ElapsedSeconds: 102}})
	els := taskTestElements(t, card)
	if len(els) != 2 || els[1].(map[string]any)["tag"] != "button" || taskTestTitle(els)["content"] != "已停止 · 3 条消息待执行 · 用时 1 分 42 秒" || !strings.Contains(card, `"action":"continue"`) || !strings.Contains(card, `"revision":8`) {
		t.Fatal("known App queue did not retain its top-level Continue control")
	}
}
