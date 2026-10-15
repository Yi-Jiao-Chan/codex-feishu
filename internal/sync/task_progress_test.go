package sync

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
)

func TestTaskProgressRenderUpdatesSameRealProgressMessageAndParent(t *testing.T) {
	e, _ := engineFixture(t)
	bindEngine(t, e)
	e.ProgressRender = func(thread, turn, status string, progress cards.ProgressView) string {
		if thread != "thread" || turn != "turn" {
			t.Fatal("progress renderer received foreign scope")
		}
		kind, count := "stop", -1
		if status == "interrupted" {
			kind, count = "continue", 2
		}
		return cards.Task(cards.TaskView{Status: status, Progress: progress, Kind: kind, State: "available", Token: "opaque", Revision: 3, QueueCount: count})
	}
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", ItemsView: "full", Items: []codexclient.Item{input("actual-user", "original request"), agent("public", "working", "commentary")}}
	apply(t, e, 200, false, turn)
	g := &cardGateway{}
	drain(t, e, g, 1000)
	if len(g.writes) != 2 || !strings.Contains(g.writes[1].Card, `"action":"stop"`) {
		t.Fatal("initial process card omitted task control")
	}
	progressID, parentID := g.writes[1].ID, g.writes[0].ID
	turn.Status = "interrupted"
	apply(t, e, 201, false, turn)
	drain(t, e, g, 1001)
	if len(g.writes) != 4 || g.writes[2].Operation != "update" || g.writes[2].ID != progressID || !strings.Contains(g.writes[2].Card, `"action":"continue"`) || g.writes[3].Parent != parentID {
		t.Fatal("stop created a second process card or moved its reply anchor")
	}
	apply(t, e, 202, true, turn)
	drain(t, e, g, 9000)
	if len(g.writes) != 4 {
		t.Fatal("full-history replay overwrote or duplicated the same task card")
	}
	list, err := e.Store.Mirrors(context.Background(), "host", "thread")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, m := range list {
		if m.Kind == "progress" {
			count++
			if m.Key != mirrorKey("progress", "turn") || m.RemoteID != progressID || m.ParentKey != mirrorKey("human", "turn", "actual-user", "0") || !m.Terminal {
				t.Fatal("task controls changed the established progress identity")
			}
		}
	}
	if count != 1 {
		t.Fatal("more than one progress card exists for the turn")
	}
}

func TestInterruptedTaskWithoutExistingProgressCreatesOnlySelectedControl(t *testing.T) {
	e, _ := engineFixture(t)
	bindEngine(t, e)
	e.ProgressRender = func(_, turn, status string, progress cards.ProgressView) string {
		if turn != "latest" {
			return ""
		}
		return cards.Task(cards.TaskView{Status: status, Progress: progress, Kind: "continue", State: "available", Token: "opaque", Revision: 1, QueueCount: 1})
	}
	turns := []codexclient.Turn{
		{ID: "older", Status: "interrupted", ItemsView: "full", Items: []codexclient.Item{input("old-user", "old request")}},
		{ID: "latest", Status: "interrupted", ItemsView: "full", Items: []codexclient.Item{input("latest-user", "latest request")}},
	}
	apply(t, e, 200, true, turns...)
	g := &cardGateway{}
	drain(t, e, g, 1000)
	progress := 0
	list, err := e.Store.Mirrors(context.Background(), "host", "thread")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range list {
		if m.Kind == "progress" {
			progress++
			if m.TurnID != "latest" || m.ParentKey != mirrorKey("human", "latest", "latest-user", "0") || m.RemoteID == "" || !strings.Contains(m.Card, `"action":"continue"`) {
				t.Fatal("minimal control did not reference the actual initiating input")
			}
		}
	}
	if progress != 1 {
		t.Fatal("historical stopped turns acquired unrelated controls")
	}
	writes := len(g.writes)
	apply(t, e, 201, true, turns...)
	drain(t, e, g, 9000)
	if len(g.writes) != writes {
		t.Fatal("minimal control was recreated on every complete scan")
	}
}

func TestProgressRenderBoundsActualControlCardAndRejectsInvalidJSONAtomically(t *testing.T) {
	e, _ := engineFixture(t)
	e.MaxCardBytes = len(cards.Task(cards.TaskView{Status: "inProgress", Progress: cards.ProgressView{Body: "…（内容已截短）", OmittedRecords: 1, ElapsedSeconds: -1}, Kind: "stop", State: "available", Token: "opaque", Revision: 1, QueueCount: -1})) + cards.MaxRequestMarkerBytes + 30
	e.ProgressRender = func(_, _, status string, progress cards.ProgressView) string {
		return cards.Task(cards.TaskView{Status: status, Progress: progress, Kind: "stop", State: "available", Token: "opaque", Revision: 1, QueueCount: -1})
	}
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", Items: []codexclient.Item{input("user", "request"), agent("public", strings.Repeat("中文😀<>", 500), "commentary")}}
	apply(t, e, 200, false, turn)
	m, err := e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("progress", "turn"))
	if err != nil || len(m.Card) > e.MaxCardBytes-cards.MaxRequestMarkerBytes || !strings.Contains(m.Card, `"token":"opaque"`) || !strings.Contains(m.Card, "…") {
		t.Fatal("progress size bound removed controls or exceeded the actual JSON limit", err)
	}
	bad, _ := engineFixture(t)
	bad.ProgressRender = func(_, _, _ string, _ cards.ProgressView) string { return "invalid" }
	if err := bad.Apply(context.Background(), "thread", []codexclient.Turn{turn}, false, 200); err == nil {
		t.Fatal("invalid custom card advanced history")
	}
	if _, err := bad.Store.Watermark(context.Background(), "host", "thread"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("invalid rendering partially committed checkpoint", err)
	}
	list, err := bad.Store.Mirrors(context.Background(), "host", "thread")
	if err != nil || len(list) != 0 {
		t.Fatal("invalid rendering partially committed public cards", err)
	}
}
