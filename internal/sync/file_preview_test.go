package sync

import (
	"context"
	"errors"
	"strings"
	"testing"

	"codex-feishu/internal/codexclient"
)

func TestPublicPreviewChangesDisplayBeforeSplittingAndKeepsNativeHistory(t *testing.T) {
	e, _ := engineFixture(t)
	bindEngine(t, e)
	e.MaxCardBytes = 1200
	local := "/project/report.md:12"
	address := "http://127.0.0.1:8787/f/" + strings.Repeat("a", 64) + "#L12"
	body := strings.Repeat("[报告]("+local+")\n\n", 80)
	turn := codexclient.Turn{ID: "turn", Status: "completed", ItemsView: "full", Items: []codexclient.Item{
		input("user", "original user input"),
		agent("progress", "[报告]("+local+")", "commentary"),
		{ID: "tool", Type: "commandExecution", Text: "private-tool-body"},
		agent("final", body, "final_answer"),
	}}
	calls := 0
	e.PreparePublic = func(ctx context.Context, thread, text string) (string, error) {
		if thread != "thread" || strings.Contains(text, "private-tool-body") || strings.Contains(text, "original user input") {
			t.Fatal("preview received non-public text or incorrect scope")
		}
		calls++
		return strings.ReplaceAll(text, local, address), nil
	}
	apply(t, e, 200, false, turn)
	if turn.Items[1].Text != "[报告]("+local+")" || turn.Items[3].Text != body || calls != 2 {
		t.Fatal("preview changed native/caller history or did not prepare both public phases")
	}
	mirrors, err := e.Store.Mirrors(context.Background(), "host", "thread")
	if err != nil {
		t.Fatal(err)
	}
	finals := 0
	for _, m := range mirrors {
		if len(m.Card) > e.MaxCardBytes {
			t.Fatal("replacement links exceeded serialized card budget")
		}
		if m.Kind == "final" {
			finals++
			if strings.Contains(m.Card, local) {
				t.Fatal("final retained an unconverted local destination")
			}
		}
	}
	if finals < 2 {
		t.Fatal("fixture did not exercise replacement URL splitting")
	}
	g := &cardGateway{}
	drain(t, e, g, 1000)
	count := len(g.writes)
	apply(t, e, 201, true, turn)
	drain(t, e, g, 5000)
	if len(g.writes) != count {
		t.Fatal("stable preview links changed delivered cards on replay")
	}
}

func TestPublicPreviewFailureDoesNotStageMessagesOrAdvanceWatermark(t *testing.T) {
	e, _ := engineFixture(t)
	e.PreparePublic = func(context.Context, string, string) (string, error) {
		return "", errors.New("secret-access-code-and-path-must-not-leak")
	}
	turn := codexclient.Turn{ID: "turn", Status: "completed", Items: []codexclient.Item{input("u", "request"), agent("a", "reply", "final_answer")}}
	err := e.Apply(context.Background(), "thread", []codexclient.Turn{turn}, false, 200)
	if err == nil || strings.Contains(err.Error(), "secret-access") {
		t.Fatal("preview preparation failure was swallowed or leaked private details")
	}
	mirrors, err := e.Store.Mirrors(context.Background(), "host", "thread")
	if err != nil || len(mirrors) != 0 {
		t.Fatal("failed registration staged a partial snapshot", err)
	}
}

func TestPublicPreviewValidatesSnapshotBeforeRegisteringFiles(t *testing.T) {
	e, _ := engineFixture(t)
	called := false
	e.PreparePublic = func(context.Context, string, string) (string, error) { called = true; return "unused", nil }
	turn := codexclient.Turn{ID: "turn", Status: "completed", Items: []codexclient.Item{agent("a", "[file](/project/a.go)", "final_answer")}}
	if err := e.Apply(context.Background(), "thread", []codexclient.Turn{turn}, false, 200); err == nil || called {
		t.Fatal("invalid public history registered files before validation")
	}
}
