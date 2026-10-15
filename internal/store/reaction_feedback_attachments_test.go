package store

import (
	"context"
	"testing"
)

func confirmReceivedFeedback(t *testing.T, s *Store, scope SubmissionScope, r Receipt, emoji string) {
	t.Helper()
	transition := reactionClaim(t, s, scope, r, emoji, 1000)
	if err := s.FinishReactionTransition(context.Background(), transition, "confirmed", "reaction-"+r.ID+emoji); err != nil {
		t.Fatal(err)
	}
}

func assertReceivedFeedbackBindings(t *testing.T, s *Store, scope SubmissionScope, count int) {
	t.Helper()
	bindings, err := s.ReceivedFeedbackBindingsAt(context.Background(), scope.HostID, scope.AppID, 2000)
	if err != nil || len(bindings) != count {
		t.Fatalf("feedback bindings=%#v count=%d err=%v", bindings, count, err)
	}
	if len(bindings) > 0 && (bindings[0].HostID != scope.HostID || bindings[0].ThreadID != scope.ThreadID || bindings[0].ChatID != "chat") {
		t.Fatalf("foreign feedback binding: %#v", bindings)
	}
}

func draftReactionProjectionFixture(t *testing.T) (*Store, SubmissionScope, AttachmentBatch, Receipt, Receipt) {
	t.Helper()
	s, _, scope, ordinary := submissionStore(t)
	confirmReceivedFeedback(t, s, scope, ordinary, "Get")
	receiveAttachment(t, s, attachmentInput(scope, "alice", "draft-image", "", AttachmentPart{Kind: "image", ResourceKey: "img_fixture"}))
	receipts, err := s.ReceiptsForDeployment(context.Background(), scope.HostID, scope.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	var source Receipt
	for _, r := range receipts {
		if r.MessageID == "draft-image" {
			source = r
		}
	}
	if source.ID == "" || source.State != "draft" {
		t.Fatal("draft source absent")
	}
	confirmReceivedFeedback(t, s, scope, source, "Get")
	b := receiveAttachment(t, s, attachmentInput(scope, "alice", "sealed-instruction", "inspect", AttachmentPart{Kind: "text", Text: "inspect"}))
	target, err := s.Submission(context.Background(), scope, b.ReceiptID)
	if err != nil || target.State != "collecting" {
		t.Fatalf("collecting target: %#v %v", target, err)
	}
	confirmReceivedFeedback(t, s, scope, target.Receipt, "THINKING")
	return s, scope, b, source, target.Receipt
}

func TestReceivedFeedbackDraftFollowsCollectingTargetUntilThinkingConfirmed(t *testing.T) {
	s, scope, _, source, _ := draftReactionProjectionFixture(t)
	assertReceivedFeedbackBindings(t, s, scope, 1)
	confirmReceivedFeedback(t, s, scope, source, "THINKING")
	assertReceivedFeedbackBindings(t, s, scope, 0)
}

func TestReceivedFeedbackDraftTargetOutsideReceivedPhaseDoesNotPoll(t *testing.T) {
	for _, state := range []string{"queued", "consumed", "steered", "failed", "removed", "unknown"} {
		t.Run(state, func(t *testing.T) {
			s, scope, _, source, target := draftReactionProjectionFixture(t)
			confirmReceivedFeedback(t, s, scope, source, "THINKING")
			// The native actor state is authoritative even while the receipt row
			// still says collecting; this is how Submission reads the projection.
			if _, err := s.DB.Exec(`UPDATE submission_inputs SET state=? WHERE receipt_id=?`, state, target.ID); err != nil {
				t.Fatal(err)
			}
			assertReceivedFeedbackBindings(t, s, scope, 0)
		})
	}
}

func TestReceivedFeedbackDraftFollowsReceivedTargetGet(t *testing.T) {
	s, scope, _, source, target := draftReactionProjectionFixture(t)
	confirmReceivedFeedback(t, s, scope, source, "THINKING")
	if _, err := s.DB.Exec(`UPDATE submission_inputs SET state='received' WHERE receipt_id=?`, target.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE m0_receipts SET state='received' WHERE id=?`, target.ID); err != nil {
		t.Fatal(err)
	}
	target.State = "received"
	confirmReceivedFeedback(t, s, scope, target, "Get")
	assertReceivedFeedbackBindings(t, s, scope, 1)
	confirmReceivedFeedback(t, s, scope, source, "Get")
	assertReceivedFeedbackBindings(t, s, scope, 0)
}

func TestReceivedFeedbackUnsealedDraftAndOrdinaryReceiptRemainCandidates(t *testing.T) {
	s, _, scope, ordinary := submissionStore(t)
	assertReceivedFeedbackBindings(t, s, scope, 1)
	confirmReceivedFeedback(t, s, scope, ordinary, "Get")
	assertReceivedFeedbackBindings(t, s, scope, 0)
	if _, err := s.DB.Exec(`UPDATE m0_receipts SET state='collecting' WHERE id=?`, ordinary.ID); err != nil {
		t.Fatal(err)
	}
	ordinary.State = "collecting"
	assertReceivedFeedbackBindings(t, s, scope, 1)
	confirmReceivedFeedback(t, s, scope, ordinary, "THINKING")
	assertReceivedFeedbackBindings(t, s, scope, 0)
	receiveAttachment(t, s, attachmentInput(scope, "alice", "unsealed-image", "", AttachmentPart{Kind: "image", ResourceKey: "img_unsealed"}))
	assertReceivedFeedbackBindings(t, s, scope, 1)
	receipts, err := s.ReceiptsForDeployment(context.Background(), scope.HostID, scope.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range receipts {
		if r.MessageID == "unsealed-image" {
			confirmReceivedFeedback(t, s, scope, r, "Get")
		}
	}
	assertReceivedFeedbackBindings(t, s, scope, 0)
}

func addReactionOtherBinding(t *testing.T, s *Store, host, app, thread, chat string) {
	t.Helper()
	if _, err := s.BeginBaseline(context.Background(), host, app, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`INSERT INTO thread_observations(host_id,thread_id,origin,state) VALUES(?,?,'new','eligible')`, host, thread); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`INSERT INTO create_intents(intent_id,host_id,thread_id,owner_id,name,members_json,state,chat_id,created_at) VALUES('other-intent',?,?,'owner','name','[]','confirmed',?,1)`, host, thread, chat); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`INSERT INTO bindings(host_id,thread_id,intent_id,chat_id) VALUES(?,?,'other-intent',?)`, host, thread, chat); err != nil {
		t.Fatal(err)
	}
}

func TestReceivedFeedbackDraftProjectionRequiresMatchingBatchScope(t *testing.T) {
	for _, change := range []string{"host", "app", "thread", "chat", "cleared", "empty-target"} {
		t.Run(change, func(t *testing.T) {
			s, scope, b, source, target := draftReactionProjectionFixture(t)
			confirmReceivedFeedback(t, s, scope, source, "THINKING")
			if _, err := s.DB.Exec(`UPDATE submission_inputs SET state='queued' WHERE receipt_id=?`, target.ID); err != nil {
				t.Fatal(err)
			}
			var query string
			switch change {
			case "host":
				addReactionOtherBinding(t, s, "other-host", scope.AppID, scope.ThreadID, "chat")
				query = `UPDATE attachment_batches SET host_id='other-host' WHERE batch_id=?`
			case "app":
				query = `UPDATE attachment_batches SET app_id='other-app' WHERE batch_id=?`
			case "thread":
				addReactionOtherBinding(t, s, scope.HostID, scope.AppID, "other-thread", "other-chat")
				query = `UPDATE attachment_batches SET thread_id='other-thread' WHERE batch_id=?`
			case "chat":
				query = `UPDATE attachment_batches SET chat_id='other-chat' WHERE batch_id=?`
			case "cleared":
				query = `UPDATE attachment_batches SET state='cleared' WHERE batch_id=?`
			case "empty-target":
				query = `UPDATE attachment_batches SET receipt_id='' WHERE batch_id=?`
			}
			if _, err := s.DB.Exec(query, b.ID); err != nil {
				t.Fatal(err)
			}
			// A foreign/cleared/empty target cannot suppress this draft's Get.
			assertReceivedFeedbackBindings(t, s, scope, 1)
			confirmReceivedFeedback(t, s, scope, source, "Get")
			assertReceivedFeedbackBindings(t, s, scope, 0)
			foreign := scope
			foreign.AppID = "foreign-app"
			assertReceivedFeedbackBindings(t, s, foreign, 0)
		})
	}
}

func TestReceivedFeedbackDraftProjectionKeepsUnknownReactionAndThrottleFrozen(t *testing.T) {
	for _, mode := range []string{"unknown", "throttle"} {
		t.Run(mode, func(t *testing.T) {
			s, scope, _, source, _ := draftReactionProjectionFixture(t)
			transition := reactionClaim(t, s, scope, source, "THINKING", 1000)
			if mode == "unknown" {
				if err := s.FinishReactionTransition(context.Background(), transition, "unknown", ""); err != nil {
					t.Fatal(err)
				}
			} else {
				reactionBegin(t, s, transition, "delete")
				if err := s.RecordReactionDeletion(context.Background(), transition); err != nil {
					t.Fatal(err)
				}
				reactionBegin(t, s, transition, "add")
				if err := s.DeferReactionTransition(context.Background(), transition, "add", 1000, 5000); err != nil {
					t.Fatal(err)
				}
			}
			assertReceivedFeedbackBindings(t, s, scope, 0)
			if mode == "throttle" {
				bindings, err := s.ReceivedFeedbackBindingsAt(context.Background(), scope.HostID, scope.AppID, 6000)
				if err != nil || len(bindings) != 1 {
					t.Fatalf("due draft feedback lost: %#v %v", bindings, err)
				}
			}
		})
	}
}
