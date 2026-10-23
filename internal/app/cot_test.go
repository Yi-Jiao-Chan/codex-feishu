package app

import (
	"context"
	"testing"

	"codex-feishu/internal/feishu"
)

type cotServiceGateway struct{ *serviceGateway }

func (*cotServiceGateway) CreateCOT(context.Context, string, string, bool) (feishu.COTMessage, error) {
	return feishu.COTMessage{}, nil
}
func (*cotServiceGateway) AppendCOT(context.Context, string, string, []feishu.COTEvent) error {
	return nil
}
func (*cotServiceGateway) CompleteCOT(context.Context, string, string, string) error { return nil }

func TestCOTServiceUsesNativeGatewayAcrossGenerations(t *testing.T) {
	db, cfg, root := serviceFixture(t)
	s := &Service{Config: cfg, Store: db, Gateway: &cotServiceGateway{&serviceGateway{}}, BotID: "ou_bot"}
	for i := 0; i < 2; i++ {
		if !s.generation(serviceTestNative(root)).NativeCOT {
			t.Fatal("native gateway did not enable CoT in runtime generation")
		}
	}
	s.Gateway = &serviceGateway{}
	if s.generation(serviceTestNative(root)).NativeCOT {
		t.Fatal("legacy adapter was falsely treated as a native CoT gateway")
	}
}
