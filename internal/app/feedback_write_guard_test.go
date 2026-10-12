package app

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestFeedbackGuardRejectsBeforeTransitionOrAttempt(t *testing.T) {
	f, g, _, _ := feedbackRetryFixture(t)
	releases := 0
	f.BeginWrite = func(context.Context, string) (context.Context, func(), error) {
		return nil, func() { releases++ }, errors.New("PRIVATE refusal")
	}
	err := f.Received(context.Background(), "thread")
	if err == nil || strings.Contains(err.Error(), "PRIVATE") || releases != 1 || len(g.adds) != 0 {
		t.Fatal("refusal leaked or dispatched", err, releases)
	}
	for _, table := range []string{"m0_reactions", "m0_operations"} {
		var count int
		if err := f.Store.DB.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("refused feedback wrote attempt", table, count, err)
		}
	}
	if receipts, err := f.Store.Receipts(context.Background(), "thread"); err != nil || len(receipts) != 1 || receipts[0].State != "received" {
		t.Fatal("refusal lost receipt", err)
	}
}

func TestFeedbackKnownDeleteRefusalRetainsExactRemoteState(t *testing.T) {
	f, g, r, _ := feedbackRetryFixture(t)
	ctx := context.Background()
	if err := f.Received(ctx, "thread"); err != nil {
		t.Fatal(err)
	}
	grants, releases := 0, 0
	f.BeginWrite = func(parent context.Context, _ string) (context.Context, func(), error) {
		grants++
		if grants == 2 {
			return nil, func() { releases++ }, errors.New("PRIVATE next-stage refusal")
		}
		return parent, func() { releases++ }, nil
	}
	if err := f.set(ctx, "thread", r, "THINKING"); err == nil || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("following add accepted or leaked", err)
	}
	emoji, state, err := f.Store.ReactionFeedbackState(ctx, r.ID)
	if err != nil || emoji != "" || state != "confirmed" || len(g.deletes) != 1 || len(g.adds) != 1 || releases != 2 {
		t.Fatal("known deletion was lost", emoji, state, err, releases)
	}
	f.BeginWrite = nil
	if err := f.set(ctx, "thread", r, "THINKING"); err != nil || len(g.deletes) != 1 || len(g.adds) != 2 {
		t.Fatal("resume repeated deletion", err)
	}
}

type scopeReactionGateway struct {
	*reactionRetryGateway
	inspect func(context.Context)
}

func (g *scopeReactionGateway) AddReaction(ctx context.Context, id, emoji string) (string, error) {
	g.inspect(ctx)
	return g.reactionRetryGateway.AddReaction(ctx, id, emoji)
}

func TestFeedbackScopePersistsUntilConfirmation(t *testing.T) {
	f, g, r, _ := feedbackRetryFixture(t)
	type key struct{}
	released := false
	f.Gateway = &scopeReactionGateway{reactionRetryGateway: g, inspect: func(ctx context.Context) {
		if ctx.Value(key{}) != true || released || ctx.Err() != nil {
			t.Fatal("gateway lacked live scope")
		}
	}}
	f.BeginWrite = func(parent context.Context, _ string) (context.Context, func(), error) {
		return context.WithValue(parent, key{}, true), func() {
			_, state, err := f.Store.ReactionFeedbackState(context.Background(), r.ID)
			if err != nil || state != "confirmed" {
				t.Error("scope released before outcome persistence", state, err)
			}
			released = true
		}, nil
	}
	if err := f.Received(context.Background(), "thread"); err != nil || !released {
		t.Fatal(err)
	}
}

type scopeReminderGateway struct {
	*retryRequestGateway
	inspect func(context.Context) error
}

func (g *scopeReminderGateway) ReplyCard(ctx context.Context, parent, card, id string) (string, error) {
	if err := g.inspect(ctx); err != nil {
		return "", err
	}
	return g.retryRequestGateway.ReplyCard(ctx, parent, card, id)
}

func TestReminderGuardRejectsWithoutAttemptAndHoldsScope(t *testing.T) {
	tr, _, g := liveRetryTracker(t)
	ctx := context.Background()
	releases := 0
	tr.BeginWrite = func(context.Context, string) (context.Context, func(), error) {
		return nil, func() { releases++ }, errors.New("PRIVATE")
	}
	if sent, err := tr.DispatchOne(ctx, "thread"); sent || err == nil || strings.Contains(err.Error(), "PRIVATE") || g.calls != 0 || releases != 1 {
		t.Fatal("guard refusal dispatched/leaked", sent, err)
	}
	var count int
	if err := tr.Store.DB.QueryRow(`SELECT count(*) FROM request_reminder_attempts`).Scan(&count); err != nil || count != 0 {
		t.Fatal("refused reminder acquired attempt", count, err)
	}
	type key struct{}
	tr.Gateway = &scopeReminderGateway{retryRequestGateway: g, inspect: func(call context.Context) error {
		if call.Value(key{}) != true || releases != 1 {
			t.Error("reminder lost operation scope")
		}
		return nil
	}}
	tr.BeginWrite = func(parent context.Context, _ string) (context.Context, func(), error) {
		return context.WithValue(parent, key{}, true), func() {
			var state string
			if err := tr.Store.DB.QueryRow(`SELECT state FROM request_reminder_attempts`).Scan(&state); err != nil || state != "confirmed" {
				t.Error("release preceded reminder confirmation", state, err)
			}
			releases++
		}, nil
	}
	if sent, err := tr.DispatchOne(ctx, "thread"); !sent || err != nil || g.calls != 1 || releases != 2 {
		t.Fatal(sent, err, releases)
	}
}

func TestReminderScopeCancellationLeavesDispatching(t *testing.T) {
	tr, _, g := liveRetryTracker(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	released := false
	tr.BeginWrite = func(context.Context, string) (context.Context, func(), error) {
		return ctx, func() { released = true }, nil
	}
	tr.Gateway = &scopeReminderGateway{retryRequestGateway: g, inspect: func(call context.Context) error {
		cancel()
		return call.Err()
	}}
	if sent, err := tr.DispatchOne(context.Background(), "thread"); !sent || err == nil || !released || g.calls != 0 {
		t.Fatal(sent, err, released)
	}
	var state string
	if err := tr.Store.DB.QueryRow(`SELECT state FROM request_reminder_attempts`).Scan(&state); err != nil || state != "dispatching" {
		t.Fatal("cancellation forged known result", state, err)
	}
}

func TestFeedbackJournalFailureReleasesOnlyKnownUnsentStage(t *testing.T) {
	for _, failedKind := range []string{"add", "delete", "add-after-delete"} {
		t.Run(failedKind, func(t *testing.T) {
			f, g, r, _ := feedbackRetryFixture(t)
			ctx := context.Background()
			if failedKind != "add" {
				if err := f.Received(ctx, "thread"); err != nil {
					t.Fatal(err)
				}
			}
			kind := failedKind
			if kind == "add-after-delete" {
				kind = "add"
			}
			if _, err := f.Store.DB.Exec(`CREATE TRIGGER fail_reaction_write BEFORE INSERT ON m0_operations WHEN NEW.kind='feishu.reaction.` + kind + `' BEGIN SELECT RAISE(FAIL,'fixture journal failure'); END`); err != nil {
				t.Fatal(err)
			}
			if err := f.set(ctx, "thread", r, "THINKING"); err == nil {
				t.Fatal("journal failure dispatched")
			}
			emoji, state, err := f.Store.ReactionFeedbackState(ctx, r.ID)
			if err != nil || state != "confirmed" {
				t.Fatal("known unsent stage froze as unknown", emoji, state, err)
			}
			if failedKind == "delete" && (emoji != "Get" || len(g.deletes) != 0) || failedKind == "add" && len(g.adds) != 0 || failedKind == "add-after-delete" && (emoji != "" || len(g.deletes) != 1 || len(g.adds) != 1) {
				t.Fatal("known remote state changed despite journal failure", emoji)
			}
			if _, err := f.Store.DB.Exec(`DROP TRIGGER fail_reaction_write`); err != nil {
				t.Fatal(err)
			}
			deletes := len(g.deletes)
			if err := f.set(ctx, "thread", r, "THINKING"); err != nil || failedKind == "add-after-delete" && len(g.deletes) != deletes {
				t.Fatal("retry repeated completed deletion", err)
			}
		})
	}
}
