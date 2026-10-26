package sync

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/cot"
)

func projectedTool(t *testing.T, item codexclient.Item) cot.Item {
	t.Helper()
	item.ID = "synthetic-tool"
	raw, err := cotDocument("thread", codexclient.Turn{ID: "turn", Status: "inProgress", ItemsView: "full", Items: []codexclient.Item{item}}, nil, 1000)
	doc := parseProjection(t, raw, err)
	if len(doc.Items) != 1 {
		t.Fatal("expected exactly one native tool invocation")
	}
	return doc.Items[0]
}

func TestCOTToolTitlesSeparateCompositeActionsAndSafeDetails(t *testing.T) {
	for _, tc := range []struct {
		name, command, want string
		actions             []codexclient.CommandAction
		details             []string
	}{
		{"single", "cat /PRIVATE_DIRECTORY/fixture.go", "读取 fixture.go", []codexclient.CommandAction{{Type: "read", Path: "/PRIVATE_DIRECTORY/fixture.go"}}, nil},
		{"read-search", "cat /PRIVATE_DIRECTORY/fixture.go && rg PRIVATE_QUERY .", "读取与检索文件", []codexclient.CommandAction{{Type: "read", Path: "/PRIVATE_DIRECTORY/fixture.go"}, {Type: "search"}}, []string{"读取 fixture.go", "搜索内容"}},
		{"mixed", "cat /PRIVATE_DIRECTORY/fixture.go && git status && go test ./... -run PRIVATE_PATTERN", "执行复合命令", []codexclient.CommandAction{{Type: "read", Path: "/PRIVATE_DIRECTORY/fixture.go"}, {Type: "read", Path: "/PRIVATE_DIRECTORY/other.go"}}, []string{"读取 fixture.go", "读取 other.go", "查看 Git 状态", "运行 Go 测试"}},
		{"unknown", "tail PRIVATE_ARGUMENT", "运行 tail", nil, nil},
		{"unsupported", "PRIVATE_PROGRAM PRIVATE_ARGUMENT", "运行自定义命令", nil, nil},
		{"mixed-unknown", "cat fixture.go && PRIVATE_PROGRAM PRIVATE_ARGUMENT", "执行复合命令", nil, []string{"读取文件", "运行自定义命令"}},
		{"mixed-unknown-native", "cat fixture.go && ./PRIVATE_PROGRAM PRIVATE_ARGUMENT", "执行复合命令", []codexclient.CommandAction{{Type: "read", Path: "/PRIVATE_DIRECTORY/fixture.go"}}, []string{"读取 fixture.go", "运行自定义命令"}},
		{"mixed-unknown-native-wrapped", "bash -lc 'cat fixture.go && ./PRIVATE_PROGRAM PRIVATE_ARGUMENT'", "执行复合命令", []codexclient.CommandAction{{Type: "read", Path: "/PRIVATE_DIRECTORY/fixture.go"}}, []string{"读取 fixture.go", "运行自定义命令"}},
		{"search-unknown-native", "rg PRIVATE_QUERY . && ./PRIVATE_PROGRAM PRIVATE_ARGUMENT", "执行复合命令", []codexclient.CommandAction{{Type: "search"}}, []string{"搜索内容", "运行自定义命令"}},
		{"empty-native", "", "读取 fixture.go", []codexclient.CommandAction{{Type: "read", Path: "/PRIVATE_DIRECTORY/fixture.go"}}, nil},
		{"unknown-native", "./PRIVATE_PROGRAM PRIVATE_ARGUMENT", "读取 fixture.go", []codexclient.CommandAction{{Type: "read", Path: "/PRIVATE_DIRECTORY/fixture.go"}}, nil},
		{"unparsed-native", "for PRIVATE_VAR in PRIVATE_LIST; do cat fixture.go; done", "读取 fixture.go", []codexclient.CommandAction{{Type: "read", Path: "/PRIVATE_DIRECTORY/fixture.go"}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			public := projectedTool(t, codexclient.Item{Type: "commandExecution", Command: tc.command, CommandActions: tc.actions, Status: "completed"})
			if public.Title != tc.want || public.ToolName != "commandExecution" || !reflect.DeepEqual(public.Details, tc.details) {
				t.Fatalf("unexpected projection: %+v", public)
			}
			// The heading/details classifier remains metadata-only. Execution
			// excerpts are covered independently by the preview regressions.
			public.InputPreview = ""
			encoded, _ := json.Marshal(public)
			if strings.Contains(string(encoded), "PRIVATE_") || strings.Contains(string(encoded), "用途未识别") || public.ResultSummary != "" {
				t.Fatal("projection exposed raw source or invented execution results")
			}
		})
	}
}

func TestCOTFileChangeResultsCountDistinctPathsOnlyAfterSuccess(t *testing.T) {
	changes := []codexclient.FileChange{
		{Path: "/PRIVATE_DIRECTORY/one/fixture.go", Kind: json.RawMessage(`{"type":"update"}`), Diff: "@@\n+password=PRIVATE_DIFF"},
		{Path: "/PRIVATE_DIRECTORY/two/fixture.go", Kind: json.RawMessage(`{"type":"add"}`)},
		{Path: "/PRIVATE_DIRECTORY/one/fixture.go", Kind: json.RawMessage(`{"type":"update"}`)},
	}
	for _, status := range []string{"inProgress", "completed", "failed", "declined", "interrupted", ""} {
		public := projectedTool(t, codexclient.Item{Type: "fileChange", Status: status, Changes: changes})
		want := ""
		if status == "completed" {
			want = "已修改 2 个文件"
		}
		if public.Title != "修改文件" || public.ResultSummary != want || !reflect.DeepEqual(public.Details, []string{"编辑 fixture.go", "写入 fixture.go"}) {
			t.Fatalf("file status %q was misrepresented: %+v", status, public)
		}
		encoded, _ := json.Marshal(public)
		if strings.Contains(string(encoded), "PRIVATE_") {
			t.Fatal("full path or diff escaped the file projection")
		}
	}
	public := projectedTool(t, codexclient.Item{Type: "fileChange", Status: "completed", Changes: []codexclient.FileChange{{}}})
	if public.ResultSummary != "" {
		t.Fatal("missing paths became a successful file count")
	}
}

func TestCOTExecutionFactsRespectNativeVariantsAndNullableZero(t *testing.T) {
	zero, negative, outside := 0, -1, int(int64(1)<<31)
	zeroMS, positiveMS, negativeMS := int64(0), int64(25), int64(-1)
	for _, tc := range []struct {
		name       string
		item       codexclient.Item
		exit       *int
		durationMS *int64
	}{
		{"missing", codexclient.Item{Type: "commandExecution"}, nil, nil},
		{"zero", codexclient.Item{Type: "commandExecution", ExitCode: &zero, DurationMS: &zeroMS}, &zero, &zeroMS},
		{"negative-exit", codexclient.Item{Type: "commandExecution", ExitCode: &negative, DurationMS: &positiveMS}, &negative, &positiveMS},
		{"invalid", codexclient.Item{Type: "commandExecution", ExitCode: &outside, DurationMS: &negativeMS}, nil, nil},
		{"mcp", codexclient.Item{Type: "mcpToolCall", ExitCode: &zero, DurationMS: &positiveMS}, nil, &positiveMS},
		{"dynamic", codexclient.Item{Type: "dynamicToolCall", ExitCode: &zero, DurationMS: &zeroMS}, nil, &zeroMS},
		{"file", codexclient.Item{Type: "fileChange", ExitCode: &zero, DurationMS: &positiveMS}, nil, nil},
		{"web", codexclient.Item{Type: "webSearch", ExitCode: &zero, DurationMS: &positiveMS}, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			public := projectedTool(t, tc.item)
			if !reflect.DeepEqual(public.ExitCode, tc.exit) || !reflect.DeepEqual(public.DurationMS, tc.durationMS) {
				t.Fatalf("execution facts changed: %+v", public)
			}
			if public.TimestampMS != 1000 || public.Status != "unknown" {
				t.Fatal("metadata invented a status or native occurrence time")
			}
		})
	}
}

func TestCOTToolDetailBoundsAndSafeDescriptorNames(t *testing.T) {
	item := codexclient.Item{Type: "commandExecution", Command: "PRIVATE_PROGRAM PRIVATE_ARGUMENT"}
	for i := range 12 {
		item.CommandActions = append(item.CommandActions, codexclient.CommandAction{Type: "read", Path: fmt.Sprintf("/PRIVATE_DIRECTORY/%d-%s.go", i, strings.Repeat("界", 100))})
	}
	public := projectedTool(t, item)
	if len(public.Details) != 6 || public.Details[5] != "另有其他文件或动作" || public.Title != "读取文件" {
		t.Fatalf("missing bounded detail omission: %+v", public)
	}
	for _, text := range append(public.Details, public.Title, public.ResultSummary, public.ToolName) {
		if !utf8.ValidString(text) || len([]rune(text)) > 160 || strings.ContainsAny(text, "\r\n") {
			t.Fatal("tool content exceeded its Unicode or single-line boundary")
		}
	}
	for _, typ := range []string{"mcpToolCall", "dynamicToolCall"} {
		public = projectedTool(t, codexclient.Item{Type: typ, Server: strings.Repeat("服", 100), Namespace: strings.Repeat("域", 100), Tool: strings.Repeat("具", 100)})
		if len([]rune(public.Title)) > 32 || len([]rune(public.ToolName)) > 64 || public.ToolName == typ {
			t.Fatal("long native descriptors were lost or unbounded")
		}
		if !strings.HasSuffix(public.ToolName, "."+strings.Repeat("具", 32)) {
			t.Fatal("long namespace hid the actual tool name")
		}
		public = projectedTool(t, codexclient.Item{Type: typ, Server: "unsafe/PRIVATE_SERVER", Namespace: "unsafe/PRIVATE_NAMESPACE", Tool: "<at id=all>PRIVATE_TOOL</at>"})
		encoded, _ := json.Marshal(public)
		if strings.Contains(string(encoded), "PRIVATE_") || public.ToolName != typ {
			t.Fatal("unvalidated tool descriptor escaped its boundary")
		}
	}
}

func TestCOTToolDetailsPublishOnlyRedactedSupportedNativeText(t *testing.T) {
	var turn codexclient.Turn
	err := json.Unmarshal([]byte(`{"id":"turn","status":"completed","itemsView":"full","items":[
		{"id":"command","type":"commandExecution","status":"completed","command":"tail fixture.log --token PRIVATE_COMMAND_ARGUMENT","cwd":"/PRIVATE_CWD","aggregatedOutput":"matched 3 files\nAPI_KEY=PRIVATE_STDOUT_STDERR","exitCode":0,"durationMs":25,"commandActions":[{"type":"read","path":"/PRIVATE_DIRECTORY/fixture.go","command":"PRIVATE_ACTION_COMMAND"}]},
		{"id":"mcp","type":"mcpToolCall","status":"failed","server":"fixture_server","tool":"lookup","arguments":{"api_key":"PRIVATE_ARGUMENTS","query":"fixture"},"result":{"content":[{"type":"text","text":"found fixture\nsecret=PRIVATE_RESULT"},{"type":"image","text":"PRIVATE_MEDIA"}]},"error":{"message":"lookup failed\npassword=PRIVATE_ERROR"},"durationMs":30},
		{"id":"dynamic","type":"dynamicToolCall","status":"inProgress","namespace":"fixture_namespace","tool":"inspect","arguments":{"secret":"PRIVATE_DYNAMIC_ARGUMENTS","path":"fixture.go"},"contentItems":[{"type":"inputText","text":"checked fixture.go\nAuthorization: Bearer PRIVATE_DYNAMIC_OUTPUT"},{"type":"inputImage","text":"PRIVATE_IMAGE"}],"success":null},
		{"id":"reasoning","type":"reasoning","text":"PRIVATE_REASONING"}
	]}`), &turn)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := cotDocument("thread", turn, nil, 1000)
	doc := parseProjection(t, raw, err)
	if len(doc.Items) != 3 || strings.Contains(raw, "PRIVATE_") || doc.Items[1].ToolName != "fixture_server.lookup" || doc.Items[2].ToolName != "fixture_namespace.inspect" {
		t.Fatal("private native payload escaped the tool projection")
	}
	for _, want := range []string{"tail fixture.log", "matched 3 files", "lookup failed", "found fixture", "checked fixture.go"} {
		if !strings.Contains(raw, want) {
			t.Fatalf("useful native detail %q was dropped", want)
		}
	}
}
