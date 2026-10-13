package store

import (
	"context"
	"errors"
	"math"
	"testing"
)

func reactionClaim(t *testing.T, s *Store, scope SubmissionScope, r Receipt, desired string, at int64) ReactionTransition {
	t.Helper()
	transition, claimed, err := s.ClaimReactionTransitionAt(context.Background(), scope, r.ID, r.State, desired, at)
	if err != nil || !claimed {
		t.Fatal("reaction not claimed", err)
	}
	return transition
}
func reactionBegin(t *testing.T, s *Store, transition ReactionTransition, operation string) {
	t.Helper()
	fresh, err := s.Begin(context.Background(), ReactionOperationKey(transition, operation), "feishu.reaction."+operation)
	if err != nil || !fresh {
		t.Fatal("reaction attempt not durable", err)
	}
}

func TestReactionDeferredSurvivesRestartAndUsesLatestReceipt(t *testing.T) {
	s, path, scope, r := submissionStore(t)
	ctx := context.Background()
	tr := reactionClaim(t, s, scope, r, "Get", 1000)
	reactionBegin(t, s, tr, "add")
	if err := s.DeferReactionTransition(ctx, tr, "add", 2000, 15000); err != nil {
		t.Fatal(err)
	}
	if err := s.DeferReactionTransition(ctx, tr, "add", 9999, 99000); err != nil {
		t.Fatal(err)
	}
	var due, count int64
	if err := s.DB.QueryRow("SELECT next_retry_at_ms,rejection_count FROM reaction_throttles WHERE receipt_id=?", r.ID).Scan(&due, &count); err != nil || due != 17000 || count != 1 {
		t.Fatal("deferral replay advanced clock", due, count, err)
	}
	health, err := s.Health(ctx, SchemaVersion, scope.HostID, scope.AppID)
	if err != nil || health.Reactions["deferred"] != 1 {
		t.Fatal("deferred feedback hidden in health", err)
	}
	bindings, err := s.ReceivedFeedbackBindingsAt(ctx, scope.HostID, scope.AppID, 16999)
	if err != nil || len(bindings) != 0 {
		t.Fatal("waiting received feedback polls members", err)
	}
	bindings, err = s.ReceivedFeedbackBindingsAt(ctx, scope.HostID, scope.AppID, 17000)
	if err != nil || len(bindings) != 1 {
		t.Fatal("due received feedback lost", err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.RecoverReactionTransitions(ctx, scope.HostID, scope.AppID); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := s.ClaimReactionTransitionAt(ctx, scope, r.ID, "received", "Get", 16999); err != nil || claimed {
		t.Fatal("restart ignored delay", err)
	}
	if err := s.ObserveSubmission(ctx, scope, r.ID, "queued", "q", 1, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := s.ClaimReactionTransitionAt(ctx, scope, r.ID, "received", "Get", 17000); err != nil || claimed {
		t.Fatal("stale input feedback claimed", err)
	}
	r.State = "queued"
	next := reactionClaim(t, s, scope, r, "THINKING", 17000)
	if next.Revision != tr.Revision+1 || next.Desired != "THINKING" || next.PreviousID != "" {
		t.Fatal("rejected emoji reused", next)
	}
	if err := s.FinishReactionTransition(ctx, next, "confirmed", "new"); err != nil {
		t.Fatal(err)
	}
	state, id, err := s.Operation(ctx, ReactionOperationKey(tr, "add"))
	if err != nil || state != "rejected" || id != "" {
		t.Fatal("old attempt overwritten", err)
	}
	var n int
	if err := s.DB.QueryRow("SELECT count(*) FROM reaction_throttles").Scan(&n); err != nil || n != 0 {
		t.Fatal("successful retry retained throttle", err)
	}
}

func TestReactionDeletionAndDeferralAreAtomic(t *testing.T) {
	s, _, scope, r := submissionStore(t)
	ctx := context.Background()
	initial := reactionClaim(t, s, scope, r, "Get", 1000)
	if err := s.FinishReactionTransition(ctx, initial, "confirmed", "known-old"); err != nil {
		t.Fatal(err)
	}
	tr := reactionClaim(t, s, scope, r, "OnIt", 2000)
	reactionBegin(t, s, tr, "delete")
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_reaction_delete BEFORE UPDATE ON m0_operations BEGIN SELECT RAISE(FAIL,'fixture'); END;`); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordReactionDeletion(ctx, tr); err == nil {
		t.Fatal("fault accepted")
	}
	var old string
	if err := s.DB.QueryRow("SELECT reaction_id FROM m0_reactions WHERE receipt_id=?", r.ID).Scan(&old); err != nil || old != "known-old" {
		t.Fatal("half-committed deletion", err)
	}
	if _, err := s.DB.Exec("DROP TRIGGER fail_reaction_delete"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordReactionDeletion(ctx, tr); err != nil {
		t.Fatal(err)
	}
	reactionBegin(t, s, tr, "add")
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_reaction_defer BEFORE UPDATE ON m0_operations BEGIN SELECT RAISE(FAIL,'fixture'); END;`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeferReactionTransition(ctx, tr, "add", 2000, 0); err == nil {
		t.Fatal("fault accepted")
	}
	var n int
	if err := s.DB.QueryRow("SELECT count(*) FROM reaction_throttles").Scan(&n); err != nil || n != 0 {
		t.Fatal("half-committed throttle", err)
	}
	if _, err := s.DB.Exec("DROP TRIGGER fail_reaction_defer"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeferReactionTransition(ctx, tr, "add", 2000, 0); err != nil {
		t.Fatal(err)
	}
	next := reactionClaim(t, s, scope, r, "CheckMark", 3000)
	if next.PreviousID != "" {
		t.Fatal("known deleted reaction will be deleted again")
	}
}

func TestReactionRetryBackoffAndUnknownFreeze(t *testing.T) {
	s, _, scope, r := submissionStore(t)
	ctx := context.Background()
	at := int64(1000)
	for i, delay := range []int64{1000, 2000, 4000, 8000, 16000, 32000, 60000, 60000} {
		tr := reactionClaim(t, s, scope, r, "Get", at)
		reactionBegin(t, s, tr, "add")
		if err := s.DeferReactionTransition(ctx, tr, "add", at, 0); err != nil {
			t.Fatal(err)
		}
		var due, count int64
		if err := s.DB.QueryRow("SELECT next_retry_at_ms,rejection_count FROM reaction_throttles WHERE receipt_id=?", r.ID).Scan(&due, &count); err != nil || due != at+delay || count != int64(i+1) {
			t.Fatal("wrong backoff", i, due, count, err)
		}
		at = due
	}
	tr := reactionClaim(t, s, scope, r, "Get", at)
	reactionBegin(t, s, tr, "add")
	if err := s.DeferReactionTransition(ctx, tr, "add", math.MaxInt64, 1000); !errors.Is(err, ErrConflict) {
		t.Fatal("overflow accepted", err)
	}
	if err := s.Finish(ctx, ReactionOperationKey(tr, "add"), "unknown", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishReactionTransition(ctx, tr, "unknown", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DeferReactionTransition(ctx, tr, "add", at, 0); !errors.Is(err, ErrConflict) {
		t.Fatal("unknown reopened", err)
	}
	if _, claimed, err := s.ClaimReactionTransitionAt(ctx, scope, r.ID, r.State, "Get", at+999999); err != nil || claimed {
		t.Fatal("unknown repeated", err)
	}
}

func TestReactionRetryMigrationPreservesExistingFeedback(t *testing.T) {
	s, path, scope, r := submissionStore(t)
	ctx := context.Background()
	tr := reactionClaim(t, s, scope, r, "Get", 1000)
	if err := s.FinishReactionTransition(ctx, tr, "confirmed", "known"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("DROP TABLE reaction_throttles; ALTER TABLE mirrors DROP COLUMN progress_observed_at_json; PRAGMA user_version=11"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	emoji, state, err := upgraded.ReactionFeedbackState(ctx, r.ID)
	if err != nil || emoji != "Get" || state != "confirmed" {
		t.Fatal("migration changed known feedback", err)
	}
	var version int
	if err := upgraded.DB.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatal("migration missing", err)
	}
}
