package sync

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"codex-feishu/internal/feishu"
)

type sharedReadbackFake struct {
	message   feishu.CardMessage
	readID    string
	readCalls int
	listCalls int
}

func (f *sharedReadbackFake) ReadCardMessage(_ context.Context, id string) (feishu.CardMessage, error) {
	f.readID = id
	f.readCalls++
	return f.message, nil
}

func (f *sharedReadbackFake) ListCardMessages(_ context.Context, _, _, _ string) (feishu.CardMessagePage, error) {
	f.listCalls++
	return feishu.CardMessagePage{}, errors.New("unexpected list read")
}

func sharedReadbackProof() CardReadbackProof {
	return CardReadbackProof{
		AppID:           "app",
		ChatID:          "chat",
		TargetMessageID: "target-message",
		Card:            `{"schema":"2.0","body":{"elements":[{"tag":"markdown","element_id":"marker","content":"result"}]}}`,
		Marker:          "marker",
	}
}

func sharedReadbackMessage(proof CardReadbackProof) feishu.CardMessage {
	deleted := false
	return feishu.CardMessage{
		ID:           proof.TargetMessageID,
		ChatID:       proof.ChatID,
		Type:         "interactive",
		SenderID:     proof.AppID,
		SenderIDType: "app_id",
		SenderType:   "app",
		Deleted:      &deleted,
		Content:      proof.Card,
	}
}

func TestFindMarkedCardRejectsNilReaderAndInvalidProof(t *testing.T) {
	valid := sharedReadbackProof()
	cases := map[string]CardReadbackProof{
		"missing app":    func() CardReadbackProof { p := valid; p.AppID = ""; return p }(),
		"missing chat":   func() CardReadbackProof { p := valid; p.ChatID = ""; return p }(),
		"missing card":   func() CardReadbackProof { p := valid; p.Card = ""; return p }(),
		"missing marker": func() CardReadbackProof { p := valid; p.Marker = ""; return p }(),
	}
	for name, proof := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := FindMarkedCard(context.Background(), &sharedReadbackFake{}, proof)
			if got != "" || !errors.Is(err, ErrReadbackIncomplete) {
				t.Fatalf("FindMarkedCard() = %q, %v", got, err)
			}
		})
	}

	got, err := FindMarkedCard(context.Background(), nil, valid)
	if got != "" || !errors.Is(err, ErrReadbackIncomplete) {
		t.Fatalf("nil reader FindMarkedCard() = %q, %v", got, err)
	}
}

func TestCardReadbackProofIsNotSerializable(t *testing.T) {
	encoded, err := json.Marshal(sharedReadbackProof())
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "{}" {
		t.Fatalf("CardReadbackProof JSON = %s, want {}", encoded)
	}
}

func TestFindMarkedCardKnownTargetRequiresExactTargetAndExplicitNotDeleted(t *testing.T) {
	proof := sharedReadbackProof()
	reader := &sharedReadbackFake{message: sharedReadbackMessage(proof)}
	got, err := FindMarkedCard(context.Background(), reader, proof)
	if err != nil || got != proof.TargetMessageID || reader.readID != proof.TargetMessageID || reader.readCalls != 1 || reader.listCalls != 0 {
		t.Fatalf("known target FindMarkedCard() = %q, %v; reader=%+v", got, err, reader)
	}

	reader.message.Deleted = nil
	got, err = FindMarkedCard(context.Background(), reader, proof)
	if err != nil || got != "" {
		t.Fatalf("missing deleted proof FindMarkedCard() = %q, %v", got, err)
	}
}
