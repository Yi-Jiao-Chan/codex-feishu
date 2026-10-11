package store

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestReactionClaimSerializesActorsAndOfflineReceiptFeedback(t *testing.T) {
	s, _, scope, r := submissionStore(t)
	ctx := context.Background()
	var claims atomic.Int32
	var wg sync.WaitGroup
	var mu sync.Mutex
	var transition ReactionTransition
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, claimed, err := s.ClaimReactionTransition(ctx, scope, r.ID, "received", "Get")
			if err != nil {
				t.Error(err)
			}
			if claimed {
				claims.Add(1)
				mu.Lock()
				transition = got
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if claims.Load() != 1 {
		t.Fatal("optional feedback duplicated", claims.Load())
	}
	if err := s.FinishReactionTransition(ctx, transition, "confirmed", "reaction"); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveSubmission(ctx, scope, r.ID, "queued", "q", 1, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := s.ClaimReactionTransition(ctx, scope, r.ID, "received", "Get"); err != nil || claimed {
		t.Fatal("stale received state replaced queued feedback", err)
	}
	next, claimed, err := s.ClaimReactionTransition(ctx, scope, r.ID, "queued", "THINKING")
	if err != nil || !claimed || next.PreviousID != "reaction" {
		t.Fatal("known reaction ID lost", err)
	}
	if err := s.FinishReactionTransition(ctx, next, "unknown", ""); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := s.ClaimReactionTransition(ctx, scope, r.ID, "queued", "OnIt"); err != nil || claimed {
		t.Fatal("ambiguous feedback retried", err)
	}
	if _, _, err := s.ClaimReactionTransition(ctx, SubmissionScope{"host", "foreign-app", "thread"}, r.ID, "queued", "OnIt"); !errors.Is(err, ErrConflict) {
		t.Fatal("foreign application changed reaction", err)
	}
}

func TestReactionCrashRecoveryDoesNotAcknowledgeAnUnknownRemoteWrite(t *testing.T) {
	s, path, scope, r := submissionStore(t)
	ctx := context.Background()
	if _, claimed, err := s.ClaimReactionTransition(ctx, scope, r.ID, "received", "Get"); err != nil || !claimed {
		t.Fatal(err)
	}
	s.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.RecoverReactionTransitions(ctx, scope.HostID, scope.AppID); err != nil {
		t.Fatal(err)
	}
	emoji, state, err := reopened.ReactionFeedbackState(ctx, r.ID)
	if err != nil || emoji != "" || state != "unknown" {
		t.Fatal("crash invented successful feedback", err)
	}
	if _, claimed, err := reopened.ClaimReactionTransition(ctx, scope, r.ID, "received", "Get"); err != nil || claimed {
		t.Fatal("crash recovery repeated external feedback", err)
	}
}
