package submissions

import (
	"context"
	"testing"

	"codex-feishu/internal/store"
)

func TestContinueExpectedOfflineClaimRejectsEveryNewerNativeTurn(t *testing.T) {
	for _, status := range []string{"inProgress", "completed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			c, f, path := fixture(t, true)
			r := claim(t, c, "preserve-pending")
			tick(t, c)
			f.turns[0].Status, f.paused = "interrupted", true
			callback := &Controller{Store: c.Store, HostID: c.HostID, AppID: c.AppID, ThreadID: c.ThreadID}
			ctx := context.Background()
			op, err := callback.ContinueExpected(ctx, "old-stop-card", "t1")
			if err != nil || op.ExpectedTurnID != "t1" || op.State != "ready" || f.starts != 0 {
				t.Fatal("offline callback did not capture the displayed stopped turn", err)
			}
			f.start("")
			f.turns[1].Status = status
			c = reopen(t, c, path)
			tick(t, c)
			got, err := c.Store.SubmissionAction(ctx, c.scope(), op.ID)
			if err != nil || got.State != "rejected" || got.Result != "changed" || got.ExpectedTurnID != "t1" || f.starts != 0 || view(t, c, r.ID).State != "queued" {
				t.Fatal("stale Continue card started a later queue or lost its target", err)
			}
			callback.Store = c.Store
			replay, err := callback.ContinueExpected(ctx, "old-stop-card", "t2")
			if err != nil || replay.ID != op.ID || replay.State != "rejected" || replay.ExpectedTurnID != "t1" || replay.Result != "changed" {
				t.Fatal("terminal replay retargeted Continue", err)
			}
		})
	}
}

func TestContinueExpectedRechecksSeededPreparedStageAfterReopen(t *testing.T) {
	c, f, path := fixture(t, true)
	r := claim(t, c, "pending")
	tick(t, c)
	f.turns[0].Status, f.paused = "interrupted", true
	ctx := context.Background()
	op, err := c.ContinueExpected(ctx, "prepared-card", "t1")
	if err != nil {
		t.Fatal(err)
	}
	q := view(t, c, r.ID)
	if err = c.Store.PrepareSubmissionAction(ctx, c.scope(), op.ID, "start", "t1", q.QueueID, q.ClientID); err != nil {
		t.Fatal(err)
	}
	f.start("")
	f.end()
	c = reopen(t, c, path)
	tick(t, c)
	got, err := c.Store.SubmissionAction(ctx, c.scope(), op.ID)
	if err != nil || got.State != "rejected" || got.Result != "changed" || got.ExpectedTurnID != "t1" || f.starts != 0 {
		t.Fatal("durable prepared stage resumed after its displayed turn changed", err)
	}
}

func TestContinueExpectedLatestInterruptedTurnStartsOnlyExplicitQueue(t *testing.T) {
	c, f, _ := fixture(t, true)
	r := claim(t, c, "pending")
	tick(t, c)
	f.turns[0].Status, f.paused = "interrupted", true
	ctx := context.Background()
	op, err := c.ContinueExpected(ctx, "current-stop-card", "t1")
	if err != nil {
		t.Fatal(err)
	}
	tick(t, c)
	got, err := c.Store.SubmissionAction(ctx, c.scope(), op.ID)
	if err != nil || got.State != "confirmed" || got.Result != "continued" || got.ExpectedTurnID != "t1" || f.starts != 1 || f.active() != "t2" || len(f.queue) != 0 || f.addCalls != 1 || f.steerCalls != 0 {
		t.Fatal("explicit Continue did not preserve its original target or used a different mutation", err)
	}
	tick(t, c)
	if view(t, c, r.ID).State != "consumed" {
		t.Fatal("continued input was not linked to native consumption")
	}
	replay, err := c.ContinueExpected(ctx, "current-stop-card", "t2")
	if err != nil || replay.ID != op.ID || replay.ExpectedTurnID != "t1" || replay.Result != "continued" {
		t.Fatal("successful Continue replay changed its original result", err)
	}
	tick(t, c)
	if f.starts != 1 {
		t.Fatal("completed Continue callback was dispatched again")
	}
}

func TestContinueExpectedRequiresCardTurnAndImmutableSeed(t *testing.T) {
	c, _, _ := fixture(t, false)
	ctx := context.Background()
	if _, err := c.ContinueExpected(ctx, "invalid", ""); err != store.ErrSubmissionStale {
		t.Fatal("Continue card omitted its native turn")
	}
	op, err := c.ContinueExpected(ctx, "seeded", "original")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Store.PrepareSubmissionAction(ctx, c.scope(), op.ID, "start", "replacement", "queue", "client"); err != store.ErrSubmissionStale {
		t.Fatal("Continue preparation replaced the original displayed turn", err)
	}
}
