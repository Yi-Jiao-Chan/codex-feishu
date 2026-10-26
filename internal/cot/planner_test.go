package cot

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"codex-feishu/internal/feishu"
)

func fixtureDocument(status string, items ...Item) Document {
	return Document{Version: Version, ThreadID: "fixture-thread", TurnID: "fixture-turn", Status: status, Items: items}
}

func publicMessage(id, text string) Item {
	return Item{ID: id, Kind: "message", Text: text, TimestampMS: 1000}
}

func publicTool(id, status string) Item {
	return Item{ID: id, Kind: "tool", Title: "运行 Go 测试", Status: status, Icon: "bash", TimestampMS: 1001}
}

func checkedPlan(t *testing.T, doc Document, state State) Batch {
	t.Helper()
	b, err := Plan(doc, state, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Events) > feishu.MaxCOTEvents {
		t.Fatal("event count exceeded platform batch bound")
	}
	for _, event := range b.Events {
		encoded, err := json.Marshal(event)
		var content map[string]any
		if err != nil || len(encoded) > feishu.MaxCOTEventBytes || !utf8.ValidString(event.Content) || json.Unmarshal([]byte(event.Content), &content) != nil || content == nil {
			t.Fatal("invalid event or serialized event bound")
		}
	}
	return b
}

func eventTypes(events []feishu.COTEvent) []string {
	var result []string
	for _, event := range events {
		result = append(result, event.EventType)
	}
	return result
}

func eventContent(t *testing.T, event feishu.COTEvent) map[string]any {
	t.Helper()
	var content map[string]any
	if err := json.Unmarshal([]byte(event.Content), &content); err != nil {
		t.Fatal(err)
	}
	return content
}

func TestDocumentMarkerRoundTripAndInvalidNativeDocumentNeverFallsBack(t *testing.T) {
	doc := fixtureDocument("inProgress", publicMessage("message", "公开进度"))
	raw, err := Encode(doc)
	if err != nil {
		t.Fatal(err)
	}
	parsed, native, err := Parse(raw)
	if err != nil || !native || !reflect.DeepEqual(parsed, doc) {
		t.Fatal("native document did not round trip")
	}
	for _, raw := range []string{`{"schema":"2.0","body":{}}`, `null`, `invalid`} {
		if _, native, err := Parse(raw); native || err != nil {
			t.Fatal("ordinary card was treated as native COT")
		}
	}
	for _, raw := range []string{`{"codex_feishu_cot":3}`, `{"codex_feishu_cot":"1"}`, `{"codex_feishu_cot":null}`} {
		if _, native, err := Parse(raw); !native || err == nil {
			t.Fatal("invalid native marker fell back to card delivery")
		}
	}
}

func TestPlannerConfirmedPrefixDeltaAndDuplicateSnapshotAreIdempotent(t *testing.T) {
	doc := fixtureDocument("inProgress", publicMessage("message", "正在查看"))
	first := checkedPlan(t, doc, State{})
	if !first.CaughtUp || first.State.Finished || !reflect.DeepEqual(eventTypes(first.Events), []string{"RUN_STARTED", "REASONING_START", "REASONING_MESSAGE_START", "REASONING_MESSAGE_CONTENT"}) {
		t.Fatal("initial live progress did not remain open")
	}
	before, _ := json.Marshal(first.State)
	doc.Items[0].Text += "公开文件。"
	next := checkedPlan(t, doc, first.State)
	after, _ := json.Marshal(first.State)
	if string(before) != string(after) || len(next.Events) != 1 || next.Events[0].EventType != "REASONING_MESSAGE_CONTENT" || eventContent(t, next.Events[0])["delta"] != "公开文件。" {
		t.Fatal("increment duplicated content or mutated confirmed state")
	}
	again := checkedPlan(t, doc, next.State)
	if len(again.Events) != 0 || !again.CaughtUp {
		t.Fatal("duplicate complete snapshot generated new events")
	}
}

func TestPlannerNonPrefixCorrectionGetsExplicitNewStableMessageID(t *testing.T) {
	doc := fixtureDocument("inProgress", publicMessage("message", "旧的公开说明"))
	first := checkedPlan(t, doc, State{})
	oldID := eventContent(t, first.Events[1])["messageId"]
	doc.Items[0].Text = "更正后的公开说明"
	next := checkedPlan(t, doc, first.State)
	if !reflect.DeepEqual(eventTypes(next.Events), []string{"REASONING_MESSAGE_END", "REASONING_END", "REASONING_START", "REASONING_MESSAGE_START", "REASONING_MESSAGE_CONTENT"}) {
		t.Fatal("correction did not close old message and start an explicit replacement generation")
	}
	newID := eventContent(t, next.Events[3])["messageId"]
	if oldID == newID || eventContent(t, next.Events[4])["delta"] != "公开进度已更正：\n更正后的公开说明" {
		t.Fatal("non-prefix correction silently appended the whole text")
	}
	again := checkedPlan(t, doc, next.State)
	if len(again.Events) != 0 || again.State.Items["message"].Message.Generation != 1 {
		t.Fatal("correction generation was not stable on replay")
	}
}

func TestPlannerBatchesLongUTF8ByActualEscapedWireBytesWithoutTruncation(t *testing.T) {
	text := strings.Repeat("中文😀<>&\\\"\n", 25000)
	doc := fixtureDocument("completed", publicMessage("long", text))
	state := State{}
	var reconstructed strings.Builder
	started, finished, batches := 0, 0, 0
	var lastTimestamp int64
	for ; batches < 100; batches++ {
		b := checkedPlan(t, doc, state)
		if len(b.Events) == 0 && !b.CaughtUp {
			t.Fatal("bounded planner made no progress")
		}
		for _, event := range b.Events {
			if event.Timestamp <= lastTimestamp {
				t.Fatal("event observation order changed or went backwards")
			}
			lastTimestamp = event.Timestamp
			switch event.EventType {
			case "RUN_STARTED":
				started++
			case "RUN_FINISHED":
				finished++
			case "REASONING_MESSAGE_CONTENT":
				reconstructed.WriteString(eventContent(t, event)["delta"].(string))
			}
		}
		state = b.State
		if b.CaughtUp {
			break
		}
	}
	if batches < 1 || batches >= 100 || reconstructed.String() != text || started != 1 || finished != 1 || !state.Finished {
		t.Fatal("long text lost UTF-8, duplicated lifecycle, or completed before draining")
	}
}

func TestPlannerToolLifecycleAndResultUpdatesUseStableNativeCallID(t *testing.T) {
	doc := fixtureDocument("inProgress", publicTool("tool", "inProgress"))
	first := checkedPlan(t, doc, State{})
	if !reflect.DeepEqual(eventTypes(first.Events), []string{"RUN_STARTED", "TOOL_CALL_START", "TOOL_CALL_ARGS", "TOOL_CALL_END"}) || eventContent(t, first.Events[2])["delta"] != "{}" {
		t.Fatal("tool start/empty public args/end order changed")
	}
	id := eventContent(t, first.Events[1])["toolCallId"]
	doc.Items[0].Status = "completed"
	next := checkedPlan(t, doc, first.State)
	if len(next.Events) != 1 || next.Events[0].EventType != "TOOL_CALL_RESULT" || eventContent(t, next.Events[0])["toolCallId"] != id || eventContent(t, next.Events[0])["content"] != "已完成" || eventContent(t, next.Events[0])["isError"] != false {
		t.Fatal("confirmed completion restarted call or invented raw output")
	}
	doc.Items[0].Status, doc.Items[0].Title = "failed", "运行 Go 测试（状态更正）"
	changed := checkedPlan(t, doc, next.State)
	if len(changed.Events) != 1 || changed.Events[0].EventType != "TOOL_CALL_RESULT" || eventContent(t, changed.Events[0])["toolCallId"] != id || !strings.Contains(eventContent(t, changed.Events[0])["content"].(string), "执行失败") {
		t.Fatal("safe result correction did not overwrite by stable tool call ID")
	}
}

func TestPlannerUnavailableAndDeclinedToolsNeverInventExecutionOrSuccess(t *testing.T) {
	for _, status := range []string{"unknown", "declined"} {
		doc := fixtureDocument("inProgress", publicTool("tool", status))
		b := checkedPlan(t, doc, State{})
		for _, event := range b.Events {
			if strings.HasPrefix(event.EventType, "TOOL_CALL_") || strings.Contains(event.Content, "已完成") {
				t.Fatal("unconfirmed tool inflated native execution count or became successful")
			}
		}
		doc.Items[0].Status = "inProgress"
		confirmed := checkedPlan(t, doc, b.State)
		if !reflect.DeepEqual(eventTypes(confirmed.Events), []string{"TOOL_CALL_START", "TOOL_CALL_ARGS", "TOOL_CALL_END"}) {
			t.Fatal("later verified execution did not produce the native lifecycle")
		}
	}
	doc := fixtureDocument("inProgress", publicTool("started", "inProgress"))
	started := checkedPlan(t, doc, State{})
	doc.Status = "interrupted"
	terminal := checkedPlan(t, doc, started.State)
	if !reflect.DeepEqual(eventTypes(terminal.Events), []string{"TOOL_CALL_RESULT", "RUN_FINISHED"}) || !strings.Contains(terminal.Events[0].Content, "结束状态未确认") || eventContent(t, terminal.Events[1])["status"] != "interrupted" {
		t.Fatal("interruption invented a completed tool")
	}
}

func TestPlannerOnlyAuthoritativeCompletedTurnProducesDone(t *testing.T) {
	for _, status := range []string{"completed", "failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			doc := fixtureDocument(status, publicMessage("message", "公开说明"))
			b := checkedPlan(t, doc, State{})
			last := b.Events[len(b.Events)-1]
			if !b.CaughtUp || !b.State.Finished {
				t.Fatal("terminal snapshot did not drain and close lifecycle")
			}
			if status == "failed" {
				if last.EventType != "RUN_ERROR" || strings.Contains(last.Content, "\"status\":\"done\"") {
					t.Fatal("failed run was marked done")
				}
			} else {
				want := status
				if status == "completed" {
					want = "done"
				}
				if last.EventType != "RUN_FINISHED" || eventContent(t, last)["status"] != want {
					t.Fatal("native terminal status changed")
				}
			}
			if again := checkedPlan(t, doc, b.State); len(again.Events) != 0 {
				t.Fatal("finished lifecycle was reopened")
			}
		})
	}
}

func TestPlannerManyToolsBatchWithoutArrayOffsetAcknowledgements(t *testing.T) {
	var tools []Item
	for i := range 40 {
		tools = append(tools, publicTool(strings.Repeat("x", i+1), "completed"))
	}
	doc := fixtureDocument("completed", tools...)
	state := State{}
	startIDs := map[string]bool{}
	for range 10 {
		b := checkedPlan(t, doc, state)
		for _, event := range b.Events {
			if event.EventType == "TOOL_CALL_START" {
				id := eventContent(t, event)["toolCallId"].(string)
				if startIDs[id] {
					t.Fatal("batch boundary replayed a previously confirmed tool start")
				}
				startIDs[id] = true
			}
		}
		state = b.State
		if b.CaughtUp {
			break
		}
	}
	if len(startIDs) != 40 || !state.Finished {
		t.Fatal("planner reused the old card's 32-item display window")
	}
}

func TestPlannerRejectsForeignScopeAndIDsRemainTurnScoped(t *testing.T) {
	doc := fixtureDocument("inProgress", publicTool("tool", "completed"))
	b := checkedPlan(t, doc, State{})
	foreign := doc
	foreign.TurnID = "other-turn"
	if _, err := Plan(foreign, b.State, 3000); err == nil {
		t.Fatal("confirmed state was accepted for another turn")
	}
	if StableID(doc, "tool", "tool", 0) == StableID(foreign, "tool", "tool", 0) {
		t.Fatal("native item reuse across turns collided")
	}
	doc.Items = append(doc.Items, doc.Items[0])
	if _, err := Plan(doc, State{}, 3000); err == nil {
		t.Fatal("duplicate public item accepted")
	}
}

func TestPlannerClosesPreviouslyConfirmedEntitiesMissingFromTerminalSnapshot(t *testing.T) {
	doc := fixtureDocument("inProgress", publicTool("tool", "inProgress"), publicMessage("legacy", "公开的旧版本文本"))
	initial := checkedPlan(t, doc, State{})
	doc.Status, doc.Items = "completed", nil
	terminal := checkedPlan(t, doc, initial.State)
	types := eventTypes(terminal.Events)
	if !terminal.State.Finished || !terminal.State.Items["legacy"].Message.Ended || !strings.Contains(terminal.State.Items["tool"].Tool.Result, "结束状态未确认") || types[len(types)-1] != "RUN_FINISHED" {
		t.Fatal("terminal scan left a removed legacy message or tool open")
	}
}

func TestPlannerRunAndItemIDsAreTurnScopedButThreadIDIsStable(t *testing.T) {
	first := fixtureDocument("inProgress")
	second := first
	second.TurnID = "second-turn"
	a, b := checkedPlan(t, first, State{}), checkedPlan(t, second, State{})
	ca, cb := eventContent(t, a.Events[0]), eventContent(t, b.Events[0])
	if ca["threadId"] != cb["threadId"] || ca["runId"] == cb["runId"] {
		t.Fatal("run identity was reused or thread identity changed with the turn")
	}
}

func TestPlannerReasoningWrappersRemainBalancedAcrossBatchBoundaries(t *testing.T) {
	var items []Item
	for i := 0; i < 61; i++ {
		items = append(items, publicMessage(string(rune('a'+i)), "公开进度"))
	}
	doc := fixtureDocument("completed", items...)
	state := State{}
	phases := map[string]int{}
	for n := 0; n < 100; n++ {
		batch := checkedPlan(t, doc, state)
		for _, e := range batch.Events {
			c := eventContent(t, e)
			id, _ := c["messageId"].(string)
			switch e.EventType {
			case "REASONING_START":
				if phases[id] != 0 {
					t.Fatal("duplicate reasoning wrapper start")
				}
				phases[id] = 1
			case "REASONING_MESSAGE_START":
				if phases[id] != 1 {
					t.Fatal("message outside reasoning wrapper")
				}
				phases[id] = 2
			case "REASONING_MESSAGE_CONTENT":
				if phases[id] != 2 {
					t.Fatal("content outside open message")
				}
			case "REASONING_MESSAGE_END":
				if phases[id] != 2 {
					t.Fatal("message ended twice or without start")
				}
				phases[id] = 3
			case "REASONING_END":
				if phases[id] != 3 {
					t.Fatal("reasoning wrapper ended before message")
				}
				phases[id] = 4
			}
		}
		state = batch.State
		if batch.CaughtUp {
			if len(phases) != len(items) {
				t.Fatal("missing reasoning wrappers")
			}
			for _, phase := range phases {
				if phase != 4 {
					t.Fatal("unclosed reasoning wrapper")
				}
			}
			return
		}
	}
	t.Fatal("planner did not finish bounded batches")
}

func TestPlannerAbsentEntityCloseAcknowledgesPartialBatch(t *testing.T) {
	for _, note := range []bool{false, true} {
		doc := fixtureDocument("inProgress", publicMessage("absent", "早先公开进度"))
		initial := checkedPlan(t, doc, State{}).State
		if note {
			initial.Items["absent"] = ItemState{Kind: "tool", Note: initial.Items["absent"].Message}
		}
		doc.Status = "completed"
		doc.Items = nil
		for i := 0; i < 11; i++ {
			doc.Items = append(doc.Items, publicTool(string(rune('a'+i)), "completed"))
		}
		doc.Items = append(doc.Items, publicMessage("last", "本次公开进度"))
		batch := checkedPlan(t, doc, initial)
		if len(batch.Events) != 50 || batch.CaughtUp || batch.Events[49].EventType != "REASONING_MESSAGE_END" {
			t.Fatal("fixture did not straddle absent wrapper closure")
		}
		next := checkedPlan(t, doc, batch.State)
		for _, e := range next.Events {
			if e.EventType == "REASONING_MESSAGE_END" {
				t.Fatal("already acknowledged message-end emitted twice")
			}
		}
		if !next.CaughtUp || !next.State.Finished {
			t.Fatal("absent wrapper did not finish on next batch")
		}
	}
}

func TestPlannerToolTransitionClosesPartiallyAcknowledgedNote(t *testing.T) {
	for _, status := range []string{"inProgress", "completed"} {
		doc := fixtureDocument("inProgress", publicTool("tool", status))
		state := State{ThreadID: doc.ThreadID, TurnID: doc.TurnID, DocumentVersion: Version, Started: true, LastTimestampMS: 1000, Items: map[string]ItemState{
			"tool": {Kind: "tool", Note: MessageState{ReasoningStarted: true, Started: true, Text: "已有的公开状态说明"}},
		}}
		batch := checkedPlan(t, doc, state)
		kinds := eventTypes(batch.Events)
		if len(kinds) < 3 || kinds[0] != "REASONING_MESSAGE_END" || kinds[1] != "REASONING_END" || kinds[2] != "TOOL_CALL_START" {
			t.Fatal("tool transition left its prior public note open")
		}
		if !batch.State.Items["tool"].Note.ReasoningEnded {
			t.Fatal("note closure not acknowledged")
		}
	}
}

func TestPlannerToolTitleReversionReconcilesLatestAcknowledgedNote(t *testing.T) {
	doc := fixtureDocument("inProgress", publicTool("tool", "inProgress"))
	initial := checkedPlan(t, doc, State{})
	original := doc.Items[0].Title
	doc.Items[0].Title = "查看 Git 状态"
	changed := checkedPlan(t, doc, initial.State)
	if !changed.CaughtUp || !strings.Contains(changed.State.Items["tool"].Note.Text, doc.Items[0].Title) {
		t.Fatal("changed title was not acknowledged")
	}
	encoded, _ := json.Marshal(changed.State)
	var recovered State
	if err := json.Unmarshal(encoded, &recovered); err != nil {
		t.Fatal(err)
	}
	doc.Items[0].Title = original
	restored := checkedPlan(t, doc, recovered)
	if len(restored.Events) == 0 || !restored.CaughtUp || !strings.HasSuffix(restored.State.Items["tool"].Note.Text, "工具动作摘要更新："+original) {
		t.Fatal("return to original title left the previous public note visible")
	}
	for _, e := range restored.Events {
		if strings.HasPrefix(e.EventType, "TOOL_CALL_") {
			t.Fatal("title correction restarted the tool")
		}
	}
	if duplicate := checkedPlan(t, doc, restored.State); len(duplicate.Events) != 0 {
		t.Fatal("unchanged latest summary was sent twice")
	}
	doc.Items[0].Title = "查看 Git 状态"
	again := checkedPlan(t, doc, restored.State)
	if len(again.Events) == 0 || !strings.HasSuffix(again.State.Items["tool"].Note.Text, "工具动作摘要更新：查看 Git 状态") {
		t.Fatal("subsequent title transition lost latest summary")
	}
}

func TestPlannerToolSummaryUpdateResumesAcrossBatchBoundary(t *testing.T) {
	for _, prelude := range []struct{ tools, messages int }{{11, 1}, {12, 0}, {8, 3}, {9, 2}, {10, 1}} {
		doc := fixtureDocument("inProgress", publicTool("target", "inProgress"))
		initial := checkedPlan(t, doc, State{}).State
		target := doc.Items[0]
		target.Title = "查看 Git 状态"
		doc.Items = nil
		for i := 0; i < prelude.tools; i++ {
			doc.Items = append(doc.Items, publicTool("done-"+string(rune('a'+i)), "completed"))
		}
		for i := 0; i < prelude.messages; i++ {
			doc.Items = append(doc.Items, publicMessage("message-"+string(rune('a'+i)), "公开进度"))
		}
		doc.Items = append(doc.Items, target)
		first := checkedPlan(t, doc, initial)
		if len(first.Events) != 50 {
			t.Fatal("fixture did not reach the event batch limit")
		}
		second := checkedPlan(t, doc, first.State)
		if !second.CaughtUp {
			t.Fatal("partially acknowledged summary did not resume")
		}
		id := StableID(doc, "message", "tool-note:target", 0)
		counts := map[string]int{}
		for _, e := range append(append([]feishu.COTEvent{}, first.Events...), second.Events...) {
			if eventContent(t, e)["messageId"] == id {
				counts[e.EventType]++
			}
		}
		for _, kind := range []string{"REASONING_START", "REASONING_MESSAGE_START", "REASONING_MESSAGE_CONTENT", "REASONING_MESSAGE_END", "REASONING_END"} {
			if counts[kind] != 1 {
				t.Fatalf("partial summary event %s repeated or lost: %d", kind, counts[kind])
			}
		}
		if duplicate := checkedPlan(t, doc, second.State); len(duplicate.Events) != 0 {
			t.Fatal("completed summary batch replayed events")
		}
		// A newer snapshot may reverse the title before the pending note closes.
		doc.Items[len(doc.Items)-1].Title = "运行 Go 测试"
		reverted := checkedPlan(t, doc, first.State)
		if !reverted.CaughtUp {
			t.Fatal("title reversal could not close a partial note")
		}
		note := reverted.State.Items["target"].Note
		if !note.ReasoningEnded {
			t.Fatal("title reversal left a summary wrapper open")
		}
		if note.Text != "" && !strings.HasSuffix(note.Text, "工具动作摘要更新：运行 Go 测试") {
			t.Fatal("partially acknowledged old title survived reversion")
		}
	}
}

func TestPlannerToolTitleReversionAfterPartialCorrection(t *testing.T) {
	for _, earlierTitles := range [][]string{{"查看 Git 状态"}, {"查看 Git 状态", "查看 Git 差异"}} {
		for _, prelude := range []struct{ tools, messages int }{{11, 1}, {12, 0}, {8, 3}, {9, 2}, {10, 1}} {
			doc := fixtureDocument("inProgress", publicTool("target", "inProgress"))
			state := checkedPlan(t, doc, State{}).State
			original := doc.Items[0].Title
			for _, title := range earlierTitles {
				doc.Items[0].Title = title
				state = checkedPlan(t, doc, state).State
			}
			target := doc.Items[0]
			target.Title = "构建 Go 程序"
			doc.Items = nil
			for i := 0; i < prelude.tools; i++ {
				doc.Items = append(doc.Items, publicTool("done-"+string(rune('a'+i)), "completed"))
			}
			for i := 0; i < prelude.messages; i++ {
				doc.Items = append(doc.Items, publicMessage("message-"+string(rune('a'+i)), "公开进度"))
			}
			doc.Items = append(doc.Items, target)
			partial := checkedPlan(t, doc, state)
			if len(partial.Events) != 50 || partial.State.Items["target"].Note.Generation != len(earlierTitles) {
				t.Fatal("fixture did not cut a later correction at the batch boundary")
			}
			// Restart from only the ACKed part of a correction, which may not
			// contain any text yet, then return to the immutable initial title.
			encoded, _ := json.Marshal(partial.State)
			var recovered State
			if err := json.Unmarshal(encoded, &recovered); err != nil {
				t.Fatal(err)
			}
			doc.Items[len(doc.Items)-1].Title = original
			reverted := checkedPlan(t, doc, recovered)
			note := reverted.State.Items["target"].Note
			if !reverted.CaughtUp || !note.ReasoningEnded || !strings.HasSuffix(note.Text, "工具动作摘要更新："+original) {
				t.Fatal("partial correction left the earlier public summary visible after reversion")
			}
			if duplicate := checkedPlan(t, doc, reverted.State); len(duplicate.Events) != 0 {
				t.Fatal("resumed title reversion was not idempotent")
			}
		}
	}
}
