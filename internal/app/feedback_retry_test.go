package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

type reactionRetryGateway struct {
	adds, deletes         []string
	addError, deleteError error
	onAdd                 func()
}

func (g *reactionRetryGateway) AddReaction(_ context.Context, _ string, emoji string) (string, error) {
	g.adds = append(g.adds, emoji)
	if g.onAdd != nil {
		g.onAdd()
	}
	return fmt.Sprint("id-", len(g.adds)), g.addError
}
func (g *reactionRetryGateway) DeleteReaction(_ context.Context, _ string, id string) error {
	g.deletes = append(g.deletes, id)
	return g.deleteError
}
func feedbackRetryFixture(t *testing.T) (*Feedback, *reactionRetryGateway, store.Receipt, *int64) {
	t.Helper()
	db, cfg, _ := serviceFixture(t)
	r, _, err := db.Claim(context.Background(), store.Receipt{AppID: cfg.Feishu.AppID, ThreadID: "thread", ChatID: "chat", MessageID: "original", Body: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	at := int64(1000)
	g := &reactionRetryGateway{}
	f := &Feedback{Store: db, Gateway: g, HostID: cfg.Host.ID, AppID: cfg.Feishu.AppID, Now: func() time.Time { return time.UnixMilli(at) }}
	return f, g, r, &at
}

func TestFeedbackRetryAfterKnownDeleteUsesLatestEmoji(t *testing.T) {
	f, g, r, at := feedbackRetryFixture(t)
	ctx := context.Background()
	if err := f.Received(ctx, "thread"); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.StageSubmissions(ctx, f.scope("thread")); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.ObserveSubmission(ctx, f.scope("thread"), r.ID, "queued", "q", 1, "", ""); err != nil {
		t.Fatal(err)
	}
	r.State = "queued"
	g.addError = &feishu.RateLimited{RetryAfter: 5 * time.Second}
	if err := f.set(ctx, "thread", r, "THINKING"); err != nil {
		t.Fatal(err)
	}
	if len(g.adds) != 2 || len(g.deletes) != 1 {
		t.Fatal("replacement did not run once")
	}
	emoji, state, err := f.Store.ReactionFeedbackState(ctx, r.ID)
	if err != nil || emoji != "" || state != "confirmed" {
		t.Fatal("deleted reaction ID survived", err)
	}
	if err := f.set(ctx, "thread", r, "OnIt"); err != nil || len(g.adds) != 2 {
		t.Fatal("rate floor bypassed", err)
	}
	*at = 6000
	g.addError = nil
	if err := f.set(ctx, "thread", r, "OnIt"); err != nil {
		t.Fatal(err)
	}
	if len(g.deletes) != 1 || len(g.adds) != 3 || g.adds[2] != "OnIt" {
		t.Fatal("retry repeated deletion or obsolete emoji", g.adds, g.deletes)
	}
	receipts, err := f.Store.Receipts(ctx, "thread")
	if err != nil || len(receipts) != 1 || receipts[0].State != "queued" || receipts[0].ClientID != r.ClientID {
		t.Fatal("optional reaction changed input", err)
	}
}

func TestFeedbackDeleteLimitKeepsKnownReactionUntilDue(t *testing.T) {
	f, g, r, at := feedbackRetryFixture(t)
	ctx := context.Background()
	if err := f.set(ctx, "thread", r, "Get"); err != nil {
		t.Fatal(err)
	}
	g.deleteError = &feishu.RateLimited{RetryAfter: 2 * time.Second}
	if err := f.set(ctx, "thread", r, "OnIt"); err != nil {
		t.Fatal(err)
	}
	emoji, state, err := f.Store.ReactionFeedbackState(ctx, r.ID)
	if err != nil || emoji != "Get" || state != "confirmed" || len(g.adds) != 1 {
		t.Fatal("rejected deletion cleared feedback", err)
	}
	*at = 2999
	if err := f.set(ctx, "thread", r, "CheckMark"); err != nil || len(g.deletes) != 1 {
		t.Fatal("delete retried early", err)
	}
	*at = 3000
	g.deleteError = nil
	if err := f.set(ctx, "thread", r, "CheckMark"); err != nil || len(g.deletes) != 2 || len(g.adds) != 2 || g.adds[1] != "CheckMark" {
		t.Fatal("due delete not recovered", err)
	}
}

func TestFeedbackAmbiguousAndPermanentRejectionsNeverRetry(t *testing.T) {
	for _, err := range []error{&feishu.Unknown{}, &feishu.Rejected{}, errors.New("lost response")} {
		f, g, r, at := feedbackRetryFixture(t)
		g.addError = err
		if err := f.set(context.Background(), "thread", r, "Get"); err != nil {
			t.Fatal(err)
		}
		*at = 1000000
		g.addError = nil
		if err := f.set(context.Background(), "thread", r, "CheckMark"); err != nil || len(g.adds) != 1 {
			t.Fatal("non-rate failure reopened", err)
		}
	}
}

func TestFeedbackRetryClockStartsAtResponseReceipt(t *testing.T) {
	f, g, _, at := feedbackRetryFixture(t)
	g.onAdd = func() { *at = 10000 }
	g.addError = &feishu.RateLimited{RetryAfter: 7 * time.Second}
	if err := f.Received(context.Background(), "thread"); err != nil {
		t.Fatal(err)
	}
	*at = 16999
	g.onAdd = nil
	g.addError = nil
	if err := f.Received(context.Background(), "thread"); err != nil || len(g.adds) != 1 {
		t.Fatal("wait started before response", err)
	}
	*at = 17000
	if err := f.Received(context.Background(), "thread"); err != nil || len(g.adds) != 2 {
		t.Fatal("due acknowledgement lost", err)
	}
}
