package sync

import (
	"context"
	"strings"
	"testing"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/cot"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

func TestFrozenCOTSkipsOnlyAcknowledgedTerminalProjectionAfterRestart(t *testing.T) {
	for _, state := range []string{"terminal-pending", "terminal-unknown", "terminal-ack", "complete-unknown", "completed"} {
		t.Run(state, func(t *testing.T) {
			f := newCOTOutboundFixture(t, false, "synthetic progress")
			f.dispatch(t, "create")
			f.dispatch(t, "append")
			f.terminal(t)
			fail := func(step string) {
				f.g.failStep, f.g.failure = step, &feishu.Unknown{Operation: "fixture " + step}
				r, err := f.o.DispatchOne(context.Background(), "host", "thread", f.now)
				f.now += 3000
				if err != nil || r.State != "unknown" {
					t.Fatal("fixture did not preserve unknown", r, err)
				}
			}
			switch state {
			case "terminal-unknown":
				fail("append")
			case "terminal-ack", "complete-unknown", "completed":
				f.dispatch(t, "append")
				if state == "complete-unknown" {
					fail("complete")
				} else if state == "completed" {
					f.dispatch(t, "complete")
				}
			}
			f.recover(t)
			ctx := context.Background()
			key := mirrorKey("progress", "turn")
			before, err := f.e.Store.Mirror(ctx, "host", "thread", key)
			if err != nil {
				t.Fatal(err)
			}
			frozen := state == "terminal-ack" || state == "complete-unknown" || state == "completed"
			keys, err := f.e.Store.FrozenCOTMirrors(ctx, "host", "thread")
			if err != nil || keys[key] != frozen {
				t.Fatal("missing or premature terminal-ACK proof", keys, err)
			}
			for _, scope := range [][2]string{{"other-host", "thread"}, {"host", "other-thread"}} {
				other, err := f.e.Store.FrozenCOTMirrors(ctx, scope[0], scope[1])
				if err != nil || len(other) != 0 {
					t.Fatal("terminal proof escaped its scope", err)
				}
			}
			// Start completely cold, with same-ID corrections in the closed turn.
			f.e.COTProjectionCache = NewCOTProjectionCache()
			for i := range f.turn.Items {
				switch f.turn.Items[i].ID {
				case "tool":
					f.turn.Items[i].Result = &codexclient.MCPToolResult{Content: []codexclient.ToolOutputContent{{Type: "text", Text: "corrected tool result"}}}
				case "final":
					f.turn.Items[i].Text = "corrected final answer"
				}
			}
			if err := f.e.Apply(ctx, "thread", []codexclient.Turn{f.turn}, true, 400); err != nil {
				t.Fatal(err)
			}
			stats := f.e.COTProjectionCache.stats()
			if frozen && stats.misses != 0 || !frozen && stats.misses != 1 {
				t.Fatal("cold projection skipped unsettled work or rebuilt frozen history", stats)
			}
			after, err := f.e.Store.Mirror(ctx, "host", "thread", key)
			if err != nil || frozen && (after.Revision != before.Revision || after.CardHash != before.CardHash || after.State != before.State) {
				t.Fatal("frozen progress or its unknown journal changed", err)
			}
			final, err := f.e.Store.Mirror(ctx, "host", "thread", mirrorKey("final", "turn", "0"))
			if err != nil || !strings.Contains(final.Card, "corrected final answer") {
				t.Fatal("frozen progress suppressed a final answer correction", err)
			}
			mark, _ := f.e.Store.Watermark(ctx, "host", "thread")
			// A frozen document must never bypass complete snapshot validation.
			bad := f.turn
			bad.Items = append(append([]codexclient.Item(nil), bad.Items...), codexclient.Item{ID: "tool", Type: "commandExecution"})
			if err := f.e.Apply(ctx, "thread", []codexclient.Turn{bad}, true, 500); err == nil {
				t.Fatal("frozen history accepted conflicting item identities")
			}
			if next, _ := f.e.Store.Watermark(ctx, "host", "thread"); next != mark {
				t.Fatal("invalid frozen snapshot advanced its watermark")
			}
			// A later active turn still projects even when the old turn is frozen.
			f.e.COTProjectionCache = NewCOTProjectionCache()
			live := codexclient.Turn{ID: "next", Status: "inProgress", ItemsView: "full", Items: []codexclient.Item{
				input("next-user", "new request"), {ID: "tool", Type: "commandExecution", Command: "go test ./...", Status: "inProgress"},
			}}
			if err := f.e.Apply(ctx, "thread", []codexclient.Turn{f.turn, live}, false, 600); err != nil {
				t.Fatal(err)
			}
			want := uint64(1)
			if !frozen {
				want++
			}
			if stats := f.e.COTProjectionCache.stats(); stats.misses != want {
				t.Fatal("frozen proof leaked into the next turn", stats)
			}
		})
	}
}

func BenchmarkFrozenCOTHistory(b *testing.B) {
	turn := benchmarkCOTHistory()
	turn.Items = append([]codexclient.Item{input("user", "synthetic request")}, turn.Items...)
	key := mirrorKey("progress", turn.ID)
	old := []store.Mirror{{Key: key, TurnID: turn.ID, Kind: "progress", Terminal: true, ProjectionVersion: cot.Version,
		ParentKey: mirrorKey("human", turn.ID, "user", "0")}}
	for _, frozen := range []bool{false, true} {
		name := "reproject-cold"
		if frozen {
			name = "frozen-terminal"
		}
		b.Run(name, func(b *testing.B) {
			for range b.N {
				e := Engine{NativeCOT: true, COTProjectionCache: NewCOTProjectionCache()}
				if frozen {
					e.frozenCOT = map[string]bool{key: true}
				}
				if _, err := e.renderSnapshot("thread", []codexclient.Turn{turn}, nil, old, true, 1000); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
