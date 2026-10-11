package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/store"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

type serviceNative struct {
	*runtimeNative
	qmu          sync.Mutex
	queue        []codexclient.Queued
	adds, closes atomic.Int32
	queueReads   atomic.Int32
}

func (n *serviceNative) Add(_ context.Context, thread, body, client string) (codexclient.Queued, error) {
	if thread != "thread" {
		return codexclient.Queued{}, errors.New("wrong thread")
	}
	n.adds.Add(1)
	input, _ := json.Marshal(codexclient.TextInput(body))
	q := codexclient.Queued{ID: "native-queued", Input: input, ClientUserMessageID: &client}
	n.qmu.Lock()
	n.queue = append(n.queue, q)
	n.qmu.Unlock()
	return q, nil
}
func (n *serviceNative) Queue(context.Context, string) ([]codexclient.Queued, error) {
	n.queueReads.Add(1)
	n.qmu.Lock()
	defer n.qmu.Unlock()
	return append([]codexclient.Queued(nil), n.queue...), nil
}
func (*serviceNative) Delete(context.Context, string, string) (bool, error) {
	return false, errors.New("unexpected delete")
}
func (*serviceNative) Steer(context.Context, string, string, string, string) error {
	return errors.New("unexpected steer")
}
func (*serviceNative) Interrupt(context.Context, string, string) error {
	return errors.New("unexpected interrupt")
}
func (*serviceNative) StartQueue(context.Context, string) error {
	return errors.New("unexpected queue start")
}
func (n *serviceNative) Close() { n.closes.Add(1) }

type serviceGateway struct {
	*runtimeGateway
	ready      atomic.Bool
	listens    atomic.Int32
	muFeedback sync.Mutex
	reactions  []string
	listening  chan struct{}
	message    func(context.Context, *im.P2MessageReceiveV1) error
	action     func(context.Context, *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error)
}

func (g *serviceGateway) Ready() bool                                   { return g.ready.Load() }
func (*serviceGateway) BotInChat(context.Context, string) (bool, error) { return true, nil }
func (*serviceGateway) MemberIDs(context.Context, string) ([]string, error) {
	return []string{"ou_operator"}, nil
}
func (g *serviceGateway) Listen(ctx context.Context, message func(context.Context, *im.P2MessageReceiveV1) error, action func(context.Context, *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error)) error {
	g.listens.Add(1)
	g.message, g.action = message, action
	g.ready.Store(true)
	close(g.listening)
	<-ctx.Done()
	g.ready.Store(false)
	return ctx.Err()
}
func (g *serviceGateway) AddReaction(_ context.Context, _, emoji string) (string, error) {
	g.muFeedback.Lock()
	defer g.muFeedback.Unlock()
	g.reactions = append(g.reactions, emoji)
	return "reaction-" + emoji, nil
}
func (*serviceGateway) DeleteReaction(context.Context, string, string) error { return nil }

func serviceFixture(t *testing.T) (*store.Store, config.Config, string) {
	t.Helper()
	root := t.TempDir()
	db, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var cfg config.Config
	cfg.SchemaVersion = 1
	cfg.Host.ID = "host"
	cfg.Codex.CLIPath = "auto"
	cfg.Codex.Endpoint = "auto"
	cfg.Feishu.AppID = "cli_service_app"
	cfg.Feishu.AppSecretFile = filepath.Join(root, "secret")
	cfg.Access.OwnerOpenID = "ou_operator"
	cfg.Access.OperatorOpenIDs = []string{"ou_operator"}
	cfg.Discovery.AllowedRoots = []string{root}
	cfg.Discovery.FirstRun = "baseline"
	cfg.Discovery.ScanIntervalSeconds = 15
	cfg.Notifications.Timezone = "Asia/Shanghai"
	cfg.Notifications.TargetOpenIDs = []string{"ou_operator"}
	cfg.Notifications.MentionOn = []string{"completed", "failed", "needs_user"}
	cfg.Sync.CardUpdateMinIntervalMS = 100
	cfg.Storage.DatabaseFile = filepath.Join(root, "unused.db")
	ctx := context.Background()
	if _, err := db.BeginBaseline(ctx, "host", cfg.Feishu.AppID, 100); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteBaseline(ctx, "host", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.ObserveNew(ctx, "host", "thread", root, 101); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkEligible(ctx, "host", "thread", "first-input", root, root); err != nil {
		t.Fatal(err)
	}
	i, err := db.PrepareGroup(ctx, "host", "thread", cfg.Access.OwnerOpenID, "test", nil)
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
	return db, cfg, root
}

func serviceTestNative(root string) *serviceNative {
	n := &serviceNative{runtimeNative: &runtimeNative{threads: map[string]codexclient.Thread{}, turns: map[string][]codexclient.Turn{}, events: make(chan codexclient.Event, 64)}}
	n.set("thread", root, "inProgress")
	return n
}

func TestServiceReceivesOfflineAndReconnectsWithoutSecondReceiverOrAdd(t *testing.T) {
	db, cfg, root := serviceFixture(t)
	first, second := serviceTestNative(root), serviceTestNative(root)
	g := &serviceGateway{runtimeGateway: &runtimeGateway{}, listening: make(chan struct{})}
	var allowNative atomic.Bool
	var connections atomic.Int32
	s := &Service{Config: cfg, Store: db, Gateway: g, BotID: "ou_bot", RetryInterval: 5 * time.Millisecond, Connect: func(context.Context) (ServiceNative, error) {
		if !allowNative.Load() {
			return nil, errors.New("test offline")
		}
		if connections.Add(1) == 1 {
			return first, nil
		}
		if first.closes.Load() != 1 {
			return nil, errors.New("prior generation still running")
		}
		return second, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case <-g.listening:
	case err := <-done:
		t.Fatal("service stopped", err)
	case <-time.After(3 * time.Second):
		t.Fatal("receiver missing")
	}
	if s.NativeConnected() {
		t.Fatal("offline native reported connected")
	}
	if err := g.message(ctx, receiverMessage(t, "chat", "ou_operator", "offline-input", "group", true)); err != nil {
		t.Fatal(err)
	}
	runtimeWait(t, func() bool {
		rs, err := db.Receipts(ctx, "thread")
		if err != nil || len(rs) != 1 {
			return false
		}
		emoji, state, err := db.ReactionFeedbackState(ctx, rs[0].ID)
		return err == nil && emoji == "Get" && state == "confirmed"
	})
	allowNative.Store(true)
	runtimeWait(t, func() bool {
		ns, err := db.Submissions(ctx, store.SubmissionScope{HostID: "host", AppID: cfg.Feishu.AppID, ThreadID: "thread"})
		return err == nil && len(ns) == 1 && ns[0].State == "queued" && first.adds.Load() == 1
	})
	ns, _ := db.Submissions(ctx, store.SubmissionScope{HostID: "host", AppID: cfg.Feishu.AppID, ThreadID: "thread"})
	key := store.QueueCardKey(ns[0].ID)
	runtimeWait(t, func() bool {
		m, err := db.Mirror(ctx, "host", "thread", key)
		return err == nil && m.State == "confirmed" && m.ParentMessageID == "offline-input"
	})
	q, _ := first.Queue(ctx, "thread")
	second.qmu.Lock()
	second.queue = q
	second.qmu.Unlock()
	close(first.events)
	runtimeWait(t, func() bool { return connections.Load() >= 2 && s.NativeConnected() })
	// The second generation reconciles the same client/queue identity. A
	// duplicated ws callback must also keep exactly that original receipt.
	if err := g.message(ctx, receiverMessage(t, "chat", "ou_operator", "offline-input", "group", true)); err != nil {
		t.Fatal(err)
	}
	runtimeWait(t, func() bool {
		ns, err := db.Submissions(ctx, store.SubmissionScope{HostID: "host", AppID: cfg.Feishu.AppID, ThreadID: "thread"})
		return err == nil && len(ns) == 1 && ns[0].QueueID == "native-queued" && second.queueReads.Load() >= 1 && s.actionAvailable("thread")
	})
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not join workers")
	}
	if g.listens.Load() != 1 || first.closes.Load() != 1 || second.closes.Load() != 1 || second.adds.Load() != 0 {
		t.Fatal("reconnect duplicated receiver/input or leaked generation")
	}
	rs, _ := db.Receipts(context.Background(), "thread")
	if len(rs) != 1 {
		t.Fatal("offline callback replay duplicated input")
	}
	ms, _ := db.Mirrors(context.Background(), "host", "thread")
	for _, m := range ms {
		if m.Kind == "human" && m.ParentMessageID == "offline-input" {
			t.Fatal("Feishu input echoed as bot human card")
		}
	}
}

func TestServiceButtonsRequireReconciliationInCurrentGeneration(t *testing.T) {
	s := &Service{}
	s.nativeConnected.Store(true)
	if s.actionAvailable("thread") {
		t.Fatal("socket handshake enabled stale controls")
	}
	s.actionsReady = map[string]bool{"thread": true}
	if !s.actionAvailable("thread") || s.actionAvailable("other") {
		t.Fatal("fresh thread readiness was not scoped")
	}
	s.nativeConnected.Store(false)
	if s.actionAvailable("thread") {
		t.Fatal("disconnected generation retained actionable controls")
	}
}
