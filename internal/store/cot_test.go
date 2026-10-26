package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/cot"
)

const (
	cotTestKey       = "cot-progress"
	cotTestRemoteID  = "cot-message"
	cotTestNativeID  = "native-cot"
	cotTestInputID   = "original-input"
	cotTestToolCount = 20
)

func cotTestDocument(status string, completed bool) cot.Document {
	items := make([]cot.Item, 0, cotTestToolCount)
	for i := 0; i < cotTestToolCount; i++ {
		itemStatus := "inProgress"
		if completed {
			itemStatus = "completed"
		}
		items = append(items, cot.Item{
			ID:          "tool-" + string(rune('a'+i)),
			Kind:        "tool",
			Title:       "synthetic tool " + string(rune('a'+i)),
			Status:      itemStatus,
			TimestampMS: int64(100 + i),
		})
	}
	return cot.Document{
		Version:  cot.Version,
		ThreadID: "thread",
		TurnID:   "turn",
		Status:   status,
		Items:    items,
	}
}

func cotTestSmallDocument(status, title, itemStatus string) cot.Document {
	return cot.Document{
		Version:  cot.Version,
		ThreadID: "thread",
		TurnID:   "turn",
		Status:   status,
		Items: []cot.Item{{
			ID:          "tool-a",
			Kind:        "tool",
			Title:       title,
			Status:      itemStatus,
			TimestampMS: 100,
		}},
	}
}

func cotTestCard(t *testing.T, doc cot.Document) string {
	t.Helper()
	card, err := cot.Encode(doc)
	if err != nil {
		t.Fatal(err)
	}
	return card
}

func cotTestMirror(t *testing.T, doc cot.Document, terminal bool) Mirror {
	t.Helper()
	return Mirror{
		Key:             cotTestKey,
		TurnID:          doc.TurnID,
		Kind:            "progress",
		Card:            cotTestCard(t, doc),
		Terminal:        terminal,
		ParentMessageID: cotTestInputID,
	}
}

func cotTestFixture(t *testing.T) (*Store, string) {
	t.Helper()
	s, path := discoveryStore(t)
	eligible(t, s, "thread")
	bindMirrorThread(t, s)
	return s, path
}

func cotTestStage(t *testing.T, s *Store, at int64, doc cot.Document, terminal bool) Mirror {
	t.Helper()
	m := cotTestMirror(t, doc, terminal)
	stage(t, s, at, m)
	return m
}

func cotTestStageKey(t *testing.T, s *Store, at int64, key string, doc cot.Document, terminal bool) Mirror {
	t.Helper()
	m := cotTestMirror(t, doc, terminal)
	m.Key = key
	stage(t, s, at, m)
	return m
}

func cotTestClaimKey(t *testing.T, s *Store, key string, nowMS int64) MirrorAttempt {
	t.Helper()
	ctx := context.Background()
	m, err := s.Mirror(ctx, "host", "thread", key)
	if err != nil {
		t.Fatal(err)
	}
	a, claimed, err := s.ClaimMirror(ctx, "host", "thread", key, m.Revision, nowMS)
	if err != nil || !claimed {
		t.Fatalf("CoT claim %s: claimed=%v err=%v", key, claimed, err)
	}
	return a
}

func cotTestClaim(t *testing.T, s *Store, nowMS int64) MirrorAttempt {
	t.Helper()
	ctx := context.Background()
	m, err := s.Mirror(ctx, "host", "thread", cotTestKey)
	if err != nil {
		t.Fatal(err)
	}
	a, claimed, err := s.ClaimMirror(ctx, "host", "thread", cotTestKey, m.Revision, nowMS)
	if err != nil || !claimed {
		t.Fatalf("CoT claim: claimed=%v err=%v", claimed, err)
	}
	return a
}

func cotTestDelivery(t *testing.T, s *Store) (cotID, stateJSON string, completed bool) {
	t.Helper()
	var completedInt int
	err := s.DB.QueryRow(`SELECT cot_id,state_json,completed FROM cot_deliveries WHERE host_id='host' AND thread_id='thread' AND mirror_key=?`, cotTestKey).Scan(&cotID, &stateJSON, &completedInt)
	if err != nil {
		t.Fatal(err)
	}
	return cotID, stateJSON, completedInt != 0
}

func cotTestMirrorState(t *testing.T, s *Store) Mirror {
	t.Helper()
	m, err := s.Mirror(context.Background(), "host", "thread", cotTestKey)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func cotTestFinish(t *testing.T, s *Store, a MirrorAttempt) {
	t.Helper()
	if err := s.FinishCOTAttempt(context.Background(), a.ID, "confirmed", cotTestRemoteID, cotTestNativeID); err != nil {
		t.Fatal(err)
	}
}

func cotTestPrepareUnknownStage(t *testing.T, s *Store, target string) MirrorAttempt {
	t.Helper()
	inProgress := cotTestDocument("inProgress", false)
	cotTestStage(t, s, 100, inProgress, false)
	create := cotTestClaim(t, s, 1000)
	if target == "create" {
		return create
	}
	cotTestFinish(t, s, create)
	appendAttempt := cotTestClaim(t, s, 4000)
	if target == "append" {
		return appendAttempt
	}
	cotTestFinish(t, s, appendAttempt)
	appendCaughtUp := cotTestClaim(t, s, 7000)
	cotTestFinish(t, s, appendCaughtUp)
	terminal := cotTestDocument("completed", true)
	cotTestStage(t, s, 101, terminal, true)
	terminalAttempt := cotTestClaim(t, s, 10000)
	if target == "terminal" {
		return terminalAttempt
	}
	cotTestFinish(t, s, terminalAttempt)
	if target == "complete" {
		return cotTestClaim(t, s, 13000)
	}
	t.Fatalf("unknown CoT stage %q", target)
	return MirrorAttempt{}
}

func cotTestCommandFixture(t *testing.T) (*Store, CommandRequest, time.Time) {
	t.Helper()
	s, req, now := commandStoreRequest(t)
	ctx := context.Background()
	if err := s.ObserveNew(ctx, "host", "thread", "/project", 2); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkEligible(ctx, "host", "thread", "first-input", "/project", "/project"); err != nil {
		t.Fatal(err)
	}
	bindMirrorThread(t, s)
	req.MessageID = "stop-command"
	req.ChatID = "chat"
	req.ChatType = "group"
	req.UserID = "ou_operator"
	req.ThreadID = "thread"
	req.Kind = "stop"
	req.Operators = []string{"ou_operator"}
	req.NotificationTargets = nil
	return s, req, now
}

func TestCOTCreateAppendTerminalCompleteAndSnapshotIdempotence(t *testing.T) {
	ctx := context.Background()
	s, _ := cotTestFixture(t)
	inProgress := cotTestDocument("inProgress", false)
	cotTestStage(t, s, 100, inProgress, false)

	create := cotTestClaim(t, s, 1000)
	if create.COTStep != "create" || create.COTID != "" || create.COTEvents != "[]" || create.COTState != "{}" || create.COTStateBefore != cotStateHash("{}") || create.COTCaughtUp {
		t.Fatalf("invalid frozen create: %+v", create)
	}
	if err := s.FinishMirrorAttempt(ctx, create.ID, "confirmed", cotTestRemoteID); !errors.Is(err, ErrConflict) {
		t.Fatalf("ordinary finish accepted native CoT: %v", err)
	}
	if got, err := s.MirrorAttempt(ctx, create.ID); err != nil || got.State != "dispatching" {
		t.Fatalf("ordinary finish changed create: %+v %v", got, err)
	}
	cotTestFinish(t, s, create)
	m := cotTestMirrorState(t, s)
	if m.RemoteID != cotTestRemoteID || m.DeliveredRevision != 0 || m.State != "ready" {
		t.Fatalf("create confirmation advanced the wrong state: %+v", m)
	}
	cotID, stateJSON, completed := cotTestDelivery(t, s)
	if cotID != cotTestNativeID || stateJSON != create.COTState || completed {
		t.Fatalf("create receipt: id=%q state=%q completed=%v", cotID, stateJSON, completed)
	}

	appendAttempt := cotTestClaim(t, s, 4000)
	if appendAttempt.COTStep != "append" || appendAttempt.COTID != cotTestNativeID || appendAttempt.COTEvents == "" || appendAttempt.COTEvents == create.COTEvents || appendAttempt.COTCaughtUp {
		t.Fatalf("invalid frozen append: %+v", appendAttempt)
	}
	cotTestFinish(t, s, appendAttempt)
	appendCaughtUp := cotTestClaim(t, s, 7000)
	if appendCaughtUp.COTStep != "append" || appendCaughtUp.COTID != cotTestNativeID || appendCaughtUp.COTEvents == "" || !appendCaughtUp.COTCaughtUp {
		t.Fatalf("invalid caught-up append: %+v", appendCaughtUp)
	}
	cotTestFinish(t, s, appendCaughtUp)
	m = cotTestMirrorState(t, s)
	if m.RemoteID != cotTestRemoteID || m.DeliveredRevision != m.Revision || m.State != "confirmed" {
		t.Fatalf("caught-up append did not deliver the revision: %+v", m)
	}

	// A complete scan with the same snapshot must not reopen or resend the
	// already caught-up public progress card.
	cotTestStage(t, s, 101, inProgress, false)
	m = cotTestMirrorState(t, s)
	if m.Revision != 1 || m.DeliveredRevision != 1 || m.State != "confirmed" {
		t.Fatalf("same snapshot changed delivery: %+v", m)
	}
	if _, claimed, err := s.ClaimMirror(ctx, "host", "thread", cotTestKey, m.Revision, 10000); err != nil || claimed {
		t.Fatalf("same snapshot was resent: claimed=%v err=%v", claimed, err)
	}

	terminalDoc := cotTestDocument("completed", true)
	cotTestStage(t, s, 102, terminalDoc, true)
	m = cotTestMirrorState(t, s)
	if m.Revision != 2 || m.DeliveredRevision != 1 || m.State != "ready" {
		t.Fatalf("terminal correction did not reopen one revision: %+v", m)
	}
	terminalAttempt := cotTestClaim(t, s, 13000)
	if terminalAttempt.COTStep != "append" || terminalAttempt.COTID != cotTestNativeID || !terminalAttempt.COTCaughtUp {
		t.Fatalf("invalid terminal append: %+v", terminalAttempt)
	}
	var terminalState cot.State
	if err := json.Unmarshal([]byte(terminalAttempt.COTState), &terminalState); err != nil || !terminalState.Finished {
		t.Fatalf("terminal state was not frozen as finished: %q %v", terminalAttempt.COTState, err)
	}
	cotTestFinish(t, s, terminalAttempt)
	m = cotTestMirrorState(t, s)
	if m.DeliveredRevision != 1 || m.State != "ready" {
		t.Fatalf("terminal append delivered before complete: %+v", m)
	}

	complete := cotTestClaim(t, s, 16000)
	if complete.COTStep != "complete" || complete.COTID != cotTestNativeID || !complete.COTCaughtUp || complete.COTEvents != "[]" {
		t.Fatalf("invalid frozen complete: %+v", complete)
	}
	cotTestFinish(t, s, complete)
	m = cotTestMirrorState(t, s)
	if m.DeliveredRevision != 2 || m.State != "confirmed" || m.RemoteID != cotTestRemoteID {
		t.Fatalf("complete did not deliver terminal revision: %+v", m)
	}
	cotID, stateJSON, completed = cotTestDelivery(t, s)
	if cotID != cotTestNativeID || stateJSON != complete.COTState || !completed {
		t.Fatalf("complete receipt: id=%q state=%q completed=%v", cotID, stateJSON, completed)
	}

	// Once complete, a corrected terminal projection remains closed and does
	// not create a new revision or reopen the native COT lifecycle.
	corrected := cotTestDocument("failed", true)
	cotTestStage(t, s, 103, corrected, true)
	m = cotTestMirrorState(t, s)
	if m.Card != cotTestCard(t, terminalDoc) || m.Revision != 2 || m.DeliveredRevision != 2 || m.State != "confirmed" {
		t.Fatalf("completed CoT reopened after correction: %+v", m)
	}
	if _, claimed, err := s.ClaimMirror(ctx, "host", "thread", cotTestKey, m.Revision, 17000); err != nil || claimed {
		t.Fatalf("completed CoT was claimed after correction: claimed=%v err=%v", claimed, err)
	}
}

func TestCOTSnapshotCannotChangePresentationVersion(t *testing.T) {
	for _, version := range []int{cot.LegacyVersion, cot.Version} {
		for _, confirmed := range []bool{false, true} {
			s, _ := cotTestFixture(t)
			doc := cotTestSmallDocument("inProgress", "synthetic action", "inProgress")
			doc.Version = version
			cotTestStage(t, s, 100, doc, false)
			if confirmed {
				cotTestFinish(t, s, cotTestClaim(t, s, 1000))
				cotTestFinish(t, s, cotTestClaim(t, s, 4000))
			}
			before := cotTestMirrorState(t, s)
			doc.Version = cot.LegacyVersion
			if version == cot.LegacyVersion {
				doc.Version = cot.Version
			}
			err := s.StageMirrors(context.Background(), "host", "thread", []Mirror{cotTestMirror(t, doc, false)}, Watermark{TurnID: "turn", ItemID: "tool-a", ObservedAtMS: 101})
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("version %d changed (confirmed=%v): %v", version, confirmed, err)
			}
			after := cotTestMirrorState(t, s)
			if after.Card != before.Card || after.Revision != before.Revision {
				t.Fatal("rejected presentation change mutated the persisted snapshot")
			}
		}
	}
}

func TestCOTUnknownAtEveryStageSurvivesRestartWithoutReclaim(t *testing.T) {
	for _, stageName := range []string{"create", "append", "terminal", "complete"} {
		t.Run(stageName, func(t *testing.T) {
			ctx := context.Background()
			s, path := cotTestFixture(t)
			a := cotTestPrepareUnknownStage(t, s, stageName)
			beforeID, beforeState, beforeCompleted := cotTestDelivery(t, s)
			if err := s.FinishCOTAttempt(ctx, a.ID, "unknown", "", ""); err != nil {
				t.Fatal(err)
			}
			frozen, err := s.MirrorAttempt(ctx, a.ID)
			if err != nil || frozen.State != "unknown" {
				t.Fatalf("unknown attempt not frozen: %+v %v", frozen, err)
			}
			m := cotTestMirrorState(t, s)
			if m.State != "unknown" {
				t.Fatalf("unknown attempt changed mirror to %q", m.State)
			}
			gotID, gotState, gotCompleted := cotTestDelivery(t, s)
			if gotID != beforeID || gotState != beforeState || gotCompleted != beforeCompleted {
				t.Fatalf("unknown advanced CoT receipt: before=%q/%q/%v after=%q/%q/%v", beforeID, beforeState, beforeCompleted, gotID, gotState, gotCompleted)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err := s.RecoverMirrorDispatches(ctx, "host"); err != nil {
				t.Fatal(err)
			}
			m = cotTestMirrorState(t, s)
			if m.State != "unknown" {
				t.Fatalf("restart changed unknown state to %q", m.State)
			}
			if _, claimed, err := s.ClaimMirror(ctx, "host", "thread", cotTestKey, m.Revision, a.CreatedAtMS+10000); err != nil || claimed {
				t.Fatalf("unknown CoT was reclaimed after restart: claimed=%v err=%v", claimed, err)
			}
			frozen, err = s.MirrorAttempt(ctx, a.ID)
			if err != nil || frozen.State != "unknown" {
				t.Fatalf("restart changed attempt: %+v %v", frozen, err)
			}
		})
	}
}

func TestCOTConfirmationRejectsBadIDAndStateHashAtomically(t *testing.T) {
	t.Run("cot id", func(t *testing.T) {
		s, _ := cotTestFixture(t)
		doc := cotTestDocument("inProgress", false)
		cotTestStage(t, s, 100, doc, false)
		create := cotTestClaim(t, s, 1000)
		cotTestFinish(t, s, create)
		a := cotTestClaim(t, s, 4000)
		beforeID, beforeState, beforeCompleted := cotTestDelivery(t, s)
		if err := s.FinishCOTAttempt(context.Background(), a.ID, "confirmed", cotTestRemoteID, "wrong-cot"); !errors.Is(err, ErrConflict) {
			t.Fatalf("wrong CoT ID accepted: %v", err)
		}
		got, err := s.MirrorAttempt(context.Background(), a.ID)
		if err != nil || got.State != "dispatching" || got.RemoteID != "" {
			t.Fatalf("wrong CoT ID changed attempt: %+v %v", got, err)
		}
		m := cotTestMirrorState(t, s)
		if m.State != "dispatching" || m.RemoteID != cotTestRemoteID || m.DeliveredRevision != 0 {
			t.Fatalf("wrong CoT ID changed mirror: %+v", m)
		}
		cotID, stateJSON, completed := cotTestDelivery(t, s)
		if cotID != beforeID || stateJSON != beforeState || completed != beforeCompleted {
			t.Fatalf("wrong CoT ID changed receipt: before=%q/%q/%v after=%q/%q/%v", beforeID, beforeState, beforeCompleted, cotID, stateJSON, completed)
		}
		cotTestFinish(t, s, a)
	})

	t.Run("state hash", func(t *testing.T) {
		s, _ := cotTestFixture(t)
		doc := cotTestDocument("inProgress", false)
		cotTestStage(t, s, 100, doc, false)
		a := cotTestClaim(t, s, 1000)
		if _, err := s.DB.Exec(`UPDATE outbound_attempts SET cot_state_before='tampered' WHERE attempt_id=?`, a.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishCOTAttempt(context.Background(), a.ID, "confirmed", cotTestRemoteID, cotTestNativeID); !errors.Is(err, ErrConflict) {
			t.Fatalf("bad state hash accepted: %v", err)
		}
		got, err := s.MirrorAttempt(context.Background(), a.ID)
		if err != nil || got.State != "dispatching" || got.RemoteID != "" {
			t.Fatalf("bad state hash changed attempt: %+v %v", got, err)
		}
		m := cotTestMirrorState(t, s)
		if m.State != "dispatching" || m.RemoteID != "" || m.DeliveredRevision != 0 {
			t.Fatalf("bad state hash changed mirror: %+v", m)
		}
		if _, err := s.DB.Exec(`UPDATE outbound_attempts SET cot_state_before=? WHERE attempt_id=?`, cotStateHash("{}"), a.ID); err != nil {
			t.Fatal(err)
		}
		cotTestFinish(t, s, a)
	})
}

func TestCOTSchemaMigrationPreservesLegacyCard(t *testing.T) {
	s, path := discoveryStore(t)
	eligible(t, s, "thread")
	legacy := mirrorDoc("legacy", "progress", "legacy card")
	legacy.ParentKey = ""
	legacy.ParentMessageID = cotTestInputID
	stage(t, s, 100, legacy)
	if _, err := s.DB.Exec(`DROP TABLE cot_deliveries;
ALTER TABLE outbound_attempts DROP COLUMN cot_caught_up;
ALTER TABLE outbound_attempts DROP COLUMN cot_state_before;
ALTER TABLE outbound_attempts DROP COLUMN cot_state_json;
ALTER TABLE outbound_attempts DROP COLUMN cot_events_json;
ALTER TABLE outbound_attempts DROP COLUMN cot_id;
ALTER TABLE outbound_attempts DROP COLUMN cot_step;
PRAGMA user_version=30;`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.Mirror(context.Background(), "host", "thread", legacy.Key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Card != legacy.Card || got.Revision != 1 || got.DeliveredRevision != 0 {
		t.Fatalf("legacy card changed during CoT migration: %+v", got)
	}
	var version int
	if err := reopened.DB.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("schema version after CoT migration: %d %v", version, err)
	}
	var deliveries int
	if err := reopened.DB.QueryRow(`SELECT COUNT(*) FROM cot_deliveries`).Scan(&deliveries); err != nil || deliveries != 0 {
		t.Fatalf("unexpected CoT receipt for legacy card: %d %v", deliveries, err)
	}
}

func TestCOTStopCommandFreezesUniqueTurnAndDeduplicates(t *testing.T) {
	ctx := context.Background()
	s, req, now := cotTestCommandFixture(t)
	inProgress := cotTestDocument("inProgress", false)
	cotTestStage(t, s, 100, inProgress, false)

	reply, fresh, err := s.ApplyLocalCommand(ctx, req, now)
	if err != nil || !fresh {
		t.Fatalf("unique CoT stop: fresh=%v err=%v", fresh, err)
	}
	if reply.Card != "" || reply.State != "confirmed" || reply.RemoteID != "" {
		t.Fatal("accepted CoT stop should not queue a separate reply card")
	}
	var expected string
	var actionState string
	if err := s.DB.QueryRow(`SELECT expected_turn_id,state FROM submission_actions WHERE host_id=? AND app_id=? AND thread_id=? AND kind='stop'`, req.HostID, req.AppID, req.ThreadID).Scan(&expected, &actionState); err != nil {
		t.Fatal(err)
	}
	if expected != inProgress.TurnID || actionState != "ready" {
		t.Fatalf("stop target was not frozen: %q", expected)
	}
	if err := s.RecoverCommandDispatches(ctx, req.HostID); err != nil {
		t.Fatal(err)
	}
	if candidates, err := s.CommandReplyCandidates(ctx, req.HostID, now.Add(time.Second).UnixMilli(), 64); err != nil || len(candidates) != 0 {
		t.Fatal("silent stop became a reply candidate after recovery", err)
	}
	if _, claimed, err := s.ClaimCommandReply(ctx, req.HostID, now.Add(time.Second).UnixMilli()); err != nil || claimed {
		t.Fatal("silent stop claimed a remote reply attempt", err)
	}
	var actions int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM submission_actions WHERE host_id=? AND app_id=? AND thread_id=?`, req.HostID, req.AppID, req.ThreadID).Scan(&actions); err != nil || actions != 1 {
		t.Fatalf("initial stop action count: %d %v", actions, err)
	}
	replay, fresh, err := s.ApplyLocalCommand(ctx, req, now.Add(time.Second))
	if err != nil || fresh || replay != reply {
		t.Fatalf("duplicate stop was reapplied: fresh=%v reply=%+v err=%v", fresh, replay, err)
	}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM submission_actions WHERE host_id=? AND app_id=? AND thread_id=?`, req.HostID, req.AppID, req.ThreadID).Scan(&actions); err != nil || actions != 1 {
		t.Fatalf("duplicate stop added an action: %d %v", actions, err)
	}
}

func TestCOTStopCommandKeepsFeedbackWhenControlIsBlocked(t *testing.T) {
	ctx := context.Background()
	s, req, now := cotTestCommandFixture(t)
	cotTestStage(t, s, 100, cotTestDocument("inProgress", false), false)
	if _, err := s.DB.Exec(`INSERT INTO submission_actions(action_id,host_id,app_id,thread_id,token,revision,kind,stage,state,created_at_ms) VALUES('unresolved','host','app','thread','earlier-stop',0,'stop','interrupt','unknown',1)`); err != nil {
		t.Fatal(err)
	}
	reply, fresh, err := s.ApplyLocalCommand(ctx, req, now)
	if err != nil || !fresh || reply.State != "ready" || !strings.Contains(reply.Card, "正在核验") {
		t.Fatal("blocked CoT stop hid its actionable feedback", err)
	}
	var actions int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM submission_actions`).Scan(&actions); err != nil || actions != 1 {
		t.Fatal("blocked stop created another native action", err)
	}
}

func TestCOTStopCommandIgnoresTerminalTurnWithUncompletedDelivery(t *testing.T) {
	for _, outcome := range []string{"pending", "rejected", "unknown"} {
		for _, ambiguous := range []bool{false, true} {
			name := outcome + "/unique-active"
			if ambiguous {
				name = outcome + "/ambiguous-active"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				s, req, now := cotTestCommandFixture(t)
				old := cotTestSmallDocument("inProgress", "synthetic old tool", "inProgress")
				cotTestStage(t, s, 100, old, false)
				cotTestFinish(t, s, cotTestClaim(t, s, 1000))
				cotTestFinish(t, s, cotTestClaim(t, s, 4000))

				// The native stop and its terminal public events are known even
				// when the separate Feishu Complete operation has not succeeded.
				old.Status = "interrupted"
				cotTestStage(t, s, 101, old, true)
				terminal := cotTestClaim(t, s, 7000)
				if terminal.COTStep != "append" || !strings.Contains(terminal.COTEvents, "RUN_FINISHED") || !strings.Contains(terminal.COTEvents, "interrupted") {
					t.Fatal("old turn did not append its interrupted terminal events")
				}
				cotTestFinish(t, s, terminal)
				if outcome != "pending" {
					complete := cotTestClaim(t, s, 10000)
					if complete.COTStep != "complete" {
						t.Fatalf("expected a separate Complete attempt, got %q", complete.COTStep)
					}
					if err := s.FinishCOTAttempt(ctx, complete.ID, outcome, "", ""); err != nil {
						t.Fatal(err)
					}
				}
				before := cotTestMirrorState(t, s)
				beforeID, beforeState, beforeCompleted := cotTestDelivery(t, s)
				if !before.Terminal || before.RemoteID == "" || beforeID == "" || beforeCompleted {
					t.Fatal("fixture lost the known terminal turn or its unfinished delivery")
				}
				active := cotTestSmallDocument("inProgress", "synthetic current tool", "inProgress")
				active.TurnID = "current-turn"
				cotTestStageKey(t, s, 102, "cot-current", active, false)
				if ambiguous {
					other := active
					other.TurnID = "other-active-turn"
					cotTestStageKey(t, s, 103, "cot-other-active", other, false)
				}

				reply, fresh, err := s.ApplyLocalCommand(ctx, req, now)
				if err != nil || !fresh {
					t.Fatalf("stop command: fresh=%v err=%v", fresh, err)
				}
				var actions int
				if err := s.DB.QueryRow(`SELECT COUNT(*) FROM submission_actions WHERE host_id=? AND app_id=? AND thread_id=?`, req.HostID, req.AppID, req.ThreadID).Scan(&actions); err != nil {
					t.Fatal(err)
				}
				if ambiguous {
					if actions != 0 || !strings.Contains(reply.Card, "未提交停止") {
						t.Fatal("two nonterminal candidates did not fail closed")
					}
				} else {
					if actions != 1 || reply.Card != "" || reply.State != "confirmed" {
						t.Fatalf("terminal CoT delivery blocked the unique active turn: actions=%d", actions)
					}
					var expected, state string
					if err := s.DB.QueryRow(`SELECT expected_turn_id,state FROM submission_actions WHERE host_id=? AND app_id=? AND thread_id=? AND kind='stop'`, req.HostID, req.AppID, req.ThreadID).Scan(&expected, &state); err != nil {
						t.Fatal(err)
					}
					if expected != active.TurnID || state != "ready" {
						t.Fatal("stop did not freeze the unique current native turn")
					}
				}
				after := cotTestMirrorState(t, s)
				afterID, afterState, afterCompleted := cotTestDelivery(t, s)
				if after.State != before.State || after.RemoteID != before.RemoteID || after.Revision != before.Revision || after.DeliveredRevision != before.DeliveredRevision || afterID != beforeID || afterState != beforeState || afterCompleted != beforeCompleted {
					t.Fatal("stop command changed the old CoT delivery evidence")
				}
			})
		}
	}
}

func TestCOTStopCommandRejectsAmbiguousOrMissingActiveTurn(t *testing.T) {
	for _, tc := range []struct {
		name string
		docs []Mirror
	}{
		{
			name: "multiple unfinished with mixed presentation versions",
			docs: func() []Mirror {
				firstDoc := cotTestDocument("inProgress", false)
				firstDoc.Version = cot.LegacyVersion
				first := cotTestMirror(t, firstDoc, false)
				first.Key = "cot-old"
				secondDoc := cotTestDocument("inProgress", false)
				secondDoc.TurnID = "second-turn"
				second := cotTestMirror(t, secondDoc, false)
				second.Key = "cot-current"
				return []Mirror{first, second}
			}(),
		},
		{
			name: "unfinished terminal only",
			docs: func() []Mirror {
				oldDoc := cotTestDocument("completed", true)
				oldDoc.TurnID = "old-turn"
				old := cotTestMirror(t, oldDoc, true)
				old.Key = "cot-old-terminal"
				return []Mirror{old}
			}(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, req, now := cotTestCommandFixture(t)
			if err := s.StageMirrors(ctx, "host", "thread", tc.docs, Watermark{TurnID: "turn", ItemID: "last", ObservedAtMS: 100}); err != nil {
				t.Fatal(err)
			}
			reply, fresh, err := s.ApplyLocalCommand(ctx, req, now)
			if err != nil || !fresh || !strings.Contains(reply.Card, "未提交停止") {
				t.Fatalf("ambiguous stop did not return feedback: fresh=%v card=%q err=%v", fresh, reply.Card, err)
			}
			var actions int
			if err := s.DB.QueryRow(`SELECT COUNT(*) FROM submission_actions WHERE host_id=? AND app_id=? AND thread_id=?`, req.HostID, req.AppID, req.ThreadID).Scan(&actions); err != nil || actions != 0 {
				t.Fatalf("ambiguous stop created action: %d %v", actions, err)
			}
		})
	}
}

func TestCOTStopCommandRejectsNonOperatorAndLegacyKeepsEmptyTarget(t *testing.T) {
	t.Run("nonoperator", func(t *testing.T) {
		s, req, now := cotTestCommandFixture(t)
		req.MessageID = "stop-stranger"
		req.UserID = "ou_stranger"
		if _, fresh, err := s.ApplyLocalCommand(context.Background(), req, now); !errors.Is(err, ErrConflict) || fresh {
			t.Fatalf("non-operator stop accepted: fresh=%v err=%v", fresh, err)
		}
		var replies, actions int
		if err := s.DB.QueryRow(`SELECT COUNT(*) FROM command_replies`).Scan(&replies); err != nil || replies != 0 {
			t.Fatalf("non-operator command journalled: %d %v", replies, err)
		}
		if err := s.DB.QueryRow(`SELECT COUNT(*) FROM submission_actions`).Scan(&actions); err != nil || actions != 0 {
			t.Fatalf("non-operator action journalled: %d %v", actions, err)
		}
	})

	t.Run("legacy", func(t *testing.T) {
		s, req, now := cotTestCommandFixture(t)
		legacy := mirrorDoc("legacy-progress", "progress", "legacy public progress")
		legacy.ParentKey = ""
		legacy.ParentMessageID = cotTestInputID
		stage(t, s, 100, legacy)
		if _, fresh, err := s.ApplyLocalCommand(context.Background(), req, now); err != nil || !fresh {
			t.Fatalf("legacy stop: fresh=%v err=%v", fresh, err)
		}
		var expected string
		if err := s.DB.QueryRow(`SELECT expected_turn_id FROM submission_actions WHERE host_id=? AND app_id=? AND thread_id=? AND kind='stop'`, req.HostID, req.AppID, req.ThreadID).Scan(&expected); err != nil {
			t.Fatal(err)
		}
		if expected != "" {
			t.Fatalf("legacy stop invented CoT target %q", expected)
		}
	})
}

func TestCOTStopIgnoresRejectedNeverCreatedTurnButUnknownStillBlocks(t *testing.T) {
	for _, tc := range []struct {
		name       string
		outcome    string
		wantAction bool
		wantTurn   string
	}{
		{name: "rejected create", outcome: "rejected", wantAction: true, wantTurn: "turn-2"},
		{name: "unknown create", outcome: "unknown", wantAction: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, req, now := cotTestCommandFixture(t)
			t1 := cotTestDocument("inProgress", false)
			cotTestStageKey(t, s, 100, "cot-t1", t1, false)
			t1Attempt := cotTestClaimKey(t, s, "cot-t1", 1000)
			if t1Attempt.COTStep != "create" {
				t.Fatalf("first turn was not a create: %+v", t1Attempt)
			}
			if err := s.FinishCOTAttempt(ctx, t1Attempt.ID, tc.outcome, "", ""); err != nil {
				t.Fatal(err)
			}
			old, err := s.Mirror(ctx, "host", "thread", "cot-t1")
			if err != nil {
				t.Fatal(err)
			}
			if old.RemoteID != "" {
				t.Fatalf("%s create acquired a remote message: %+v", tc.outcome, old)
			}
			var oldCOTID string
			if err := s.DB.QueryRow(`SELECT cot_id FROM cot_deliveries WHERE host_id='host' AND thread_id='thread' AND mirror_key='cot-t1'`).Scan(&oldCOTID); err != nil {
				t.Fatal(err)
			}
			if oldCOTID != "" {
				t.Fatalf("%s create acquired a native identity: %q", tc.outcome, oldCOTID)
			}

			t2 := cotTestDocument("inProgress", false)
			t2.TurnID = "turn-2"
			cotTestStageKey(t, s, 101, "cot-t2", t2, false)
			req.MessageID = "stop-" + tc.outcome
			reply, fresh, err := s.ApplyLocalCommand(ctx, req, now)
			if err != nil || !fresh {
				t.Fatalf("stop after %s create: fresh=%v err=%v", tc.outcome, fresh, err)
			}
			var actionCount int
			if err := s.DB.QueryRow(`SELECT COUNT(*) FROM submission_actions WHERE host_id=? AND app_id=? AND thread_id=?`, req.HostID, req.AppID, req.ThreadID).Scan(&actionCount); err != nil {
				t.Fatal(err)
			}
			if tc.wantAction {
				if actionCount != 1 {
					t.Fatalf("rejected never-created turn blocked active turn: actions=%d card=%q", actionCount, reply.Card)
				}
				var expected string
				if err := s.DB.QueryRow(`SELECT expected_turn_id FROM submission_actions WHERE host_id=? AND app_id=? AND thread_id=? AND kind='stop'`, req.HostID, req.AppID, req.ThreadID).Scan(&expected); err != nil {
					t.Fatal(err)
				}
				if expected != tc.wantTurn {
					t.Fatalf("stop targeted the rejected turn: %q, want %q", expected, tc.wantTurn)
				}
			} else if actionCount != 0 {
				t.Fatalf("unknown prior turn was treated as ended: actions=%d card=%q", actionCount, reply.Card)
			}
		})
	}
}

func TestCOTTerminalAckFreezesAttemptDocumentBeforeComplete(t *testing.T) {
	ctx := context.Background()
	s, _ := cotTestFixture(t)
	inProgress := cotTestSmallDocument("inProgress", "working", "inProgress")
	cotTestStage(t, s, 100, inProgress, false)
	create := cotTestClaim(t, s, 1000)
	cotTestFinish(t, s, create)
	appendAttempt := cotTestClaim(t, s, 4000)
	if appendAttempt.COTStep != "append" || !appendAttempt.COTCaughtUp {
		t.Fatalf("small progress did not catch up in one append: %+v", appendAttempt)
	}
	cotTestFinish(t, s, appendAttempt)

	terminalDoc := cotTestSmallDocument("completed", "sent terminal", "completed")
	terminalCard := cotTestCard(t, terminalDoc)
	cotTestStage(t, s, 101, terminalDoc, true)
	terminalAttempt := cotTestClaim(t, s, 7000)
	if terminalAttempt.COTStep != "append" || !terminalAttempt.COTCaughtUp {
		t.Fatalf("small terminal did not catch up in one append: %+v", terminalAttempt)
	}
	var terminalState cot.State
	if err := json.Unmarshal([]byte(terminalAttempt.COTState), &terminalState); err != nil || !terminalState.Finished {
		t.Fatalf("terminal append did not freeze Finished=true: %q %v", terminalAttempt.COTState, err)
	}

	// A newer public terminal snapshot can arrive while the terminal batch is
	// in flight. The ACK must later restore the exact document whose events it
	// acknowledged, rather than completing the newer unseen projection.
	unseenDoc := cotTestSmallDocument("failed", "unseen failure", "failed")
	unseenCard := cotTestCard(t, unseenDoc)
	cotTestStage(t, s, 102, unseenDoc, true)
	m := cotTestMirrorState(t, s)
	if m.Card != unseenCard || m.Revision != terminalAttempt.Revision+1 || m.State != "dispatching" {
		t.Fatalf("in-flight terminal snapshot was not staged: %+v", m)
	}
	if err := s.FinishCOTAttempt(ctx, terminalAttempt.ID, "confirmed", cotTestRemoteID, cotTestNativeID); err != nil {
		t.Fatal(err)
	}
	m = cotTestMirrorState(t, s)
	if m.Card != terminalCard || m.Revision != terminalAttempt.Revision+2 || m.DeliveredRevision != appendAttempt.Revision || m.State != "ready" {
		t.Fatalf("terminal ACK did not restore its frozen document: %+v", m)
	}

	// Finished=true closes the projection before Complete. A later failed
	// status must not become the document that Complete claims to have sent.
	laterDoc := cotTestSmallDocument("failed", "later failure", "failed")
	cotTestStage(t, s, 103, laterDoc, true)
	m = cotTestMirrorState(t, s)
	if m.Card != terminalCard || m.Revision != terminalAttempt.Revision+2 || m.State != "ready" {
		t.Fatalf("Finished=true projection reopened before Complete: %+v", m)
	}
	complete := cotTestClaim(t, s, 10000)
	if complete.COTStep != "complete" || complete.Card != terminalCard || complete.Revision != m.Revision {
		t.Fatalf("Complete borrowed an unseen terminal projection: %+v", complete)
	}
	cotTestFinish(t, s, complete)
	m = cotTestMirrorState(t, s)
	if m.Card != terminalCard || m.Revision != complete.Revision || m.DeliveredRevision != complete.Revision || m.State != "confirmed" {
		t.Fatalf("Complete did not freeze the acknowledged terminal document: %+v", m)
	}
	var completed int
	if err := s.DB.QueryRow(`SELECT completed FROM cot_deliveries WHERE host_id='host' AND thread_id='thread' AND mirror_key=?`, cotTestKey).Scan(&completed); err != nil || completed != 1 {
		t.Fatalf("CoT completion receipt: completed=%d err=%v", completed, err)
	}
}
