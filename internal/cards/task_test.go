package cards

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTaskUsesCompactNotationAndOnlyScopedOpaqueCallback(t *testing.T) {
	card := Task(TaskView{Status: "inProgress", Body: "public <at id=\"private\">", Token: "opaque", Revision: 7, Kind: "stop", State: "available", QueueCount: -1, Waiting: true})
	var doc map[string]any
	if err := json.Unmarshal([]byte(card), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["header"]; ok {
		t.Fatal("progress gained a header")
	}
	body := doc["body"].(map[string]any)
	if body["padding"] != "12px 12px" || body["vertical_spacing"] != "8px" {
		t.Fatal("progress lost the confirmed compact layout")
	}
	els := body["elements"].([]any)
	if els[0].(map[string]any)["text_size"] != "notation" || els[1].(map[string]any)["text_size"] != "normal" {
		t.Fatal("status and public body lost their distinct hierarchy")
	}
	button := els[2].(map[string]any)
	if button["tag"] != "button" || button["type"] != "text" || button["size"] != "small" {
		t.Fatal("task button lost its compact native styling")
	}
	value := button["behaviors"].([]any)[0].(map[string]any)["value"].(map[string]any)
	if len(value) != 3 || value["action"] != "stop" || value["token"] != "opaque" || value["revision"] != float64(7) {
		t.Fatal("callback includes untrusted routing/body values or wrong revision")
	}
	if !strings.Contains(card, "请回 Codex App 处理审批或问题") || strings.Contains(card, "<at") || strings.Contains(card, "approve") || strings.Contains(card, "answer") {
		t.Fatal("waiting text answered a native request or exposed a mention")
	}
}

func TestTaskRecordsUseSmallIndependentCollapsedPreviewsInEveryState(t *testing.T) {
	text := "第一段很长的公开执行记录，应该在单行摘要中省略而且保留全文。\n\n第二段\n```go\nfmt.Println(\"public\")\n```"
	at := time.Date(2026, 10, 13, 14, 12, 3, 0, time.UTC).UnixMilli()
	for _, status := range []string{"inProgress", "completed", "interrupted", "failed"} {
		t.Run(status, func(t *testing.T) {
			card := Task(TaskView{Status: status, Progress: ProgressView{
				Records:  []ProgressRecord{{ItemID: "public-1", Text: text, ReceivedAtMS: at}, {ItemID: "public-2", Text: "同一条摘要", ReceivedAtMS: at + 1000}},
				TimeZone: "Asia/Shanghai", RunningTools: 2, FinishedTools: 9,
			}})
			if strings.Contains(card, "工具：") || strings.Contains(card, "项已结束") {
				t.Fatal("meaningless tool counts remain visible")
			}
			var doc map[string]any
			if err := json.Unmarshal([]byte(card), &doc); err != nil {
				t.Fatal(err)
			}
			els := doc["body"].(map[string]any)["elements"].([]any)
			outer := els[len(els)-1].(map[string]any)
			title := outer["header"].(map[string]any)["title"].(map[string]any)
			if title["text_size"] != "notation" || title["text_color"] != "grey" || outer["vertical_spacing"] != "4px" {
				t.Fatal("history title lost explicit small native type or compact spacing")
			}
			rows := outer["elements"].([]any)
			if len(rows) != 2 {
				t.Fatal("public history was merged into paragraphs")
			}
			ids := map[string]bool{}
			for n, value := range rows {
				row := value.(map[string]any)
				if row["tag"] != "collapsible_panel" || row["expanded"] != false {
					t.Fatal("record did not retain its individual closed disclosure")
				}
				id := row["element_id"].(string)
				if ids[id] {
					t.Fatal("different native records share a panel identity")
				}
				ids[id] = true
				heading := row["header"].(map[string]any)["title"].(map[string]any)
				preview := heading["content"].(string)
				if strings.ContainsAny(preview, "\r\n") || !strings.HasPrefix(preview, "22:12:0") || heading["text_size"] != "notation" {
					t.Fatal("record preview is multiline or lost its configured-time-zone timestamp")
				}
				full := row["elements"].([]any)[0].(map[string]any)
				if full["margin"] != "0px" || full["text_size"] != "notation" || n == 0 && (full["content"] != text || !strings.HasSuffix(preview, "…")) {
					t.Fatal("preview truncation altered full Markdown or reintroduced loose record spacing")
				}
			}
		})
	}
}

func TestCompletedTaskRetainsCollapsedPublicHistory(t *testing.T) {
	base := Task(TaskView{Status: "completed", Progress: ProgressView{Body: "已验证布局", CurrentItemID: "b", Records: []ProgressRecord{{ItemID: "a", Text: "已检查模板"}, {ItemID: "b", Text: "已验证布局"}}, ElapsedSeconds: 102}})
	marked, ok, err := WithRequestMarker(base, "cabcdefghijklmnopqrs")
	if err != nil || !ok || len(marked)-len(base) != MaxRequestMarkerBytes {
		t.Fatal("collapsed task did not reserve its exact delivery marker budget", err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(marked), &doc); err != nil {
		t.Fatal(err)
	}
	els := doc["body"].(map[string]any)["elements"].([]any)
	panel := els[0].(map[string]any)
	title := panel["header"].(map[string]any)["title"].(map[string]any)
	if len(els) != 1 || panel["tag"] != "collapsible_panel" || panel["element_id"] != "execution_history" || panel["expanded"] != false || title["content"] != "已完成 · 用时 1 分 42 秒" {
		t.Fatal("completed execution did not collapse to a single reviewable line")
	}
	children := panel["elements"].([]any)
	for n, expected := range []string{"已检查模板", "已验证布局"} {
		row := children[n].(map[string]any)
		if row["tag"] != "collapsible_panel" || row["expanded"] != false {
			t.Fatal("completed history record is not individually collapsed")
		}
		text := row["elements"].([]any)[0].(map[string]any)
		if text["content"] != expected || n == 0 && text["element_id"] != "cabcdefghijklmnopqrs" {
			t.Fatal("terminal card lost full history or its nested delivery marker")
		}
	}

}

func TestTaskSeparatesCurrentProgressFromRunningHistory(t *testing.T) {
	records := []ProgressRecord{
		{ItemID: "first", Text: "同一条公开动作"},
		{ItemID: "latest", Text: "同一条公开动作"},
	}
	original := append([]ProgressRecord(nil), records...)
	card := Task(TaskView{Status: "inProgress", Progress: ProgressView{
		Body: "同一条公开动作", CurrentItemID: "latest", Records: records,
	}})
	var doc map[string]any
	if err := json.Unmarshal([]byte(card), &doc); err != nil {
		t.Fatal(err)
	}
	els := doc["body"].(map[string]any)["elements"].([]any)
	if len(els) != 3 {
		t.Fatalf("running card should contain status, current body and history: %s", card)
	}
	history := els[2].(map[string]any)
	rows := history["elements"].([]any)
	if len(rows) != 1 {
		t.Fatalf("current record was not excluded by ID while the earlier identical record was retained: %s", card)
	}
	if !reflect.DeepEqual(records, original) {
		t.Fatal("rendering changed the complete records projection")
	}

	single := Task(TaskView{Status: "inProgress", Progress: ProgressView{
		Body: "同一条公开动作", CurrentItemID: "latest", Records: records[1:],
	}})
	if strings.Contains(single, `"tag":"collapsible_panel"`) {
		t.Fatal("a single current progress record created an empty history panel")
	}
}

func TestTaskLegacyCurrentFallbackOnlyRemovesLastMatchingRecord(t *testing.T) {
	records := []ProgressRecord{{Text: "相同正文"}, {Text: "相同正文"}}
	card := Task(TaskView{Status: "inProgress", Progress: ProgressView{Body: "相同正文", Records: records}})
	var doc map[string]any
	if err := json.Unmarshal([]byte(card), &doc); err != nil {
		t.Fatal(err)
	}
	els := doc["body"].(map[string]any)["elements"].([]any)
	if len(els) != 3 || len(els[2].(map[string]any)["elements"].([]any)) != 1 {
		t.Fatalf("legacy fallback removed more than the last matching record: %s", card)
	}
}

func TestTaskCurrentIDExclusionSurvivesTruncatedBody(t *testing.T) {
	full := strings.Repeat("公开动作", 300)
	body := string([]rune(full)[:800]) + "…（内容已截短）"
	card := Task(TaskView{Status: "inProgress", Progress: ProgressView{
		Body: body, CurrentItemID: "latest", Records: []ProgressRecord{{ItemID: "latest", Text: full}},
	}})
	if strings.Contains(card, `"tag":"collapsible_panel"`) {
		t.Fatal("truncated current body failed to exclude its full record by ID")
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
		{Status: "inProgress", Kind: "continue", State: "available", QueueCount: 2, Token: "opaque", Revision: 2},
	} {
		card := Task(v)
		if strings.Contains(card, `"tag":"button"`) {
			t.Fatalf("unavailable native state has a button: %+v", v)
		}
		if v.State == "pending" && !strings.Contains(card, "正在停止") || v.State == "unknown" && !strings.Contains(card, "正在核验") || v.Status == "interrupted" && v.QueueCount < 0 && !strings.Contains(card, "队列状态待核验") {
			t.Fatal("card claimed a more certain outcome than its state")
		}
	}
	card := Task(TaskView{Status: "interrupted", Kind: "continue", State: "available", QueueCount: 3, Token: "opaque", Revision: 8})
	if !strings.Contains(card, "3 条消息待执行") || !strings.Contains(card, `"action":"continue"`) || !strings.Contains(card, `"revision":8`) {
		t.Fatal("known App queue did not render its exact Continue control")
	}
}
