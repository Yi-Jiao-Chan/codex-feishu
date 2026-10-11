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
	if body["padding"] != "8px 12px" || body["vertical_spacing"] != "4px" {
		t.Fatal("progress lost the confirmed compact layout")
	}
	els := body["elements"].([]any)
	for _, e := range els[:2] {
		if e.(map[string]any)["text_size"] != "notation" {
			t.Fatal("progress text is not notation")
		}
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
