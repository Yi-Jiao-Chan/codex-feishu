package cot

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func resultEventContent(t *testing.T, b Batch) map[string]any {
	t.Helper()
	for _, event := range b.Events {
		if event.EventType == "TOOL_CALL_RESULT" {
			return eventContent(t, event)
		}
	}
	t.Fatal("missing tool result")
	return nil
}

func TestPlannerToolDetailsRemainAttachedToOneCallAcrossRestartAndCorrection(t *testing.T) {
	code, elapsed := 0, int64(0)
	tool := publicTool("read", "completed")
	tool.Title, tool.ToolName = "读取 fixture.go", "commandExecution"
	tool.Details = []string{"文件：fixture.go", "文件：other.go"}
	tool.ExitCode, tool.DurationMS = &code, &elapsed
	doc := fixtureDocument("inProgress", tool)
	first := checkedPlan(t, doc, State{})
	start := eventContent(t, first.Events[1])
	result := resultEventContent(t, first)
	if start["toolCallName"] != "commandExecution" || start["title"] != tool.Title || result["toolCallId"] != start["toolCallId"] {
		t.Fatal("tool identity, heading, and details were conflated")
	}
	body := result["content"].(string)
	for _, expected := range []string{"文件：fixture.go", "文件：other.go"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("missing safe detail %q", expected)
		}
	}
	for _, unwanted := range []string{tool.Title, "仅同步状态", "原始输出未同步", "用途未识别", "退出码：0", "耗时：", "已完成"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("result repeated heading or diagnostic boilerplate %q", unwanted)
		}
	}
	if result["status"] != "completed" || result["isError"] != false || result["exitCode"] != float64(0) || result["durationMs"] != float64(0) {
		t.Fatal("authoritative zero-valued metadata was lost")
	}
	encoded, _ := json.Marshal(first.State)
	var recovered State
	if err := json.Unmarshal(encoded, &recovered); err != nil {
		t.Fatal(err)
	}
	if again := checkedPlan(t, doc, recovered); len(again.Events) != 0 {
		t.Fatal("restart duplicated already confirmed details")
	}
	doc.Items[0].Details = []string{"文件：fixture.go", "文件：corrected.go"}
	corrected := checkedPlan(t, doc, recovered)
	if !reflect.DeepEqual(eventTypes(corrected.Events), []string{"TOOL_CALL_RESULT"}) || resultEventContent(t, corrected)["toolCallId"] != start["toolCallId"] {
		t.Fatal("detail correction created a second tool or public note")
	}
	if again := checkedPlan(t, doc, corrected.State); len(again.Events) != 0 {
		t.Fatal("unchanged corrected result was repeated")
	}
	elapsed = 25
	metadataOnly := checkedPlan(t, doc, corrected.State)
	if !reflect.DeepEqual(eventTypes(metadataOnly.Events), []string{"TOOL_CALL_RESULT"}) || resultEventContent(t, metadataOnly)["durationMs"] != float64(25) || resultEventContent(t, metadataOnly)["toolCallId"] != start["toolCallId"] {
		t.Fatal("late execution metadata was lost or created another tool")
	}
}

func TestPlannerFailureMetadataDoesNotDependOnFirstObservationTiming(t *testing.T) {
	code := 2
	tool := publicTool("command", "inProgress")
	tool.ToolName = "commandExecution"
	doc := fixtureDocument("inProgress", tool)
	running := checkedPlan(t, doc, State{})
	doc.Items[0].Status, doc.Items[0].ExitCode = "failed", &code
	late := resultEventContent(t, checkedPlan(t, doc, running.State))
	earlyBatch := checkedPlan(t, doc, State{})
	early := resultEventContent(t, earlyBatch)
	if !reflect.DeepEqual(late, early) || late["status"] != "error" || late["isError"] != true || late["exitCode"] != float64(2) {
		t.Fatal("failure representation depended on observing the running state")
	}
	if eventContent(t, earlyBatch.Events[1])["title"] != tool.Title {
		t.Fatal("execution status was embedded into the immutable heading")
	}
	if strings.Contains(late["content"].(string), "已完成") || !strings.Contains(late["content"].(string), "退出码：2") || earlyBatch.State.Finished {
		t.Fatal("tool failure invented success or failed the whole run")
	}
}

func TestPlannerToolPreviewShowsExecutionAndResultWithoutStatusBoilerplate(t *testing.T) {
	code, elapsed := 0, int64(0)
	tool := publicTool("command", "completed")
	tool.ToolName, tool.Title = "commandExecution", "查看 Git 状态"
	tool.InputPreview, tool.OutputPreview = "git status --short", " M internal/fixture.go\n?? docs/fixture.md"
	tool.ExitCode, tool.DurationMS = &code, &elapsed
	doc := fixtureDocument("inProgress", tool)
	first := checkedPlan(t, doc, State{})
	result := resultEventContent(t, first)
	body := result["content"].(string)
	for _, want := range []string{"git status --short", "internal/fixture.go", "docs/fixture.md", "执行内容：", "结果："} {
		if !strings.Contains(body, want) {
			t.Fatalf("expanded tool is missing execution evidence %q", want)
		}
	}
	for _, unwanted := range []string{"已完成", "退出码", "耗时", "涉及内容", "仅同步状态"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("expanded tool repeated unhelpful metadata %q", unwanted)
		}
	}
	doc.Items[0].OutputPreview += "\n M internal/other.go"
	updated := checkedPlan(t, doc, first.State)
	if !reflect.DeepEqual(eventTypes(updated.Events), []string{"TOOL_CALL_RESULT"}) || resultEventContent(t, updated)["toolCallId"] != result["toolCallId"] {
		t.Fatal("late output created another tool call instead of updating its result")
	}
	if len(checkedPlan(t, doc, updated.State).Events) != 0 {
		t.Fatal("unchanged output preview was repeated")
	}
}

func TestPlannerToolPreviewBalancesEscapedBudgetAndClosesCodeFences(t *testing.T) {
	for _, text := range []string{"<&\"\\界😀\n", "``````\n"} {
		tool := publicTool("script", "failed")
		tool.InputPreview = "python3 - <<'PY'\n" + strings.Repeat(text, 150) + "\nPY"
		tool.OutputPreview = "Traceback: fixture\n" + strings.Repeat(text, 300) + "\nValueError: expected 2, got 3"
		code := 2
		tool.ExitCode = &code
		b := checkedPlan(t, fixtureDocument("inProgress", tool), State{})
		body := resultEventContent(t, b)["content"].(string)
		for _, want := range []string{"python3", "结果：", "Traceback", "expected 2, got 3", "退出码：2", "详情已省略"} {
			if !strings.Contains(body, want) {
				t.Fatalf("long input displaced a useful result or failure fact %q", want)
			}
		}
		fence, blocks := "", 0
		for _, line := range strings.Split(body, "\n") {
			if fence != "" {
				if line == fence {
					fence = ""
				}
			} else if strings.HasSuffix(line, "text") {
				candidate := strings.TrimSuffix(line, "text")
				if len(candidate) >= 3 && strings.Trim(candidate, "`") == "" {
					fence, blocks = candidate, blocks+1
				}
			}
		}
		if fence != "" || blocks != 2 {
			t.Fatal("wire clipping broke a code block or allowed native text to escape")
		}
	}
}

func TestPlannerUnknownMetadataRemainsNeutralWithoutInventingCalls(t *testing.T) {
	for _, status := range []string{"unknown", "declined"} {
		doc := fixtureDocument("completed", publicTool("unconfirmed", status))
		b := checkedPlan(t, doc, State{})
		var text strings.Builder
		for _, event := range b.Events {
			if strings.HasPrefix(event.EventType, "TOOL_CALL_") {
				t.Fatal("metadata-only tool became a confirmed invocation")
			}
			if event.EventType == "REASONING_MESSAGE_CONTENT" {
				text.WriteString(eventContent(t, event)["delta"].(string))
			}
		}
		want := doc.Items[0].Title
		if status == "declined" {
			want += " · 已拒绝"
		}
		if text.String() != want {
			t.Fatal("missing status was rendered as a problem requiring action")
		}
	}
	doc := fixtureDocument("inProgress", publicTool("started", "inProgress"))
	b := checkedPlan(t, doc, State{})
	doc.Status = "interrupted"
	result := resultEventContent(t, checkedPlan(t, doc, b.State))
	if _, present := result["isError"]; present || result["content"] != "结束状态未确认" {
		t.Fatal("interrupted unfinished call acquired an invented outcome")
	}
}

func TestPlannerDetailedResultRespectsEscapedWireBudgetAndDisclosesOmission(t *testing.T) {
	tool := publicTool("large", "completed")
	tool.ToolName, tool.ResultSummary = "fileChange", "已修改 20 个文件"
	for i := range 6 {
		tool.Details = append(tool.Details, string(rune('a'+i))+strings.Repeat("<&\"😀", 39))
	}
	doc := fixtureDocument("inProgress", tool)
	b := checkedPlan(t, doc, State{})
	body := resultEventContent(t, b)["content"].(string)
	if !strings.HasPrefix(body, tool.ResultSummary) || !strings.HasSuffix(body, "…（详情已省略）") || strings.Contains(body, "仅同步状态") {
		t.Fatal("bounded detail lost the outcome or silently omitted content")
	}
	if !b.CaughtUp || len(checkedPlan(t, doc, b.State).Events) != 0 {
		t.Fatal("bounded result did not make stable progress")
	}
}

func TestPlannerLegacyVersionKeepsFrozenPayloadAndOldAcknowledgements(t *testing.T) {
	doc := fixtureDocument("inProgress", publicTool("tool", "inProgress"))
	doc.Version = LegacyVersion
	started := checkedPlan(t, doc, State{})
	if started.State.DocumentVersion != 0 {
		t.Fatal("legacy ACK shape changed")
	}
	doc.Items[0].Status = "completed"
	finished := checkedPlan(t, doc, started.State)
	result := resultEventContent(t, finished)
	if result["content"] != doc.Items[0].Title+" · 已完成（仅同步状态，原始输出未同步）" {
		t.Fatal("active legacy COT changed its established presentation")
	}
	if _, exists := result["isError"]; exists {
		t.Fatal("legacy result received version 2 metadata")
	}
	doc.Version = Version
	if _, err := Plan(doc, started.State, 3000); err == nil {
		t.Fatal("legacy ACK accepted a presentation upgrade mid-run")
	}
	current := checkedPlan(t, doc, State{})
	doc.Version = LegacyVersion
	if _, err := Plan(doc, current.State, 3000); err == nil {
		t.Fatal("current ACK accepted a presentation downgrade mid-run")
	}
}

func TestDocumentRejectsUnboundedOrMisplacedToolDetails(t *testing.T) {
	for _, mutate := range []func(*Document){
		func(d *Document) { d.Items[0].Title = strings.Repeat("界", 33) },
		func(d *Document) { d.Items[0].Details = []string{"line\nprivate"} },
		func(d *Document) { d.Items[0].Details = []string{strings.Repeat("界", 161)} },
		func(d *Document) { d.Items[0].Details = []string{"1", "2", "3", "4", "5", "6", "7"} },
		func(d *Document) { d.Items[0].ToolName = "unsafe/name" },
		func(d *Document) { d.Items[0].ResultSummary = "hidden\u202econtrol" },
		func(d *Document) { d.Items[0].InputPreview = strings.Repeat("界", MaxToolInputRunes+1) },
		func(d *Document) { d.Items[0].OutputPreview = strings.Repeat("界", MaxToolOutputRunes+1) },
		func(d *Document) { d.Items[0].OutputPreview = "hidden\u202econtrol" },
		func(d *Document) { d.Version = LegacyVersion; d.Items[0].InputPreview = "git status" },
		func(d *Document) { v := int64(-1); d.Items[0].DurationMS = &v },
		func(d *Document) { v := int64(1 << 32); code := int(v); d.Items[0].ExitCode = &code },
		func(d *Document) { d.Version = LegacyVersion; d.Items[0].Details = []string{"new details"} },
		func(d *Document) { d.Items[0] = publicMessage("message", "public"); d.Items[0].ToolName = "tool" },
		func(d *Document) { d.Version = Version + 1 },
	} {
		doc := fixtureDocument("inProgress", publicTool("tool", "completed"))
		mutate(&doc)
		if _, err := Encode(doc); err == nil {
			t.Fatal("invalid tool details were accepted")
		}
	}
}
