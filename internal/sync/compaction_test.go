package sync

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"codex-feishu/internal/codexclient"
)

func compaction(id string) codexclient.Item {
	return codexclient.Item{ID: id, Type: "contextCompaction"}
}

func completedTurn(id, userID, question, finalID, answer string) codexclient.Turn {
	return codexclient.Turn{
		ID: id, Status: "completed", ItemsView: "full",
		Items: []codexclient.Item{
			input(userID, question),
			agent(finalID, answer, "final_answer"),
		},
	}
}

func assertCompactionOnlyState(t *testing.T, e Engine, turnID, itemID string, observedAt int64) {
	t.Helper()
	ctx := context.Background()
	mirrors, err := e.Store.Mirrors(ctx, "host", "thread")
	if err != nil {
		t.Fatal(err)
	}
	if len(mirrors) != 0 {
		t.Fatalf("compaction turn created public mirrors: %+v", mirrors)
	}
	var outbound int
	if err := e.Store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound WHERE host_id=? AND thread_id=?`, "host", "thread").Scan(&outbound); err != nil {
		t.Fatal(err)
	}
	if outbound != 0 {
		t.Fatalf("compaction turn created outbound work: %d", outbound)
	}
	mark, err := e.Store.Watermark(ctx, "host", "thread")
	if err != nil {
		t.Fatal(err)
	}
	if mark.TurnID != turnID || mark.ItemID != itemID || mark.ObservedAtMS != observedAt {
		t.Fatalf("wrong compaction watermark: %+v", mark)
	}
}

func TestCompactionOnlyTurnsAdvanceWatermarkWithoutMirrors(t *testing.T) {
	for _, status := range []string{"inProgress", "completed", "failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			e, _ := engineFixture(t)
			turnID, itemID, observedAt := "compaction-"+status, "compact-item-"+status, int64(2000)
			turn := codexclient.Turn{
				ID: turnID, Status: status, ItemsView: "full",
				Items: []codexclient.Item{compaction(itemID)},
			}
			apply(t, e, observedAt, false, turn)
			assertCompactionOnlyState(t, e, turnID, itemID, observedAt)
		})
	}
}

func TestCompactionInHistoryKeepsAnchorsAndReplayIdempotent(t *testing.T) {
	e, _ := engineFixture(t)
	first := completedTurn("turn-first", "human-first", "first request", "final-first", "first answer")
	compact := codexclient.Turn{
		ID: "turn-compact", Status: "completed", ItemsView: "full",
		Items: []codexclient.Item{compaction("compact-item")},
	}
	second := completedTurn("turn-second", "human-second", "second request", "final-second", "second answer")
	tailCompact := codexclient.Turn{
		ID: "turn-tail-compact", Status: "completed", ItemsView: "full",
		Items: []codexclient.Item{compaction("tail-compact-item")},
	}
	third := completedTurn("turn-third", "human-third", "third request", "final-third", "third answer")

	apply(t, e, 1000, false, first)
	bindEngine(t, e)
	g := &cardGateway{}
	drain(t, e, g, 2000)
	if len(g.writes) != 2 || g.writes[0].Operation != "send" || g.writes[1].Operation != "reply" || g.writes[1].Parent != g.writes[0].ID {
		t.Fatalf("initial normal turn was not delivered from its own human anchor: %+v", g.writes)
	}

	apply(t, e, 3000, false, first, compact)
	drain(t, e, g, 3001)
	if len(g.writes) != 2 {
		t.Fatalf("compaction turn generated outbound writes: %+v", g.writes)
	}
	mark, err := e.Store.Watermark(context.Background(), "host", "thread")
	if err != nil || mark.TurnID != compact.ID || mark.ItemID != "compact-item" || mark.ObservedAtMS != 3000 {
		t.Fatalf("compaction did not become the scan watermark: %+v %v", mark, err)
	}

	apply(t, e, 4000, false, first, compact, second)
	drain(t, e, g, 4001)
	if len(g.writes) != 4 || g.writes[2].Operation != "send" || g.writes[3].Operation != "reply" || g.writes[3].Parent != g.writes[2].ID || !strings.Contains(g.writes[3].Card, "second answer") {
		t.Fatalf("follow-up final lost its own human anchor after compaction: %+v", g.writes)
	}

	apply(t, e, 5000, true, first, compact, second)
	drain(t, e, g, 5001)
	if len(g.writes) != 4 {
		t.Fatalf("full-history replay duplicated cards around compaction: %+v", g.writes)
	}

	apply(t, e, 6000, false, first, compact, second, tailCompact)
	drain(t, e, g, 6001)
	if len(g.writes) != 4 {
		t.Fatalf("trailing compaction generated outbound writes: %+v", g.writes)
	}
	mark, err = e.Store.Watermark(context.Background(), "host", "thread")
	if err != nil || mark.TurnID != tailCompact.ID || mark.ItemID != "tail-compact-item" || mark.ObservedAtMS != 6000 {
		t.Fatalf("trailing compaction did not advance watermark: %+v %v", mark, err)
	}

	apply(t, e, 7000, false, first, compact, second, tailCompact, third)
	drain(t, e, g, 7001)
	if len(g.writes) != 6 || g.writes[4].Operation != "send" || g.writes[5].Operation != "reply" || g.writes[5].Parent != g.writes[4].ID || !strings.Contains(g.writes[5].Card, "third answer") {
		t.Fatalf("normal turn after trailing compaction was not anchored independently: %+v", g.writes)
	}
	mark, err = e.Store.Watermark(context.Background(), "host", "thread")
	if err != nil || mark.TurnID != third.ID || mark.ItemID != "final-third" || mark.ObservedAtMS != 7000 {
		t.Fatalf("follow-up turn did not advance watermark: %+v %v", mark, err)
	}

	apply(t, e, 8000, true, first, compact, second, tailCompact, third)
	drain(t, e, g, 8001)
	if len(g.writes) != 6 {
		t.Fatalf("replay after a trailing compaction duplicated cards: %+v", g.writes)
	}
}

func TestInvalidCompactionSnapshotsDoNotAdvanceExistingState(t *testing.T) {
	cases := []struct {
		name string
		turn codexclient.Turn
	}{
		{
			name: "compaction plus agent message",
			turn: codexclient.Turn{
				ID: "bad-agent", Status: "completed", ItemsView: "full",
				Items: []codexclient.Item{compaction("compact"), agent("orphan-agent", "orphan public text", "commentary")},
			},
		},
		{
			name: "compaction plus unknown item",
			turn: codexclient.Turn{
				ID: "bad-unknown", Status: "completed", ItemsView: "full",
				Items: []codexclient.Item{compaction("compact"), {ID: "unknown", Type: "unknownNativeItem"}},
			},
		},
		{
			name: "missing item id",
			turn: codexclient.Turn{
				ID: "bad-missing-id", Status: "completed", ItemsView: "full",
				Items: []codexclient.Item{{Type: "contextCompaction"}},
			},
		},
		{
			name: "conflicting duplicate item id",
			turn: codexclient.Turn{
				ID: "bad-duplicate", Status: "completed", ItemsView: "full",
				Items: []codexclient.Item{
					{ID: "duplicate", Type: "contextCompaction", Text: "summary one"},
					{ID: "duplicate", Type: "contextCompaction", Text: "summary two"},
				},
			},
		},
		{
			name: "summary view",
			turn: codexclient.Turn{
				ID: "bad-summary", Status: "completed", ItemsView: "summary",
				Items: []codexclient.Item{compaction("summary-item")},
			},
		},
		{
			name: "unknown status",
			turn: codexclient.Turn{
				ID: "bad-status", Status: "queued", ItemsView: "full",
				Items: []codexclient.Item{compaction("status-item")},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := engineFixture(t)
			base := completedTurn("turn-base", "human-base", "baseline request", "final-base", "baseline answer")
			apply(t, e, 1000, false, base)
			beforeMirrors, err := e.Store.Mirrors(context.Background(), "host", "thread")
			if err != nil {
				t.Fatal(err)
			}
			beforeMark, err := e.Store.Watermark(context.Background(), "host", "thread")
			if err != nil {
				t.Fatal(err)
			}
			var beforeOutbound int
			if err := e.Store.DB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM outbound WHERE host_id=? AND thread_id=?`, "host", "thread").Scan(&beforeOutbound); err != nil {
				t.Fatal(err)
			}

			if err := e.Apply(context.Background(), "thread", []codexclient.Turn{base, tc.turn}, false, 2000); err == nil {
				t.Fatal("invalid compaction snapshot was accepted")
			}
			afterMirrors, err := e.Store.Mirrors(context.Background(), "host", "thread")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(afterMirrors, beforeMirrors) {
				t.Fatalf("invalid snapshot partially changed mirrors: before=%+v after=%+v", beforeMirrors, afterMirrors)
			}
			afterMark, err := e.Store.Watermark(context.Background(), "host", "thread")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(afterMark, beforeMark) {
				t.Fatalf("invalid snapshot advanced watermark: before=%+v after=%+v", beforeMark, afterMark)
			}
			var afterOutbound int
			if err := e.Store.DB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM outbound WHERE host_id=? AND thread_id=?`, "host", "thread").Scan(&afterOutbound); err != nil {
				t.Fatal(err)
			}
			if afterOutbound != beforeOutbound {
				t.Fatalf("invalid snapshot partially changed outbound: before=%d after=%d", beforeOutbound, afterOutbound)
			}
		})
	}
}
