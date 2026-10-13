package cards

import (
	"encoding/json"
	"strings"
	"testing"
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

func TestCompletedTaskRetainsCollapsedPublicHistory(t *testing.T) {
	base := Task(TaskView{Status: "completed", Progress: ProgressView{Records: []string{"已检查模板", "已验证布局"}, ElapsedSeconds: 102}})
	marked, ok, err := WithRequestMarker(base, "cabcdefghijklmnopqrs")
	if err != nil || !ok || len(marked)-len(base) != MaxRequestMarkerBytes {
		t.Fatal("collapsed task did not reserve its exact delivery marker budget", err)
	}
	var doc struct {
		Body struct {
			Elements []struct {
				Tag       string
				ElementID string `json:"element_id"`
				Expanded  bool
				Header    struct{ Title struct{ Content string } }
				Elements  []struct {
					Content   string
					ElementID string `json:"element_id"`
				}
			} `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(marked), &doc); err != nil {
		t.Fatal(err)
	}
	els := doc.Body.Elements
	if len(els) != 1 || els[0].Tag != "collapsible_panel" || els[0].ElementID != "execution_history" || els[0].Expanded || els[0].Header.Title.Content != "已完成 · 用时 1 分 42 秒" {
		t.Fatal("completed execution did not collapse to a single reviewable line")
	}
	if !strings.Contains(els[0].Elements[0].Content, "已检查模板") || !strings.Contains(els[0].Elements[0].Content, "已验证布局") || els[0].Elements[0].ElementID != "cabcdefghijklmnopqrs" {
		t.Fatal("terminal card lost public history or its hidden delivery marker")
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
