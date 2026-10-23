package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

func TestNativeToolGroupsFollowPublicHistoryBoundariesAndKeepStatesSeparate(t *testing.T) {
	var turn codexclient.Turn
	if err := json.Unmarshal([]byte(`{"id":"turn","status":"inProgress","itemsView":"full","items":[
	 {"id":"user","type":"userMessage","content":[{"type":"text","text":"PRIVATE_USER"}]},
	 {"id":"first","type":"agentMessage","phase":"commentary","text":"先查看文件。"},
	 {"id":"read","type":"commandExecution","status":"completed","command":"PRIVATE_COMMAND","commandActions":[{"type":"read","path":"/PRIVATE_DIRECTORY/fixture.go"},{"type":"search","query":"PRIVATE_QUERY"},{"type":"read"},{"type":"listFiles"}]},
	 {"id":"reason","type":"reasoning","summary":["PRIVATE_REASONING"]},
	 {"id":"mcp","type":"mcpToolCall","server":"fixture","tool":"lookup","status":"failed","arguments":{"secret":"PRIVATE_ARGUMENT"}},
	 {"id":"dynamic","type":"dynamicToolCall","namespace":"fixture","tool":"inspect","status":"inProgress"},
	 {"id":"file","type":"fileChange","status":"declined","changes":[]},
	 {"id":"future","type":"commandExecution","status":"future"},
	 {"id":"web","type":"webSearch","query":"PRIVATE_SEARCH","action":{"type":"search"}},
	 {"id":"image","type":"imageView","path":"/PRIVATE_IMAGE"},
	 {"id":"collab","type":"collabAgentToolCall","tool":"spawnAgent","status":"interrupted","agentsStates":{"child":{"status":"completed"}}},
	 {"id":"activity","type":"subAgentActivity","kind":"toolCall","agentThreadId":"PRIVATE_CHILD"},
	 {"id":"sleep","type":"sleep","durationMs":100},
	 {"id":"next","type":"agentMessage","phase":"partial_answer","text":"继续校验。"},
	 {"id":"command2","type":"commandExecution","status":"completed","commandActions":[{"type":"unknown"}]},
	 {"id":"final","type":"agentMessage","phase":"final_answer","text":"PRIVATE_FINAL"},
	 {"id":"command3","type":"commandExecution","status":"inProgress"},
	 {"id":"user2","type":"userMessage","content":[{"type":"text","text":"PRIVATE_USER2"}]},
	 {"id":"command4","type":"commandExecution","status":"failed"}
	]}`), &turn); err != nil {
		t.Fatal(err)
	}
	view := progressView(turn, 200000)
	if len(view.Records) != 6 || view.Body != "继续校验。" || view.CurrentItemID != "next" || view.RunningTools != 2 || view.FinishedTools != 5 {
		t.Fatalf("native grouping or live summary mismatch: %+v", view)
	}
	first := view.Records[1]
	wantGroup := &cards.ProgressTools{Running: 1, Completed: 1, Failed: 1, Declined: 1, Unknown: 4, Details: []string{"读取文件", "搜索内容", "列出目录", "MCP · fixture / lookup", "工具 · fixture.inspect", omittedToolDetails}}
	actualGroup := *first.Tools
	actualGroup.Items = nil
	if first.ItemID != "read" || first.Text != "" || first.ReceivedAtMS != 0 || !reflect.DeepEqual(&actualGroup, wantGroup) || len(first.Tools.Items) != 8 || first.Tools.Items[0].Title != "读取 fixture.go 等；搜索内容；列出目录" {
		t.Fatalf("tool group lost facts or bounded labels: %+v", first)
	}
	if got := first.Tools.Items[1]; got.ItemID != "mcp" || got.Kind != "mcp" || got.Title != "MCP · fixture / lookup" || got.Status != "failed" {
		t.Fatal("native MCP invocation lost its safe descriptor or status")
	}
	if got := first.Tools.Items[2]; got.ItemID != "dynamic" || got.Kind != "dynamic" || got.Title != "工具 · fixture.inspect" || got.Status != "inProgress" {
		t.Fatal("native dynamic invocation lost its safe descriptor or status")
	}
	for _, index := range []int{3, 4, 5} {
		if view.Records[index].Tools == nil || view.Records[index].Text != "" {
			t.Fatal("final/user boundaries did not split tool groups")
		}
	}
	encoded, _ := json.Marshal(view)
	if strings.Contains(string(encoded), "PRIVATE_") {
		t.Fatal("tool projection leaked private content")
	}
	turn.Status = "completed"
	terminal := progressView(turn, 300000)
	if terminal.Body != "" || terminal.CurrentItemID != "" || !reflect.DeepEqual(terminal.Records, view.Records) {
		t.Fatal("terminal snapshot erased ordered tool facts")
	}
}

func TestToolLabelMetadataIsSafeBoundedAndNeverCountsUnavailableStatusAsSuccess(t *testing.T) {
	for _, name := range []string{"/private/path", `C:\private\path`, "x\ny", "<at id=all>", "bad name", "https://secret"} {
		for _, typ := range []string{"mcpToolCall", "dynamicToolCall"} {
			labels, _ := progressToolLabels(codexclient.Item{Type: typ, Tool: name, Server: name, Namespace: name})
			if strings.Contains(strings.Join(labels, ""), name) {
				t.Fatalf("unsafe descriptor became a public label: %q", name)
			}
		}
	}
	labels, _ := progressToolLabels(codexclient.Item{Type: "dynamicToolCall", Tool: strings.Repeat("safe", 100)})
	if len([]rune(labels[0])) > 35 || !strings.Contains(labels[0], "…") {
		t.Fatal("descriptor was not visibly bounded")
	}
	view := progressView(codexclient.Turn{Status: "inProgress", Items: []codexclient.Item{
		{ID: "web", Type: "webSearch", Status: "completed"},
		{ID: "image", Type: "imageView", Status: "completed"},
		{ID: "mcp", Type: "mcpToolCall", Status: "declined"},
		{ID: "dynamic", Type: "dynamicToolCall", Status: "declined"},
	}}, 200000)
	if view.FinishedTools != 0 || view.Records[0].Tools.Unknown != 4 || view.Records[0].Tools.Completed != 0 || view.Records[0].Tools.Declined != 0 {
		t.Fatal("unavailable or unsupported native states became execution facts")
	}
}

func TestToolGroupsReplayDeduplicateAndDoNotCreateReceptionClocks(t *testing.T) {
	e, path := engineFixture(t)
	var latest cards.ProgressView
	e.ProgressRender = func(_, _, status string, v cards.ProgressView) string {
		latest = v
		return cards.Task(cards.TaskView{Status: status, Progress: v})
	}
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", ItemsView: "full", Items: []codexclient.Item{
		input("user", "request"), agent("public", "checking", "commentary"),
		{ID: "tool", Type: "mcpToolCall", Server: "fixture", Tool: "lookup", Status: "inProgress"},
	}}
	turn.Items = append(turn.Items, turn.Items[2])
	apply(t, e, 200000, false, turn)
	if len(latest.Records) != 2 || latest.Records[1].Tools.Running != 1 || latest.Records[1].ReceivedAtMS != 0 || len(latest.Records[1].Tools.Items) != 1 || latest.Records[1].Tools.Items[0].ItemID != "tool" {
		t.Fatal("duplicate tool was counted or assigned a commentary clock")
	}
	ctx := context.Background()
	m, err := e.Store.Mirror(ctx, "host", "thread", mirrorKey("progress", "turn"))
	if err != nil || !reflect.DeepEqual(m.ProgressObservedAt, map[string]int64{"public": 200000}) {
		t.Fatal("tool group wrote public-message clocks", err)
	}
	e.Store.Close()
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	e.Store = reopened
	apply(t, e, 300000, true, turn)
	again, err := e.Store.Mirror(ctx, "host", "thread", m.Key)
	if err != nil || again.Card != m.Card || again.Revision != m.Revision || !reflect.DeepEqual(again.ProgressObservedAt, m.ProgressObservedAt) {
		t.Fatal("identical reconnect duplicated or retimed tool groups", err)
	}
	turn.Items[3].Tool = "conflicting_descriptor"
	if err := e.Apply(ctx, "thread", []codexclient.Turn{turn}, false, 301000); err == nil {
		t.Fatal("conflicting duplicate native tool metadata was accepted")
	}
}

func TestToolItemsKeepSameNameCallsDistinctAndExposeOnlyFileBasenames(t *testing.T) {
	var turn codexclient.Turn
	if err := json.Unmarshal([]byte(`{"status":"inProgress","items":[
	 {"id":"read-one","type":"commandExecution","command":"PRIVATE_COMMAND","commandActions":[{"type":"read","name":"fixture.go","path":"/PRIVATE_DIRECTORY/fixture.go"}]},
	 {"id":"read-two","type":"commandExecution","status":"completed","commandActions":[{"type":"read","name":"fixture.go","path":"/PRIVATE_DIRECTORY/fixture.go"}]},
	 {"id":"write","type":"fileChange","status":"completed","changes":[{"path":"/PRIVATE_DIRECTORY/new.go","kind":{"type":"add"},"diff":"PRIVATE_DIFF"}]},
	 {"id":"edit","type":"fileChange","status":"failed","changes":[{"path":"/PRIVATE_DIRECTORY/existing.go","kind":{"type":"update","move_path":"/PRIVATE_DESTINATION/renamed.go"},"diff":"PRIVATE_DIFF"}]},
	 {"id":"delete","type":"fileChange","status":"declined","changes":[{"path":"/PRIVATE_DIRECTORY/old.go","kind":{"type":"delete"},"diff":"PRIVATE_DIFF"}]},
	 {"id":"mixed","type":"fileChange","status":"completed","changes":[{"path":"/PRIVATE_DIRECTORY/first.go","kind":{"type":"add"},"diff":"PRIVATE_DIFF"},{"path":"/PRIVATE_DIRECTORY/second.go","kind":{"type":"update"},"diff":"PRIVATE_DIFF"},{"path":"/PRIVATE_DIRECTORY/third.go","kind":{"type":"delete"},"diff":"PRIVATE_DIFF"}]},
	 {"id":"search","type":"commandExecution","status":"completed","commandActions":[{"type":"search","query":"PRIVATE_QUERY"}]},
	 {"id":"list","type":"commandExecution","status":"completed","commandActions":[{"type":"listFiles","path":"/PRIVATE_DIRECTORY/project"}]}
	]}`), &turn); err != nil {
		t.Fatal(err)
	}
	view := progressView(turn, 200000)
	tools := view.Records[0].Tools
	want := []struct{ id, kind, status, title string }{
		{"read-one", "read", "unknown", "读取 fixture.go"},
		{"read-two", "read", "completed", "读取 fixture.go"},
		{"write", "write", "completed", "写入 new.go"},
		{"edit", "edit", "failed", "编辑 existing.go"},
		{"delete", "delete", "declined", "删除 old.go"},
		{"mixed", "edit", "completed", "写入 first.go 等"},
		{"search", "search", "completed", "搜索内容"},
		{"list", "list", "completed", "列出目录 project"},
	}
	if len(tools.Items) != len(want) || tools.Completed != 5 || tools.Unknown != 1 || tools.Failed != 1 || tools.Declined != 1 || tools.OmittedItems != 0 {
		t.Fatalf("native calls were merged or counted per file/action: %+v", tools)
	}
	for n, expected := range want {
		got := tools.Items[n]
		if got.ItemID != expected.id || got.Kind != expected.kind || got.Status != expected.status || got.Title != expected.title || len(got.Details) == 0 || len(got.Details) > 6 {
			t.Fatalf("call %d lost identity, classification or safe metadata: %+v", n, got)
		}
	}
	if !reflect.DeepEqual(tools.Items[5].Details, []string{"写入 first.go", "编辑 second.go", "删除 third.go"}) {
		t.Fatal("multi-file call lost its ordered per-file actions")
	}
	encoded, _ := json.Marshal(view)
	if strings.Contains(string(encoded), "PRIVATE_") || strings.Contains(string(encoded), "renamed.go") {
		t.Fatal("progress exposed command/query/output, directories or unmodeled move metadata")
	}
}

func TestToolFileMetadataRejectsActiveSyntaxURLsAndControlCharacters(t *testing.T) {
	for _, value := range []string{"/private/<at id=all>.go", "/private/[link](evil).go", "/private/`tool`.go", "/private/a\nb.go", "/private/a\u202eb.go", "https://secret.invalid/file.go", "https:/secret.invalid/file.go", "data:text/file.go", "  fixture.go", "/private/..", "/"} {
		if got := progressFileName(value); got != "" {
			t.Fatalf("unsafe file metadata became a title: %q -> %q", value, got)
		}
		view := progressView(codexclient.Turn{Status: "inProgress", Items: []codexclient.Item{{ID: "tool", Type: "commandExecution", CommandActions: []codexclient.CommandAction{{Type: "read", Path: value}}}}}, 200000)
		if view.Records[0].Tools.Items[0].Title != "读取文件" {
			t.Fatal("unavailable file identity was guessed")
		}
	}
	for value, expected := range map[string]string{"/project/fixture.go": "fixture.go", `C:\project\fixture.go`: "fixture.go", "../../fixture.go": "fixture.go", "/project/中文文件.go": "中文文件.go", "/project/file with spaces.go": "file with spaces.go", "/project/foo_bar.go": "foo_bar.go", "/project/AGENTS.md": "AGENTS.md", "/project/foo-bar.go": "foo-bar.go", "/project/cafe\u0301.go": "cafe\u0301.go"} {
		if got := progressFileName(value); got != expected {
			t.Fatalf("safe metadata lost basename: %q -> %q", value, got)
		}
	}
	if got := progressFileName(strings.Repeat("字", 100) + ".go"); len([]rune(got)) != 40 || !strings.HasSuffix(got, "…") {
		t.Fatal("long file names were not visibly bounded")
	}
	var actions []codexclient.CommandAction
	for n := 0; n < 8; n++ {
		actions = append(actions, codexclient.CommandAction{Type: "read", Name: fmt.Sprintf("file-%d.go", n)})
	}
	view := progressView(codexclient.Turn{Status: "inProgress", Items: []codexclient.Item{{ID: "tool", Type: "commandExecution", CommandActions: actions}}}, 200000)
	tool := view.Records[0].Tools.Items[0]
	if len(tool.Details) != 6 || tool.Details[5] != "另有其他文件或动作" || view.Records[0].Tools.Unknown != 1 {
		t.Fatal("a multi-action call lost its bound, omission disclosure or single-call count")
	}
}

func TestToolGroupKeepsLatestNativeItemsAndFullCountsAcrossReconciliation(t *testing.T) {
	e, _ := engineFixture(t)
	var latest cards.ProgressView
	e.ProgressRender = func(_, _, _ string, v cards.ProgressView) string {
		latest = v
		encoded, _ := json.Marshal(v)
		return string(encoded)
	}
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", ItemsView: "full", Items: []codexclient.Item{input("user", "request")}}
	for n := 0; n < cards.MaxProgressToolItems+8; n++ {
		turn.Items = append(turn.Items, codexclient.Item{ID: fmt.Sprintf("tool-%02d", n), Type: "commandExecution", Status: "completed", CommandActions: []codexclient.CommandAction{{Type: "read", Name: "fixture.go"}}})
	}
	apply(t, e, 200000, false, turn)
	tools := latest.Records[0].Tools
	if latest.Records[0].ItemID != "tool-00" || tools.Completed != 40 || len(tools.Items) != cards.MaxProgressToolItems || tools.OmittedItems != 8 || tools.Items[0].ItemID != "tool-08" || tools.Items[31].ItemID != "tool-39" {
		t.Fatalf("group retention cap altered native counts or order: %+v", tools)
	}
	first, _ := json.Marshal(latest)
	apply(t, e, 300000, true, turn)
	again, _ := json.Marshal(latest)
	if string(first) != string(again) {
		t.Fatal("complete replay accumulated duplicate calls or omission counts")
	}
}

func TestProgressBudgetCopiesNestedPerCallDetailsBeforeRendering(t *testing.T) {
	view := cards.ProgressView{ElapsedSeconds: -1, Records: []cards.ProgressRecord{{ItemID: "group", Tools: &cards.ProgressTools{Completed: 1, Details: []string{"读取文件"}, Items: []cards.ProgressTool{{ItemID: "call", Kind: "read", Status: "completed", Title: "读取 fixture.go", Details: []string{"读取 fixture.go"}}}}}}}
	before, _ := json.Marshal(view)
	render := func(v cards.ProgressView) string {
		// Exercise the ownership boundary even if a renderer clips or rewrites
		// nested display metadata while applying its component budget.
		v.Records[0].Tools.Items[0].Details[0] = "display copy"
		v.Records[0].Tools.Items[0].Title = "display title"
		v.Records[0].Tools.Details[0] = "display category"
		encoded, _ := json.Marshal(v)
		return string(encoded)
	}
	card, err := boundedRenderedProgress(view, 4096, render)
	if err != nil || len(card) > 4096 || !json.Valid([]byte(card)) {
		t.Fatal("nested tool metadata failed its actual JSON budget", err)
	}
	after, _ := json.Marshal(view)
	if string(before) != string(after) {
		t.Fatal("budget rendering mutated original per-call slices or details")
	}
}

func TestProgressToolByteBudgetKeepsNewestCallsFullCountsAndAccurateOmissions(t *testing.T) {
	tools := &cards.ProgressTools{Completed: 40, OmittedItems: 8, Details: []string{"读取文件"}}
	for n := 8; n < 40; n++ {
		tools.Items = append(tools.Items, cards.ProgressTool{ItemID: fmt.Sprintf("tool-%02d", n), Kind: "read", Status: "completed", Title: "读取 fixture.go", Details: []string{strings.Repeat("安全元信息", 20)}})
	}
	view := cards.ProgressView{ElapsedSeconds: -1, Records: []cards.ProgressRecord{{ItemID: "tool-00", Tools: tools}}}
	before, _ := json.Marshal(view)
	var rendered cards.ProgressView
	render := func(v cards.ProgressView) string {
		rendered = v
		encoded, _ := json.Marshal(v)
		return string(encoded)
	}
	card, err := boundedRenderedProgress(view, 1600, render)
	if err != nil || !json.Valid([]byte(card)) || len(card) > 1600 || len(rendered.Records) != 1 || rendered.OmittedRecords != 0 {
		t.Fatal("tool detail eviction discarded a group whose summary and latest calls fit", err)
	}
	got := rendered.Records[0].Tools
	if len(got.Items) == 0 || len(got.Items) >= cards.MaxProgressToolItems || got.Completed != 40 || got.OmittedItems+len(got.Items) != 40 || got.Items[0].ItemID != fmt.Sprintf("tool-%02d", got.OmittedItems) || got.Items[len(got.Items)-1].ItemID != "tool-39" {
		t.Fatalf("byte clipping changed counts, omission disclosure or native order: %+v", got)
	}
	after, _ := json.Marshal(view)
	if string(before) != string(after) {
		t.Fatal("byte clipping changed the complete native projection")
	}
	minimal := render(cards.ProgressView{ElapsedSeconds: -1, OmittedRecords: 1})
	card, err = boundedRenderedProgress(view, len(minimal), render)
	if err != nil || len(card) > len(minimal) || len(rendered.Records) != 0 || rendered.OmittedRecords != 1 {
		t.Fatal("a summary that cannot fit was not omitted with an explicit record count", err)
	}
}

func TestToolGroupsRespectRecordCountAndActualJSONBudgetWithoutMutatingProjection(t *testing.T) {
	view := cards.ProgressView{ElapsedSeconds: -1}
	for n := 0; n < 40; n++ {
		view.Records = append(view.Records, cards.ProgressRecord{ItemID: fmt.Sprintf("tool-%02d", n), Tools: &cards.ProgressTools{Completed: 1, Details: []string{strings.Repeat("中文😀<at>\\\"", 100)}}})
	}
	before, _ := json.Marshal(view)
	var rendered cards.ProgressView
	render := func(v cards.ProgressView) string {
		rendered = v
		data, _ := json.Marshal(v)
		return string(data)
	}
	card, err := boundedRenderedProgress(view, 1024, render)
	if err != nil || len(card) > 1024 || !json.Valid([]byte(card)) || len(rendered.Records) != 0 || rendered.OmittedRecords != 40 {
		t.Fatalf("oversized empty-text tool group was not omitted accurately: %+v, %v", rendered, err)
	}
	after, _ := json.Marshal(view)
	if string(after) != string(before) {
		t.Fatal("budget rendering mutated tool metadata")
	}
	card, err = boundedRenderedProgress(view, 1000000, render)
	if err != nil || len(rendered.Records) != cards.MaxProgressRecords || rendered.OmittedRecords != 8 || rendered.Records[0].ItemID != "tool-08" || len(card) > 1000000 {
		t.Fatal("tool groups did not share the 32-record cap", err)
	}
}
