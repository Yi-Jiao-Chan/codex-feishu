package sync

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-feishu/internal/codexclient"
)

// Use the real native-origin reader: accepting any turn without input would
// hide the regression while weakening the complete-history validation.
func capacityRetryReader(t *testing.T) func(context.Context, string, []string) (map[string]bool, error) {
	t.Helper()
	root := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	if err := os.Mkdir(sessions, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sessions, "rollout.jsonl")
	records := []any{
		map[string]any{"type": "session_meta", "payload": map[string]any{"id": "thread"}},
		map[string]any{"type": "event_msg", "payload": map[string]any{
			"type": "task_started", "turn_id": "retry",
			"turn_attribution": map[string]any{"turn_id": "retry", "turn_trigger": "capacity_retry_manual", "root_turn_id": "retry", "parent_turn_id": nil},
		}},
	}
	var contents []byte
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		contents = append(append(contents, line...), '\n')
	}
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
	metadata := func(context.Context, string) (codexclient.Thread, error) {
		return codexclient.Thread{ID: "thread", Path: &path}, nil
	}
	return codexclient.NewUnanchoredTurnReader(metadata, root).Read
}

func TestCapacityRetryWithoutInputDoesNotBlockLaterTurns(t *testing.T) {
	for _, backfill := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "backfill"}[backfill], func(t *testing.T) {
			ctx := context.Background()
			e, _ := engineFixture(t)
			e.ReadUnanchoredTurns = capacityRetryReader(t)
			failed := codexclient.Turn{ID: "failed", Status: "failed", ItemsView: "full", Items: []codexclient.Item{input("request", "synthetic request")}}
			apply(t, e, 1000, false, failed)
			bindEngine(t, e)
			g := &cardGateway{}
			drain(t, e, g, 2000)
			if len(g.writes) != 2 || !strings.Contains(g.writes[1].Card, "本轮执行失败") {
				t.Fatal("fixture did not deliver the failed outcome")
			}
			retry := goalTurn("retry", "inProgress", agent("retry-progress", "synthetic retry progress", "commentary"))
			if !backfill {
				apply(t, e, 3000, false, failed, retry)
				drain(t, e, g, 3001)
				if len(g.writes) != 3 || g.writes[2].Operation != "send" || g.writes[2].Parent != "" {
					t.Fatal("manual retry progress did not use the original binding directly")
				}
			}
			retry.Status = "completed"
			retry.Items = append(retry.Items, agent("retry-final", "synthetic recovered answer", "final_answer"))
			followup := completedTurn("followup", "followup-input", "synthetic follow-up", "followup-final", "synthetic follow-up answer")
			apply(t, e, 4000, backfill, failed, retry, followup)
			drain(t, e, g, 4001)
			assertGoalMirrorsHaveNoHumanOrParent(t, e, retry.ID)
			followupHuman, recovered := "", false
			for _, write := range g.writes {
				if strings.Contains(write.Card, "synthetic recovered answer") {
					recovered = true
					if write.Operation != "send" || write.Parent != "" {
						t.Fatal("manual retry borrowed an earlier turn's reply anchor")
					}
				}
				if strings.Contains(write.Card, "synthetic follow-up") && write.Operation == "send" {
					followupHuman = write.ID
				}
			}
			last := g.writes[len(g.writes)-1]
			if !recovered || followupHuman == "" || !strings.Contains(last.Card, "synthetic follow-up answer") || last.Operation != "reply" || last.Parent != followupHuman {
				t.Fatal("the retry or later normal reply failed to deliver with its own anchor")
			}
			failedMirror, err := e.Store.Mirror(ctx, "host", "thread", mirrorKey("final", failed.ID, "0"))
			if err != nil || failedMirror.Revision != 1 || failedMirror.RemoteID != g.writes[1].ID {
				t.Fatal("retry rewrote the earlier failed result", err)
			}
			mark, err := e.Store.Watermark(ctx, "host", "thread")
			if err != nil || mark.TurnID != followup.ID || mark.ItemID != "followup-final" {
				t.Fatal("retry stopped the full-history watermark", err)
			}
			// Recreate the reader and engine as after a restart. Settled sends
			// remain deduplicated while the native source is proved again.
			before := len(g.writes)
			restarted := Engine{Store: e.Store, HostID: e.HostID, ReadUnanchoredTurns: capacityRetryReader(t)}
			apply(t, restarted, 5000, true, failed, retry, followup)
			drain(t, restarted, g, 5001)
			if len(g.writes) != before {
				t.Fatal("restart replay duplicated the recovered result")
			}
		})
	}
}

func TestCapacityRetryNativeCOTUsesTheExistingBinding(t *testing.T) {
	e, _ := engineFixture(t)
	e.NativeCOT, e.ReadUnanchoredTurns = true, capacityRetryReader(t)
	failed := codexclient.Turn{ID: "failed", Status: "failed", ItemsView: "full", Items: []codexclient.Item{input("request", "synthetic request")}}
	apply(t, e, 1000, false, failed)
	bindEngine(t, e)
	g := &cotOutboundGateway{s: e.Store, t: t}
	o := Outbound{Store: e.Store, Gateway: g}
	now := int64(2000)
	deliver := func() {
		t.Helper()
		for n := 0; n < 32; n++ {
			result, err := o.DispatchOne(context.Background(), "host", "thread", now)
			now += 3000
			if err != nil || result.AttemptID != "" && result.State != "confirmed" {
				t.Fatal("retry delivery did not settle", err)
			}
			if result.AttemptID == "" {
				return
			}
		}
		t.Fatal("retry delivery exceeded the fixture batch")
	}
	deliver()
	retry := goalTurn("retry", "inProgress", agent("progress", "synthetic retry progress", "commentary"))
	apply(t, e, 3000, false, failed, retry)
	deliver()
	if len(g.cotCalls) != 2 || g.cotCalls[0].step != "create" || g.cotCalls[0].chat != "chat" || g.cotCalls[0].origin != "" || g.cotCalls[0].inThread {
		t.Fatal("retry COT did not create directly in its existing group")
	}
	retry.Status = "completed"
	retry.Items = append(retry.Items, agent("answer", "synthetic recovered answer", "final_answer"))
	apply(t, e, 4000, false, failed, retry)
	deliver()
	if len(g.cotCalls) != 4 || g.cotCalls[3].step != "complete" {
		t.Fatal("retry COT did not finish normally")
	}
	last := g.writes[len(g.writes)-1]
	if last.Operation != "send" || last.Parent != "" || !strings.Contains(last.Card, "synthetic recovered answer") {
		t.Fatal("retry final did not use its existing binding directly")
	}
	before := g.writesCount()
	apply(t, e, 5000, true, failed, retry)
	deliver()
	if g.writesCount() != before {
		t.Fatal("retry COT or final duplicated on replay")
	}
}
