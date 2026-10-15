package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"codex-feishu/internal/rules"
)

func TestCommandFollowTargetFrozenForTwoTopicsInOneChat(t *testing.T) {
	s, _ := discoveryStore(t)
	ctx := context.Background()
	a := topicBinding(t, s, "one", "root-one")
	b := topicBinding(t, s, "two", "root-two")
	for _, binding := range []Binding{a, b} {
		req := CommandRequest{HostID: "host", AppID: "app", ThreadID: binding.ThreadID, ChatID: binding.ChatID, ChatType: "group", MessageID: "command-" + binding.ThreadID, UserID: "operator", Operators: []string{"operator"}, Kind: "help"}
		if _, _, err := s.ApplyLocalCommand(ctx, req, time.Unix(200, 0)); err != nil {
			t.Fatal(err)
		}
		claimed, ok, err := s.ClaimCommandReply(ctx, "host", 200000)
		if err != nil || !ok {
			t.Fatal(claimed, ok, err)
		}
		target, err := s.FollowAttemptTarget(ctx, claimed.ID)
		if err != nil || target.FollowMode != "topic" || target.ThreadID != binding.ThreadID || target.RootMessageID != binding.RootMessageID || target.ParentMessageID != req.MessageID {
			t.Fatal("command lost frozen topic", target, err)
		}
		if err = s.FinishCommandReply(ctx, claimed.ID, "confirmed", "remote-"+binding.ThreadID); err != nil {
			t.Fatal(err)
		}
	}
}
func TestTopicFollowClaimRejectsUnknownParentAndMissingFreezeRollsBack(t *testing.T) {
	s, _ := discoveryStore(t)
	ctx := context.Background()
	b := topicBinding(t, s, "one", "root-one")
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = freezeFollowAttemptTx(ctx, tx, "attempt", b.HostID, b.ThreadID, b.ChatID, "other-topic-input"); !errors.Is(err, ErrConflict) {
		t.Fatal("unproven parent accepted", err)
	}
	tx.Rollback()
	s2, req, now := commandStoreRequest(t)
	r, _, err := s2.ApplyLocalCommand(ctx, req, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s2.DB.Exec(`CREATE TRIGGER ignore_follow_freeze BEFORE INSERT ON frozen_follow_attempts BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s2.ClaimCommandReply(ctx, "host", now.UnixMilli()); ok || !errors.Is(err, ErrConflict) {
		t.Fatal("unfrozen claim escaped", ok, err)
	}
	current, err := s2.CommandReply(ctx, "host", "app", r.MessageID)
	if err != nil || current.State != "ready" {
		t.Fatal("failed freeze left claim consumed", current, err)
	}
	var n int
	if err = s2.DB.QueryRow(`SELECT COUNT(*) FROM command_reply_attempts`).Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}
func TestRequestReminderTopicFreezesKnownOriginalParent(t *testing.T) {
	ctx := context.Background()
	event := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	p, _ := rules.DefaultPolicy("UTC")
	s, _, options, r := requestFixture(t, p, event)
	observeRequest(t, s, r, options, event)
	bindMirrorThread(t, s)
	if _, err := s.DB.Exec(`UPDATE bindings SET follow_mode='topic',root_message_id='root',platform_thread_id='platform'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Claim(ctx, Receipt{AppID: "app", ThreadID: "thread", ChatID: "chat", MessageID: "original-input"}); err != nil {
		t.Fatal(err)
	}
	a := claimRequest(t, s, options, event.Add(time.Second))
	target, err := s.FollowAttemptTarget(ctx, a.ID)
	if err != nil || target.ParentMessageID != "original-input" || a.ParentMessageID != target.ParentMessageID || target.FollowMode != "topic" || target.RootMessageID != "root" {
		t.Fatal(target, a, err)
	}
	if err = s.FinishRequestReminder(ctx, a.ID, "unknown", ""); err != nil {
		t.Fatal(err)
	}
	proof, ok, err := s.ClaimCardReadback(ctx, "reminder", "host", "app", "thread", event.Add(2*time.Second).UnixMilli(), 30000)
	if err != nil || !ok || proof.Target != target {
		t.Fatal("readback target drifted", proof, ok, err)
	}
}
func TestReceivedFeedbackBindingRetainsTopicAndRepairMetadata(t *testing.T) {
	s, _ := discoveryStore(t)
	ctx := context.Background()
	want := topicBinding(t, s, "one", "root-one")
	if _, _, err := s.Claim(ctx, Receipt{AppID: "app", ThreadID: want.ThreadID, ChatID: want.ChatID, MessageID: "received"}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkBindingRepair(ctx, "host", "one", "root_deleted"); err != nil {
		t.Fatal(err)
	}
	want.RepairRequired, want.RepairReason = true, "root_deleted"
	got, err := s.ReceivedFeedbackBindingsAt(ctx, "host", "app", 1000)
	if err != nil || len(got) != 1 || got[0] != want {
		t.Fatal("feedback binding dropped target", got, err)
	}
}
