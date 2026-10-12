package store

import (
	"context"
	"testing"
)

func inputEvidenceFixture(t *testing.T) (*Store, VerificationBinding, SubmissionScope) {
	t.Helper()
	s, n, g, bot, now := verificationBindingFixture(t, true)
	b, err := s.RegisterVerificationBinding(context.Background(), n, g, bot, now)
	if err != nil {
		t.Fatal(err)
	}
	return s, b, SubmissionScope{b.HostID, b.AppID, b.ThreadID}
}

func inputEvidenceReceipt(t *testing.T, s *Store, b VerificationBinding, message string) Receipt {
	t.Helper()
	r, _, err := s.Claim(context.Background(), Receipt{AppID: b.AppID, ThreadID: b.ThreadID, ChatID: b.ChatID, MessageID: message, Body: "fixture input"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StageSubmissions(context.Background(), SubmissionScope{b.HostID, b.AppID, b.ThreadID}); err != nil {
		t.Fatal(err)
	}
	return r
}

func inputEvidenceAttempt(t *testing.T, s *Store, scope SubmissionScope, receipt, action, stage string) SubmissionAttempt {
	t.Helper()
	a, claimed, err := s.ClaimSubmissionAttempt(context.Background(), scope, receipt, action, stage)
	if err != nil || !claimed {
		t.Fatal("claim fixture", claimed, err)
	}
	return a
}

func assertInputEvidence(t *testing.T, s *Store, b VerificationBinding, want VerificationInputEvidence) {
	t.Helper()
	var changesBefore, changesAfter int
	if err := s.DB.QueryRow(`SELECT total_changes()`).Scan(&changesBefore); err != nil {
		t.Fatal(err)
	}
	got, err := s.VerificationInputEvidence(context.Background(), b)
	if err != nil || got != want {
		t.Fatalf("evidence: got %+v want %+v err %v", got, want, err)
	}
	if err := s.DB.QueryRow(`SELECT total_changes()`).Scan(&changesAfter); err != nil || changesAfter != changesBefore {
		t.Fatal("query wrote state", changesBefore, changesAfter, err)
	}
}

func TestVerificationInputEvidenceDistinguishesAcceptedQueueHistoryAndUnknown(t *testing.T) {
	s, b, scope := inputEvidenceFixture(t)
	ctx := context.Background()
	assertInputEvidence(t, s, b, VerificationInputEvidence{})
	r, _, err := s.Claim(ctx, Receipt{AppID: b.AppID, ThreadID: b.ThreadID, ChatID: b.ChatID, MessageID: "unstaged", Body: "fixture input"})
	if err != nil {
		t.Fatal(err)
	}
	assertInputEvidence(t, s, b, VerificationInputEvidence{})
	if err := s.StageSubmissions(ctx, scope); err != nil {
		t.Fatal(err)
	}
	a := inputEvidenceAttempt(t, s, scope, r.ID, "", "add")
	assertInputEvidence(t, s, b, VerificationInputEvidence{StagedInputs: 1, UnresolvedAttempts: 1})
	if err := s.FinishSubmissionAttempt(ctx, scope, a.ID, "confirmed", "queue-1", -1); err != nil {
		t.Fatal(err)
	}
	assertInputEvidence(t, s, b, VerificationInputEvidence{StagedInputs: 1, AddConfirmed: 1})
	if err := s.ObserveSubmission(ctx, scope, r.ID, "queued", "queue-1", 3, "", ""); err != nil {
		t.Fatal(err)
	}
	assertInputEvidence(t, s, b, VerificationInputEvidence{StagedInputs: 1, AddConfirmed: 1, QueuedSnapshot: 1})
	history := inputEvidenceReceipt(t, s, b, "history")
	a = inputEvidenceAttempt(t, s, scope, history.ID, "", "add")
	if err := s.FinishSubmissionAttempt(ctx, scope, a.ID, "confirmed", "queue-2", -1); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveSubmission(ctx, scope, history.ID, "consumed", "queue-2", 0, "turn-2", "item-2"); err != nil {
		t.Fatal(err)
	}
	lost := inputEvidenceReceipt(t, s, b, "lost-response")
	a = inputEvidenceAttempt(t, s, scope, lost.ID, "", "add")
	if err := s.RecoverSubmissionDispatches(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveSubmission(ctx, scope, lost.ID, "consumed", "", 0, "turn-3", "item-3"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishSubmissionAttempt(ctx, scope, a.ID, "confirmed", "", -1); err != nil {
		t.Fatal(err)
	}
	unknown := inputEvidenceReceipt(t, s, b, "unknown")
	a = inputEvidenceAttempt(t, s, scope, unknown.ID, "", "add")
	if err := s.FinishSubmissionAttempt(ctx, scope, a.ID, "unknown", "", -1); err != nil {
		t.Fatal(err)
	}
	want := VerificationInputEvidence{StagedInputs: 4, AddConfirmed: 3, QueuedSnapshot: 1, HistoryLinked: 2, UnresolvedAttempts: 1}
	assertInputEvidence(t, s, b, want)
	var sequence int
	var name, path string
	if err := s.DB.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	readonly, _, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer readonly.Close()
	assertInputEvidence(t, readonly, b, want)
	attempts, err := s.SubmissionAttempts(ctx, scope)
	if err != nil || attempts[len(attempts)-1].State != "unknown" {
		t.Fatal("query changed unresolved journal", err)
	}
}

func inputEvidenceSteer(t *testing.T) (*Store, VerificationBinding, SubmissionScope, Receipt, SubmissionAction) {
	t.Helper()
	s, b, scope := inputEvidenceFixture(t)
	ctx := context.Background()
	r := inputEvidenceReceipt(t, s, b, "steer")
	a := inputEvidenceAttempt(t, s, scope, r.ID, "", "add")
	if err := s.FinishSubmissionAttempt(ctx, scope, a.ID, "confirmed", "native-queue", -1); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveSubmission(ctx, scope, r.ID, "queued", "native-queue", 1, "", ""); err != nil {
		t.Fatal(err)
	}
	ns, err := s.Submissions(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.ClaimSubmissionAction(ctx, scope, r.ID, "steer", "native-queue", ns[0].Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PrepareSubmissionAction(ctx, scope, op.ID, "delete", "target-turn", "native-queue", r.ClientID); err != nil {
		t.Fatal(err)
	}
	a = inputEvidenceAttempt(t, s, scope, r.ID, op.ID, "delete")
	if err := s.FinishSubmissionAttempt(ctx, scope, a.ID, "confirmed", "", 1); err != nil {
		t.Fatal(err)
	}
	a = inputEvidenceAttempt(t, s, scope, r.ID, op.ID, "steer")
	if err := s.FinishSubmissionAttempt(ctx, scope, a.ID, "confirmed", "", -1); err != nil {
		t.Fatal(err)
	}
	return s, b, scope, r, op
}

func TestVerificationInputEvidenceRequiresFullSteerAndExactTargetHistory(t *testing.T) {
	s, b, scope, r, _ := inputEvidenceSteer(t)
	assertInputEvidence(t, s, b, VerificationInputEvidence{StagedInputs: 1, AddConfirmed: 1, SteerConfirmed: 1})
	if err := s.ObserveSubmission(context.Background(), scope, r.ID, "steered", "native-queue", 0, "target-turn", "target-item"); err != nil {
		t.Fatal(err)
	}
	assertInputEvidence(t, s, b, VerificationInputEvidence{StagedInputs: 1, AddConfirmed: 1, HistoryLinked: 1, SteerConfirmed: 1, SteerHistoryLinked: 1})
	for _, corruption := range []string{"missing-delete", "unknown-delete", "delete-false", "wrong-delete-queue", "wrong-steer-turn", "wrong-action-client", "wrong-action-app", "wrong-history-turn"} {
		t.Run(corruption, func(t *testing.T) {
			s, b, scope, r, op := inputEvidenceSteer(t)
			turn := "target-turn"
			if corruption == "wrong-history-turn" {
				turn = "another-turn"
			}
			if err := s.ObserveSubmission(context.Background(), scope, r.ID, "steered", "native-queue", 0, turn, "history-item"); err != nil {
				t.Fatal(err)
			}
			queries := map[string]string{
				"missing-delete":      `DELETE FROM submission_attempts WHERE action_id=? AND stage='delete'`,
				"unknown-delete":      `UPDATE submission_attempts SET state='unknown' WHERE action_id=? AND stage='delete'`,
				"delete-false":        `UPDATE submission_attempts SET deleted=0 WHERE action_id=? AND stage='delete'`,
				"wrong-delete-queue":  `UPDATE submission_attempts SET queue_id='another-queue' WHERE action_id=? AND stage='delete'`,
				"wrong-steer-turn":    `UPDATE submission_attempts SET expected_turn_id='another-turn' WHERE action_id=? AND stage='steer'`,
				"wrong-action-client": `UPDATE submission_actions SET client_id='another-client' WHERE action_id=?`,
				"wrong-action-app":    `UPDATE submission_actions SET app_id='another-app' WHERE action_id=?`,
			}
			if query := queries[corruption]; query != "" {
				if _, err := s.DB.Exec(query, op.ID); err != nil {
					t.Fatal(err)
				}
			}
			want := VerificationInputEvidence{StagedInputs: 1, AddConfirmed: 1, HistoryLinked: 1}
			if corruption == "wrong-history-turn" {
				want.SteerConfirmed = 1
			}
			if corruption == "unknown-delete" {
				want.UnresolvedAttempts = 1
			}
			assertInputEvidence(t, s, b, want)
		})
	}
}

func TestVerificationInputEvidenceExcludesWrongScopeAndIncompleteLinks(t *testing.T) {
	for _, corruption := range []string{"receipt-chat", "receipt-app", "receipt-thread", "input-app", "attempt-client", "attempt-app", "link-turn", "link-item", "link-thread", "queue-ordinal", "queue-empty"} {
		t.Run(corruption, func(t *testing.T) {
			s, b, scope := inputEvidenceFixture(t)
			ctx := context.Background()
			r := inputEvidenceReceipt(t, s, b, "input")
			a := inputEvidenceAttempt(t, s, scope, r.ID, "", "add")
			if err := s.ObserveSubmission(ctx, scope, r.ID, "consumed", "", 0, "turn", "item"); err != nil {
				t.Fatal(err)
			}
			if err := s.FinishSubmissionAttempt(ctx, scope, a.ID, "confirmed", "", -1); err != nil {
				t.Fatal(err)
			}
			want := VerificationInputEvidence{StagedInputs: 1, AddConfirmed: 1, HistoryLinked: 1}
			query := ""
			switch corruption {
			case "receipt-chat":
				query = `UPDATE m0_receipts SET chat_id='another-chat'`
				want = VerificationInputEvidence{}
			case "receipt-app":
				query = `UPDATE m0_receipts SET app_id='another-app'`
				want = VerificationInputEvidence{}
			case "receipt-thread":
				query = `UPDATE m0_receipts SET thread_id='another-thread'`
				want = VerificationInputEvidence{}
			case "input-app":
				query = `UPDATE submission_inputs SET app_id='another-app'`
				want = VerificationInputEvidence{}
			case "attempt-client":
				query = `UPDATE submission_attempts SET client_id='another-client'`
				want.AddConfirmed = 0
			case "attempt-app":
				query = `UPDATE submission_attempts SET app_id='another-app'`
				want.AddConfirmed = 0
			case "link-turn":
				query = `UPDATE input_history_links SET turn_id='another-turn'`
				want.AddConfirmed, want.HistoryLinked = 0, 0
			case "link-item":
				query = `UPDATE input_history_links SET item_id=''`
				want.AddConfirmed, want.HistoryLinked = 0, 0
			case "link-thread":
				if err := s.ObserveNew(ctx, b.HostID, "another-thread", b.CWD, 1); err != nil {
					t.Fatal(err)
				}
				query = `UPDATE input_history_links SET thread_id='another-thread'`
				want.AddConfirmed, want.HistoryLinked = 0, 0
			case "queue-ordinal", "queue-empty":
				if err := s.ObserveSubmission(ctx, scope, r.ID, "queued", "queue", 2, "", ""); err != nil {
					t.Fatal(err)
				}
				query = `UPDATE submission_inputs SET queue_ordinal=0`
				if corruption == "queue-empty" {
					query = `UPDATE submission_inputs SET queue_id=''`
				}
			}
			if _, err := s.DB.Exec(query); err != nil {
				t.Fatal(err)
			}
			assertInputEvidence(t, s, b, want)
		})
	}
}

func TestVerificationInputEvidenceRejectsChangedPrerequisitesAndReadFailures(t *testing.T) {
	for _, corruption := range []string{"caller-binding", "stored-binding", "deployment", "route-alias", "intent-owner", "intent-state", "missing-table", "canceled"} {
		t.Run(corruption, func(t *testing.T) {
			s, b, _ := inputEvidenceFixture(t)
			ctx := context.Background()
			queries := map[string]string{
				"stored-binding": `UPDATE verification_bindings SET cwd='changed'`,
				"deployment":     `UPDATE deployment SET app_id='another-app'`,
				"route-alias":    `UPDATE bindings SET chat_id='another-chat'`,
				"intent-owner":   `UPDATE create_intents SET owner_id='another-owner'`,
				"intent-state":   `UPDATE create_intents SET state='unknown'`,
				"missing-table":  `DROP TABLE input_history_links`,
			}
			if query := queries[corruption]; query != "" {
				if _, err := s.DB.Exec(query); err != nil {
					t.Fatal(err)
				}
			}
			if corruption == "caller-binding" {
				b.BotID = "another-bot"
			}
			if corruption == "canceled" {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			if _, err := s.VerificationInputEvidence(ctx, b); err == nil {
				t.Fatal("invalid snapshot reported successful zero evidence")
			}
		})
	}
}
