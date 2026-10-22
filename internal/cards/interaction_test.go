package cards

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestInteractionApprovalUsesNativeLayoutAndScopedCallback(t *testing.T) {
	card := Interaction(InteractionView{
		Kind:     "command",
		State:    "pending",
		Reason:   "需要执行验证命令",
		CWD:      "/workspace/project",
		Command:  "go test ./...",
		Token:    "invalid-example-token",
		Revision: 7,
		Decisions: []InteractionDecision{
			{Value: "accept", Label: "允许本次", Description: "仅允许当前请求"},
			{Value: "decline", Label: "拒绝", Description: "拒绝当前请求"},
		},
		Details: []InteractionDetail{{Title: "权限范围", Content: "仅访问工作目录"}},
		Enabled: true,
	})
	doc := decodeInteraction(t, card)
	if doc["schema"] != "2.0" {
		t.Fatalf("schema = %v", doc["schema"])
	}
	config := doc["config"].(map[string]any)
	if config["update_multi"] != true || config["enable_forward"] != false || config["enable_forward_interaction"] != false || config["width_mode"] != "compact" {
		t.Fatalf("unexpected config: %#v", config)
	}
	if _, ok := doc["header"]; ok {
		t.Fatal("interaction card gained a colored/header section")
	}
	body := doc["body"].(map[string]any)
	if body["padding"] != "12px 12px" || body["vertical_spacing"] != "8px" {
		t.Fatalf("unexpected body layout: %#v", body)
	}
	els := body["elements"].([]any)
	if els[0].(map[string]any)["text_size"] != "normal" || els[0].(map[string]any)["content"] != "**允许执行这条命令？**" {
		t.Fatalf("approval title = %#v", els[0])
	}
	if els[1].(map[string]any)["text_size"] != "notation" {
		t.Fatal("status is not notation-sized")
	}
	if els[2].(map[string]any)["text_size"] != "normal" || !strings.Contains(card, "需要执行验证命令") {
		t.Fatal("reason did not remain visible as normal body text")
	}
	if !strings.Contains(card, "```bash\\ngo test ./...") || !strings.Contains(card, "工作目录：/workspace/project") || !strings.Contains(card, "权限范围") || !strings.Contains(card, "允许当前请求") {
		t.Fatal("approval body lost command/details/decision description")
	}

	buttonValues := interactionButtonValues(t, doc)
	if len(buttonValues) != 2 {
		t.Fatalf("button count = %d", len(buttonValues))
	}
	for _, value := range buttonValues {
		if value["action"] != "interaction_submit" || value["token"] != "invalid-example-token" || value["revision"] != float64(7) {
			t.Fatalf("unscoped callback: %#v", value)
		}
		decision, ok := value["decision"].(string)
		if !ok || (decision != "accept" && decision != "decline") {
			t.Fatalf("unexpected decision: %#v", value)
		}
	}
	last := els[len(els)-1].(map[string]any)
	if last["text_size"] != "notation" || last["content"] != "> 审批请求 · Codex" {
		t.Fatal("approval source is not the final notation quote")
	}
}

func TestInteractionBudgetsLeaveRoomForWireMarkerAndMention(t *testing.T) {
	view := InteractionView{Kind: "question", State: "pending", Enabled: true, Token: "invalid", Revision: 1,
		Reason: strings.Repeat("较长的原生问题背景。", 180), MaxCardBytes: 6000,
		Questions: []InteractionQuestion{{Field: "q_0", Question: "请选择", Other: true, Options: []InteractionOption{{Value: "0", Label: "保留"}, {Value: "1", Label: "调整"}}}}}
	base := Interaction(view)
	if len(base) > view.MaxCardBytes {
		t.Fatal("renderer ignored reserved wire budget")
	}
	mentioned, err := WithMentions(base, []string{"ou_fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := WithRequestMarker(mentioned, "c1234567890123456789"); err != nil || !ok {
		t.Fatal("wire transforms exceeded the total card limit", err)
	}
	for i := 0; i < 24; i++ {
		view.Questions = append(view.Questions, InteractionQuestion{Field: fmt.Sprintf("q_%d", i+1), Question: "合成问题", Options: []InteractionOption{{Value: "0", Label: "A", Description: "说明"}, {Value: "1", Label: "B", Description: "说明"}, {Value: "2", Label: "C", Description: "说明"}}})
	}
	large := Interaction(view)
	if interactionComponentCount(large) > 200 || strings.Contains(large, "interaction_submit") || !strings.Contains(large, "内容超出卡片展示范围") {
		t.Fatal("oversized form was sent partially or beyond component limit")
	}
}

func TestLongDetailPathRetainsFullScopeAfterExpanding(t *testing.T) {
	path := "/workspace/" + strings.Repeat("subdirectory/", 9) + "file.go"
	view := InteractionView{Kind: "file", State: "pending", Enabled: true, Token: "invalid", Revision: 1,
		Details: []InteractionDetail{{Title: path, Content: "完整文件差异"}}, Decisions: []InteractionDecision{{Value: "accept", Label: "允许本次"}}}
	card := Interaction(view)
	if !strings.Contains(card, path) || !strings.Contains(card, "interaction_submit") || strings.Contains(card, "内容超出卡片展示范围") {
		t.Fatal("long scope path was silently truncated or disabled despite complete expanded content")
	}
}

func TestInteractionControlsRequirePendingAndAllCredentials(t *testing.T) {
	cases := []InteractionView{
		{Kind: "command", State: "pending", Enabled: false, Token: "invalid", Revision: 1, Decisions: []InteractionDecision{{Value: "accept", Label: "允许"}}},
		{Kind: "command", State: "pending", Enabled: true, Token: "", Revision: 1, Decisions: []InteractionDecision{{Value: "accept", Label: "允许"}}},
		{Kind: "command", State: "pending", Enabled: true, Token: "invalid", Revision: 0, Decisions: []InteractionDecision{{Value: "accept", Label: "允许"}}},
		{Kind: "command", State: "claimed", Enabled: true, Token: "invalid", Revision: 1, Decisions: []InteractionDecision{{Value: "accept", Label: "允许"}}},
		{Kind: "question", State: "resolved", Enabled: true, Token: "invalid", Revision: 1, Questions: []InteractionQuestion{{Field: "q_0", Question: "已处理"}}},
	}
	for _, v := range cases {
		card := Interaction(v)
		if strings.Contains(card, `"tag":"button"`) || strings.Contains(card, `"tag":"form"`) {
			t.Fatalf("unauthorized/non-pending view gained controls: %+v", v)
		}
	}

	processed := Interaction(InteractionView{
		Kind:    "command",
		State:   "resolved",
		Summary: "请求已处理",
		Details: []InteractionDetail{{Title: "完整命令", Content: "echo retained"}},
		Enabled: true, Token: "invalid", Revision: 2,
		Decisions: []InteractionDecision{{Value: "accept", Label: "允许"}},
	})
	if !strings.Contains(processed, "echo retained") || !strings.Contains(processed, "请求已处理") || strings.Contains(processed, "审批成功") {
		t.Fatal("processed receipt lost details or introduced an approval-success claim")
	}
}

func TestInteractionAsyncQueuedReusesQueueControls(t *testing.T) {
	card := Interaction(InteractionView{
		Kind:   "async_question",
		State:  "queued",
		Reason: "回答已保存",
		Queue:  &QueueView{State: "queued", Token: "queue-token", QueueID: "native-q", Revision: 17, QueueOrdinal: 3, Active: true},
	})
	if !strings.Contains(card, "回答已排队") || !strings.Contains(card, "第 3 条") {
		t.Fatalf("queued async status lost queue ordinal: %s", card)
	}
	doc := decodeInteraction(t, card)
	callbacks := interactionQueueCallbacks(t, doc)
	if len(callbacks) != 2 {
		t.Fatalf("queued async controls = %#v", callbacks)
	}
	seen := map[string]bool{}
	for _, callback := range callbacks {
		action, _ := callback["action"].(string)
		seen[action] = true
		if callback["token"] != "queue-token" || callback["queue_id"] != "native-q" || callback["revision"] != float64(17) {
			t.Fatalf("queue callback identity = %#v", callback)
		}
	}
	if !seen["steer"] || !seen["remove"] {
		t.Fatalf("queue callbacks = %#v", seen)
	}
}

func TestInteractionAsyncQueueControlsRequireLiveCredentials(t *testing.T) {
	card := Interaction(InteractionView{
		Kind:  "async_question",
		State: "queued",
		Queue: &QueueView{State: "queued", QueueID: "native-q", Revision: 17, Active: false},
	})
	if strings.Contains(card, `"tag":"button"`) || strings.Contains(card, `"tag":"form"`) {
		t.Fatalf("offline/inactive async queue gained controls: %s", card)
	}
}

func TestInteractionAsyncQueueControlsDisappearForTerminalOrUnknownQueue(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		queue       *QueueView
	}{
		{name: "answered", state: "resolved"},
		{name: "removed", state: "removed", queue: &QueueView{State: "removed", Token: "queue-token", QueueID: "native-q", Revision: 17}},
		{name: "unknown", state: "unknown", queue: &QueueView{State: "unknown", Token: "queue-token", QueueID: "native-q", Revision: 17}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			card := Interaction(InteractionView{Kind: "async_question", State: tc.state, Queue: tc.queue})
			if strings.Contains(card, `"tag":"button"`) || strings.Contains(card, `"tag":"form"`) {
				t.Fatalf("%s async queue retained stale controls: %s", tc.name, card)
			}
		})
	}
}

func TestInteractionAsyncQueueControlsAreReadOnlyWhenLimited(t *testing.T) {
	card := Interaction(InteractionView{
		Kind:         "async_question",
		State:        "queued",
		Reason:       strings.Repeat("超长回答背景。", 1000),
		MaxCardBytes: 2048,
		Queue:        &QueueView{State: "queued", Token: "queue-token", QueueID: "native-q", Revision: 17, Active: true},
	})
	if strings.Contains(card, `"tag":"button"`) || strings.Contains(card, `"tag":"form"`) {
		t.Fatalf("limited async queue retained actionable controls: %s", card)
	}
	if !strings.Contains(card, "内容超出卡片展示范围") {
		t.Fatal("limited async queue omitted read-only warning")
	}
}

func TestInteractionQuestionReceiptUsesRecordedAnswersAndCollapsedDetails(t *testing.T) {
	card := Interaction(InteractionView{
		Kind: "question", State: "resolved", AnswerStatus: "recorded", Reason: "保留解释性原因",
		Questions: []InteractionQuestion{
			{
				Header: "布局", Question: "采用哪种布局？", Answers: []string{"保留布局"},
				Options: []InteractionOption{
					{Value: "keep", Label: "保留布局", Description: "保留现有布局"},
					{Value: "change", Label: "调整布局", Description: "未选择的调整说明"},
				},
			},
			{Header: "说明", Question: "请提供补充说明", Answers: []string{"无需修改"}, Options: []InteractionOption{{Value: "yes", Label: "继续", Description: "继续当前流程"}}},
		},
		Details: []InteractionDetail{{Title: "来源详情", Content: "保留原始来源详情"}},
	})
	doc := decodeInteraction(t, card)
	elements := doc["body"].(map[string]any)["elements"].([]any)
	if got := elements[0].(map[string]any)["content"]; got != "已回答 · 2 项" {
		t.Fatalf("recorded status = %#v", got)
	}
	var outside strings.Builder
	var questionDetails, sourceDetails map[string]any
	for _, raw := range elements {
		element, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if element["tag"] == "collapsible_panel" {
			header := element["header"].(map[string]any)["title"].(map[string]any)
			switch header["content"] {
			case "问题详情":
				questionDetails = element
			case "来源详情":
				sourceDetails = element
			}
			continue
		}
		walkInteractionJSON(element, func(child map[string]any) {
			if content, ok := child["content"].(string); ok {
				outside.WriteString(content)
			}
		})
	}
	if questionDetails == nil || questionDetails["expanded"] != false {
		t.Fatalf("question details panel = %#v", questionDetails)
	}
	if sourceDetails == nil || !strings.Contains(card, "保留原始来源详情") {
		t.Fatal("source details were lost from the receipt")
	}
	visible := outside.String()
	if !strings.Contains(visible, "**采用哪种布局？**") || !strings.Contains(visible, "保留布局") || !strings.Contains(visible, "**请提供补充说明**") || !strings.Contains(visible, "无需修改") {
		t.Fatalf("compact receipt lost question/answer text: %q", visible)
	}
	if strings.Contains(visible, "未选择的调整说明") || strings.Contains(visible, "继续当前流程") {
		t.Fatalf("unselected option descriptions leaked outside details: %q", visible)
	}
	if strings.Contains(card, `"tag":"button"`) || strings.Contains(card, `"tag":"form"`) {
		t.Fatal("resolved question receipt retained controls")
	}
}

func TestInteractionQuestionDetailsRetainsLongOptionLabel(t *testing.T) {
	label := strings.Repeat("长选项标签", 18)
	card := Interaction(InteractionView{
		Kind: "question", State: "resolved",
		Questions: []InteractionQuestion{{Question: "选择处理方式", Options: []InteractionOption{{Label: label, Description: "完整选项说明"}}}},
	})
	doc := decodeInteraction(t, card)
	elements := doc["body"].(map[string]any)["elements"].([]any)
	var details map[string]any
	for _, raw := range elements {
		element, ok := raw.(map[string]any)
		if !ok || element["tag"] != "collapsible_panel" {
			continue
		}
		header := element["header"].(map[string]any)["title"].(map[string]any)
		if header["content"] == "问题详情" {
			details = element
			break
		}
	}
	if details == nil || details["expanded"] != false {
		t.Fatalf("question details panel = %#v", details)
	}
	if !strings.Contains(string(mustJSON(t, details)), label) {
		t.Fatal("long option label was clipped inside the collapsed question details")
	}
	if strings.Contains(string(mustJSON(t, details)), "选项内容已截短") {
		t.Fatal("long option label was marked truncated despite fitting the display budget")
	}
}

func TestInteractionUnknownQuestionReceiptDoesNotGuessSelection(t *testing.T) {
	card := Interaction(InteractionView{
		Kind: "question", State: "unknown", Reason: "回答归属仍在核验",
		Questions: []InteractionQuestion{{Question: "选择处理方式", Options: []InteractionOption{
			{Label: "保留", Description: "保留选项说明"},
			{Label: "调整", Description: "调整选项说明"},
		}}},
	})
	doc := decodeInteraction(t, card)
	elements := doc["body"].(map[string]any)["elements"].([]any)
	if got := elements[0].(map[string]any)["content"]; got != "正在核验 · 请勿重复提交" {
		t.Fatalf("unknown status = %#v", got)
	}
	var visible strings.Builder
	for _, raw := range elements {
		element, ok := raw.(map[string]any)
		if !ok || element["tag"] == "collapsible_panel" {
			continue
		}
		if content, ok := element["content"].(string); ok {
			visible.WriteString(content)
		}
	}
	if !strings.Contains(visible.String(), "**选择处理方式**") || strings.Contains(visible.String(), "保留选项说明") || strings.Contains(visible.String(), "调整选项说明") {
		t.Fatalf("unknown receipt guessed or leaked an option: %q", visible.String())
	}
	if strings.Contains(card, `"tag":"button"`) || strings.Contains(card, `"tag":"form"`) || strings.Contains(card, "已提交的回答") {
		t.Fatal("unknown question receipt exposed an answer or control")
	}
}

func TestInteractionSubmittedQuestionReceiptDoesNotClaimRecordedAnswer(t *testing.T) {
	card := Interaction(InteractionView{
		Kind: "question", State: "resolved", AnswerStatus: "submitted",
		Questions: []InteractionQuestion{{Question: "是否继续？", Answers: []string{"继续"}, Options: []InteractionOption{{Label: "继续", Description: "继续说明"}}}},
	})
	doc := decodeInteraction(t, card)
	status := doc["body"].(map[string]any)["elements"].([]any)[0].(map[string]any)["content"]
	if status != "请求已处理" {
		t.Fatalf("submitted evidence changed resolved status: %#v", status)
	}
	if !strings.Contains(card, "已提交的回答") || strings.Contains(card, "已回答 ·") {
		t.Fatalf("submitted evidence was presented as recorded: %s", card)
	}
}

func TestInteractionLongQuestionAnswerIsBoundedAndReadOnly(t *testing.T) {
	card := Interaction(InteractionView{
		Kind: "question", State: "unknown", AnswerStatus: "submitted", MaxCardBytes: 3500,
		Questions: []InteractionQuestion{{Question: "请说明原因", Answers: []string{strings.Repeat("很长的回答。", 3000)}}},
	})
	if len(card) > 3500 || interactionComponentCount(card) > 200 {
		t.Fatalf("long answer exceeded renderer budget: bytes=%d components=%d", len(card), interactionComponentCount(card))
	}
	assertInteractionReadOnly(t, card)
	if !strings.Contains(card, "回答内容已截短") || !strings.Contains(card, `"tag":"collapsible_panel"`) {
		t.Fatal("long answer had no collapsed disclosure or explicit truncation note")
	}
}

func TestInteractionQuestionFormIsTopLevelAndNamesRemainUnique(t *testing.T) {
	card := Interaction(InteractionView{
		Kind:   "question",
		State:  "pending",
		Reason: "请完成本轮问答",
		Token:  "invalid-question-token", Revision: 3, Enabled: true,
		Questions: []InteractionQuestion{
			{Field: "q_0", Header: "布局", Question: "采用哪种布局？", Other: true, Options: []InteractionOption{
				{Value: "choice_0", Label: "保留布局", Description: "保留现有布局"},
				{Value: "choice_1", Label: "调整布局", Description: "同时调整正文"},
			}},
			{Field: "q_1", Header: "说明", Question: "请提供补充说明", Other: false},
		},
	})
	doc := decodeInteraction(t, card)
	els := doc["body"].(map[string]any)["elements"].([]any)
	var forms []map[string]any
	for _, raw := range els {
		if element, ok := raw.(map[string]any); ok && element["tag"] == "form" {
			forms = append(forms, element)
		}
	}
	if len(forms) != 1 || forms[0]["name"] != interactionAnswersName {
		t.Fatalf("top-level forms = %#v", forms)
	}
	form := forms[0]
	if doc["config"].(map[string]any)["width_mode"] != "compact" {
		t.Fatalf("question card is not compact: %#v", doc["config"])
	}
	names := map[string]bool{}
	var selectSeen, inputSeen, submitSeen bool
	walkInteractionJSON(form["elements"], func(element map[string]any) {
		if name, ok := element["name"].(string); ok {
			if names[name] {
				t.Fatalf("duplicate form name %q", name)
			}
			names[name] = true
		}
		switch element["tag"] {
		case "select_static":
			selectSeen = true
			if element["name"] != "q_0" {
				t.Fatalf("select name = %v", element["name"])
			}
			if _, ok := element["required"]; ok {
				t.Fatal("choice select unexpectedly became required")
			}
			if element["width"] != "fill" {
				t.Fatalf("choice select width = %#v", element["width"])
			}
			options := element["options"].([]any)
			if len(options) != 2 || options[0].(map[string]any)["value"] != "choice_0" || options[1].(map[string]any)["value"] != "choice_1" {
				t.Fatalf("option values changed: %#v", options)
			}
		case "input":
			name := element["name"].(string)
			inputSeen = true
			if element["width"] != "fill" || element["input_type"] != "multiline_text" || element["max_length"] != float64(1000) || element["rows"] != float64(1) || element["auto_resize"] != true || element["max_rows"] != float64(4) {
				t.Fatalf("input shape = %#v", element)
			}
			if _, ok := element["type"]; ok {
				t.Fatalf("input retained invalid style field: %#v", element)
			}
			if name == "q_0_notes" {
				if _, required := element["required"]; required {
					t.Fatal("optional notes became required")
				}
				if got := element["placeholder"].(map[string]any)["content"]; got != "补充说明或其他答案（选填）" {
					t.Fatalf("choice placeholder = %#v", got)
				}
			} else if name == "q_1_notes" {
				if element["required"] != true {
					t.Fatal("no-option question is not required")
				}
				if got := element["placeholder"].(map[string]any)["content"]; got != "填写回答" {
					t.Fatalf("text placeholder = %#v", got)
				}
			}
		case "button":
			if element["name"] != "submit_answers" || element["form_action_type"] != "submit" || element["type"] != "primary" {
				t.Fatalf("submit button shape = %#v", element)
			}
			if text := element["text"].(map[string]any)["content"]; text != "提交回答" {
				t.Fatalf("synchronous submit label = %#v", text)
			}
			submitSeen = true
			behaviors := element["behaviors"].([]any)
			value := behaviors[0].(map[string]any)["value"].(map[string]any)
			if len(value) != 3 || value["action"] != "interaction_submit" || value["token"] != "invalid-question-token" || value["revision"] != float64(3) {
				t.Fatalf("submit callback leaked native IDs or wrong shape: %#v", value)
			}
			if _, ok := value["decision"]; ok {
				t.Fatal("synchronous question unexpectedly carried an append decision")
			}
		}
	})
	if !selectSeen || !inputSeen || !submitSeen || !strings.Contains(card, "保留布局：保留现有布局") || !strings.Contains(card, "**采用哪种布局？**") || !strings.Contains(card, "1 / 2 · 布局") || strings.Contains(card, "可自填") {
		t.Fatal("question form lost compact prompt/option/input layout")
	}
	var submitRow map[string]any
	walkInteractionJSON(form["elements"], func(element map[string]any) {
		if element["tag"] != "column_set" {
			return
		}
		columns, _ := element["columns"].([]any)
		for _, raw := range columns {
			column, _ := raw.(map[string]any)
			if column["width"] != "auto" {
				continue
			}
			elems, _ := column["elements"].([]any)
			if len(elems) != 1 {
				continue
			}
			child, _ := elems[0].(map[string]any)
			if child["tag"] == "button" && child["name"] == "submit_answers" {
				submitRow = element
			}
		}
	})
	if submitRow == nil {
		t.Fatal("question submit is not in an auto-width column_set")
	}
	if strings.Contains(card, "<at") || strings.Contains(card, "审批成功") {
		t.Fatal("question card contains an unsafe native tag or success claim")
	}
}

func TestInteractionAsyncQuestionUsesAppendAnswerSubmit(t *testing.T) {
	card := Interaction(InteractionView{
		Kind:   "async_question",
		State:  "pending",
		Reason: "请回答当前问题",
		Token:  "invalid-async-question-token", Revision: 4, Enabled: true,
		Questions: []InteractionQuestion{{Field: "q_0", Question: "是否继续？", Options: []InteractionOption{{Value: "yes", Label: "继续"}}}},
	})
	if !strings.Contains(card, "提交回答并追加") || !strings.Contains(card, "忙碌时追加到当前任务，空闲时继续会话。") {
		t.Fatalf("async question lost append guidance: %s", card)
	}
	doc := decodeInteraction(t, card)
	var submit map[string]any
	walkInteractionJSON(doc, func(element map[string]any) {
		if element["tag"] == "button" && element["name"] == "submit_answers" {
			submit = element
		}
	})
	if submit == nil {
		t.Fatal("async question has no submit button")
	}
	if submit["type"] != "primary" || submit["text"].(map[string]any)["content"] != "提交回答并追加" {
		t.Fatalf("async submit shape = %#v", submit)
	}
	value := submit["behaviors"].([]any)[0].(map[string]any)["value"].(map[string]any)
	if value["action"] != "interaction_submit" || value["token"] != "invalid-async-question-token" || value["revision"] != float64(4) || value["decision"] != "append_answer" {
		t.Fatalf("async submit callback = %#v", value)
	}
}

func TestInteractionAsyncQuestionSubmitIsReadOnlyWhenDisabledLimitedOrResolved(t *testing.T) {
	questions := []InteractionQuestion{{Field: "q_0", Question: "是否继续？", Options: []InteractionOption{{Value: "yes", Label: "继续"}}}}
	cases := []struct {
		name string
		view InteractionView
	}{
		{name: "disabled", view: InteractionView{Kind: "async_question", State: "pending", Token: "disabled-token", Revision: 1, Questions: questions}},
		{name: "limited", view: InteractionView{Kind: "async_question", State: "pending", Reason: strings.Repeat("很长的背景。", 3000), Token: "limited-token", Revision: 2, Enabled: true, MaxCardBytes: 3500, Questions: questions}},
		{name: "resolved", view: InteractionView{Kind: "async_question", State: "resolved", Token: "resolved-token", Revision: 3, Enabled: true, Questions: questions}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			card := Interaction(tc.view)
			if strings.Contains(card, "submit_answers") || strings.Contains(card, "append_answer") {
				t.Fatalf("%s async question retained submit action: %s", tc.name, card)
			}
			if tc.view.MaxCardBytes > 0 && len(card) > tc.view.MaxCardBytes {
				t.Fatalf("%s async question exceeded card budget: %d > %d", tc.name, len(card), tc.view.MaxCardBytes)
			}
		})
	}
}

func TestInteractionPermissionsUseTopLevelMultiSelectAndTurnButtons(t *testing.T) {
	card := Interaction(InteractionView{
		Kind:   "permissions",
		State:  "pending",
		Reason: "需要额外权限",
		Token:  "invalid-permission-token", Revision: 4, Enabled: true,
		Permissions: []InteractionOption{
			{Value: "p_0", Label: "读取工作目录", Description: "允许读取当前工作目录"},
			{Value: "p_1", Label: "访问网络", Description: "允许本轮网络访问"},
		},
	})
	doc := decodeInteraction(t, card)
	els := doc["body"].(map[string]any)["elements"].([]any)
	var form map[string]any
	for _, raw := range els {
		if element, ok := raw.(map[string]any); ok && element["tag"] == "form" {
			if form != nil {
				t.Fatal("permission card has multiple forms")
			}
			form = element
		}
	}
	if form == nil || form["name"] != interactionPermissionsName {
		t.Fatalf("permission form = %#v", form)
	}
	children := form["elements"].([]any)
	var selectElement, row map[string]any
	for _, raw := range children {
		element := raw.(map[string]any)
		switch element["tag"] {
		case "multi_select_static":
			selectElement = element
		case "column_set":
			row = element
		}
	}
	if selectElement == nil || selectElement["name"] != "permissions" {
		t.Fatalf("permission multi-select = %#v", selectElement)
	}
	if _, required := selectElement["required"]; required {
		t.Fatal("permission multi-select became required")
	}
	options := selectElement["options"].([]any)
	if len(options) != 2 || options[0].(map[string]any)["value"] != "p_0" || options[1].(map[string]any)["value"] != "p_1" {
		t.Fatalf("permission values changed: %#v", options)
	}
	if row == nil {
		t.Fatal("permission buttons are not in a column_set")
	}
	columns := row["columns"].([]any)
	if len(columns) != 3 {
		t.Fatalf("permission button columns = %#v", columns)
	}
	wantLabels := []string{"允许", "允许本轮全部", "拒绝"}
	names := map[string]bool{}
	for i, raw := range columns {
		column := raw.(map[string]any)
		if column["width"] != "auto" {
			t.Fatalf("permission column %d width = %#v", i, column["width"])
		}
		elements := column["elements"].([]any)
		if len(elements) != 1 {
			t.Fatalf("permission column %d elements = %#v", i, elements)
		}
		button := elements[0].(map[string]any)
		if button["tag"] != "button" || button["form_action_type"] != "submit" || button["size"] != "small" {
			t.Fatalf("permission button %d shape = %#v", i, button)
		}
		name, _ := button["name"].(string)
		if name == "" || names[name] {
			t.Fatalf("permission button names are not unique: %#v", columns)
		}
		names[name] = true
		if got := button["text"].(map[string]any)["content"]; got != wantLabels[i] {
			t.Fatalf("permission button %d label = %#v", i, got)
		}
		value := button["behaviors"].([]any)[0].(map[string]any)["value"].(map[string]any)
		if value["action"] != "interaction_submit" || value["token"] != "invalid-permission-token" || value["revision"] != float64(4) {
			t.Fatalf("permission callback %d = %#v", i, value)
		}
		if i == 0 {
			if len(value) != 3 || button["type"] != "primary" || value["decision"] != nil {
				t.Fatalf("selected permission callback = %#v / %#v", button, value)
			}
			continue
		}
		wantDecision := []string{"accept", "decline"}[i-1]
		if value["decision"] != wantDecision {
			t.Fatalf("permission decision %d = %#v", i, value)
		}
		wantType := "primary"
		if wantDecision == "decline" {
			wantType = "default"
		}
		if button["type"] != wantType {
			t.Fatalf("permission button %d type = %#v", i, button["type"])
		}
	}
	if !strings.Contains(card, "允许本轮网络访问") {
		t.Fatal("permission controls lost descriptions")
	}
}

func TestInteractionTruncatesLongDetailsWithinCardBudget(t *testing.T) {
	card := Interaction(InteractionView{
		Kind:   "file",
		State:  "pending",
		Reason: strings.Repeat("reason ", 1000),
		CWD:    "/workspace/project",
		Token:  "invalid-long-detail-token", Revision: 2, Enabled: true,
		Decisions: []InteractionDecision{{Value: "accept", Label: "允许本次"}, {Value: "decline", Label: "拒绝"}},
		Details:   []InteractionDetail{{Title: "diff", Content: strings.Repeat("<at id=\"all\"></at> 长详情 ", 10000)}},
	})
	if !json.Valid([]byte(card)) {
		t.Fatal("long interaction card is not valid JSON")
	}
	if len(card) > DefaultMaxCardBytes || len(card) > 64*1024 {
		t.Fatalf("interaction card exceeded budget: %d", len(card))
	}
	assertInteractionReadOnly(t, card)
	if !strings.Contains(card, "内容已截短") || !strings.Contains(card, "后续内容已省略") {
		t.Fatal("long detail has no explicit omission note")
	}
	if strings.Contains(card, "<at") {
		t.Fatal("untrusted detail generated a native mention")
	}
}

func TestInteractionBudgetLimitedPermissionsAreReadOnly(t *testing.T) {
	permissions := make([]InteractionOption, 0, interactionMaxOptions+8)
	for i := 0; i < interactionMaxOptions+8; i++ {
		permissions = append(permissions, InteractionOption{
			Value:       fmt.Sprintf("p_%d", i),
			Label:       fmt.Sprintf("范围 %d", i),
			Description: "当前工作目录",
		})
	}
	card := Interaction(InteractionView{
		Kind:        "permissions",
		State:       "pending",
		Reason:      "需要多个权限范围",
		Token:       "invalid-permission-budget-token",
		Revision:    9,
		Enabled:     true,
		Permissions: permissions,
	})
	assertInteractionReadOnly(t, card)
	if !strings.Contains(card, "范围 0") || !strings.Contains(card, "权限范围未在卡片显示") {
		t.Fatal("limited permission card lost its readable preview")
	}
}

func TestInteractionBudgetLimitedLongCommandIsReadOnly(t *testing.T) {
	card := Interaction(InteractionView{
		Kind:    "command",
		State:   "pending",
		Reason:  "请确认长命令",
		CWD:     "/workspace/project",
		Command: strings.Repeat("echo preview ", 10000),
		Token:   "invalid-command-budget-token", Revision: 10, Enabled: true,
		Decisions: []InteractionDecision{
			{Value: "accept", Label: "允许本次"},
			{Value: "decline", Label: "拒绝"},
		},
	})
	assertInteractionReadOnly(t, card)
	if !strings.Contains(card, "echo preview") || !strings.Contains(card, "内容已截短") {
		t.Fatal("limited command card lost its readable preview")
	}
}

func TestInteractionBudgetLimitedQuestionsAreReadOnly(t *testing.T) {
	questions := make([]InteractionQuestion, 0, interactionMaxQuestions+4)
	for i := 0; i < interactionMaxQuestions+4; i++ {
		questions = append(questions, InteractionQuestion{
			Field:    fmt.Sprintf("q_%d", i),
			Header:   fmt.Sprintf("问题 %d", i),
			Question: "请选择本轮处理方式",
			Options: []InteractionOption{{
				Value:       "choice_0",
				Label:       "继续",
				Description: "继续当前流程",
			}},
		})
	}
	card := Interaction(InteractionView{
		Kind:      "question",
		State:     "pending",
		Reason:    "需要回答多个问题",
		Token:     "invalid-question-budget-token",
		Revision:  11,
		Enabled:   true,
		Questions: questions,
	})
	assertInteractionReadOnly(t, card)
	if !strings.Contains(card, "问题 0") || !strings.Contains(card, "继续当前流程") {
		t.Fatal("limited question card lost its readable preview")
	}
}

func TestInteractionApprovalVisualHierarchy(t *testing.T) {
	cases := []struct {
		kind  string
		title string
	}{
		{kind: "command", title: "允许执行这条命令？"},
		{kind: "file", title: "允许修改这些文件？"},
		{kind: "network", title: "允许访问这个网络地址？"},
		{kind: "permissions", title: "允许这些临时权限？"},
	}
	for _, tc := range cases {
		card := Interaction(InteractionView{
			Kind:    tc.kind,
			State:   "pending",
			Reason:  "需要你确认这项操作",
			CWD:     "/tmp/<unsafe>",
			Command: "echo hello",
			Token:   "invalid-visual-token", Revision: 13, Enabled: true,
			Decisions: []InteractionDecision{
				{Value: "accept", Label: "允许本次"},
				{Value: "decline", Label: "拒绝"},
			},
		})
		doc := decodeInteraction(t, card)
		elements := doc["body"].(map[string]any)["elements"].([]any)
		if len(elements) < 4 {
			t.Fatalf("%s approval card is too short: %#v", tc.kind, elements)
		}
		if elements[0].(map[string]any)["text_size"] != "normal" || elements[0].(map[string]any)["content"] != "**"+tc.title+"**" {
			t.Fatalf("%s title = %#v", tc.kind, elements[0])
		}
		if elements[1].(map[string]any)["text_size"] != "notation" || elements[1].(map[string]any)["content"] != "等待你确认" {
			t.Fatalf("%s status = %#v", tc.kind, elements[1])
		}
		commandIndex, cwdIndex := -1, -1
		for i, raw := range elements {
			element, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch element["tag"] {
			case "markdown":
				content, _ := element["content"].(string)
				if strings.Contains(content, "```bash\necho hello\n```") {
					commandIndex = i
					if element["text_size"] != "normal" {
						t.Fatalf("%s command size = %#v", tc.kind, element)
					}
				}
				if strings.Contains(content, "工作目录：") {
					cwdIndex = i
					if element["text_size"] != "notation" || !strings.Contains(content, "&lt;unsafe&gt;") {
						t.Fatalf("%s cwd = %#v", tc.kind, element)
					}
				}
			}
		}
		if commandIndex < 0 || cwdIndex <= commandIndex {
			t.Fatalf("%s command/CWD order = %d/%d", tc.kind, commandIndex, cwdIndex)
		}
		types := interactionButtonTypes(t, doc)
		if types["accept"] != "primary" || types["decline"] != "default" {
			t.Fatalf("%s decision button types = %#v", tc.kind, types)
		}
	}

	resolved := decodeInteraction(t, Interaction(InteractionView{Kind: "command", State: "resolved", Summary: "请求已处理", Reason: "保留已处理请求"}))
	resolvedElements := resolved["body"].(map[string]any)["elements"].([]any)
	if resolvedElements[0].(map[string]any)["content"] != "**命令审批请求**" || resolvedElements[0].(map[string]any)["text_size"] != "normal" {
		t.Fatalf("resolved approval title = %#v", resolvedElements[0])
	}
}

func TestInteractionLongCommandAndCWDUseFoldedNativeMarkdown(t *testing.T) {
	command := strings.Repeat("echo long-command; ", 20)
	cwd := strings.Repeat("/segment", 30)
	doc := decodeInteraction(t, Interaction(InteractionView{
		Kind:    "command",
		State:   "pending",
		Reason:  "请确认长命令",
		CWD:     cwd,
		Command: command,
		Token:   "invalid-folded-token", Revision: 14, Enabled: true,
		Decisions: []InteractionDecision{
			{Value: "accept", Label: "允许本次"},
			{Value: "decline", Label: "拒绝"},
		},
	}))
	elements := doc["body"].(map[string]any)["elements"].([]any)
	var commandPanel, cwdPanel map[string]any
	for _, raw := range elements {
		element, ok := raw.(map[string]any)
		if !ok || element["tag"] != "collapsible_panel" {
			continue
		}
		header := element["header"].(map[string]any)["title"].(map[string]any)
		switch header["content"] {
		case "完整命令":
			commandPanel = element
		case "工作目录":
			cwdPanel = element
		}
	}
	if commandPanel == nil || commandPanel["expanded"] != false || cwdPanel == nil || cwdPanel["expanded"] != false {
		t.Fatalf("folded command/CWD panels = %#v / %#v", commandPanel, cwdPanel)
	}
	commandChildren := commandPanel["elements"].([]any)
	commandCode := commandChildren[0].(map[string]any)
	if commandCode["tag"] != "markdown" || commandCode["text_size"] != "normal" || !strings.Contains(commandCode["content"].(string), "```bash\n"+command+"\n```") {
		t.Fatalf("folded command code = %#v", commandCode)
	}
	cwdChildren := cwdPanel["elements"].([]any)
	if len(cwdChildren) != 1 || cwdChildren[0].(map[string]any)["text_size"] != "notation" || cwdChildren[0].(map[string]any)["content"] != cwd {
		t.Fatalf("folded CWD = %#v", cwdChildren)
	}
	if types := interactionButtonTypes(t, doc); types["accept"] != "primary" || types["decline"] != "default" {
		t.Fatalf("folded command changed decision types = %#v", types)
	}
}

func TestInteractionDecisionAndSubmitButtonSemantics(t *testing.T) {
	doc := decodeInteraction(t, Interaction(InteractionView{
		Kind:  "command",
		State: "pending",
		Token: "invalid-button-type-token", Revision: 15, Enabled: true,
		Decisions: []InteractionDecision{
			{Value: "acceptForSession", Label: "允许本会话"},
			{Value: "cancel", Label: "停止本轮"},
		},
	}))
	types := interactionButtonTypes(t, doc)
	if types["acceptForSession"] != "primary" || types["cancel"] != "default" {
		t.Fatalf("decision types = %#v", types)
	}
	if strings.Contains(string(mustJSON(t, doc)), `"type":"danger"`) {
		t.Fatal("approval card introduced a danger button")
	}

	question := decodeInteraction(t, Interaction(InteractionView{
		Kind: "question", State: "pending", Token: "invalid-submit-token", Revision: 16, Enabled: true,
		Questions: []InteractionQuestion{{Field: "q_0", Question: "你要继续吗?", Options: []InteractionOption{{Value: "yes", Label: "继续"}}}},
	}))
	var submit map[string]any
	walkInteractionJSON(question, func(element map[string]any) {
		if element["tag"] == "button" && element["name"] == "submit_answers" {
			submit = element
		}
	})
	if submit == nil || submit["type"] != "primary" {
		t.Fatalf("question submit = %#v", submit)
	}
}

func TestInteractionFallbackMinimalIsReadOnly(t *testing.T) {
	card := Interaction(InteractionView{
		Kind:     "command",
		State:    "pending",
		Reason:   "保留这段可读预览",
		Token:    strings.Repeat("invalid-token-", 5000),
		Revision: 12,
		Enabled:  true,
		Decisions: []InteractionDecision{
			{Value: "accept", Label: "允许本次"},
			{Value: "decline", Label: "拒绝"},
		},
	})
	assertInteractionReadOnly(t, card)
	if !strings.Contains(card, "保留这段可读预览") {
		t.Fatal("minimal fallback lost its readable preview")
	}
}

func assertInteractionReadOnly(t *testing.T, card string) {
	t.Helper()
	if !json.Valid([]byte(card)) {
		t.Fatal("read-only interaction card is not valid JSON")
	}
	if len(card) > DefaultMaxCardBytes || len(card) > 64*1024 {
		t.Fatalf("read-only interaction card exceeded budget: %d", len(card))
	}
	for _, tag := range []string{`"tag":"button"`, `"tag":"input"`, `"tag":"select_static"`, `"tag":"multi_select_static"`, `"tag":"form"`} {
		if strings.Contains(card, tag) {
			t.Fatalf("budget-limited interaction card retained %s", tag)
		}
	}
	if !strings.Contains(card, "内容超出卡片展示范围，请回 Codex App 处理") {
		t.Fatal("budget-limited interaction card omitted its read-only warning")
	}
}

func interactionButtonTypes(t *testing.T, doc map[string]any) map[string]string {
	t.Helper()
	types := map[string]string{}
	walkInteractionJSON(doc, func(element map[string]any) {
		if element["tag"] != "button" {
			return
		}
		behaviors, ok := element["behaviors"].([]any)
		if !ok || len(behaviors) == 0 {
			return
		}
		behavior, ok := behaviors[0].(map[string]any)
		if !ok {
			return
		}
		value, ok := behavior["value"].(map[string]any)
		if !ok {
			return
		}
		decision, ok := value["decision"].(string)
		if ok {
			types[decision] = element["type"].(string)
		}
	})
	return types
}

func walkInteractionJSON(raw any, visit func(map[string]any)) {
	switch value := raw.(type) {
	case []any:
		for _, child := range value {
			walkInteractionJSON(child, visit)
		}
	case map[string]any:
		visit(value)
		for _, child := range value {
			walkInteractionJSON(child, visit)
		}
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func decodeInteraction(t *testing.T, card string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(card), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func interactionButtonValues(t *testing.T, doc map[string]any) []map[string]any {
	t.Helper()
	var values []map[string]any
	var walk func(any)
	walk = func(raw any) {
		switch value := raw.(type) {
		case []any:
			for _, child := range value {
				walk(child)
			}
		case map[string]any:
			if value["tag"] == "button" {
				if behaviors, ok := value["behaviors"].([]any); ok && len(behaviors) > 0 {
					if behavior, ok := behaviors[0].(map[string]any); ok {
						if callback, ok := behavior["value"].(map[string]any); ok && callback["decision"] != nil {
							values = append(values, callback)
						}
					}
				}
			}
			for _, child := range value {
				walk(child)
			}
		}
	}
	walk(doc)
	return values
}

func interactionQueueCallbacks(t *testing.T, doc map[string]any) []map[string]any {
	t.Helper()
	var values []map[string]any
	walkInteractionJSON(doc, func(element map[string]any) {
		if element["tag"] != "button" {
			return
		}
		behaviors, ok := element["behaviors"].([]any)
		if !ok || len(behaviors) == 0 {
			return
		}
		behavior, ok := behaviors[0].(map[string]any)
		if !ok {
			return
		}
		callback, ok := behavior["value"].(map[string]any)
		if !ok {
			return
		}
		action, _ := callback["action"].(string)
		if action == "steer" || action == "remove" || action == "cancel_pending" {
			values = append(values, callback)
		}
	})
	return values
}
