package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/control"
	"codex-feishu/internal/store"
	"codex-feishu/internal/verification"
)

func TestReceiverRoutesVerificationOutsideCommandsAndNativeReceipts(t *testing.T) {
	r, db := receiverFixture(t)
	ctx := context.Background()
	now := time.Now()
	id, token := store.ID(), store.ID()
	c, err := db.PrepareVerificationCard(ctx, store.VerificationCard{RequestID: id, HostID: "host", AppID: "app", OwnerID: "ou_operator", BridgeCommit: "test-build", BinarySHA256: strings.Repeat("b", 64), Token: token, UUID: store.ID(), Card: "test-card", CreatedAtMS: now.UnixMilli(), ExpiresAtMS: now.Add(time.Minute).UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ClaimVerificationCard(ctx, c, now.UnixMilli()); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := db.FinishVerificationCard(ctx, c, "confirmed", "om_card"); err != nil {
		t.Fatal(err)
	}
	r.Verification = &verification.Manager{Store: db, HostID: "host", AppID: "app", OwnerID: "ou_operator", Commit: "test-build", BinarySHA256: strings.Repeat("b", 64), Now: func() time.Time { return now }}
	commandCalls, wakes := 0, 0
	r.Command = func(context.Context, InboundCommand) error { commandCalls++; return nil }
	r.Wake = func(string) { wakes++ }
	for _, user := range []string{"ou_other", "ou_operator", "ou_operator"} {
		e := receiverMessage(t, "oc_private", user, "om_ping", "p2p", false)
		body, _ := json.Marshal(map[string]string{"text": "/verify " + token})
		e.Event.Message.Content = new(string)
		*e.Event.Message.Content = string(body)
		if err := r.Message(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	for _, user := range []string{"ou_other", "ou_operator", "ou_operator"} {
		e := receiverAction(t, user, "om_card", token, "", 1)
		e.Event.Action.Value["action"] = "verification_confirm"
		response, err := r.Action(ctx, e)
		if err != nil || response.Toast == nil {
			t.Fatal(response, err)
		}
		if user == "ou_operator" && response.Toast.Type != "success" {
			t.Fatal(response)
		}
	}
	proof, err := r.Verification.State(ctx, id)
	if err != nil || proof.CallbackStatus != "pass" || proof.PrivateMessageStatus != "pass" {
		t.Fatal(proof, err)
	}
	if commandCalls != 0 || wakes != 0 {
		t.Fatal("verification entered command/task path")
	}
	receipts, err := db.Receipts(ctx, "thread")
	if err != nil || len(receipts) != 0 {
		t.Fatal("verification queued as task", receipts, err)
	}
}

func TestServiceManagedCardAndGroupUseExistingReceiverAndJournal(t *testing.T) {
	db, cfg, _ := serviceFixture(t)
	g := &serviceGateway{runtimeGateway: &runtimeGateway{}, listening: make(chan struct{})}
	s := &Service{Config: cfg, Store: db, Gateway: g, BotID: "ou_bot", RetryInterval: 5 * time.Millisecond, Connect: func(context.Context) (ServiceNative, error) { return nil, errors.New("offline native") }}
	m := &verification.Manager{Store: db, Gateway: g, HostID: cfg.Host.ID, AppID: cfg.Feishu.AppID, OwnerID: cfg.Access.OwnerOpenID, Commit: "running-test-build", BinarySHA256: strings.Repeat("b", 64), Ready: func() bool { return g.Ready() && s.HealthReadyForVerification() }}
	s.Verification = m
	gm := &verification.GroupManager{Store: db, Gateway: g, HostID: cfg.Host.ID, AppID: cfg.Feishu.AppID, OwnerID: cfg.Access.OwnerOpenID, BotID: s.BotID, Commit: m.Commit, BinarySHA256: m.BinarySHA256, Ready: m.Ready}
	s.VerificationGroups = gm
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serviceDone := make(chan error, 1)
	go func() { serviceDone <- s.Run(ctx) }()
	select {
	case <-g.listening:
	case err := <-serviceDone:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("receiver missing")
	}
	dir, err := os.MkdirTemp("", "cfprobe-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	endpoint := filepath.Join(dir, "control.sock")
	identity := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	server, err := control.Listen(ctx, endpoint, identity, s.Health)
	if err != nil {
		t.Fatal(err)
	}
	server.Verification = m
	server.VerificationGroups = gm
	controlDone := make(chan error, 1)
	go func() { controlDone <- server.Serve(ctx) }()
	defer func() {
		cancel()
		server.Close()
		select {
		case <-controlDone:
		case <-time.After(3 * time.Second):
			t.Error("control did not stop")
		}
		select {
		case <-serviceDone:
		case <-time.After(3 * time.Second):
			t.Error("service did not stop")
		}
	}()
	id := store.ID()
	result, err := control.VerifyCard(ctx, endpoint, identity, id, true)
	if err != nil || result.CardStatus != "pass" {
		t.Fatal(result, err)
	}
	c, err := db.VerificationCard(ctx, id, cfg.Host.ID, cfg.Feishu.AppID, cfg.Access.OwnerOpenID)
	if err != nil {
		t.Fatal(err)
	}
	e := receiverAction(t, cfg.Access.OwnerOpenID, result.MessageID, c.Token, "", 1)
	e.Event.Action.Value["action"] = "verification_confirm"
	e.Event.Context.OpenChatID = "oc_private"
	if reply, err := g.action(ctx, e); err != nil || reply.Toast.Type != "success" {
		t.Fatal(reply, err)
	}
	message := receiverMessage(t, "oc_private", cfg.Access.OwnerOpenID, "om_probe_ping", "p2p", false)
	body, _ := json.Marshal(map[string]string{"text": "/verify " + c.Token})
	*message.Event.Message.Content = string(body)
	if err := g.message(ctx, message); err != nil {
		t.Fatal(err)
	}
	for _, begin := range []bool{false, true} {
		result, err = control.VerifyCard(ctx, endpoint, identity, id, begin)
		if err != nil || result.CallbackStatus != "pass" || result.PrivateMessageStatus != "pass" {
			t.Fatal(result, err)
		}
	}
	groupResult, err := control.VerifyGroup(ctx, endpoint, identity, id, true)
	if err != nil || groupResult.GroupStatus != "pass" || groupResult.MembershipStatus != "pass" || groupResult.GroupMessageStatus != "pending" {
		t.Fatal(groupResult, err)
	}
	for _, args := range []struct {
		chat, user string
		at         bool
	}{{groupResult.ChatID, "ou_other", true}, {"chat", cfg.Access.OwnerOpenID, true}, {groupResult.ChatID, cfg.Access.OwnerOpenID, false}} {
		event := receiverMessage(t, args.chat, args.user, "ignored-group-ping", "group", args.at)
		body, _ := json.Marshal(map[string]string{"text": "@_user_1 /verify-group " + id})
		*event.Event.Message.Content = string(body)
		if err := g.message(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	groupResult, err = control.VerifyGroup(ctx, endpoint, identity, id, false)
	if err != nil || groupResult.GroupMessageStatus != "pending" {
		t.Fatal("invalid group event passed", groupResult, err)
	}
	groupMessage := receiverMessage(t, groupResult.ChatID, cfg.Access.OwnerOpenID, "om_group_ping", "group", true)
	groupBody, _ := json.Marshal(map[string]string{"text": "@_user_1 /verify-group " + id})
	*groupMessage.Event.Message.Content = string(groupBody)
	if err := g.message(ctx, groupMessage); err != nil {
		t.Fatal(err)
	}
	for _, begin := range []bool{false, true} {
		groupResult, err = control.VerifyGroup(ctx, endpoint, identity, id, begin)
		if err != nil || groupResult.GroupMessageStatus != "pass" {
			t.Fatal(groupResult, err)
		}
	}
	if _, err := db.BindingByChat(ctx, cfg.Host.ID, groupResult.ChatID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("verification group bound to native thread", err)
	}
	health, err := control.Query(ctx, endpoint, identity)
	if err != nil || !health.VerificationCardSupported || !health.VerificationGroupSupported || health.NativeConnected {
		t.Fatal(health, err)
	}
	g.mu.Lock()
	writes, groups := append([]runtimeWrite(nil), g.writes...), g.groups
	g.mu.Unlock()
	if len(writes) != 1 || writes[0].target != cfg.Access.OwnerOpenID || groups != 1 || g.listens.Load() != 1 {
		t.Fatal("probe duplicated receiver/group/card", writes, groups)
	}
	receipts, err := db.Receipts(ctx, "thread")
	if err != nil || len(receipts) != 0 {
		t.Fatal("probe entered native input", receipts, err)
	}
}
