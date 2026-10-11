//go:build linux

package control

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/pairing"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func controlPairEvent(t *testing.T, code, owner, kind string) *im.P2MessageReceiveV1 {
	t.Helper()
	text, _ := json.Marshal(map[string]string{"text": "/pair " + code})
	b, _ := json.Marshal(map[string]any{"schema": "2.0", "header": map[string]string{"app_id": "cli_pair_control", "event_type": "im.message.receive_v1"}, "event": map[string]any{"sender": map[string]any{"sender_type": "user", "sender_id": map[string]string{"open_id": owner}}, "message": map[string]string{"message_id": "om_pair", "chat_id": "oc_private", "chat_type": kind, "message_type": "text", "content": string(text)}}})
	var e im.P2MessageReceiveV1
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	return &e
}

func TestRealUnixPairingUsesSameIdentityAndNeverReturnsCodeInStatus(t *testing.T) {
	path, identity := controlFixture(t)
	manager, err := pairing.New("cli_pair_control", "ou_fixture_owner_valid")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx, cancel := context.WithCancel(context.Background())
	server, err := Listen(ctx, path, identity, func(context.Context) (Snapshot, error) { return snapshot(), nil }, manager)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	defer func() { cancel(); server.Close(); <-done }()
	id, _ := pairing.GenerateRequestID()
	if _, err := PairBegin(ctx, path, strings.Repeat("b", 64), id, 20*time.Second); !errors.Is(err, ErrIdentity) {
		t.Fatal("wrong configuration started pairing", err)
	}
	if _, err := manager.State(id); !errors.Is(err, pairing.ErrNotFound) {
		t.Fatal("wrong identity reached pairing provider", err)
	}
	c, err := PairBegin(ctx, path, identity, id, 20*time.Second)
	if err != nil || c.State != pairing.StateWaiting || len(c.Code) != 26 {
		t.Fatal("start failed", c, err)
	}
	secondID, _ := pairing.GenerateRequestID()
	if _, err := PairBegin(ctx, path, identity, secondID, 20*time.Second); !errors.Is(err, pairing.ErrBusy) {
		t.Fatal("second active pairing accepted", err)
	}
	again, err := PairBegin(ctx, path, identity, id, time.Second)
	if err != nil || again.Code != c.Code || again.ExpiresAtMS != c.ExpiresAtMS {
		t.Fatal("start replay changed challenge", err)
	}
	state, err := PairState(ctx, path, identity, id)
	b, _ := json.Marshal(state)
	if err != nil || state.State != pairing.StateWaiting || strings.Contains(string(b), c.Code) || strings.Contains(string(b), `"code"`) {
		t.Fatal("status disclosed code", state, err)
	}
	manager.Handle(controlPairEvent(t, c.Code, "ou_other_fixture_user", "p2p"))
	manager.Handle(controlPairEvent(t, c.Code, "ou_fixture_owner_valid", "group"))
	if s, _ := PairState(ctx, path, identity, id); s.State != pairing.StateWaiting {
		t.Fatal("wrong owner or group accepted", s)
	}
	manager.Handle(controlPairEvent(t, c.Code, "ou_fixture_owner_valid", "p2p"))
	state, err = PairState(ctx, path, identity, id)
	if err != nil || state.State != pairing.StateConfirmed || state.OwnerOpenID != "ou_fixture_owner_valid" {
		t.Fatal("private callback not observed", state, err)
	}
	terminal, err := PairBegin(ctx, path, identity, id, 20*time.Second)
	if err != nil || terminal.Code != "" || terminal.State != pairing.StateConfirmed {
		t.Fatal("terminal retry returned code", terminal, err)
	}
	if s, err := PairCancel(ctx, path, identity, id); err != nil || s.State != pairing.StateConfirmed {
		t.Fatal("cancel rewrote confirmed result", s, err)
	}
	if _, err := Query(ctx, path, identity); err != nil {
		t.Fatal("pairing interfered with health", err)
	}
}

func TestPairingManagementRejectsUnsupportedAndMalformedRequests(t *testing.T) {
	path, identity := controlFixture(t)
	_, cancel, done := runControl(t, path, identity, func(context.Context) (Snapshot, error) { return snapshot(), nil })
	id, _ := pairing.GenerateRequestID()
	if _, err := PairBegin(context.Background(), path, identity, id, time.Second); !errors.Is(err, ErrUnsupported) {
		t.Fatal("unsupported pairing claimed accepted", err)
	}
	if _, err := PairBegin(context.Background(), path, identity, id, 1500*time.Millisecond); !errors.Is(err, pairing.ErrInvalid) {
		t.Fatal("fractional protocol TTL accepted", err)
	}
	cancel()
	<-done
	manager, _ := pairing.New("cli_pair_control", "")
	defer manager.Close()
	ctx, stop := context.WithCancel(context.Background())
	server, err := Listen(ctx, path, identity, func(context.Context) (Snapshot, error) { return snapshot(), nil }, manager)
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(ctx) }()
	defer func() { stop(); server.Close(); <-finished }()
	for _, body := range []string{
		`{"schema_version":1,"method":"pair.begin","identity":"` + identity + `","pair":{"request_id":"` + id + `","ttl_seconds":301}}`,
		`{"schema_version":1,"method":"pair.begin","identity":"` + identity + `","pair":{"request_id":"bad","ttl_seconds":20}}`,
		`{"schema_version":1,"method":"pair.state","identity":"` + identity + `","pair":{"request_id":"` + id + `","ttl_seconds":20}}`,
		`{"schema_version":1,"method":"pair.begin","identity":"` + identity + `","pair":{"request_id":"` + id + `","ttl_seconds":20,"owner":"ou_fake"}}`,
		`{"schema_version":1,"method":"health","identity":"` + identity + `","pair":{"request_id":"` + id + `"}}`,
	} {
		var response map[string]any
		json.Unmarshal(wire(t, path, body), &response)
		if response["code"] == "ok" {
			t.Fatal("malformed method succeeded", response)
		}
	}
	if _, err := manager.State(id); !errors.Is(err, pairing.ErrNotFound) {
		t.Fatal("malformed request reached pairing Begin", err)
	}
}
