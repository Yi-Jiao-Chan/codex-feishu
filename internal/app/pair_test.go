package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"codex-feishu/internal/pairing"
	"codex-feishu/internal/store"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
)

func TestServicePairingConsumesCommandsBeforeDurableTaskOrCommandWork(t *testing.T) {
	db, cfg, _ := serviceFixture(t)
	manager, err := pairing.New(cfg.Feishu.AppID, cfg.Access.OwnerOpenID)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	gateway := &serviceGateway{runtimeGateway: &runtimeGateway{}, listening: make(chan struct{})}
	service := &Service{Config: cfg, Store: db, Gateway: gateway, BotID: "ou_bot", Pairing: manager, Connect: func(context.Context) (ServiceNative, error) { return nil, errors.New("fixture native offline") }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	select {
	case <-gateway.listening:
	case err := <-done:
		t.Fatal("service stopped", err)
	case <-time.After(3 * time.Second):
		t.Fatal("listener missing")
	}
	id, _ := pairing.GenerateRequestID()
	challenge, err := manager.Begin(id, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for i, kind := range []string{"group", "p2p"} {
		e := receiverMessage(t, "chat", cfg.Access.OwnerOpenID, "pair-command-"+kind, kind, true)
		e.EventV2Base = &larkevent.EventV2Base{Schema: "2.0", Header: &larkevent.EventHeader{AppID: cfg.Feishu.AppID, EventType: "im.message.receive_v1"}}
		b, _ := json.Marshal(map[string]string{"text": "/pair " + challenge.Code})
		e.Event.Message.Content = func() *string { s := string(b); return &s }()
		if err := gateway.message(ctx, e); err != nil {
			t.Fatal(err)
		}
		r, err := manager.State(id)
		if err != nil || i == 0 && r.State != pairing.StateWaiting || i == 1 && r.State != pairing.StateConfirmed {
			t.Fatal("pairing scope incorrect", r, err)
		}
	}
	health, err := service.Health(ctx)
	if err != nil || !health.PairingSupported || !health.Running || !health.FeishuConnected {
		t.Fatal("existing receiver management unavailable", health, err)
	}
	receipts, err := db.Receipts(ctx, "thread")
	if err != nil || len(receipts) != 0 {
		t.Fatal("pairing command persisted as task", receipts, err)
	}
	healthCounts, err := db.Health(ctx, store.SchemaVersion, cfg.Host.ID, cfg.Feishu.AppID)
	if err != nil {
		t.Fatal(err)
	}
	for _, states := range []map[string]int64{healthCounts.CommandReplies, healthCounts.Inputs, healthCounts.NativeActions, healthCounts.Outbound} {
		for _, count := range states {
			if count != 0 {
				t.Fatal("pairing produced a command/task/card journal", healthCounts)
			}
		}
	}
	if gateway.listens.Load() != 1 {
		t.Fatal("pairing started another listener")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("receiver not joined on shutdown")
	}
	if gateway.Ready() {
		t.Fatal("receiver remained ready after shutdown")
	}
}
