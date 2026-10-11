package store

import (
	"context"
	"errors"
	"testing"
)

func submissionStore(t *testing.T) (*Store, string, SubmissionScope, Receipt) {
	t.Helper()
	s, path := discoveryStore(t)
	eligible(t, s, "thread")
	ctx := context.Background()
	i, err := s.PrepareGroup(ctx, "host", "thread", "owner", "group", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimGroupDispatch(ctx, i.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmGroup(ctx, i.ID, "chat"); err != nil {
		t.Fatal(err)
	}
	if err = s.BindConfirmedGroup(ctx, i.ID); err != nil {
		t.Fatal(err)
	}
	r, _, err := s.Claim(ctx, Receipt{AppID: "app", ThreadID: "thread", ChatID: "chat", MessageID: "message", Body: "fixture input"})
	if err != nil {
		t.Fatal(err)
	}
	scope := SubmissionScope{"host", "app", "thread"}
	if err = s.StageSubmissions(ctx, scope); err != nil {
		t.Fatal(err)
	}
	return s, path, scope, r
}

func TestSubmissionCardSavePreservesConcurrentNativeState(t *testing.T) {
	s, _, scope, r := submissionStore(t)
	ctx := context.Background()
	if err := s.ObserveSubmission(ctx, scope, r.ID, "queued", "native-q", 3, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSubmissionCard(ctx, scope, r.ID, "card"); err != nil {
		t.Fatal(err)
	}
	list, err := s.Submissions(ctx, scope)
	if err != nil || len(list) != 1 || list[0].State != "queued" || list[0].QueueID != "native-q" || list[0].QueueOrdinal != 3 || list[0].CardID != "card" {
		t.Fatal("presentation save lost native state", err)
	}
	if err := s.SaveSubmissionCard(ctx, scope, r.ID, "different-card"); !errors.Is(err, ErrConflict) {
		t.Fatal("persisted card identity was overwritten")
	}
}

func TestSubmissionDispatchCrashKeepsIdentityAndNoSecondClaim(t *testing.T) {
	s, path, scope, r := submissionStore(t)
	ctx := context.Background()
	a, claimed, err := s.ClaimSubmissionAttempt(ctx, scope, r.ID, "", "add")
	if err != nil || !claimed {
		t.Fatal(err)
	}
	s.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err = reopened.RecoverSubmissionDispatches(ctx, scope); err != nil {
		t.Fatal(err)
	}
	got, claimed, err := reopened.ClaimSubmissionAttempt(ctx, scope, r.ID, "", "add")
	if err != nil || claimed || got.ID != a.ID || got.State != "unknown" || got.ClientID != r.ClientID {
		t.Fatal("crashed dispatch got a new identity", err)
	}
	list, _ := reopened.Submissions(ctx, scope)
	if list[0].State != "unknown" {
		t.Fatal("crashed dispatch did not freeze receipt")
	}
}

func TestSubmissionObservationCannotUndoConcurrentActionClaim(t *testing.T) {
	s, _, scope, r := submissionStore(t)
	ctx := context.Background()
	if err := s.ObserveSubmission(ctx, scope, r.ID, "queued", "q", 1, "", ""); err != nil {
		t.Fatal(err)
	}
	list, _ := s.Submissions(ctx, scope)
	a, err := s.ClaimSubmissionAction(ctx, scope, r.ID, "steer", "q", list[0].Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ObserveSubmission(ctx, scope, r.ID, "queued", "q", 1, "", ""); !errors.Is(err, ErrSubmissionStale) {
		t.Fatal("late queue snapshot overwrote active action")
	}
	list, _ = s.Submissions(ctx, scope)
	if list[0].State != "steering" {
		t.Fatal("claimed action became clickable again")
	}
	if err = s.PrepareSubmissionAction(ctx, scope, a.ID, "delete", "original-turn", "q", r.ClientID); err != nil {
		t.Fatal(err)
	}
	if err = s.PrepareSubmissionAction(ctx, scope, a.ID, "delete", "new-turn", "q", r.ClientID); !errors.Is(err, ErrSubmissionStale) {
		t.Fatal("action's captured turn was retargeted")
	}
}

func TestSubmissionScopeCannotClaimForeignActionsOrRecoverTheirWrites(t *testing.T) {
	s, _, scope, r := submissionStore(t)
	ctx := context.Background()
	a, _, err := s.ClaimSubmissionAttempt(ctx, scope, r.ID, "", "add")
	if err != nil {
		t.Fatal(err)
	}
	for _, foreign := range []SubmissionScope{{"host", "other-app", "thread"}, {"other-host", "app", "thread"}, {"host", "app", "other-thread"}} {
		if err := s.RecoverSubmissionDispatches(ctx, foreign); !errors.Is(err, ErrConflict) {
			t.Fatal("foreign recovery was authorized")
		}
		if err := s.FinishSubmissionAttempt(ctx, foreign, a.ID, "confirmed", "q", -1); !errors.Is(err, ErrConflict) {
			t.Fatal("foreign completion was authorized")
		}
	}
	attempts, err := s.SubmissionAttempts(ctx, scope)
	if err != nil || attempts[0].State != "dispatching" {
		t.Fatal("foreign actor changed original dispatch", err)
	}
}

func TestLegacyLostAddReceiptIsNeverAdoptedAsUnsent(t *testing.T) {
	s, _, scope, r := submissionStore(t)
	ctx := context.Background()
	if _, err := s.DB.Exec(`DELETE FROM submission_inputs WHERE receipt_id=?`, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin(ctx, "m0.add."+r.ID, "codex.queue.add"); err != nil {
		t.Fatal(err)
	}
	if err := s.StageSubmissions(ctx, scope); err != nil {
		t.Fatal(err)
	}
	list, _ := s.Submissions(ctx, scope)
	if list[0].State != "unknown" {
		t.Fatal("legacy journal lost its dispatch ambiguity")
	}
	if _, claimed, err := s.ClaimSubmissionAttempt(ctx, scope, r.ID, "", "add"); !errors.Is(err, ErrSubmissionBusy) || claimed {
		t.Fatal("legacy unknown was blindly retried", err)
	}
}

func TestStopCardClaimSeedCannotBeReplacedDuringPreparation(t *testing.T) {
	s, _, scope, _ := submissionStore(t)
	ctx := context.Background()
	a, err := s.ClaimThreadSubmissionActionExpected(ctx, scope, "stop", "card-stop", "original-turn")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.PrepareSubmissionAction(ctx, scope, a.ID, "interrupt", "new-turn", "", ""); !errors.Is(err, ErrSubmissionStale) {
		t.Fatal("preparation replaced the card's captured target", err)
	}
	got, err := s.SubmissionAction(ctx, scope, a.ID)
	if err != nil || got.ExpectedTurnID != "original-turn" || got.Stage != "prepare" || got.State != "ready" {
		t.Fatal("rejected preparation mutated the persisted claim", err)
	}
	if err = s.PrepareSubmissionAction(ctx, scope, a.ID, "interrupt", "original-turn", "", ""); err != nil {
		t.Fatal(err)
	}
	got, err = s.ClaimThreadSubmissionActionExpected(ctx, scope, "stop", "card-stop", "new-turn")
	if err != nil || got.ID != a.ID || got.ExpectedTurnID != "original-turn" || got.Stage != "interrupt" {
		t.Fatal("replayed claim replaced its original target", err)
	}
}
