package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func deliveredPendingCard(t *testing.T, s *Store, scope SubmissionScope, r Receipt) Submission {
	t.Helper()
	ctx := context.Background()
	if e := s.StageQueueCardWithOptions(ctx, scope, r.ID, QueueCardOptions{LocalPending: true, Offline: true}); e != nil {
		t.Fatal(e)
	}
	m, e := s.Mirror(ctx, scope.HostID, scope.ThreadID, QueueCardKey(r.ID))
	if e != nil || !strings.Contains(m.Card, "cancel_pending") || strings.Contains(m.Card, r.Body) {
		t.Fatal("local card invalid", e)
	}
	a, ok, e := s.ClaimMirror(ctx, scope.HostID, scope.ThreadID, m.Key, m.Revision, 5000)
	if e != nil || !ok || a.ParentMessageID != r.MessageID {
		t.Fatal("local card not reply", e)
	}
	if e = s.FinishMirrorAttempt(ctx, a.ID, "confirmed", "pending-card"); e != nil {
		t.Fatal(e)
	}
	ns, e := s.Submissions(ctx, scope)
	if e != nil || len(ns) != 1 {
		t.Fatal(e)
	}
	return ns[0]
}
func cancelLocal(t *testing.T, s *Store, scope SubmissionScope, n Submission) (SubmissionAction, error) {
	t.Helper()
	return s.CancelPendingSubmission(context.Background(), scope, "operator", []string{"operator"}, n.ChatID, "pending-card", n.ActionToken, n.Revision)
}
func TestLocalCancellationChecksAuthorityAndReplayAcrossRestart(t *testing.T) {
	s, path, scope, r := submissionStore(t)
	n := deliveredPendingCard(t, s, scope, r)
	ctx := context.Background()
	for _, variant := range []string{"actor", "chat", "card", "token", "revision", "app", "host"} {
		t.Run(variant, func(t *testing.T) {
			user, chat, card, token, rev, sc := "operator", n.ChatID, "pending-card", n.ActionToken, n.Revision, scope
			switch variant {
			case "actor":
				user = "outsider"
			case "chat":
				chat = "foreign"
			case "card":
				card = "original-input"
			case "token":
				token = "foreign"
			case "revision":
				rev++
			case "app":
				sc.AppID = "foreign"
			case "host":
				sc.HostID = "foreign"
			}
			if _, e := s.CancelPendingSubmission(ctx, sc, user, []string{"operator"}, chat, card, token, rev); e == nil {
				t.Fatal("invalid callback canceled")
			}
		})
	}
	a, e := cancelLocal(t, s, scope, n)
	if e != nil || a.Stage != "local" || a.State != "confirmed" {
		t.Fatal(a, e)
	}
	m, e := s.Mirror(ctx, scope.HostID, scope.ThreadID, QueueCardKey(r.ID))
	if e != nil || !m.Terminal || m.RemoteID != "pending-card" || strings.Contains(m.Card, "callback") {
		t.Fatal("cancel did not update same card", e)
	}
	s.Close()
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	again, e := cancelLocal(t, s, scope, n)
	if e != nil || again.ID != a.ID {
		t.Fatal("replay changed action", e)
	}
	if _, ok, e := s.ClaimSubmissionAttempt(ctx, scope, r.ID, "", "add"); ok || !errors.Is(e, ErrSubmissionStale) {
		t.Fatal("cancelled receipt became add", ok, e)
	}
}
func TestLocalCancellationRejectsEveryHistoricalWriteAndUnsafeReceipt(t *testing.T) {
	for _, variant := range []string{"dispatching", "unknown_attempt", "confirmed_attempt", "rejected_attempt", "m0_add", "m0_action", "pending_receipt", "unknown_receipt", "accepted_receipt", "ordinal", "queue", "history"} {
		t.Run(variant, func(t *testing.T) {
			s, _, scope, r := submissionStore(t)
			n := deliveredPendingCard(t, s, scope, r)
			ctx := context.Background()
			var e error
			switch variant {
			case "dispatching", "unknown_attempt", "confirmed_attempt", "rejected_attempt":
				state := "dispatching"
				if variant != "dispatching" {
					state = strings.TrimSuffix(variant, "_attempt")
				}
				_, e = s.DB.Exec(`INSERT INTO submission_attempts(attempt_id,host_id,app_id,thread_id,receipt_id,stage,state,client_id,created_at_ms) VALUES('old','host','app','thread',?,'add',?,?,1)`, r.ID, state, r.ClientID)
			case "m0_add":
				_, e = s.Begin(ctx, "m0.add."+r.ID, "queue.add")
			case "m0_action":
				_, e = s.DB.Exec(`INSERT INTO m0_actions(receipt_id,kind,state) VALUES(?,'steer','processing')`, r.ID)
			case "pending_receipt", "unknown_receipt", "accepted_receipt":
				_, e = s.DB.Exec(`UPDATE m0_receipts SET state=? WHERE id=?`, strings.TrimSuffix(variant, "_receipt"), r.ID)
			case "ordinal":
				_, e = s.DB.Exec(`UPDATE submission_inputs SET queue_ordinal=1 WHERE receipt_id=?`, r.ID)
			case "queue":
				_, e = s.DB.Exec(`UPDATE submission_inputs SET queue_id='native' WHERE receipt_id=?`, r.ID)
			case "history":
				_, e = s.DB.Exec(`UPDATE submission_inputs SET turn_id='turn',item_id='input' WHERE receipt_id=?`, r.ID)
			}
			if e != nil {
				t.Fatal(e)
			}
			if _, e := cancelLocal(t, s, scope, n); !errors.Is(e, ErrSubmissionStale) {
				t.Fatal("historical write accepted as local", e)
			}
			if _, e := s.ClaimSubmissionAction(ctx, scope, r.ID, "remove", "", n.Revision); !errors.Is(e, ErrSubmissionStale) {
				t.Fatal("old remove bypassed local gate", e)
			}
			if e := s.StageQueueCardWithOptions(ctx, scope, r.ID, QueueCardOptions{LocalPending: true, Offline: true}); e != nil {
				t.Fatal(e)
			}
			m, e := s.Mirror(ctx, scope.HostID, scope.ThreadID, QueueCardKey(r.ID))
			if e != nil || strings.Contains(m.Card, "cancel_pending") || !strings.Contains(m.Card, "正在核验") {
				t.Fatal("unsafe local button", e)
			}
		})
	}
}
func TestLocalCancelAndActorSubmissionHaveOneTransactionWinner(t *testing.T) {
	for i := 0; i < 20; i++ {
		s, _, scope, r := submissionStore(t)
		n := deliveredPendingCard(t, s, scope, r)
		var wg sync.WaitGroup
		wg.Add(2)
		start := make(chan struct{})
		var cancelErr, addErr error
		var add bool
		go func() { defer wg.Done(); <-start; _, cancelErr = cancelLocal(t, s, scope, n) }()
		go func() {
			defer wg.Done()
			<-start
			_, add, addErr = s.ClaimSubmissionAttempt(context.Background(), scope, r.ID, "", "add")
		}()
		close(start)
		wg.Wait()
		if (cancelErr == nil) == add {
			t.Fatalf("two or zero winners cancel=%v add=%t %v", cancelErr, add, addErr)
		}
		ns, e := s.Submissions(context.Background(), scope)
		if e != nil {
			t.Fatal(e)
		}
		if cancelErr == nil && ns[0].State != "removed" || add && ns[0].State != "submitting" {
			t.Fatal("winner state overwritten")
		}
	}
}
func TestLocalCancelRollbackKeepsReceiptAndOriginalCard(t *testing.T) {
	s, _, scope, r := submissionStore(t)
	n := deliveredPendingCard(t, s, scope, r)
	ctx := context.Background()
	before, e := s.Mirror(ctx, scope.HostID, scope.ThreadID, QueueCardKey(r.ID))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec(`CREATE TRIGGER fail_cancel_card BEFORE UPDATE ON mirrors BEGIN SELECT RAISE(ABORT,'fixture'); END`); e != nil {
		t.Fatal(e)
	}
	if _, e := cancelLocal(t, s, scope, n); e == nil {
		t.Fatal("expected rollback")
	}
	ns, e := s.Submissions(ctx, scope)
	if e != nil || ns[0].State != "received" || ns[0].Revision != n.Revision {
		t.Fatal("failed cancel modified receipt", e)
	}
	actions, e := s.SubmissionActions(ctx, scope)
	if e != nil || len(actions) != 0 {
		t.Fatal("failed cancel retained action", e)
	}
	after, e := s.Mirror(ctx, scope.HostID, scope.ThreadID, before.Key)
	if e != nil || after.Card != before.Card || after.Revision != before.Revision {
		t.Fatal("failed cancel modified card", e)
	}
}
