package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestQueueCardUsesReplyJournalAndCannotClobberHistory(t *testing.T) {
	s, _, scope, receipt := submissionStore(t)
	ctx := context.Background()
	key := QueueCardKey(receipt.ID)
	if err := s.StageQueueCard(ctx, scope, receipt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Mirror(ctx, scope.HostID, scope.ThreadID, key); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("idle input created an extra card", err)
	}
	if err := s.StageMirrors(ctx, scope.HostID, scope.ThreadID, nil, Watermark{TurnID: "native-turn", ItemID: "native-item", ObservedAtMS: 1234}); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveSubmission(ctx, scope, receipt.ID, "queued", "native-q", 2, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.StageQueueCard(ctx, scope, receipt.ID); err != nil {
		t.Fatal(err)
	}
	m, err := s.Mirror(ctx, scope.HostID, scope.ThreadID, key)
	if err != nil || m.ParentMessageID != receipt.MessageID || m.ParentKey != "" || strings.Contains(m.Card, receipt.Body) {
		t.Fatal("queue card lost its reply or echoed input", err)
	}
	if !strings.Contains(m.Card, "native-q") {
		t.Fatal("queue ID missing from callback")
	}
	mark, err := s.Watermark(ctx, scope.HostID, scope.ThreadID)
	if err != nil || mark.TurnID != "native-turn" || mark.ItemID != "native-item" || mark.ObservedAtMS != 1234 {
		t.Fatal("UI advanced native history", err)
	}
	a, claimed, err := s.ClaimMirror(ctx, scope.HostID, scope.ThreadID, key, m.Revision, 5000)
	if err != nil || !claimed || a.Operation != "reply" || a.ParentMessageID != receipt.MessageID {
		t.Fatal("queue card did not claim a real reply", err)
	}
	if err := s.FinishMirrorAttempt(ctx, a.ID, "confirmed", "remote-card"); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveSubmission(ctx, scope, receipt.ID, "consumed", "", 0, "native-turn", "native-user"); err != nil {
		t.Fatal(err)
	}
	if err := s.StageQueueCard(ctx, scope, receipt.ID); err != nil {
		t.Fatal(err)
	}
	m, err = s.Mirror(ctx, scope.HostID, scope.ThreadID, key)
	if err != nil || m.RemoteID != "remote-card" || !m.Terminal || strings.Contains(m.Card, "callback") {
		t.Fatal("terminal card retained buttons or changed identity", err)
	}
	a, claimed, err = s.ClaimMirror(ctx, scope.HostID, scope.ThreadID, key, m.Revision, 9000)
	if err != nil || !claimed || a.Operation != "update" || a.TargetMessageID != "remote-card" {
		t.Fatal("terminal state created a second card", err)
	}
}

func TestQueueCardCannotUnfreezeUnknownOrUseForeignScope(t *testing.T) {
	s, _, scope, receipt := submissionStore(t)
	ctx := context.Background()
	if err := s.ObserveSubmission(ctx, scope, receipt.ID, "queued", "q", 1, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.StageQueueCard(ctx, scope, receipt.ID); err != nil {
		t.Fatal(err)
	}
	m, _ := s.Mirror(ctx, scope.HostID, scope.ThreadID, QueueCardKey(receipt.ID))
	a, claimed, err := s.ClaimMirror(ctx, scope.HostID, scope.ThreadID, m.Key, m.Revision, 5000)
	if err != nil || !claimed {
		t.Fatal(err)
	}
	if err := s.FinishMirrorAttempt(ctx, a.ID, "unknown", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveSubmission(ctx, scope, receipt.ID, "consumed", "", 0, "native-turn", "native-user"); err != nil {
		t.Fatal(err)
	}
	if err := s.StageQueueCard(ctx, scope, receipt.ID); err != nil {
		t.Fatal(err)
	}
	m, _ = s.Mirror(ctx, scope.HostID, scope.ThreadID, m.Key)
	if m.State != "unknown" {
		t.Fatal("presentation change retried ambiguous card")
	}
	if err := s.StageQueueCard(ctx, SubmissionScope{"host", "another-app", "thread"}, receipt.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("foreign application staged card", err)
	}
}
