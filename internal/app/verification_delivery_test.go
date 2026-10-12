package app

import (
	"context"
	"testing"
	"time"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/store"
	messages "codex-feishu/internal/sync"
)

func TestVerificationCommandWithEmptyThreadStillRequiresOriginalChat(t *testing.T) {
	s, _, g, _ := bindingServiceFixture(t)
	ctx := context.Background()
	r := store.CommandReply{HostID: s.Config.Host.ID, AppID: s.Config.Feishu.AppID, UserID: "owner", ChatID: "chat", ChatType: "group"}
	opctx, release, err := s.beginCommandDelivery(ctx, r)
	if err != nil || opctx == nil || release == nil {
		t.Fatal("valid chat scope denied", err)
	}
	if _, ok := opctx.Deadline(); !ok {
		t.Fatal("command lost fixture lifetime")
	}
	release()
	g.setRoster("owner", "ou_bot", "foreign")
	if _, release, err := s.beginCommandDelivery(ctx, r); err == nil {
		release()
		t.Fatal("empty thread bypassed changed membership")
	}
	g.setRoster("owner", "ou_bot")
	if _, err := s.Store.DB.Exec(`DELETE FROM bindings`); err != nil {
		t.Fatal(err)
	}
	if _, release, err := s.beginCommandDelivery(ctx, r); err == nil {
		release()
		t.Fatal("missing route made verification chat ordinary")
	}
}

func TestVerificationDeliveryRejectsAnotherThreadUsingMarkedChat(t *testing.T) {
	s, _, _, _ := bindingServiceFixture(t)
	b := store.Binding{HostID: s.Config.Host.ID, ThreadID: "ordinary-thread", ChatID: "chat", IntentID: "other"}
	if _, release, err := s.beginCardDelivery(context.Background(), b); err == nil {
		release()
		t.Fatal("marked chat borrowed ordinary thread authority")
	}
	// Model a legitimate-shaped ordinary route reusing the original chat after
	// removal. The private chat marker remains authoritative independently.
	for _, q := range []string{
		`INSERT INTO thread_observations(host_id,thread_id,origin,proof,state,cwd,created_at) SELECT host_id,'ordinary-thread','new','birth','eligible',cwd,created_at FROM thread_observations WHERE thread_id='thread'`,
		`INSERT INTO create_intents(intent_id,host_id,thread_id,owner_id,name,members_json,state,chat_id,created_at) SELECT 'other',host_id,'ordinary-thread',owner_id,name,members_json,'confirmed',chat_id,created_at FROM create_intents LIMIT 1`,
		`UPDATE bindings SET thread_id='ordinary-thread',intent_id='other'`,
	} {
		if _, err := s.Store.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if opctx, release, handled, err := s.beginVerificationOperation(context.Background(), "ordinary-thread"); err == nil || !handled || opctx != nil {
		if release != nil {
			release()
		}
		t.Fatal("ordinary route reclaimed a marked chat", handled, err)
	}
}

type closingDeliveryGateway struct {
	*bindingServiceGateway
	onSend func(context.Context) error
}

func (g *closingDeliveryGateway) SendCard(ctx context.Context, _, _, _, _ string) (string, error) {
	return "", g.onSend(ctx)
}

func TestVerificationCloseCancelsActualOutboxScopeAndJoins(t *testing.T) {
	s, _, g, _ := bindingServiceFixture(t)
	ctx := context.Background()
	m := store.Mirror{Key: "fixture-human", TurnID: "fixture-turn", Kind: "human", Card: cards.Message("人类发送", "fixture", "fixture input")}
	if err := s.Store.StageMirrors(ctx, s.Config.Host.ID, "thread", []store.Mirror{m}, store.Watermark{ObservedAtMS: s.now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	writes := 0
	gateway := &closingDeliveryGateway{bindingServiceGateway: g, onSend: func(call context.Context) error {
		writes++
		if _, ok := call.Deadline(); !ok {
			t.Error("outbox discarded operation context")
		}
		go func() { closed <- s.VerificationBindings.Sessions.Close() }()
		select {
		case <-call.Done():
			return call.Err()
		case <-time.After(time.Second):
			t.Error("resource close did not cancel gateway request")
			return context.DeadlineExceeded
		}
	}}
	o := messages.Outbound{Store: s.Store, Gateway: gateway, BeginWrite: s.beginCardDelivery, Now: s.now}
	result, err := o.DispatchOne(ctx, s.Config.Host.ID, "thread", s.now().UnixMilli())
	if err == nil || result.AttemptID == "" || result.State != "unknown" || writes != 1 {
		t.Fatal("canceled remote result was misclassified", result.State, err, writes)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not join released outbox scope")
	}
	stored, err := s.Store.Mirror(ctx, s.Config.Host.ID, "thread", m.Key)
	if err != nil || stored.State != "dispatching" {
		t.Fatal("cancellation forged a safe retry", stored.State, err)
	}
}
