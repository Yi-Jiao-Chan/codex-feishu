package codexclient

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNativeToolMetadataDecodesTypedTextAndDropsUnmodeledPayloads(t *testing.T) {
	// Synthetic 0.162.1 ThreadItem shapes: classifications are best-effort;
	// only explicitly modeled text and metadata fields are retained.
	var items []Item
	err := json.Unmarshal([]byte(`[
	 {"type":"commandExecution","id":"command","command":"synthetic command","commandActions":[{"type":"read","command":"PRIVATE_ACTION_COMMAND","name":"fixture.go","path":"/project/fixture.go"},{"type":"search","path":"/project","query":"PRIVATE_QUERY"},{"type":"listFiles","path":null},{"type":"unknown"}],"status":"completed","aggregatedOutput":"synthetic stdout"},
	 {"type":"mcpToolCall","id":"mcp","server":"fixture_server","tool":"lookup","status":"failed","arguments":{"path":"/project/fixture.go"},"result":{"content":[{"type":"text","text":"synthetic result"},{"type":"image","imageUrl":"PRIVATE_IMAGE","uri":"PRIVATE_URI","_meta":{"value":"PRIVATE_META"}}],"structuredContent":{"answer":"synthetic answer"},"_meta":{"value":"PRIVATE_RESULT_META"}},"error":{"message":"synthetic failure"},"durationMs":30},
	 {"type":"dynamicToolCall","id":"dynamic","namespace":"fixture_namespace","tool":"inspect","status":"inProgress","contentItems":[{"type":"inputText","text":"synthetic input"},{"type":"inputImage","imageUrl":"PRIVATE_DYNAMIC_IMAGE"}],"success":null}
	]`), &items)
	if err != nil || len(items) != 3 {
		t.Fatal("native metadata did not decode", err)
	}
	if !reflect.DeepEqual(items[0].CommandActions, []CommandAction{{Type: "read", Name: "fixture.go", Path: "/project/fixture.go"}, {Type: "search", Path: "/project"}, {Type: "listFiles"}, {Type: "unknown"}}) || items[1].Server != "fixture_server" || items[1].Tool != "lookup" || items[1].Status != "failed" || items[2].Namespace != "fixture_namespace" || items[2].Tool != "inspect" {
		t.Fatalf("native metadata was lost: %+v", items)
	}
	if items[0].AggregatedOutput == nil || *items[0].AggregatedOutput != "synthetic stdout" {
		t.Fatalf("command output was not modeled: %+v", items[0].AggregatedOutput)
	}
	if string(items[1].Arguments) != `{"path":"/project/fixture.go"}` || items[1].Result == nil || string(items[1].Result.StructuredContent) != `{"answer":"synthetic answer"}` || items[1].Error == nil || items[1].Error.Message != "synthetic failure" {
		t.Fatalf("MCP payload was not modeled: %+v", items[1])
	}
	if !reflect.DeepEqual(items[1].Result.Content, []ToolOutputContent{{Type: "text", Text: "synthetic result"}, {Type: "image"}}) || !reflect.DeepEqual(items[2].ContentItems, []ToolOutputContent{{Type: "inputText", Text: "synthetic input"}, {Type: "inputImage"}}) {
		t.Fatalf("tool content was not modeled safely: %+v", items)
	}
	for _, item := range items {
		if item.PublicText() != "" {
			t.Fatal("native tool payload became public text")
		}
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		t.Fatal("native metadata did not re-encode", err)
	}
	for _, unwanted := range []string{"PRIVATE_ACTION_COMMAND", "PRIVATE_QUERY", "PRIVATE_IMAGE", "PRIVATE_URI", "PRIVATE_META", "PRIVATE_RESULT_META", "PRIVATE_DYNAMIC_IMAGE", `"imageUrl"`, `"uri"`, `"_meta"`} {
		if strings.Contains(string(encoded), unwanted) {
			t.Fatalf("unmodeled native payload was retained: %s", unwanted)
		}
	}
}

func TestNativeToolAggregatedOutputPreservesMissingNullAndEmpty(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		present bool
	}{
		{name: "missing", raw: `{"type":"commandExecution"}`},
		{name: "null", raw: `{"type":"commandExecution","aggregatedOutput":null}`},
		{name: "empty", raw: `{"type":"commandExecution","aggregatedOutput":""}`, present: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var item Item
			if err := json.Unmarshal([]byte(tc.raw), &item); err != nil {
				t.Fatal(err)
			}
			if tc.present {
				if item.AggregatedOutput == nil || *item.AggregatedOutput != "" {
					t.Fatalf("aggregated output presence changed: %#v", item.AggregatedOutput)
				}
			} else if item.AggregatedOutput != nil {
				t.Fatalf("aggregated output presence changed: %#v", item.AggregatedOutput)
			}
			if item.PublicText() != "" {
				t.Fatal("native tool output became public text")
			}
			encoded, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			if tc.present && !strings.Contains(string(encoded), `"aggregatedOutput":""`) {
				t.Fatal("explicit empty output was omitted")
			}
			if !tc.present && strings.Contains(string(encoded), `"aggregatedOutput"`) {
				t.Fatal("missing or null output was retained")
			}
		})
	}
}

func TestNativeToolResultDecodingIgnoresImageGenerationPayloads(t *testing.T) {
	var items []Item
	err := json.Unmarshal([]byte(`[
		{"type":"imageGeneration","id":"image","result":"PRIVATE_IMAGE_BASE64"},
		{"type":"commandExecution","id":"command","result":"PRIVATE_COMMAND_RESULT"},
		{"type":"mcpToolCall","id":"mcp","result":{"content":[{"type":"text","text":"synthetic MCP result"}]}}
	]`), &items)
	if err != nil || len(items) != 3 {
		t.Fatal("mixed native tool results did not decode", err)
	}
	if items[0].Result != nil || items[1].Result != nil || items[2].Result == nil || len(items[2].Result.Content) != 1 || items[2].Result.Content[0].Text != "synthetic MCP result" {
		t.Fatalf("native result variants were decoded incorrectly: %+v", items)
	}
	for _, item := range items {
		if item.PublicText() != "" {
			t.Fatal("native tool payload became public text")
		}
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "PRIVATE_IMAGE_BASE64") || strings.Contains(string(encoded), "PRIVATE_COMMAND_RESULT") {
		t.Fatal("unmodeled native result payload was retained")
	}

	var reused Item
	if err := json.Unmarshal([]byte(`{"type":"mcpToolCall","result":{"content":[]}}`), &reused); err != nil {
		t.Fatal(err)
	}
	if reused.Result == nil {
		t.Fatal("MCP result did not decode before reuse")
	}
	if err := json.Unmarshal([]byte(`{"type":"imageGeneration","result":"PRIVATE_REUSED_IMAGE"}`), &reused); err != nil {
		t.Fatal(err)
	}
	if reused.Result != nil || reused.Type != "imageGeneration" {
		t.Fatal("stale typed result survived a non-MCP decode")
	}
}

func TestNativeToolExecutionFactsPreserveMissingNullAndZero(t *testing.T) {
	// Nullable fields follow the pinned native ThreadItem schema. Only command
	// execution defines exitCode; MCP and dynamic calls also define durationMs.
	for _, raw := range []string{
		`{"type":"commandExecution"}`,
		`{"type":"commandExecution","exitCode":null,"durationMs":null}`,
		`{"type":"commandExecution","exitCode":0,"durationMs":0}`,
		`{"type":"commandExecution","exitCode":-1,"durationMs":17}`,
		`{"type":"mcpToolCall","durationMs":0}`,
		`{"type":"dynamicToolCall","durationMs":23}`,
	} {
		var item Item
		if err := json.Unmarshal([]byte(raw), &item); err != nil {
			t.Fatal(err)
		}
		var expected struct {
			ExitCode   *int   `json:"exitCode"`
			DurationMS *int64 `json:"durationMs"`
		}
		_ = json.Unmarshal([]byte(raw), &expected)
		if !reflect.DeepEqual(item.ExitCode, expected.ExitCode) || !reflect.DeepEqual(item.DurationMS, expected.DurationMS) {
			t.Fatal("nullable execution facts were lost")
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		if item.ExitCode != nil && *item.ExitCode == 0 && !strings.Contains(string(encoded), `"exitCode":0`) ||
			item.DurationMS != nil && *item.DurationMS == 0 && !strings.Contains(string(encoded), `"durationMs":0`) {
			t.Fatal("explicit zero was omitted from the execution facts")
		}
	}
}
