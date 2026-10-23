package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"codex-feishu/internal/cards"
)

func sharedCardFixture(t *testing.T, separateInstruction bool) (*Store, SubmissionScope, AttachmentBatch, Submission) {
	t.Helper()
	s, _, scope, _ := submissionStore(t)
	body := "inspect"
	if separateInstruction {
		body = ""
	}
	b := receiveAttachment(t, s, attachmentInput(scope, "alice", "first-attachment", body, AttachmentPart{Kind: "image", ResourceKey: "img_fixture"}))
	if separateInstruction {
		b = receiveAttachment(t, s, attachmentInput(scope, "alice", "later-instruction", "inspect", AttachmentPart{Kind: "text", Text: "inspect"}))
	}
	n, err := s.Submission(context.Background(), scope, b.ReceiptID)
	if err != nil || n.State != "collecting" {
		t.Fatalf("collecting input missing: %s %v", n.State, err)
	}
	return s, scope, b, n
}

func readySharedCard(t *testing.T, s *Store) {
	t.Helper()
	p, claimed, err := s.ClaimAttachmentDownload(context.Background(), "host", "app", 100)
	if err != nil || !claimed {
		t.Fatalf("resource claim=%v err=%v", claimed, err)
	}
	p.Size, p.Digest, p.FileID = 12, "fixture-digest", "fixture-file"
	if err := s.FinishAttachmentDownload(context.Background(), p, "", false, 100); err != nil {
		t.Fatal(err)
	}
	refreshAttachments(t, s)
}

func claimSharedCard(t *testing.T, s *Store, scope SubmissionScope, key string, at int64) MirrorAttempt {
	t.Helper()
	m, err := s.Mirror(context.Background(), scope.HostID, scope.ThreadID, key)
	if err != nil {
		t.Fatal(err)
	}
	a, claimed, err := s.ClaimMirror(context.Background(), scope.HostID, scope.ThreadID, key, m.Revision, at)
	if err != nil || !claimed {
		t.Fatalf("card claim=%v err=%v", claimed, err)
	}
	return a
}

func assertNoSeparateQueueCard(t *testing.T, s *Store, scope SubmissionScope, receipt string) {
	t.Helper()
	if _, err := s.Mirror(context.Background(), scope.HostID, scope.ThreadID, QueueCardKey(receipt)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("separate queue card exists: %v", err)
	}
	mirrors, err := s.Mirrors(context.Background(), scope.HostID, scope.ThreadID)
	if err != nil || len(mirrors) != 1 {
		t.Fatalf("UI mirror count=%d err=%v", len(mirrors), err)
	}
}

func TestSharedSubmissionCardReusesAttachmentReplyAndPatch(t *testing.T) {
	s, scope, b, n := sharedCardFixture(t, true)
	ctx := context.Background()
	original := claimSharedCard(t, s, scope, AttachmentCardKey(b.ID), 5000)
	if original.Operation != "reply" || original.ParentMessageID != b.FirstMessageID {
		t.Fatal("attachment card lost original reply")
	}
	if err := s.FinishMirrorAttempt(ctx, original.ID, "confirmed", "shared-remote-card"); err != nil {
		t.Fatal(err)
	}
	readySharedCard(t, s)
	if err := s.ObserveSubmission(ctx, scope, n.ID, "queued", "native-q", 2, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.StageQueueCard(ctx, scope, n.ID); err != nil {
		t.Fatal(err)
	}
	m, err := s.SubmissionCard(ctx, scope, n.ID)
	if err != nil || m.Key != AttachmentCardKey(b.ID) || m.RemoteID != "shared-remote-card" || !strings.Contains(m.Card, "第 2 条") || strings.Contains(m.Card, "附件已进入原生队列") {
		t.Fatalf("shared queue projection=%#v err=%v", m, err)
	}
	assertNoSeparateQueueCard(t, s, scope, n.ID)
	update := claimSharedCard(t, s, scope, m.Key, 9000)
	if update.Operation != "update" || update.TargetMessageID != "shared-remote-card" || update.ParentMessageID != b.FirstMessageID {
		t.Fatalf("queue transition did not PATCH original card: %#v", update)
	}
	if before, err := s.MirrorAttempt(ctx, original.ID); err != nil || before.State != "confirmed" || before.RemoteID != "shared-remote-card" {
		t.Fatal("original delivery journal changed", err)
	}
}

func TestSharedSubmissionCardLocalCancelKeepsFirstAttachmentParent(t *testing.T) {
	s, scope, b, n := sharedCardFixture(t, true)
	ctx := context.Background()
	if n.MessageID == b.FirstMessageID {
		t.Fatal("fixture did not separate instruction and first attachment")
	}
	first := claimSharedCard(t, s, scope, AttachmentCardKey(b.ID), 5000)
	if err := s.FinishMirrorAttempt(ctx, first.ID, "confirmed", "shared-remote-card"); err != nil {
		t.Fatal(err)
	}
	readySharedCard(t, s)
	if err := s.StageQueueCardWithOptions(ctx, scope, n.ID, QueueCardOptions{LocalPending: true, Offline: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.StageDisconnectedCards(ctx, scope, "disconnected"); err != nil {
		t.Fatal(err)
	}
	m, err := s.SubmissionCard(ctx, scope, n.ID)
	if err != nil || m.ParentMessageID != b.FirstMessageID || !strings.Contains(m.Card, "cancel_pending") {
		t.Fatalf("shared offline cancellation unavailable: %#v %v", m, err)
	}
	n, err = s.Submission(ctx, scope, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CancelPendingSubmission(ctx, scope, "operator", []string{"operator"}, n.ChatID, m.RemoteID, n.ActionToken, n.Revision)
	if err != nil || a.Stage != "local" || a.State != "confirmed" {
		t.Fatalf("shared card local cancel rejected: %#v %v", a, err)
	}
	m, err = s.SubmissionCard(ctx, scope, n.ID)
	if err != nil || m.RemoteID != "shared-remote-card" || m.ParentMessageID != b.FirstMessageID || !m.Terminal || cardHasCallback(m.Card) {
		t.Fatalf("cancel changed shared card identity: %#v %v", m, err)
	}
	assertNoSeparateQueueCard(t, s, scope, n.ID)
	if again, err := s.CancelPendingSubmission(ctx, scope, "operator", []string{"operator"}, n.ChatID, m.RemoteID, n.ActionToken, n.Revision); err != nil || again.ID != a.ID {
		t.Fatal("local cancel replay lost original action", err)
	}
}

func TestSharedSubmissionCardUnknownReplyStaysFrozenUntilReadback(t *testing.T) {
	s, scope, b, n := sharedCardFixture(t, true)
	ctx := context.Background()
	original := claimSharedCard(t, s, scope, AttachmentCardKey(b.ID), 5000)
	if err := s.FinishMirrorAttempt(ctx, original.ID, "unknown", ""); err != nil {
		t.Fatal(err)
	}
	readySharedCard(t, s)
	if err := s.ObserveSubmission(ctx, scope, n.ID, "queued", "native-q", 1, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.StageQueueCard(ctx, scope, n.ID); err != nil {
		t.Fatal(err)
	}
	m, err := s.SubmissionCard(ctx, scope, n.ID)
	if err != nil || m.State != "unknown" || m.RemoteID != "" || m.Revision <= original.Revision {
		t.Fatalf("ambiguous delivery reopened: %#v %v", m, err)
	}
	if frozen, err := s.MirrorAttempt(ctx, original.ID); err != nil || frozen.State != "unknown" || frozen.Card != original.Card || frozen.ParentMessageID != b.FirstMessageID {
		t.Fatal("ambiguous request changed", err)
	}
	if _, claimed, err := s.ClaimMirror(ctx, scope.HostID, scope.ThreadID, m.Key, m.Revision, 9000); err != nil || claimed {
		t.Fatal("unknown card retried", err)
	}
	assertNoSeparateQueueCard(t, s, scope, n.ID)
	// Simulate positive readback of the original frozen reply, then verify only
	// its known remote message receives the newer queued projection.
	if err := s.FinishMirrorAttempt(ctx, original.ID, "confirmed", "readback-card"); err != nil {
		t.Fatal(err)
	}
	update := claimSharedCard(t, s, scope, m.Key, 12000)
	if update.Operation != "update" || update.TargetMessageID != "readback-card" || update.ParentMessageID != b.FirstMessageID {
		t.Fatalf("readback did not preserve shared target: %#v", update)
	}
}

func TestSharedSubmissionCardKeepsLegacyUnknownQueueJournalAuthoritative(t *testing.T) {
	s, scope, b, n := sharedCardFixture(t, true)
	ctx := context.Background()
	first := claimSharedCard(t, s, scope, AttachmentCardKey(b.ID), 5000)
	if err := s.FinishMirrorAttempt(ctx, first.ID, "confirmed", "old-attachment-card"); err != nil {
		t.Fatal(err)
	}
	readySharedCard(t, s)
	if err := s.ObserveSubmission(ctx, scope, n.ID, "queued", "native-q", 1, "", ""); err != nil {
		t.Fatal(err)
	}
	n, err := s.Submission(ctx, scope, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	legacy := QueueCardKey(n.ID)
	card := cards.QueueInputView(cards.QueueView{State: n.State, Token: n.ActionToken, QueueID: n.QueueID, Revision: n.Revision, QueueOrdinal: 1})
	if _, err := s.DB.Exec(`INSERT INTO mirrors(host_id,thread_id,mirror_key,turn_id,kind,card_json,parent_message_id) VALUES(?,?,?,?,'progress',?,?)`, scope.HostID, scope.ThreadID, legacy, legacy, card, n.MessageID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`INSERT INTO outbound(host_id,thread_id,mirror_key,state) VALUES(?,?,?,'ready')`, scope.HostID, scope.ThreadID, legacy); err != nil {
		t.Fatal(err)
	}
	unknown := claimSharedCard(t, s, scope, legacy, 9000)
	if err := s.FinishMirrorAttempt(ctx, unknown.ID, "unknown", ""); err != nil {
		t.Fatal(err)
	}
	before, err := s.Mirror(ctx, scope.HostID, scope.ThreadID, AttachmentCardKey(b.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StageQueueCardWithOptions(ctx, scope, n.ID, QueueCardOptions{Offline: true}); err != nil {
		t.Fatal(err)
	}
	m, err := s.SubmissionCard(ctx, scope, n.ID)
	if err != nil || m.Key != legacy || m.State != "unknown" || m.ParentMessageID != n.MessageID {
		t.Fatalf("legacy unknown journal replaced: %#v %v", m, err)
	}
	refreshAttachments(t, s)
	after, err := s.Mirror(ctx, scope.HostID, scope.ThreadID, before.Key)
	if err != nil || after.Revision != before.Revision || after.RemoteID != before.RemoteID || after.Card != before.Card {
		t.Fatal("legacy attachment card acquired a second queue writer", err)
	}
	if frozen, err := s.MirrorAttempt(ctx, unknown.ID); err != nil || frozen.State != "unknown" || frozen.Card != unknown.Card {
		t.Fatal("legacy unknown attempt changed", err)
	}
}

func TestSharedSubmissionCardOfflineRefreshCannotRestoreNativeButtons(t *testing.T) {
	s, scope, b, n := sharedCardFixture(t, true)
	ctx := context.Background()
	first := claimSharedCard(t, s, scope, AttachmentCardKey(b.ID), 5000)
	if err := s.FinishMirrorAttempt(ctx, first.ID, "confirmed", "shared-remote-card"); err != nil {
		t.Fatal(err)
	}
	readySharedCard(t, s)
	if err := s.ObserveSubmission(ctx, scope, n.ID, "queued", "native-q", 1, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.StageTaskControls(ctx, scope, []TaskControl{{TurnID: "active-turn", NativeStatus: "inProgress", Kind: "stop", State: "available", QueueCount: 1, Fingerprint: "active"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.StageQueueCard(ctx, scope, n.ID); err != nil {
		t.Fatal(err)
	}
	online, err := s.SubmissionCard(ctx, scope, n.ID)
	if err != nil || !strings.Contains(online.Card, `"action":"steer"`) || !strings.Contains(online.Card, `"action":"remove"`) {
		t.Fatal("fixture lacks online queue controls", err)
	}
	if err := s.StageDisconnectedCards(ctx, scope, "disconnected"); err != nil {
		t.Fatal(err)
	}
	offline, err := s.SubmissionCard(ctx, scope, n.ID)
	if err != nil || cardHasCallback(offline.Card) {
		t.Fatal("shared card retained disconnected native controls", err)
	}
	refreshAttachments(t, s)
	after, err := s.SubmissionCard(ctx, scope, n.ID)
	if err != nil || after.Card != offline.Card || after.Revision != offline.Revision {
		t.Fatal("worker restored online presentation after disconnect", err)
	}
	if err := s.StageQueueCardWithOptions(ctx, scope, n.ID, QueueCardOptions{LocalPending: true, Offline: true}); err != nil {
		t.Fatal(err)
	}
	refreshAttachments(t, s)
	after, err = s.SubmissionCard(ctx, scope, n.ID)
	if err != nil || cardHasCallback(after.Card) || after.Key != online.Key || after.RemoteID != online.RemoteID {
		t.Fatal("offline periodic stage revived native controls or changed identity", err)
	}
}

func TestSharedSubmissionCardRejectsForeignScopeAndAlteredParent(t *testing.T) {
	s, scope, b, n := sharedCardFixture(t, true)
	ctx := context.Background()
	foreign := scope
	foreign.AppID = "foreign-app"
	if _, err := s.SubmissionCard(ctx, foreign, n.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("foreign card read accepted: %v", err)
	}
	if _, err := s.DB.Exec(`UPDATE mirrors SET parent_message_id='foreign-message' WHERE mirror_key=?`, AttachmentCardKey(b.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmissionCard(ctx, scope, n.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("altered physical parent accepted: %v", err)
	}
}

func TestSharedSubmissionCardOfflineKeepsLocalAttachmentManagement(t *testing.T) {
	for _, state := range []string{"open", "collecting", "failed"} {
		t.Run(state, func(t *testing.T) {
			s, _, scope, _ := submissionStore(t)
			body := "inspect"
			if state == "open" || state == "collecting" {
				body = ""
			}
			b := receiveAttachment(t, s, attachmentInput(scope, "alice", "source", body, AttachmentPart{Kind: "image", ResourceKey: "img_fixture"}))
			if state == "collecting" {
				b = receiveAttachment(t, s, attachmentInput(scope, "alice", "instruction", "inspect", AttachmentPart{Kind: "text", Text: "inspect"}))
			}
			ctx := context.Background()
			if state == "failed" {
				p, claimed, err := s.ClaimAttachmentDownload(ctx, "host", "app", 100)
				if err != nil || !claimed {
					t.Fatal("download not claimed", err)
				}
				if err := s.FinishAttachmentDownload(ctx, p, "download-rejected", false, 100); err != nil {
					t.Fatal(err)
				}
				refreshAttachments(t, s)
				if err := s.StageQueueCard(ctx, scope, b.ReceiptID); err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.Mirror(ctx, scope.HostID, scope.ThreadID, AttachmentCardKey(b.ID))
			if err != nil {
				t.Fatal(err)
			}
			if state != "collecting" && !cardHasCallback(before.Card) {
				t.Fatal("fixture lacks local attachment management")
			}
			if err := s.StageDisconnectedCards(ctx, scope, "disconnected"); err != nil {
				t.Fatal(err)
			}
			after, err := s.Mirror(ctx, scope.HostID, scope.ThreadID, before.Key)
			if err != nil || after.Card != before.Card || after.Revision != before.Revision {
				t.Fatalf("offline replaced %s attachment management: %v", state, err)
			}
		})
	}
}

func TestAutomaticAttachmentCreatesCardWhenManagementIsNeeded(t *testing.T) {
	for _, tc := range []struct{ state, content string }{
		{"queued", "第 2 条"},
		{"unknown", "正在核验"},
		{"rejected", "提交失败"},
		{"offline", "cancel_pending"},
		{"download-failed", "attachment_retry"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			s, scope, b, n := sharedCardFixture(t, false)
			ctx := context.Background()
			if _, err := s.SubmissionCard(ctx, scope, n.ID); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("automatic input created an early receipt card: %v", err)
			}
			if tc.state == "download-failed" {
				p, claimed, err := s.ClaimAttachmentDownload(ctx, "host", "app", 100)
				if err != nil || !claimed {
					t.Fatal("download not claimed", err)
				}
				if err := s.FinishAttachmentDownload(ctx, p, "download-rejected", false, 100); err != nil {
					t.Fatal(err)
				}
				refreshAttachments(t, s)
			} else {
				readySharedCard(t, s)
			}
			switch tc.state {
			case "queued":
				if err := s.ObserveSubmission(ctx, scope, n.ID, "queued", "native-q", 2, "", ""); err != nil {
					t.Fatal(err)
				}
			case "unknown", "rejected":
				a, claimed, err := s.ClaimSubmissionAttempt(ctx, scope, n.ID, "", "add")
				if err != nil || !claimed {
					t.Fatal("native add not claimed", err)
				}
				if err := s.FinishSubmissionAttempt(ctx, scope, a.ID, tc.state, "", -1); err != nil {
					t.Fatal(err)
				}
			}
			options := QueueCardOptions{LocalPending: tc.state == "offline", Offline: tc.state == "offline"}
			if err := s.StageQueueCardWithOptions(ctx, scope, n.ID, options); err != nil {
				t.Fatal(err)
			}
			card, err := s.SubmissionCard(ctx, scope, n.ID)
			if err != nil || card.Key != AttachmentCardKey(b.ID) || card.ParentMessageID != b.FirstMessageID || !strings.Contains(card.Card, tc.content) {
				t.Fatal("automatic attachment lost its required management card", err)
			}
			assertNoSeparateQueueCard(t, s, scope, n.ID)
		})
	}
}
