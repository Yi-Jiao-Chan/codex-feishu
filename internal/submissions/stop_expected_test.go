package submissions

import (
	"context"
	"testing"

	"codex-feishu/internal/store"
)

func TestStopExpectedOfflineClaimRejectsChangedCardTurnAfterReopen(t *testing.T) {
	for _, transition := range []string{"new-turn", "idle", "missing-target"} {
		t.Run(transition, func(t *testing.T) {
			c, f, path := fixture(t, true)
			r := claim(t, c, "preserve-queue")
			tick(t, c)
			callback := &Controller{Store: c.Store, HostID: c.HostID, AppID: c.AppID, ThreadID: c.ThreadID}
			ctx := context.Background()
			expected := "t1"
			if transition == "missing-target" {
				expected = "unavailable-turn"
			}
			op, err := callback.StopExpected(ctx, "card-stop-event", expected)
			if err != nil || op.State != "ready" || op.ExpectedTurnID != expected || f.interruptCalls != 0 {
				t.Fatal("offline card claim did not persist its original target", err)
			}
			wantResult := "changed"
			if transition != "missing-target" {
				f.end()
				wantResult = "ended"
				if transition == "new-turn" {
					f.start("")
				}
			}
			active := f.active()
			c = reopen(t, c, path)
			tick(t, c)
			got, err := c.Store.SubmissionAction(ctx, c.scope(), op.ID)
			if err != nil || got.State != "rejected" || got.Result != wantResult || got.ExpectedTurnID != expected || f.interruptCalls != 0 || f.active() != active || view(t, c, r.ID).State != "queued" {
				t.Fatal("stale stop card interrupted another turn or changed queue", err)
			}
			callback.Store = c.Store
			replay, err := callback.StopExpected(ctx, "card-stop-event", "new-card-turn")
			if err != nil || replay.ID != op.ID || replay.ExpectedTurnID != expected || replay.State != "rejected" || replay.Result != wantResult {
				t.Fatal("terminal replay replaced the captured target/result", err)
			}
		})
	}
}

// The App changes turns inside the RPC boundary, after preparation/dispatch
// have committed. The interrupt must still name the captured original turn.
type appChangesDuringInterrupt struct{ *fakeNative }

func (f appChangesDuringInterrupt) Interrupt(ctx context.Context, thread, turn string) error {
	f.mu.Lock()
	f.end()
	f.start("")
	f.mu.Unlock()
	return f.fakeNative.Interrupt(ctx, thread, turn)
}

func TestStopExpectedTurnChangeDuringInterruptNeverRetargets(t *testing.T) {
	c, f, _ := fixture(t, true)
	c.Native = appChangesDuringInterrupt{f}
	ctx := context.Background()
	op, err := c.StopExpected(ctx, "card-stop-race", "t1")
	if err != nil {
		t.Fatal(err)
	}
	tick(t, c)
	got, err := c.Store.SubmissionAction(ctx, c.scope(), op.ID)
	if err != nil || got.State != "rejected" || got.ExpectedTurnID != "t1" || f.interruptCalls != 1 || len(f.expected) != 1 || f.expected[0] != "t1" || f.active() != "t2" {
		t.Fatal("interrupt race stopped the newly active App turn", err)
	}
	attempts, err := c.Store.SubmissionAttempts(ctx, c.scope())
	if err != nil || len(attempts) != 1 || attempts[0].ExpectedTurnID != "t1" || attempts[0].State != "rejected" {
		t.Fatal("interrupt attempt lost the original target", err)
	}
}

func TestSlashStopStillCapturesActualTurnWhenActorProcesses(t *testing.T) {
	c, f, _ := fixture(t, true)
	ctx := context.Background()
	op, err := c.Stop(ctx, "slash-stop-event")
	if err != nil || op.ExpectedTurnID != "" {
		t.Fatal("slash command prematurely captured a turn", err)
	}
	f.end()
	f.start("")
	tick(t, c)
	got, err := c.Store.SubmissionAction(ctx, c.scope(), op.ID)
	if err != nil || got.Result != "stopped" || got.ExpectedTurnID != "t2" || f.interruptCalls != 1 || f.expected[0] != "t2" || f.active() != "" {
		t.Fatal("slash stop did not target the actual current turn", err)
	}
}

func TestStopExpectedSuccessfulReplayKeepsOriginalTurn(t *testing.T) {
	c, f, _ := fixture(t, true)
	ctx := context.Background()
	op, err := c.StopExpected(ctx, "successful-card-stop", "t1")
	if err != nil {
		t.Fatal(err)
	}
	tick(t, c)
	f.start("")
	replay, err := c.StopExpected(ctx, "successful-card-stop", "t2")
	if err != nil || replay.ID != op.ID || replay.ExpectedTurnID != "t1" || replay.State != "confirmed" || replay.Result != "stopped" {
		t.Fatal("successful stop replay targeted a new turn", err)
	}
	tick(t, c)
	if f.interruptCalls != 1 || f.active() != "t2" {
		t.Fatal("completed replay interrupted another turn")
	}
}

func TestStopExpectedRequiresCardTarget(t *testing.T) {
	c, _, _ := fixture(t, true)
	if _, err := c.StopExpected(context.Background(), "invalid-card", ""); err != store.ErrSubmissionStale {
		t.Fatal("stop card without a target was accepted", err)
	}
}
