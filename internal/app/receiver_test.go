package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"codex-feishu/internal/store"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func receiverFixture(t *testing.T) (*Receiver, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "receiver.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if _, err := db.BeginBaseline(ctx, "host", "app", 100); err != nil {
		t.Fatal(err)
	}
	if err := db.ObserveNew(ctx, "host", "thread", "/project", 101); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkEligible(ctx, "host", "thread", "first-input", "/project", "/project"); err != nil {
		t.Fatal(err)
	}
	i, err := db.PrepareGroup(ctx, "host", "thread", "owner", "group", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimGroupDispatch(ctx, i.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.ConfirmGroup(ctx, i.ID, "chat"); err != nil {
		t.Fatal(err)
	}
	if err := db.BindConfirmedGroup(ctx, i.ID); err != nil {
		t.Fatal(err)
	}
	r := &Receiver{Store: db, BotID: "ou_bot"}
	r.Config.Host.ID, r.Config.Feishu.AppID = "host", "app"
	r.Config.Access.OperatorOpenIDs = []string{"ou_operator"}
	return r, db
}

func receiverMessage(t *testing.T, chat, user, message, kind string, mentioned bool) *im.P2MessageReceiveV1 {
	t.Helper()
	data := map[string]any{"event": map[string]any{
		"sender":  map[string]any{"sender_type": "user", "sender_id": map[string]any{"open_id": user}},
		"message": map[string]any{"chat_id": chat, "chat_type": kind, "message_id": message, "message_type": "text", "content": "{\"text\":\"@_user_1 follow up\"}"},
	}}
	if mentioned {
		data["event"].(map[string]any)["message"].(map[string]any)["mentions"] = []any{map[string]any{"key": "@_user_1", "id": map[string]any{"open_id": "ou_bot"}}}
	}
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	var e im.P2MessageReceiveV1
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	return &e
}

func receiverAction(t *testing.T, user, message, token, queue string, revision int64) *callback.CardActionTriggerEvent {
	t.Helper()
	b, err := json.Marshal(map[string]any{"event": map[string]any{
		"operator": map[string]string{"open_id": user},
		"context":  map[string]string{"open_chat_id": "chat", "open_message_id": message},
		"action":   map[string]any{"value": map[string]any{"action": "steer", "token": token, "queue_id": queue, "revision": revision}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var e callback.CardActionTriggerEvent
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	return &e
}

func TestReceiverPersistsOneReceiptAcrossReplaysWithoutNativeOrEcho(t *testing.T) {
	r, db := receiverFixture(t)
	ctx := context.Background()
	wakes := 0
	r.Wake = func(thread string) {
		if thread != "thread" {
			t.Fatal("wrong route")
		}
		wakes++
	}
	e := receiverMessage(t, "chat", "ou_operator", "human-message", "group", true)
	for i := 0; i < 2; i++ {
		if err := r.Message(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	receipts, err := db.Receipts(ctx, "thread")
	if err != nil || len(receipts) != 1 || receipts[0].Body != "follow up" || receipts[0].State != "received" || wakes != 2 {
		t.Fatal("receipt was lost or duplicated", err)
	}
	mirrors, err := db.Mirrors(ctx, "host", "thread")
	if err != nil || len(mirrors) != 0 {
		t.Fatal("input echoed before actor reconciliation", err)
	}
	for _, e := range []*im.P2MessageReceiveV1{
		receiverMessage(t, "chat", "ou_operator", "not-mentioned", "group", false),
		receiverMessage(t, "chat", "ou_outsider", "unauthorized", "group", true),
		receiverMessage(t, "unbound", "ou_operator", "unbound", "group", true),
	} {
		if err := r.Message(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	receipts, _ = db.Receipts(ctx, "thread")
	if len(receipts) != 1 {
		t.Fatal("unauthorized input reached native receipt queue")
	}
}

func TestReceiverChecksActualCardAndExactQueueRevision(t *testing.T) {
	r, db := receiverFixture(t)
	ctx := context.Background()
	if err := r.Message(ctx, receiverMessage(t, "chat", "ou_operator", "human", "group", true)); err != nil {
		t.Fatal(err)
	}
	scope := store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}
	if err := db.StageSubmissions(ctx, scope); err != nil {
		t.Fatal(err)
	}
	ns, _ := db.Submissions(ctx, scope)
	n := ns[0]
	if err := db.ObserveSubmission(ctx, scope, n.ID, "queued", "native-q", 1, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.StageQueueCard(ctx, scope, n.ID); err != nil {
		t.Fatal(err)
	}
	m, _ := db.Mirror(ctx, "host", "thread", store.QueueCardKey(n.ID))
	a, claimed, err := db.ClaimMirror(ctx, "host", "thread", m.Key, m.Revision, 5000)
	if err != nil || !claimed {
		t.Fatal(err)
	}
	if err := db.FinishMirrorAttempt(ctx, a.ID, "confirmed", "actual-card"); err != nil {
		t.Fatal(err)
	}
	ns, _ = db.Submissions(ctx, scope)
	n = ns[0]
	for _, e := range []*callback.CardActionTriggerEvent{
		receiverAction(t, "ou_outsider", "actual-card", n.ActionToken, n.QueueID, n.Revision),
		receiverAction(t, "ou_operator", "another-card", n.ActionToken, n.QueueID, n.Revision),
		receiverAction(t, "ou_operator", "actual-card", n.ActionToken, "replacement-q", n.Revision),
		receiverAction(t, "ou_operator", "actual-card", n.ActionToken, n.QueueID, n.Revision-1),
	} {
		if _, err := r.Action(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	actions, err := db.SubmissionActions(ctx, scope)
	if err != nil || len(actions) != 0 {
		t.Fatal("stale or unauthorized card claimed action", err)
	}
	e := receiverAction(t, "ou_operator", "actual-card", n.ActionToken, n.QueueID, n.Revision)
	r.ActionAllowed = func(string) bool { return false }
	response, err := r.Action(ctx, e)
	if err != nil || response.Toast.Type != "info" {
		t.Fatal("offline card had no feedback", err)
	}
	actions, err = db.SubmissionActions(ctx, scope)
	if err != nil || len(actions) != 0 {
		t.Fatal("disconnected card scheduled native action", err)
	}
	r.ActionAllowed = func(string) bool { return true }
	for i := 0; i < 2; i++ {
		if _, err := r.Action(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	actions, err = db.SubmissionActions(ctx, scope)
	if err != nil || len(actions) != 1 || actions[0].QueueID != "native-q" || actions[0].State != "ready" {
		t.Fatal("callback did not persist exactly one original-queue action", err)
	}
}
