package store

import (
	"context"
	"strings"
	"testing"

	"codex-feishu/internal/cards"
)

func TestInteractionSchemaMigrationPreservesBindingAndReceipts(t *testing.T) {
	s, path, scope, receipt := submissionStore(t)
	ctx := context.Background()
	if _, err := s.DB.Exec(`DROP TABLE interaction_attempts;DROP TABLE interaction_requests;PRAGMA user_version=23`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	if chat, err := migrated.SubmissionBinding(ctx, scope); err != nil || chat != "chat" {
		t.Fatal("binding changed", err)
	}
	if r, err := migrated.ByToken(ctx, receipt.ActionToken); err != nil || r.ID != receipt.ID || r.Body != receipt.Body {
		t.Fatal("old receipt changed", err)
	}
	var version int
	migrated.DB.QueryRow(`PRAGMA user_version`).Scan(&version)
	if version != SchemaVersion {
		t.Fatal(version)
	}
}

func TestInteractionAsyncOnlyCardUsesExactFeishuInputAndKeepsOfflineDetail(t *testing.T) {
	s, _, scope, receipt := submissionStore(t)
	ctx := context.Background()
	if err := s.StageSnapshot(ctx, scope.HostID, scope.ThreadID, nil, []InputLink{{ReceiptID: receipt.ID, TurnID: "turn", ItemID: "input"}}, Watermark{TurnID: "turn", ItemID: "input", ObservedAtMS: 1000}); err != nil {
		t.Fatal(err)
	}
	r := InteractionRequest{Key: "question", HostID: scope.HostID, AppID: scope.AppID, ThreadID: scope.ThreadID, TurnID: "turn", ItemID: "question", Mode: "async", NativeID: "question", Payload: `{}`, ConnectionID: "connection", Generation: 1, State: "pending", CreatedAtMS: 1000}
	if err := s.ObserveInteraction(ctx, r); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Interaction(ctx, r.Key)
	card := cards.Interaction(cards.InteractionView{Kind: "async_question", State: "disconnected", Reason: "需要回答的完整问题"})
	if err := s.StageInteractionCard(ctx, r, card, false); err != nil {
		t.Fatal(err)
	}
	m, err := s.Mirror(ctx, scope.HostID, scope.ThreadID, InteractionCardKey(r.Key))
	if err != nil || m.ParentMessageID != receipt.MessageID || m.ParentKey != "" {
		t.Fatal("async-only request lost its exact original input", err)
	}
	if err := s.StageDisconnectedCards(ctx, scope, "generic disconnected"); err != nil {
		t.Fatal(err)
	}
	after, err := s.Mirror(ctx, scope.HostID, scope.ThreadID, m.Key)
	if err != nil || after.Card != card || after.Revision != m.Revision || !strings.Contains(after.Card, "需要回答的完整问题") {
		t.Fatal("generic offline projection overwrote request scope", err)
	}
}
func TestRecoverMultipleInteractionRequestsIsNotATokenCollision(t *testing.T) {
	s, _, scope, _ := submissionStore(t)
	ctx := context.Background()
	for _, k := range []string{"a", "b"} {
		r := InteractionRequest{Key: k, HostID: scope.HostID, AppID: scope.AppID, ThreadID: scope.ThreadID, TurnID: "turn", ItemID: k, Mode: "native", Method: "item/tool/requestUserInput", NativeID: k, Payload: `{}`, ConnectionID: "first", Generation: 1, State: "pending", CreatedAtMS: 1}
		if err := s.ObserveInteraction(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecoverInteractions(ctx, scope.HostID); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Interactions(ctx, scope)
	if err != nil || len(rows) != 2 || rows[0].Token == rows[1].Token || rows[0].State != "disconnected" || rows[1].State != "disconnected" {
		t.Fatal("recovery altered identity", err)
	}
}

func TestAsyncPartialAnswerCannotReopenAfterReconnect(t *testing.T) {
	s, _, scope, _ := submissionStore(t)
	ctx := context.Background()
	r := InteractionRequest{Key: "partial", HostID: scope.HostID, AppID: scope.AppID, ThreadID: scope.ThreadID, TurnID: "turn", ItemID: "question", Mode: "async", Payload: `{}`, ConnectionID: "first", Generation: 1, State: "pending", CreatedAtMS: 1}
	if err := s.ObserveInteraction(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseAsyncInteraction(ctx, r.Key, r.ConnectionID, false); err != nil {
		t.Fatal(err)
	}
	r.ConnectionID, r.Generation = "second", 2
	if err := s.ObserveInteraction(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err := s.Interaction(ctx, r.Key)
	if err != nil || got.State != "unsupported" || got.Operable || got.ConnectionID != r.ConnectionID {
		t.Fatal("partial answer became actionable on reconnect", err)
	}
	if err := s.CloseAsyncInteraction(ctx, r.Key, r.ConnectionID, true); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Interaction(ctx, r.Key)
	if got.State != "resolved" {
		t.Fatal("complete later history could not finish a partial question")
	}
}
