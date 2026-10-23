package store

import (
	"context"
	"errors"
	"testing"
)

func pendingOrdinaryContinue(t *testing.T) (*Store, SubmissionScope, Receipt, Submission) {
	t.Helper()
	s, _, scope := taskControlStore(t, true)
	ctx := context.Background()
	stop, err := s.ClaimThreadSubmissionActionExpected(ctx, scope, "stop", "synthetic-stop", "turn")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ResolveSubmissionAction(ctx, scope, stop.ID, "confirmed", "stopped"); err != nil {
		t.Fatal(err)
	}
	r, _, err := s.ReceiveSubmissionInput(ctx, scope, Receipt{AppID: scope.AppID, ThreadID: scope.ThreadID, ChatID: "chat", MessageID: "synthetic-ordinary", Body: "synthetic pending content"})
	if err != nil {
		t.Fatal(err)
	}
	return s, scope, r, deliveredPendingCard(t, s, scope, r)
}

func TestPendingOrdinaryContinuePreservesLocalCancellation(t *testing.T) {
	for _, entrance := range []string{"cancel-card", "legacy-remove"} {
		t.Run(entrance, func(t *testing.T) {
			s, scope, r, n := pendingOrdinaryContinue(t)
			var err error
			if entrance == "cancel-card" {
				_, err = cancelLocal(t, s, scope, n)
			} else {
				_, err = s.ClaimSubmissionAction(context.Background(), scope, n.ID, "remove", "", n.Revision)
			}
			if err != nil {
				t.Fatal("unsent ordinary input could not be locally cancelled", err)
			}
			actions, err := s.SubmissionActions(context.Background(), scope)
			if err != nil {
				t.Fatal(err)
			}
			for _, a := range actions {
				if _, ok := InputContinueSource(a); ok && (a.State != "rejected" || a.Result != "removed") {
					t.Fatal("local removal left its continue intention runnable")
				}
			}
			if _, add, err := s.ClaimSubmissionAttempt(context.Background(), scope, r.ID, "", "add"); add || !errors.Is(err, ErrSubmissionStale) {
				t.Fatal("cancelled ordinary input reached native add", err)
			}
		})
	}
}

func TestPendingOrdinaryContinueCancellationKeepsExistingSafetyGates(t *testing.T) {
	for _, variant := range []string{"running", "unknown", "start-stage", "malformed", "native-attempt"} {
		t.Run(variant, func(t *testing.T) {
			s, scope, r, n := pendingOrdinaryContinue(t)
			var err error
			switch variant {
			case "running", "unknown":
				_, err = s.DB.Exec(`UPDATE submission_actions SET state=? WHERE receipt_id=?`, variant, r.ID)
			case "start-stage":
				_, err = s.DB.Exec(`UPDATE submission_actions SET stage='start' WHERE receipt_id=?`, r.ID)
			case "malformed":
				_, err = s.DB.Exec(`UPDATE submission_actions SET token='input-continue.invalid.source' WHERE receipt_id=?`, r.ID)
			case "native-attempt":
				_, err = s.DB.Exec(`INSERT INTO submission_attempts(attempt_id,host_id,app_id,thread_id,receipt_id,stage,state,created_at_ms) VALUES(?,'host','app','thread',?,'add','rejected',1)`, ID(), r.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cancelLocal(t, s, scope, n); !errors.Is(err, ErrSubmissionStale) {
				t.Fatal("unsafe control or historical native attempt was locally cancelled", err)
			}
			if _, err := s.ClaimSubmissionAction(context.Background(), scope, n.ID, "remove", "", n.Revision); !errors.Is(err, ErrSubmissionStale) {
				t.Fatal("legacy remove bypassed control/attempt gate", err)
			}
		})
	}
}

func TestPendingOrdinaryContinueCancellationRollbackPreservesIntent(t *testing.T) {
	s, scope, r, n := pendingOrdinaryContinue(t)
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_cancel_continue BEFORE UPDATE ON mirrors BEGIN SELECT RAISE(ABORT,'synthetic card failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := cancelLocal(t, s, scope, n); err == nil {
		t.Fatal("failed cancellation projection unexpectedly committed")
	}
	actions, err := s.SubmissionActions(context.Background(), scope)
	if err != nil || len(actions) != 2 {
		t.Fatal("rolled-back cancellation changed action set", err)
	}
	if _, ok := InputContinueSource(actions[1]); !ok || actions[1].State != "ready" || actions[1].Result != "" {
		t.Fatal("failed cancellation retired the original intention")
	}
	got, err := s.Submission(context.Background(), scope, r.ID)
	if err != nil || got.State != "received" || got.Revision != n.Revision {
		t.Fatal("failed cancellation modified original input", err)
	}
}
