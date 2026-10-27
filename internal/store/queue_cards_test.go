package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

func TestQueueCardStoppedAndIdleNeverOfferSteerOrExecutionPromise(t *testing.T) {
	s, _, scope, r := submissionStore(t)
	ctx := context.Background()
	if e := s.ObserveSubmission(ctx, scope, r.ID, "queued", "native-q", 3, "", ""); e != nil {
		t.Fatal(e)
	}
	for _, status := range []string{"inProgress", "interrupted", "completed"} {
		v := TaskControl{TurnID: "turn", NativeStatus: status, State: "available", Kind: "stop", QueueCount: 3, Fingerprint: status}
		if status == "interrupted" {
			v.Kind = "continue"
		}
		if status == "completed" {
			v.State = "unavailable"
			v.Kind = ""
		}
		if e := s.StageTaskControls(ctx, scope, []TaskControl{v}); e != nil {
			t.Fatal(e)
		}
		if e := s.StageQueueCard(ctx, scope, r.ID); e != nil {
			t.Fatal(e)
		}
		m, e := s.Mirror(ctx, scope.HostID, scope.ThreadID, QueueCardKey(r.ID))
		if e != nil {
			t.Fatal(e)
		}
		if strings.Contains(m.Card, "追加引导") != (status == "inProgress") || strings.Contains(m.Card, "当前轮结束后执行") {
			t.Fatalf("wrong queue UI %s %s", status, m.Card)
		}
		if !strings.Contains(m.Card, "第 3 条") {
			t.Fatal("lost actual ordinal")
		}
		if status == "interrupted" && !strings.Contains(m.Card, "等待显式继续") {
			t.Fatal("stopped queue lost retain guidance")
		}
	}
}
func TestDisconnectedQueueKeepsOnlySafeLocalCancellation(t *testing.T) {
	s, _, scope, r := submissionStore(t)
	ctx := context.Background()
	n := deliveredPendingCard(t, s, scope, r)
	if e := s.StageDisconnectedCards(ctx, scope, "disconnected"); e != nil {
		t.Fatal(e)
	}
	m, e := s.Mirror(ctx, scope.HostID, scope.ThreadID, QueueCardKey(r.ID))
	if e != nil || m.Card != "" || !m.HasCallback || !cardControlProofsContain(m.CallbackProofs, "cancel_pending", n.ActionToken, n.Revision) {
		t.Fatal("offline erased local cancel", e)
	}
	if _, e := s.DB.Exec(`INSERT INTO submission_attempts(attempt_id,host_id,app_id,thread_id,receipt_id,stage,state,client_id,created_at_ms) VALUES('late','host','app','thread',?,'add','unknown',?,1)`, r.ID, r.ClientID); e != nil {
		t.Fatal(e)
	}
	if e := s.StageDisconnectedCards(ctx, scope, "disconnected"); e != nil {
		t.Fatal(e)
	}
	m, e = s.Mirror(ctx, scope.HostID, scope.ThreadID, QueueCardKey(r.ID))
	if e != nil || strings.Contains(m.Card, "cancel_pending") {
		t.Fatal("offline exposed uncertain cancel", e)
	}
	if _, e := cancelLocal(t, s, scope, n); !errors.Is(e, ErrSubmissionStale) {
		t.Fatal("old safe callback canceled ambiguous item", e)
	}
}

func TestQueueCardMirrorCreationIsIndependentOfTaskLookup(t *testing.T) {
	for _, withTask := range []bool{false, true} {
		t.Run(fmt.Sprint(withTask), func(t *testing.T) {
			s, _, scope, r := submissionStore(t)
			ctx := context.Background()
			if e := s.ObserveSubmission(ctx, scope, r.ID, "queued", "native-q", 1, "", ""); e != nil {
				t.Fatal(e)
			}
			if withTask {
				v := TaskControl{TurnID: "turn", NativeStatus: "interrupted", Kind: "continue", State: "available", QueueCount: 1, Fingerprint: "stopped"}
				if e := s.StageTaskControls(ctx, scope, []TaskControl{v}); e != nil {
					t.Fatal(e)
				}
			}
			if e := s.StageQueueCard(ctx, scope, r.ID); e != nil {
				t.Fatalf("initial queued mirror task=%t: %v", withTask, e)
			}
			before, e := s.Mirror(ctx, scope.HostID, scope.ThreadID, QueueCardKey(r.ID))
			if e != nil {
				t.Fatal(e)
			}
			if e := s.ObserveSubmission(ctx, scope, r.ID, "queued", "native-q", 2, "", ""); e != nil {
				t.Fatal(e)
			}
			if e := s.StageQueueCard(ctx, scope, r.ID); e != nil {
				t.Fatalf("existing queued mirror task=%t: %v", withTask, e)
			}
			after, e := s.Mirror(ctx, scope.HostID, scope.ThreadID, before.Key)
			if e != nil || after.Revision != before.Revision+1 || !strings.Contains(after.Card, "第 2 条") {
				t.Fatal("existing queued card was not updated", e)
			}
		})
	}
}

func TestQueueCardWorktreeRestoreReusesOriginalReplyAndIsIdempotent(t *testing.T) {
	s, _, scope, r := submissionStore(t)
	ctx := context.Background()
	if err := s.StageQueueCardWithOptions(ctx, scope, r.ID, QueueCardOptions{WorktreeRestore: "restoring"}); err != nil {
		t.Fatal(err)
	}
	first, err := s.Mirror(ctx, scope.HostID, scope.ThreadID, QueueCardKey(r.ID))
	if err != nil || first.ParentMessageID != r.MessageID || first.ParentKey != "" || !strings.Contains(first.Card, "工作树已回收，正在从快照恢复。原消息已保存，无需重发。") || !strings.Contains(first.Card, "cancel_pending") || strings.Contains(first.Card, r.Body) {
		t.Fatalf("restore card did not preserve the original reply: %#v %v", first, err)
	}
	if err := s.StageQueueCardWithOptions(ctx, scope, r.ID, QueueCardOptions{WorktreeRestore: "restoring"}); err != nil {
		t.Fatal(err)
	}
	replayed, err := s.Mirror(ctx, scope.HostID, scope.ThreadID, first.Key)
	if err != nil || replayed.Revision != first.Revision || replayed.Card != first.Card {
		t.Fatalf("same restore state was not idempotent: %#v %v", replayed, err)
	}
	a, claimed, err := s.ClaimMirror(ctx, scope.HostID, scope.ThreadID, first.Key, first.Revision, 5000)
	if err != nil || !claimed || a.Operation != "reply" || a.ParentMessageID != r.MessageID {
		t.Fatalf("restore card did not use the existing reply outbox: %#v claimed=%t err=%v", a, claimed, err)
	}
	if err := s.FinishMirrorAttempt(ctx, a.ID, "confirmed", "restore-card"); err != nil {
		t.Fatal(err)
	}
	if err := s.StageQueueCardWithOptions(ctx, scope, r.ID, QueueCardOptions{WorktreeRestore: "restored"}); err != nil {
		t.Fatal(err)
	}
	updated, err := s.Mirror(ctx, scope.HostID, scope.ThreadID, first.Key)
	if err != nil || updated.Key != first.Key || updated.RemoteID != "restore-card" || updated.Revision != first.Revision+1 || !strings.Contains(updated.Card, "工作树已恢复，正在提交原消息。") || !strings.Contains(updated.Card, "cancel_pending") {
		t.Fatalf("restore update changed card identity: %#v %v", updated, err)
	}
	if err := s.StageQueueCardWithOptions(ctx, scope, r.ID, QueueCardOptions{WorktreeRestore: "restored"}); err != nil {
		t.Fatal(err)
	}
	again, err := s.Mirror(ctx, scope.HostID, scope.ThreadID, first.Key)
	if err != nil || again.Revision != updated.Revision {
		t.Fatalf("restore update replay duplicated work: %#v %v", again, err)
	}
	mirrors, err := s.Mirrors(ctx, scope.HostID, scope.ThreadID)
	if err != nil || len(mirrors) != 1 {
		t.Fatalf("restore created a second mirror: %d %v", len(mirrors), err)
	}
}

func TestQueueCardWorktreeRestoreOnlyAppliesToSafeReceivedInput(t *testing.T) {
	ctx := context.Background()
	for _, state := range []string{"queued", "unknown"} {
		t.Run(state, func(t *testing.T) {
			s, _, scope, r := submissionStore(t)
			switch state {
			case "queued":
				if err := s.ObserveSubmission(ctx, scope, r.ID, "queued", "native-q", 1, "", ""); err != nil {
					t.Fatal(err)
				}
			case "unknown":
				a, claimed, err := s.ClaimSubmissionAttempt(ctx, scope, r.ID, "", "add")
				if err != nil || !claimed {
					t.Fatalf("claim native attempt=%t err=%v", claimed, err)
				}
				if err := s.FinishSubmissionAttempt(ctx, scope, a.ID, "unknown", "", -1); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.StageQueueCardWithOptions(ctx, scope, r.ID, QueueCardOptions{WorktreeRestore: "restoring"}); err != nil {
				t.Fatal(err)
			}
			m, err := s.Mirror(ctx, scope.HostID, scope.ThreadID, QueueCardKey(r.ID))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(m.Card, "工作树") || strings.Contains(m.Card, "cancel_pending") {
				t.Fatalf("%s state accepted restore/local controls: %s", state, m.Card)
			}
			if state == "queued" && !strings.Contains(m.Card, "已排队") || state == "unknown" && !strings.Contains(m.Card, "正在核验") {
				t.Fatalf("%s state was not preserved: %s", state, m.Card)
			}
		})
	}
}

func TestQueueCardWorktreeRestoreRejectsUnallowlistedStatusWithoutCreatingCard(t *testing.T) {
	s, _, scope, r := submissionStore(t)
	ctx := context.Background()
	if err := s.StageQueueCardWithOptions(ctx, scope, r.ID, QueueCardOptions{WorktreeRestore: "raw error /tmp/private"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Mirror(ctx, scope.HostID, scope.ThreadID, QueueCardKey(r.ID)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unallowlisted restore status created a card: %v", err)
	}
}
