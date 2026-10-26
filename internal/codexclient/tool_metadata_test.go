package codexclient

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNativeToolMetadataDecodesWithoutRetainingUnusedPayloads(t *testing.T) {
	// Synthetic 0.162.1 ThreadItem shapes: classifications are best-effort;
	// descriptors are metadata, while arguments and results stay unmodeled.
	var items []Item
	err := json.Unmarshal([]byte(`[
	 {"type":"commandExecution","id":"command","command":"synthetic command","commandActions":[{"type":"read","command":"PRIVATE_ACTION_COMMAND","name":"fixture.go","path":"/project/fixture.go"},{"type":"search","path":"/project","query":"PRIVATE_QUERY"},{"type":"listFiles","path":null},{"type":"unknown"}],"status":"completed","aggregatedOutput":"PRIVATE_OUTPUT"},
	 {"type":"mcpToolCall","id":"mcp","server":"fixture_server","tool":"lookup","status":"failed","arguments":{"secret":"PRIVATE_ARGUMENT"},"result":{"content":"PRIVATE_RESULT"},"durationMs":30},
	 {"type":"dynamicToolCall","id":"dynamic","namespace":"fixture_namespace","tool":"inspect","status":"inProgress","contentItems":[{"text":"PRIVATE_CONTENT"}],"success":null}
	]`), &items)
	if err != nil || len(items) != 3 {
		t.Fatal("native metadata did not decode", err)
	}
	if !reflect.DeepEqual(items[0].CommandActions, []CommandAction{{Type: "read", Name: "fixture.go", Path: "/project/fixture.go"}, {Type: "search", Path: "/project"}, {Type: "listFiles"}, {Type: "unknown"}}) || items[1].Server != "fixture_server" || items[1].Tool != "lookup" || items[1].Status != "failed" || items[2].Namespace != "fixture_namespace" || items[2].Tool != "inspect" {
		t.Fatalf("native metadata was lost: %+v", items)
	}
	encoded, err := json.Marshal(items)
	if err != nil || strings.Contains(string(encoded), "PRIVATE_") {
		t.Fatal("unused action commands, search queries, arguments or tool output were retained", err)
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
