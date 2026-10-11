package store

import (
	"context"
	"errors"
	"testing"
)

func TestDisconnectedCardsKeepNativeFactsAndFreezeAmbiguousDelivery(t *testing.T) {
	for _, outcome := range []string{"confirmed", "unknown", "rejected"} {
		t.Run(outcome, func(t *testing.T) {
			s, _, scope, receipt := submissionStore(t)
			ctx := context.Background()
			mark := Watermark{TurnID: "native-turn", ItemID: "native-item", ObservedAtMS: 1234}
			if err := s.StageMirrors(ctx, scope.HostID, scope.ThreadID, nil, mark); err != nil {
				t.Fatal(err)
			}
			if err := s.ObserveSubmission(ctx, scope, receipt.ID, "queued", "native-q", 2, "", ""); err != nil {
				t.Fatal(err)
			}
			if err := s.StageQueueCard(ctx, scope, receipt.ID); err != nil {
				t.Fatal(err)
			}
			before, err := s.Mirror(ctx, scope.HostID, scope.ThreadID, QueueCardKey(receipt.ID))
			if err != nil || !cardHasCallback(before.Card) {
				t.Fatal("queue controls missing", err)
			}
			a, claimed, err := s.ClaimMirror(ctx, scope.HostID, scope.ThreadID, before.Key, before.Revision, 5000)
			if err != nil || !claimed {
				t.Fatal("queue delivery unclaimed", err)
			}
			remote := ""
			if outcome == "confirmed" {
				remote = "original-card"
			}
			if err := s.FinishMirrorAttempt(ctx, a.ID, outcome, remote); err != nil {
				t.Fatal(err)
			}
			if err := s.StageDisconnectedCards(ctx, scope, "Codex 连接已断开"); err != nil {
				t.Fatal(err)
			}
			after, err := s.Mirror(ctx, scope.HostID, scope.ThreadID, before.Key)
			if err != nil || cardHasCallback(after.Card) || after.RemoteID != remote || after.ParentMessageID != receipt.MessageID || after.TurnID != before.TurnID || after.Terminal != before.Terminal {
				t.Fatal("offline overlay changed identity or retained controls", err)
			}
			wantState := outcome
			if outcome == "confirmed" {
				wantState = "ready"
			}
			if after.State != wantState {
				t.Fatal("offline overlay retried uncertain delivery")
			}
			got, err := s.Watermark(ctx, scope.HostID, scope.ThreadID)
			if err != nil || got != mark {
				t.Fatal("offline UI advanced native history", err)
			}
			ns, err := s.Submissions(ctx, scope)
			if err != nil || len(ns) != 1 || ns[0].State != "queued" || ns[0].QueueID != "native-q" {
				t.Fatal("offline UI fabricated input/native state", err)
			}
			foreign := scope
			foreign.AppID = "foreign"
			if err := s.StageDisconnectedCards(ctx, foreign, "foreign"); !errors.Is(err, ErrConflict) {
				t.Fatal("foreign application changed UI", err)
			}
			if err := s.StageQueueCard(ctx, scope, receipt.ID); err != nil {
				t.Fatal(err)
			}
			fresh, err := s.Mirror(ctx, scope.HostID, scope.ThreadID, before.Key)
			if err != nil || !cardHasCallback(fresh.Card) || fresh.State != wantState || fresh.RemoteID != remote {
				t.Fatal("reconciled UI lost original journal", err)
			}
		})
	}
}

func TestDisconnectedCardsLeaveCompletedProgressAndFinalAlone(t *testing.T) {
	s, _, scope, receipt := submissionStore(t)
	ctx := context.Background()
	progress := Mirror{Key: "progress", TurnID: "native-turn", Kind: "progress", Card: `{"body":"finished"}`, Terminal: true, ParentMessageID: receipt.MessageID}
	final := progress
	final.Key, final.Kind = "final", "final"
	if err := s.StageMirrors(ctx, scope.HostID, scope.ThreadID, []Mirror{progress, final}, Watermark{ObservedAtMS: 1234}); err != nil {
		t.Fatal(err)
	}
	if err := s.StageDisconnectedCards(ctx, scope, "disconnected"); err != nil {
		t.Fatal(err)
	}
	for _, before := range []Mirror{progress, final} {
		after, err := s.Mirror(ctx, scope.HostID, scope.ThreadID, before.Key)
		if err != nil || after.Card != before.Card || after.Revision != 1 {
			t.Fatal("offline overlay rewrote a completed result", err)
		}
	}
}
