package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func goalDirectMirror(key, kind, body string, terminal bool) Mirror {
	return Mirror{
		Key:      key,
		TurnID:   "goal-turn",
		Kind:     kind,
		Card:     `{"body":"` + body + `"}`,
		Direct:   true,
		Terminal: terminal,
	}
}

func TestGoalDirectMirrorsPersistUpdateAndReplayWithoutDuplicate(t *testing.T) {
	ctx := context.Background()
	s, path := discoveryStore(t)
	eligible(t, s, "thread")
	progress := goalDirectMirror("goal-progress", "progress", "working", false)
	final := goalDirectMirror("goal-final", "final", "done", true)
	stage(t, s, 100, progress, final)

	for _, key := range []string{progress.Key, final.Key} {
		got, err := s.Mirror(ctx, "host", "thread", key)
		if err != nil || !got.Direct || got.ParentKey != "" || got.ParentMessageID != "" {
			t.Fatalf("direct identity was not readable before restart: %+v %v", got, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, key := range []string{progress.Key, final.Key} {
		got, err := s.Mirror(ctx, "host", "thread", key)
		if err != nil || !got.Direct || got.ParentKey != "" || got.ParentMessageID != "" {
			t.Fatalf("direct identity was not durable across restart: %+v %v", got, err)
		}
	}

	bindMirrorThread(t, s)
	first := claimMirror(t, s, progress.Key, 1000)
	if first.Operation != "send" || first.ParentMessageID != "" || first.TargetMessageID != "" {
		t.Fatalf("direct progress did not claim as a send: %+v", first)
	}
	if err := s.FinishMirrorAttempt(ctx, first.ID, "confirmed", "progress-card"); err != nil {
		t.Fatal(err)
	}

	progress.Card = `{"body":"updated"}`
	stage(t, s, 101, progress, final)
	update := claimMirror(t, s, progress.Key, 3000)
	if update.Operation != "update" || update.TargetMessageID != "progress-card" || update.ParentMessageID != "" || update.Card != progress.Card {
		t.Fatalf("direct progress did not update its original card: %+v", update)
	}
	if err := s.FinishMirrorAttempt(ctx, update.ID, "confirmed", "progress-card"); err != nil {
		t.Fatal(err)
	}

	last := claimMirror(t, s, final.Key, 3001)
	if last.Operation != "send" || last.ParentMessageID != "" || last.Card != final.Card {
		t.Fatalf("direct final did not claim as a send: %+v", last)
	}
	if err := s.FinishMirrorAttempt(ctx, last.ID, "confirmed", "final-card"); err != nil {
		t.Fatal(err)
	}
	stage(t, s, 102, progress, final)
	ready, err := s.ReadyMirrors(ctx, "host", "thread")
	if err != nil || len(ready) != 0 {
		t.Fatalf("direct replay created duplicate work: %d %v", len(ready), err)
	}
}

func TestGoalDirectMirrorValidationRejectsParentAndHuman(t *testing.T) {
	ctx := context.Background()
	s, _ := discoveryStore(t)
	eligible(t, s, "thread")
	cases := []Mirror{
		{Key: "direct-parent", TurnID: "goal-turn", Kind: "final", Card: `{"body":"bad parent"}`, Direct: true, ParentMessageID: "parent"},
		{Key: "direct-human", TurnID: "goal-turn", Kind: "human", Card: `{"body":"bad human"}`, Direct: true},
	}
	for _, doc := range cases {
		if err := s.StageMirrors(ctx, "host", "thread", []Mirror{doc}, Watermark{TurnID: "goal-turn", ItemID: doc.Key, ObservedAtMS: 100}); err == nil {
			t.Fatalf("invalid direct mirror was accepted: %+v", doc)
		}
		if _, err := s.Mirror(ctx, "host", "thread", doc.Key); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("rejected direct mirror was persisted: %+v %v", doc, err)
		}
	}
	if _, err := s.Watermark(ctx, "host", "thread"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("invalid direct snapshots advanced the watermark: %v", err)
	}
}

func TestGoalDirectTopicClaimRepliesToTopicRoot(t *testing.T) {
	ctx := context.Background()
	s, _ := discoveryStore(t)
	topicBinding(t, s, "goal-topic", "topic-root")
	doc := goalDirectMirror("goal-topic-final", "final", "topic answer", true)
	if err := s.StageMirrors(ctx, "host", "goal-topic", []Mirror{doc}, Watermark{TurnID: doc.TurnID, ItemID: doc.Key, ObservedAtMS: 100}); err != nil {
		t.Fatal(err)
	}
	m, err := s.Mirror(ctx, "host", "goal-topic", doc.Key)
	if err != nil {
		t.Fatal(err)
	}
	a, claimed, err := s.ClaimMirror(ctx, "host", "goal-topic", doc.Key, m.Revision, 1000)
	if err != nil || !claimed {
		t.Fatalf("topic direct mirror was not claimable: %v %v", claimed, err)
	}
	if a.Operation != "reply" || a.ParentMessageID != "topic-root" || a.FollowMode != "topic" || a.RootMessageID != "topic-root" {
		t.Fatalf("direct topic delivery did not preserve the topic root: %+v", a)
	}
}

func TestGoalDirectCOTClaimsSendThenUpdates(t *testing.T) {
	ctx := context.Background()
	s, _ := cotTestFixture(t)
	doc := cotTestDocument("inProgress", false)
	key := "goal-cot-progress"
	m := Mirror{Key: key, TurnID: doc.TurnID, Kind: "progress", Card: cotTestCard(t, doc), Direct: true}
	stage(t, s, 100, m)
	first := cotTestClaimKey(t, s, key, 1000)
	if first.Operation != "send" || first.ParentMessageID != "" || first.COTStep != "create" {
		t.Fatalf("direct goal CoT was not prepared as an initial send: %+v", first)
	}
	if err := s.FinishCOTAttempt(ctx, first.ID, "confirmed", "goal-cot-card", "goal-cot-native"); err != nil {
		t.Fatal(err)
	}

	doc.Items[0].Title = "goal COT update"
	m.Card = cotTestCard(t, doc)
	stage(t, s, 101, m)
	update := cotTestClaimKey(t, s, key, 4000)
	if update.Operation != "update" || update.TargetMessageID != "goal-cot-card" || update.ParentMessageID != "" || update.COTStep != "append" {
		t.Fatalf("direct goal CoT was not prepared as an update: %+v", update)
	}
	if err := s.FinishCOTAttempt(ctx, update.ID, "confirmed", "goal-cot-card", "goal-cot-native"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Mirror(ctx, "host", "thread", key)
	if err != nil || !got.Direct || got.RemoteID != "goal-cot-card" {
		t.Fatalf("direct goal CoT state was not retained: %+v %v", got, err)
	}
}
