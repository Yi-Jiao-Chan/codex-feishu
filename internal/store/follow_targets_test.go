package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func topicBinding(t *testing.T, s *Store, thread, root string) Binding {
	t.Helper()
	ctx := context.Background()
	eligible(t, s, thread)
	i, err := s.PrepareGroup(ctx, "host", thread, "owner", "topic", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimGroupDispatch(ctx, i.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmGroup(ctx, i.ID, "workgroup"); err != nil {
		t.Fatal(err)
	}
	b := Binding{HostID: "host", ThreadID: thread, IntentID: i.ID, ChatID: "workgroup", FollowMode: "topic", RootMessageID: root, PlatformThreadID: "platform-" + root}
	if err = s.BindFollowTarget(ctx, b); err != nil {
		t.Fatal(err)
	}
	return b
}
func TestFollowTargetsIsolateTopicsAndRejectMixedTargets(t *testing.T) {
	s, _ := discoveryStore(t)
	ctx := context.Background()
	a := topicBinding(t, s, "one", "root-one")
	b := topicBinding(t, s, "two", "root-two")
	for _, want := range []Binding{a, b} {
		got, err := s.BindingByTarget(ctx, "host", "workgroup", want.RootMessageID, want.PlatformThreadID)
		if err != nil || got != want {
			t.Fatalf("wrong topic: %+v %v", got, err)
		}
	}
	for _, v := range [][3]string{{"", "", ""}, {"root-one", "platform-root-two", ""}, {"root-one", "", "foreign"}, {"", "platform-missing", ""}} {
		host := "host"
		if v[2] != "" {
			host = v[2]
		}
		if _, err := s.BindingByTarget(ctx, host, "workgroup", v[0], v[1]); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("ambiguous target routed: %v", err)
		}
	}
	if _, err := s.BindingByMessage(ctx, "host", "workgroup", "client-thread-one"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unrecorded identity routed: %v", err)
	}
	for _, want := range []Binding{a, b} {
		got, err := s.BindingByMessage(ctx, "host", "workgroup", want.RootMessageID)
		if err != nil || got != want {
			t.Fatal(got, err)
		}
	}
	eligible(t, s, "mixed")
	i, err := s.PrepareGroup(ctx, "host", "mixed", "owner", "group", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.ClaimGroupDispatch(ctx, i.ID)
	s.ConfirmGroup(ctx, i.ID, "workgroup")
	if err = s.BindConfirmedGroup(ctx, i.ID); err == nil {
		t.Fatal("group mixed into shared topic chat")
	}
	if err = s.RecordBindingPlatformThread(ctx, "host", "one", "platform-root-two"); err == nil {
		t.Fatal("retargeted platform topic")
	}
	if err = s.MarkBindingRepair(ctx, "host", "one", "root_deleted"); err != nil {
		t.Fatal(err)
	}
	one, _ := s.Binding(ctx, "host", "one")
	two, _ := s.Binding(ctx, "host", "two")
	if !one.RepairRequired || two.RepairRequired {
		t.Fatal("repair leaked across topics")
	}
}

func TestTopicMessageRoutingUsesDurableReceiptAndMirrorCards(t *testing.T) {
	s, _ := discoveryStore(t)
	ctx := context.Background()
	a := topicBinding(t, s, "one", "root-one")
	topicBinding(t, s, "two", "root-two")
	r, _, err := s.Claim(ctx, Receipt{AppID: "app", ThreadID: a.ThreadID, ChatID: a.ChatID, MessageID: "incoming", CardID: "receipt-card"})
	if err != nil {
		t.Fatal(err)
	}
	r.CardID = "receipt-card"
	if err = s.SaveReceipt(ctx, r); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"incoming", "receipt-card"} {
		got, err := s.BindingByMessage(ctx, "host", "workgroup", id)
		if err != nil || got.ThreadID != "one" {
			t.Fatal(got, err)
		}
	}
	if _, err = s.BindingByMessage(ctx, "foreign-host", "workgroup", "receipt-card"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	if err = s.StageMirrors(ctx, "host", "one", []Mirror{{Key: "human", TurnID: "turn", Kind: "human", Card: `{"body":"safe"}`}}, Watermark{ObservedAtMS: 200}); err != nil {
		t.Fatal(err)
	}
	attempt, ok, err := s.ClaimMirror(ctx, "host", "one", "human", 1, 1000)
	if err != nil || !ok {
		t.Fatal(attempt, ok, err)
	}
	if attempt.Operation != "reply" || attempt.ParentMessageID != a.RootMessageID || attempt.RootMessageID != a.RootMessageID || attempt.PlatformThreadID != a.PlatformThreadID {
		t.Fatalf("wire target not frozen: %+v", attempt)
	}
	if err = s.FinishMirrorAttempt(ctx, attempt.ID, "confirmed", "mirror-card"); err != nil {
		t.Fatal(err)
	}
	got, err := s.BindingByMessage(ctx, "host", "workgroup", "mirror-card")
	if err != nil || got.ThreadID != "one" {
		t.Fatal(got, err)
	}
}

func TestTopicMigrationPreservesForeignKeysAndGroupData(t *testing.T) {
	s, path, scope, r := submissionStore(t)
	ctx := context.Background()
	if _, err := s.DB.Exec(`INSERT INTO attachment_batches(batch_id,host_id,app_id,thread_id,chat_id,sender_id,token,state,first_message_id,created_at_ms) VALUES('batch','host','app','thread','chat','owner','batch-token','open','attachment',100)`); err != nil {
		t.Fatal(err)
	}
	// Construct a real pre-v27 parent table while retaining populated child rows.
	if _, err := s.DB.Exec(`PRAGMA foreign_keys=OFF;
CREATE TABLE bindings_old(host_id TEXT NOT NULL,thread_id TEXT NOT NULL,intent_id TEXT NOT NULL UNIQUE REFERENCES create_intents(intent_id),chat_id TEXT NOT NULL,PRIMARY KEY(host_id,thread_id),UNIQUE(host_id,chat_id));
INSERT INTO bindings_old(host_id,thread_id,intent_id,chat_id) SELECT host_id,thread_id,intent_id,chat_id FROM bindings;
DROP TABLE bindings;ALTER TABLE bindings_old RENAME TO bindings;
ALTER TABLE outbound_attempts DROP COLUMN follow_mode;ALTER TABLE outbound_attempts DROP COLUMN root_message_id;ALTER TABLE outbound_attempts DROP COLUMN platform_thread_id;
PRAGMA user_version=26;PRAGMA foreign_keys=ON;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	b, err := reopened.BindingByChat(ctx, "host", "chat")
	if err != nil || b.FollowMode != "" || b.ThreadID != scope.ThreadID {
		t.Fatal(b, err)
	}
	if _, err = reopened.Submission(ctx, scope, r.ID); err != nil {
		t.Fatalf("receipt reference lost: %v", err)
	}
	var n int
	if err = reopened.DB.QueryRow(`SELECT COUNT(*) FROM attachment_batches WHERE batch_id='batch'`).Scan(&n); err != nil || n != 1 {
		t.Fatal("attachment reference lost", n, err)
	}
	rows, err := reopened.DB.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("migration broke foreign key")
	}
}

func TestLegacyGroupTargetAcceptsOrdinaryReplyRoot(t *testing.T) {
	s, _, _, _ := submissionStore(t)
	b, err := s.BindingByTarget(context.Background(), "host", "chat", "ordinary-reply-root", "")
	if err != nil || b.ThreadID != "thread" || b.FollowMode != "" {
		t.Fatal("legacy reply lost its group", b, err)
	}
}

func TestTopicRootRoutesBeforePlatformThreadKnown(t *testing.T) {
	s, _ := discoveryStore(t)
	ctx := context.Background()
	b := topicBinding(t, s, "one", "root-one")
	if _, err := s.DB.Exec(`UPDATE bindings SET platform_thread_id='' WHERE host_id='host'`); err != nil {
		t.Fatal(err)
	}
	got, err := s.BindingByTarget(ctx, "host", "workgroup", b.RootMessageID, "new-platform")
	if err != nil || got.ThreadID != b.ThreadID {
		t.Fatal("verified root lost before initial reply", got, err)
	}
	if _, err = s.BindingByTarget(ctx, "host", "workgroup", "", "new-platform"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("unrecorded platform guessed a topic", err)
	}
}
