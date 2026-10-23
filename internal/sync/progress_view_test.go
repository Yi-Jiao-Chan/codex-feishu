package sync

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
)

func TestProgressViewKeepsOnlyOrderedPublicRecordsAndSeparatesLegacyFinal(t *testing.T) {
	turn := codexclient.Turn{Status: "completed", Items: []codexclient.Item{
		input("user", "private input"),
		agent("one", "first public action", "commentary"),
		{ID: "reason", Type: "reasoning", Text: "private reasoning"},
		{ID: "tool", Type: "commandExecution", Text: "private tool output"},
		agent("empty", "  ", "commentary"),
		agent("two", "second public action", "partial_answer"),
		agent("other", "unsupported phase", "other"),
		agent("legacy", "legacy public action", ""),
		agent("final", "actual final", "final_answer"),
	}}
	view := progressView(turn, 200000)
	want := []string{"first public action", "second public action", "legacy public action"}
	if !reflect.DeepEqual(recordTexts(view.Records), want) || view.Body != "" || view.ElapsedSeconds != -1 {
		t.Fatalf("terminal public projection mismatch: %+v", view)
	}
	turn.Items = []codexclient.Item{
		agent("first", "identical text", ""),
		agent("explicit", "public commentary", "commentary"),
		agent("selected-final", "identical text", ""),
		agent("blank", " ", ""),
	}
	view = progressView(turn, 200000)
	if !reflect.DeepEqual(recordTexts(view.Records), []string{"identical text", "public commentary"}) || view.CurrentItemID != "" || finalBody(turn) != "identical text" {
		t.Fatal("legacy final exclusion removed an earlier item or duplicated final")
	}
	turn.Status = "inProgress"
	view = progressView(turn, 200000)
	if len(view.Records) != 3 || view.Body != "identical text" || view.CurrentItemID != "selected-final" {
		t.Fatal("running legacy items were treated as terminal final")
	}
}

func TestProgressViewUsesVerifiedNativeSecondsAndFreezesTerminalDuration(t *testing.T) {
	ptr := func(v int64) *int64 { return &v }
	for _, tc := range []struct {
		name, status string
		start, end   *int64
		observedMS   int64
		want         int64
	}{
		{"running", "inProgress", ptr(100), nil, 105999, 5},
		{"terminal", "completed", ptr(100), ptr(103), 105999, 3},
		{"terminal replay", "completed", ptr(100), ptr(103), 999000, 3},
		{"zero elapsed", "inProgress", ptr(100), nil, 100000, 0},
		{"missing start", "completed", nil, ptr(103), 105000, -1},
		{"zero start", "inProgress", ptr(0), nil, 105000, -1},
		{"future start", "inProgress", ptr(106), nil, 105000, -1},
		{"missing completion", "interrupted", ptr(100), nil, 105000, -1},
		{"completion before start", "failed", ptr(100), ptr(99), 105000, -1},
		{"future completion", "completed", ptr(100), ptr(106), 105000, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := progressView(codexclient.Turn{Status: tc.status, StartedAt: tc.start, CompletedAt: tc.end}, tc.observedMS)
			if view.ElapsedSeconds != tc.want {
				t.Fatalf("elapsed=%d, want %d", view.ElapsedSeconds, tc.want)
			}
		})
	}
}

func TestProgressCompleteSnapshotsReplaceItemDeltasWithoutDuplicatingRecords(t *testing.T) {
	e, _ := engineFixture(t)
	var latest cards.ProgressView
	e.ProgressRender = func(_, _, status string, v cards.ProgressView) string {
		latest = v
		return cards.Task(cards.TaskView{Status: status, Progress: v})
	}
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", ItemsView: "full", Items: []codexclient.Item{
		input("user", "request"), agent("comment", "partial", "commentary"),
	}}
	apply(t, e, 200000, false, turn)
	turn.Items[1].Text = "complete public action"
	turn.Items = append(turn.Items, turn.Items[1], agent("next", "next action", "partial_answer"), agent("distinct-item", "next action", "commentary"))
	apply(t, e, 201000, false, turn)
	if !reflect.DeepEqual(recordTexts(latest.Records), []string{"complete public action", "next action", "next action"}) || latest.Body != "next action" {
		t.Fatalf("complete snapshot appended deltas or duplicate IDs: %+v", latest)
	}
	m, err := e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("progress", "turn"))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, e, 201000, true, turn)
	again, err := e.Store.Mirror(context.Background(), "host", "thread", m.Key)
	if err != nil || again.Revision != m.Revision || again.Card != m.Card {
		t.Fatal("identical complete snapshot changed the progress document", err)
	}
	turn.Items = append(turn.Items, agent("comment", "conflicting public action", "commentary"))
	if err := e.Apply(context.Background(), "thread", []codexclient.Turn{turn}, false, 202000); err == nil {
		t.Fatal("conflicting duplicate public item was accepted")
	}
	unchanged, err := e.Store.Mirror(context.Background(), "host", "thread", m.Key)
	if err != nil || unchanged.Revision != m.Revision || unchanged.Card != m.Card {
		t.Fatal("conflicting public snapshot partially committed", err)
	}
}

func TestProgressBudgetOmitsOldestRecordsBeforeLatestSummary(t *testing.T) {
	v := cards.ProgressView{Body: "latest summary", Records: progressRecords(strings.Repeat("old-one", 400), strings.Repeat("old-two", 400), "newest record"), ElapsedSeconds: -1}
	var rendered cards.ProgressView
	render := func(v cards.ProgressView) string {
		rendered = v
		return cards.Task(cards.TaskView{Status: "inProgress", Progress: v})
	}
	limit := len(render(cards.ProgressView{Body: v.Body, Records: v.Records[2:], OmittedRecords: 2, ElapsedSeconds: -1}))
	card, err := boundedRenderedProgress(v, limit, render)
	if err != nil || len(card) > limit || !json.Valid([]byte(card)) || rendered.Body != v.Body || rendered.OmittedRecords != 2 || !reflect.DeepEqual(recordTexts(rendered.Records), []string{"newest record"}) {
		t.Fatalf("record eviction did not preserve the latest summary: %+v, %v", rendered, err)
	}
	if !strings.Contains(card, "省略") {
		t.Fatal("omitted records were not disclosed")
	}
}

func TestProgressBudgetTruncatesSingleLongRecordAndDoesNotMutateProjection(t *testing.T) {
	long := strings.Repeat("中文😀<at>\\\"", 500)
	v := cards.ProgressView{Body: "public summary", Records: []cards.ProgressRecord{{ItemID: "stable-id", Text: long, ReceivedAtMS: 200000}}, ElapsedSeconds: -1}
	var rendered cards.ProgressView
	render := func(v cards.ProgressView) string {
		rendered = v
		return cards.Task(cards.TaskView{Status: "completed", Progress: v})
	}
	limit := len(render(cards.ProgressView{Body: v.Body, Records: progressRecords("…（内容已截短）"), ElapsedSeconds: -1})) + 120
	card, err := boundedRenderedProgress(v, limit, render)
	if err != nil || len(card) > limit || !json.Valid([]byte(card)) || len(rendered.Records) != 1 || rendered.OmittedRecords != 0 || !strings.HasSuffix(rendered.Records[0].Text, "…（内容已截短）") || rendered.Body != v.Body {
		t.Fatal("long record lacked accurate truncation or actual byte bound", err)
	}
	if v.Records[0].Text != long || rendered.Records[0].ItemID != "stable-id" || rendered.Records[0].ReceivedAtMS != 200000 {
		t.Fatal("budget renderer mutated the complete snapshot projection")
	}
	if _, err := boundedRenderedProgress(v, 1, render); err == nil {
		t.Fatal("impossible budget silently lost required state")
	}
}

func TestProgressBudgetStillDisclosesOmissionsWhenNoRecordFits(t *testing.T) {
	v := cards.ProgressView{Body: strings.Repeat("latest public action", 100), Records: []cards.ProgressRecord{{Tools: &cards.ProgressTools{
		Completed: 1, Items: []cards.ProgressTool{{ItemID: "tool", Kind: "command", Status: "completed", Title: "运行 Go 测试"}},
	}}}, ElapsedSeconds: -1}
	var rendered cards.ProgressView
	render := func(v cards.ProgressView) string {
		rendered = v
		return cards.Task(cards.TaskView{Status: "inProgress", Progress: v, State: "available", Kind: "stop", Token: "opaque", Revision: 1})
	}
	// Reserve state, omission disclosure and controls. The empty-text tool group
	// cannot be text-truncated into a misleading partial set of factual counters.
	limit := len(render(cards.ProgressView{OmittedRecords: 1, ElapsedSeconds: -1})) + 100
	card, err := boundedRenderedProgress(v, limit, render)
	if err != nil || !json.Valid([]byte(card)) || len(card) > limit || len(rendered.Records) != 0 || rendered.OmittedRecords != 1 || !strings.Contains(card, "省略") || !strings.Contains(card, `"token":"opaque"`) {
		t.Fatal("all-omitted history hid its incompleteness or lost task control", err)
	}
}

func TestToolOnlyProgressUpdatesSameMessageAndRetainsTerminalCounts(t *testing.T) {
	e, _ := engineFixture(t)
	bindEngine(t, e)
	var latest cards.ProgressView
	e.ProgressRender = func(_, _, status string, v cards.ProgressView) string {
		latest = v
		return cards.Task(cards.TaskView{Status: status, Progress: v})
	}
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", ItemsView: "full", Items: []codexclient.Item{
		input("user", "request"),
		{ID: "command", Type: "commandExecution", Status: "inProgress", Text: "PRIVATE_TOOL_OUTPUT"},
		{ID: "mcp", Type: "mcpToolCall", Status: "inProgress"},
		{ID: "dynamic", Type: "dynamicToolCall", Status: "completed"},
		{ID: "file", Type: "fileChange", Status: "declined"},
		{ID: "unknown", Type: "fileChange", Status: "unknown"},
		{ID: "reason", Type: "reasoning", Status: "completed", Text: "PRIVATE_REASONING"},
	}}
	turn.Items = append(turn.Items, turn.Items[1]) // Exact paginated tool duplicate.
	apply(t, e, 200000, false, turn)
	if latest.Body != "" || len(latest.Records) != 1 || latest.RunningTools != 2 || latest.FinishedTools != 2 || latest.Records[0].Tools.Running != 2 || latest.Records[0].Tools.Completed != 1 || latest.Records[0].Tools.Declined != 1 || latest.Records[0].Tools.Unknown != 1 || len(latest.Records[0].Tools.Items) != 5 || !reflect.DeepEqual(latest.Records[0].Tools.Details, []string{"执行命令", "外部工具", "动态工具", "修改文件"}) {
		t.Fatalf("tool-only projection invented commentary or counted duplicate/unsupported status: %+v", latest)
	}
	g := &cardGateway{}
	drain(t, e, g, 1000)
	if len(g.writes) != 2 || !strings.Contains(g.writes[1].Card, "工具") || !strings.Contains(g.writes[1].Card, `"tag":"collapsible_panel"`) {
		t.Fatal("tool-only turn lost its factual tool group")
	}
	progressID := g.writes[1].ID
	turn.Items[1].Status, turn.Items[2].Status = "completed", "failed"
	turn.Items[len(turn.Items)-1] = turn.Items[1]
	apply(t, e, 201000, false, turn)
	drain(t, e, g, 3001)
	if len(g.writes) != 3 || g.writes[2].Operation != "update" || g.writes[2].ID != progressID {
		t.Fatal("tool state changes did not update the original progress card")
	}
	turn.Status = "completed"
	apply(t, e, 202000, false, turn)
	if latest.RunningTools != 0 || latest.FinishedTools != 4 || latest.Body != "" || len(latest.Records) != 1 || latest.Records[0].Tools.Completed != 2 || latest.Records[0].Tools.Failed != 1 || latest.Records[0].Tools.Declined != 1 {
		t.Fatal("terminal tool counts were erased or conflated")
	}
	drain(t, e, g, 5002)
	if len(g.writes) != 5 || g.writes[3].Operation != "update" || g.writes[3].ID != progressID || !strings.Contains(g.writes[3].Card, "已完成") || !strings.Contains(g.writes[3].Card, `"tag":"collapsible_panel"`) || !strings.Contains(g.writes[3].Card, "工具") {
		t.Fatal("terminal tool-only snapshot failed to update the same progress card")
	}
	if strings.Contains(g.writes[3].Card, "PRIVATE_TOOL_OUTPUT") || strings.Contains(g.writes[3].Card, "PRIVATE_REASONING") {
		t.Fatal("tool counter projection leaked private payloads")
	}
}

func recordTexts(records []cards.ProgressRecord) []string {
	var result []string
	for _, record := range records {
		if record.Tools == nil {
			result = append(result, record.Text)
		}
	}
	return result
}

func progressRecords(texts ...string) []cards.ProgressRecord {
	result := make([]cards.ProgressRecord, len(texts))
	for n, text := range texts {
		result[n] = cards.ProgressRecord{Text: text}
	}
	return result
}
