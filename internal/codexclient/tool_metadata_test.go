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
