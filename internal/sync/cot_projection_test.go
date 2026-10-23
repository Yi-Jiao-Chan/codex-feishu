package sync

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/cot"
)

func parseProjection(t *testing.T, raw string, err error) cot.Document {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	doc, native, err := cot.Parse(raw)
	if !native || err != nil {
		t.Fatal("safe projection was not a valid native document", err)
	}
	return doc
}

func TestCOTProjectionContainsOnlyPublicMessagesAndSafeToolMetadata(t *testing.T) {
	turn := codexclient.Turn{ID: "turn", Status: "completed", ItemsView: "full", Items: []codexclient.Item{
		input("user", "PRIVATE_INPUT"),
		agent("comment", "公开进度", "commentary"),
		{ID: "reasoning", Type: "reasoning", Text: "PRIVATE_REASONING"},
		{ID: "command", Type: "commandExecution", Status: "completed", Command: "go test ./... -secret PRIVATE_ARGUMENT", Text: "PRIVATE_OUTPUT", CWD: "/PRIVATE_CWD"},
		{ID: "file", Type: "fileChange", Status: "failed", Changes: []codexclient.FileChange{{Path: "/PRIVATE_DIRECTORY/fixture.go", Kind: json.RawMessage(`{"type":"update"}`), Diff: "PRIVATE_DIFF"}}},
		agent("partial", "公开部分回复", "partial_answer"),
		agent("final", "PRIVATE_FINAL_IN_SEPARATE_CHANNEL", "final_answer"),
		{ID: "image", Type: "imageGeneration", Status: "completed", Text: "PRIVATE_IMAGE_PAYLOAD"},
		{ID: "unknown", Type: "webSearch", Status: "completed", Text: "PRIVATE_SEARCH_RESULTS"},
	}}
	clocks := map[string]int64{}
	raw, err := cotDocument("thread", turn, clocks, 200000)
	doc := parseProjection(t, raw, err)
	for _, private := range []string{"PRIVATE_INPUT", "PRIVATE_REASONING", "PRIVATE_ARGUMENT", "PRIVATE_OUTPUT", "PRIVATE_CWD", "PRIVATE_DIRECTORY", "PRIVATE_DIFF", "PRIVATE_FINAL", "PRIVATE_IMAGE", "PRIVATE_SEARCH"} {
		if strings.Contains(raw, private) {
			t.Fatal("native COT projection exposed an unapproved source field")
		}
	}
	if len(doc.Items) != 6 || doc.Items[1].Title != "运行 Go 测试" || doc.Items[2].Title != "编辑 fixture.go" || doc.Items[5].Status != "unknown" {
		t.Fatal("safe projection changed native order or invented a status")
	}
	for _, item := range doc.Items {
		if clocks[item.ID] != 200000 || item.TimestampMS != 200000 {
			t.Fatal("first-observation metadata was not retained")
		}
	}
	again, err := cotDocument("thread", turn, clocks, 300000)
	if err != nil || again != raw {
		t.Fatal("unchanged projection was retimed on replay")
	}
}

func TestCOTProjectionKeepsAllSafeToolsBeyondLegacyCardWindow(t *testing.T) {
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", ItemsView: "full"}
	for i := range 80 {
		turn.Items = append(turn.Items, codexclient.Item{ID: fmt.Sprintf("tool-%d", i), Type: "commandExecution", Command: "git status --short", Status: "completed"})
	}
	raw, err := cotDocument("thread", turn, nil, 1000)
	doc := parseProjection(t, raw, err)
	if len(doc.Items) != 80 || doc.Items[0].ID != "tool-0" || doc.Items[79].ID != "tool-79" {
		t.Fatal("COT reused the legacy 32-tool truncation")
	}
}

func TestCOTProjectionLegacyFinalAsyncQuestionsAndPrivateBoundaries(t *testing.T) {
	async := "async"
	turn := codexclient.Turn{ID: "turn", Status: "completed", Items: []codexclient.Item{
		agent("legacy-first", "前序公开说明", ""),
		input("steer", "PRIVATE_STEER"),
		{ID: "question", Type: "agentMessage", Text: "PRIVATE_ASYNC_QUESTION", Delivery: &async, Questions: []codexclient.AsyncQuestion{{Title: "synthetic"}}},
		agent("legacy-final", "独立的最终正文", ""),
	}}
	raw, err := cotDocument("thread", turn, nil, 1000)
	doc := parseProjection(t, raw, err)
	if len(doc.Items) != 1 || doc.Items[0].ID != "legacy-first" || strings.Contains(raw, "PRIVATE") {
		t.Fatal("legacy final or private input/question was duplicated in COT")
	}
	turn.Status = "inProgress"
	raw, err = cotDocument("thread", turn, nil, 1000)
	doc = parseProjection(t, raw, err)
	if len(doc.Items) != 2 {
		t.Fatal("active legacy public message was prematurely selected as final")
	}
}

func TestCOTProjectionNormalizesVisualizationsAndEscapesNativeTags(t *testing.T) {
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", Items: []codexclient.Item{
		agent("public", visualized("/PRIVATE_DIRECTORY/chart.html"), "commentary"),
		agent("markup", `<at id=all>公开说明</at>`, "partial_answer"),
	}}
	raw, err := cotDocument("thread", turn, nil, 1000)
	doc := parseProjection(t, raw, err)
	if !strings.Contains(doc.Items[0].Text, codexclient.VisualizationFallback) || strings.Contains(raw, "PRIVATE_DIRECTORY") || strings.Contains(doc.Items[1].Text, "<at") {
		t.Fatal("App-only embeds or executable native markup escaped public normalization")
	}
}

func TestCOTProjectionRejectsPartialAndConflictingSnapshotsBeforeClockMutation(t *testing.T) {
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", ItemsView: "partial", Items: []codexclient.Item{agent("public", "公开说明", "commentary")}}
	clocks := map[string]int64{"old": 500}
	before := map[string]int64{"old": 500}
	if _, err := cotDocument("thread", turn, clocks, 1000); err == nil {
		t.Fatal("partial native history was accepted")
	}
	turn.ItemsView = "full"
	turn.Items = append(turn.Items, agent("public", "不同的公开说明", "commentary"))
	if _, err := cotDocument("thread", turn, clocks, 1000); err == nil || !reflect.DeepEqual(clocks, before) {
		t.Fatal("conflicting native identity was normalized away or partially changed clocks")
	}
	turn.Items[1] = turn.Items[0]
	raw, err := cotDocument("thread", turn, clocks, 1000)
	if doc := parseProjection(t, raw, err); len(doc.Items) != 1 {
		t.Fatal("exact native pagination duplicate was projected twice")
	}
}
