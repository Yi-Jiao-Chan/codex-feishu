package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestHealthContainsOnlyScopedAggregateFacts(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "health.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, host := range []string{"host-one", "host-two"} {
		if _, err := s.BeginBaseline(ctx, host, "app", 100); err != nil {
			t.Fatal(err)
		}
		if err := s.ObserveNew(ctx, host, "thread-"+host, "/project", 101); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkEligible(ctx, host, "thread-"+host, "user", "/project", "/project"); err != nil {
			t.Fatal(err)
		}
		i, err := s.PrepareGroup(ctx, host, "thread-"+host, "owner", "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimGroupDispatch(ctx, i.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.ConfirmGroup(ctx, i.ID, "chat-"+host); err != nil {
			t.Fatal(err)
		}
		if err := s.BindConfirmedGroup(ctx, i.ID); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Claim(ctx, Receipt{AppID: "app", ThreadID: "thread-" + host, ChatID: "chat-" + host, MessageID: "message-" + host, Body: "private body must not appear"}); err != nil {
			t.Fatal(err)
		}
	}
	h, err := s.Health(ctx, SchemaVersion, "host-one", "app")
	if err != nil || h.Bindings != 1 || h.Inputs["received"] != 1 {
		t.Fatal("cross-host health or incomplete count", err)
	}
	b, _ := json.Marshal(h)
	if strings.Contains(string(b), "private body") || strings.Contains(string(b), "message-") || strings.Contains(string(b), "thread-") {
		t.Fatal("diagnostic contains conversation data")
	}
	if _, err := s.Health(ctx, SchemaVersion, "host-one", "other-app"); err == nil {
		t.Fatal("wrong application used database health")
	}
}
