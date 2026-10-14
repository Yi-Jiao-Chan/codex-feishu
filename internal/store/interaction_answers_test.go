package store

import (
	"context"
	"errors"
	"testing"
)

func TestInteractionAnswerReceiptSurvivesMigrationAndReopen(t *testing.T) {
	s, path, scope, _ := submissionStore(t)
	ctx := context.Background()
	r := InteractionRequest{Key: "native-question", HostID: scope.HostID, AppID: scope.AppID, ThreadID: scope.ThreadID, TurnID: "turn", ItemID: "call", Mode: "native", Method: "item/tool/requestUserInput", Payload: `{}`, ConnectionID: "connection", Generation: 1, State: "pending", CreatedAtMS: 1000}
	if err := s.ObserveInteraction(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`DROP TABLE interaction_answers;PRAGMA user_version=24`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	result := `{"answers":{"layout":{"answers":["合成回答"]}}}`
	if err := s.RecordInteractionAnswer(ctx, r.Key, result); !errors.Is(err, ErrConflict) {
		t.Fatal("pending request acquired a resolved receipt", err)
	}
	if err := s.ResolveInteraction(ctx, r.Key, r.ConnectionID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.RecordInteractionAnswer(ctx, r.Key, result); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordInteractionAnswer(ctx, r.Key, `{"answers":{}}`); !errors.Is(err, ErrConflict) {
		t.Fatal("recorded effective answer was replaced", err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.InteractionAnswer(ctx, r.Key)
	if err != nil || got != result {
		t.Fatal("answer lost after reopen", err)
	}
	request, err := s.Interaction(ctx, r.Key)
	if err != nil || request.AttemptID != "" || request.State != "resolved" {
		t.Fatal("read-only receipt changed a submission attempt", err)
	}
}
