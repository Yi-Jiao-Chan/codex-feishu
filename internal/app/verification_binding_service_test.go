package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/store"
	"codex-feishu/internal/verification"
)

type bindingServiceNative struct {
	*serviceNative
	connection string
	done       chan struct{}
	joins      atomic.Int32
}

func (n *bindingServiceNative) GenerationID() uint64  { return 7 }
func (n *bindingServiceNative) ConnectionID() string  { return n.connection }
func (n *bindingServiceNative) Done() <-chan struct{} { return n.done }
func (n *bindingServiceNative) StartLocalVerification(_ context.Context, spec codexclient.LocalVerificationSpec) (codexclient.Thread, error) {
	thread := codexclient.Thread{ID: "thread", CWD: spec.CWD, ThreadSource: json.RawMessage(runtimeVerificationSource)}
	n.mu.Lock()
	n.threads[thread.ID] = thread
	n.mu.Unlock()
	return thread, nil
}
func (n *bindingServiceNative) AttachLocalVerification(ctx context.Context, id string, _ codexclient.LocalVerificationSpec) (codexclient.Thread, error) {
	n.joins.Add(1)
	return n.Metadata(ctx, id)
}

type bindingServiceGateway struct {
	*serviceGateway
	mu          sync.Mutex
	roster      []string
	rosterReads atomic.Int32
}

func (g *bindingServiceGateway) CreateGroupWithDescription(ctx context.Context, name, owner string, members []string, uuid, _ string) (string, error) {
	return g.CreateGroup(ctx, name, owner, members, uuid)
}

func (g *bindingServiceGateway) CreateGroup(context.Context, string, string, []string, string) (string, error) {
	return "chat", nil
}
func (g *bindingServiceGateway) MemberIDs(context.Context, string) ([]string, error) {
	g.rosterReads.Add(1)
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.roster...), nil
}
func (g *bindingServiceGateway) setRoster(ids ...string) {
	g.mu.Lock()
	g.roster = ids
	g.mu.Unlock()
}

// Provision only temporary local resources with mocked native and Feishu APIs.
// No model request, daemon connection, or real remote creation is performed.
func bindingServiceFixture(t *testing.T) (*Service, *bindingServiceNative, *bindingServiceGateway, *Receiver) {
	t.Helper()
	db, native, _, runtime := runtimeFixture(t)
	n := &bindingServiceNative{serviceNative: &serviceNative{runtimeNative: native}, connection: store.ID(), done: make(chan struct{})}
	g := &bindingServiceGateway{serviceGateway: &serviceGateway{runtimeGateway: &runtimeGateway{}}, roster: []string{"owner", "ou_bot"}}
	g.ready.Store(true)
	var cfg config.Config
	cfg.Host.ID, cfg.Feishu.AppID = runtime.HostID, runtime.AppID
	cfg.Access.OwnerOpenID = "owner"
	cfg.Access.OperatorOpenIDs = []string{"owner", "foreign"}
	cfg.Discovery.AllowedRoots = runtime.AllowedRoots
	cfg.Notifications.Timezone = "UTC"
	s := &Service{Config: cfg, Store: db, Gateway: g, BotID: "ou_bot", Now: func() time.Time { return time.UnixMilli(1_000_000) }}
	ctx := context.Background()
	if _, err := db.BeginBaseline(ctx, cfg.Host.ID, cfg.Feishu.AppID, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteBaseline(ctx, cfg.Host.ID, nil); err != nil {
		t.Fatal(err)
	}
	manager := &verification.NativeManager{Store: db, HostID: cfg.Host.ID, AppID: cfg.Feishu.AppID, OwnerID: "owner", Commit: "fixture", BinarySHA256: strings.Repeat("b", 64), Current: func() verification.NativeCreator { return n }, Now: s.Now}
	root := filepath.Join(t.TempDir(), "resources")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	sessions, err := verification.NewNativeSessions(ctx, manager, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sessions.Close() })
	groups := &verification.GroupManager{Store: db, Gateway: g, HostID: cfg.Host.ID, AppID: cfg.Feishu.AppID, OwnerID: "owner", BotID: "ou_bot", Commit: manager.Commit, BinarySHA256: manager.BinarySHA256, Ready: g.Ready, Now: s.Now}
	id := store.ID()
	if _, err := groups.Begin(ctx, id); err != nil {
		t.Fatal(err)
	}
	if ok, err := groups.Message(ctx, "owner", id, "fixture-owner-proof", "chat"); err != nil || !ok {
		t.Fatalf("owner proof: %v %v", ok, err)
	}
	if _, err := sessions.Begin(ctx, id); err != nil {
		t.Fatal(err)
	}
	s.VerificationBindings = &verification.BindingManager{Sessions: sessions, Groups: groups}
	if result, err := s.VerificationBindings.Begin(ctx, id); err != nil || result.BindingStatus != "pass" {
		t.Fatalf("binding: %s %v", result.BindingStatus, err)
	}
	r := &Receiver{Store: db, Config: cfg, BotID: "ou_bot", BeginBound: s.beginBound, TaskAction: s.claimTask, ActionAllowed: s.actionAvailable}
	return s, n, g, r
}

func TestServiceVerificationBindingWiresNativeClaimsReadinessAndDelivery(t *testing.T) {
	s, n, g, receiver := bindingServiceFixture(t)
	ctx := context.Background()
	runtime := s.generation(n)
	s.nativeConnected.Store(true)
	meta, _ := n.Metadata(ctx, "thread")
	if err := runtime.Authorize(ctx, meta, false); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Authorize(ctx, meta, true); err == nil {
		t.Fatal("automatic use accepted")
	}
	if err := runtime.HistoryStaged(ctx, "thread", nil); err != nil || !s.actionAvailable("thread") {
		t.Fatalf("valid history was not ready: %v", err)
	}
	if err := receiver.Message(ctx, receiverMessage(t, "chat", "owner", "input", "group", true)); err != nil {
		t.Fatal(err)
	}
	joins := n.joins.Load()
	if err := runtime.ActorTick(ctx, "thread"); err != nil {
		t.Fatal(err)
	}
	if n.adds.Load() != 1 || n.joins.Load() <= joins {
		t.Fatalf("controller write did not reacquire guard: adds=%d joins=%d/%d", n.adds.Load(), joins, n.joins.Load())
	}
	b, _ := s.Store.Binding(ctx, s.Config.Host.ID, "thread")
	if err := runtime.CanDeliver(ctx, b); err != nil {
		t.Fatal(err)
	}
	b.ChatID = "foreign-chat"
	if err := runtime.CanDeliver(ctx, b); err == nil {
		t.Fatal("delivery accepted a different chat")
	}
	g.setRoster("owner", "ou_bot", "foreign")
	if err := runtime.HistoryStaged(ctx, "thread", nil); err == nil || s.actionAvailable("thread") {
		t.Fatal("failed authorization retained readiness")
	}
	if _, _, err := s.Store.Claim(ctx, store.Receipt{AppID: s.Config.Feishu.AppID, ThreadID: "thread", ChatID: "chat", MessageID: "pending-fixture", Body: "fixture input"}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.ActorTick(ctx, "thread"); err == nil {
		t.Fatal("expired write authority accepted a native claim")
	}
	if n.adds.Load() != 1 {
		t.Fatal("unauthorized native add occurred")
	}
	meta.ThreadSource, meta.Source = json.RawMessage(`"user"`), json.RawMessage(`"cli"`)
	if err := runtime.Authorize(ctx, meta, false); err == nil {
		t.Fatal("ordinary metadata bypassed registered marker")
	}
	b.ChatID = "chat"
	if err := runtime.CanDeliver(ctx, b); err == nil {
		t.Fatal("changed roster permitted delivery")
	}
	scope := store.SubmissionScope{HostID: s.Config.Host.ID, AppID: s.Config.Feishu.AppID, ThreadID: "thread"}
	attempts, err := s.Store.SubmissionAttempts(ctx, scope)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("unauthorized write acquired attempt: %d %v", len(attempts), err)
	}
}

func TestServiceVerificationBindingRejectsEveryInboundMutation(t *testing.T) {
	for _, mode := range []string{"nil_manager", "changed_roster", "foreign_operator", "receiver_without_guard"} {
		t.Run(mode, func(t *testing.T) {
			s, n, g, receiver := bindingServiceFixture(t)
			ctx := context.Background()
			runtime := s.generation(n)
			s.nativeConnected.Store(true)
			s.actionsReady["thread"] = true // Old readiness is deliberately stale.
			user := "owner"
			switch mode {
			case "nil_manager":
				s.VerificationBindings = nil
			case "changed_roster":
				g.setRoster("owner", "ou_bot", "foreign")
			case "foreign_operator":
				user = "foreign"
			case "receiver_without_guard":
				receiver.BeginBound = nil
			}
			commands, tasks := 0, 0
			receiver.Command = func(context.Context, InboundCommand) error { commands++; return nil }
			receiver.TaskAction = func(context.Context, string, string, string, string, int64) (string, error) {
				tasks++
				return "thread", nil
			}
			_ = receiver.Message(ctx, receiverMessage(t, "chat", user, "input", "group", true))
			command := receiverMessage(t, "chat", user, "command", "group", true)
			command.Event.Message.Content = strPointer(`{"text":"@_user_1 /stop"}`)
			_ = receiver.Message(ctx, command)
			for _, kind := range []string{"stop", "continue", "cancel_pending", "steer", "remove"} {
				event := receiverAction(t, user, "actual-card", "fixture-token", "native-queue", 1)
				event.Event.Action.Value["action"] = kind
				if kind == "cancel_pending" {
					event.Event.Action.Value["queue_id"] = ""
				}
				if _, err := receiver.Action(ctx, event); err != nil {
					t.Fatal(err)
				}
			}
			if commands != 0 || tasks != 0 {
				t.Fatalf("guard bypassed: commands=%d tasks=%d", commands, tasks)
			}
			if mode == "nil_manager" || mode == "changed_roster" {
				s.actionsReady["thread"] = true
				if _, err := s.claimTask(ctx, "chat", "actual-card", "stop", "fixture-token", 1); err == nil || s.actionsReady["thread"] {
					t.Fatal("independent task claim retained invalid authority")
				}
				meta, _ := n.Metadata(ctx, "thread")
				if err := runtime.Authorize(ctx, meta, false); err == nil {
					t.Fatal("invalid manager accepted runtime metadata")
				}
			}
			for _, table := range []string{"m0_receipts", "submission_actions", "task_control_claims"} {
				var count int
				if err := s.Store.DB.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("%s changed: %d %v", table, count, err)
				}
			}
		})
	}
}

func TestServiceVerificationActorRejectsFirstWriteWithoutClaimingAttempt(t *testing.T) {
	s, n, g, _ := bindingServiceFixture(t)
	ctx := context.Background()
	if _, _, err := s.Store.Claim(ctx, store.Receipt{AppID: s.Config.Feishu.AppID, ThreadID: "thread", ChatID: "chat", MessageID: "pending-fixture", Body: "fixture input"}); err != nil {
		t.Fatal(err)
	}
	runtime := s.generation(n)
	g.setRoster("owner", "ou_bot", "foreign")
	if err := runtime.ActorTick(ctx, "thread"); err == nil {
		t.Fatal("invalid actor write was accepted")
	}
	scope := store.SubmissionScope{HostID: s.Config.Host.ID, AppID: s.Config.Feishu.AppID, ThreadID: "thread"}
	attempts, err := s.Store.SubmissionAttempts(ctx, scope)
	if err != nil || len(attempts) != 0 || n.adds.Load() != 0 {
		t.Fatalf("invalid write claimed/sent: attempts=%d adds=%d err=%v", len(attempts), n.adds.Load(), err)
	}
	receipts, err := s.Store.Receipts(ctx, "thread")
	if err != nil || len(receipts) != 1 || receipts[0].State != "received" {
		t.Fatalf("pending receipt was lost: %+v %v", receipts, err)
	}
}

func strPointer(value string) *string { return &value }

func TestReceiverBoundGuardOwnsContextUntilMutationCompletes(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "canceled"}[canceled], func(t *testing.T) {
			r, db := receiverFixture(t)
			type contextKey struct{}
			releases, commands, tasks, guards := 0, 0, 0, 0
			r.BeginBound = func(ctx context.Context, _ store.Binding, _ string) (context.Context, func(), error) {
				guards++
				opctx, cancel := context.WithCancel(context.WithValue(ctx, contextKey{}, true))
				if canceled {
					cancel()
				}
				return opctx, func() { releases++; cancel() }, nil
			}
			r.Command = func(ctx context.Context, _ InboundCommand) error {
				commands++
				if ctx.Value(contextKey{}) != true || releases != guards-1 {
					t.Fatal("command did not retain operation context")
				}
				return nil
			}
			r.TaskAction = func(ctx context.Context, _, _, _, _ string, _ int64) (string, error) {
				tasks++
				if ctx.Value(contextKey{}) != true || releases != guards-1 {
					t.Fatal("task did not retain operation context")
				}
				return "thread", nil
			}
			event := receiverMessage(t, "chat", "ou_operator", "command", "group", true)
			event.Event.Message.Content = strPointer(`{"text":"@_user_1 /stop"}`)
			_ = r.Message(context.Background(), event)
			_ = r.Message(context.Background(), receiverMessage(t, "chat", "ou_operator", "input", "group", true))
			for _, kind := range []string{"stop", "continue", "cancel_pending", "steer", "remove"} {
				event := receiverAction(t, "ou_operator", "actual-card", "token", "queue", 1)
				event.Event.Action.Value["action"] = kind
				if kind == "cancel_pending" {
					event.Event.Action.Value["queue_id"] = ""
				}
				if _, err := r.Action(context.Background(), event); err != nil {
					t.Fatal(err)
				}
			}
			if releases != guards || guards != 7 {
				t.Fatalf("scope leak: guards=%d releases=%d", guards, releases)
			}
			receipts, err := db.Receipts(context.Background(), "thread")
			if err != nil {
				t.Fatal(err)
			}
			if canceled && (len(receipts) != 0 || commands != 0 || tasks != 0) {
				t.Fatal("canceled guard performed mutation")
			}
			if !canceled && (len(receipts) != 1 || commands != 1 || tasks != 2) {
				t.Fatal("ordinary valid guard changed routing")
			}
		})
	}
}

func TestServiceVerificationOrdinaryOperationDoesNotRequireManager(t *testing.T) {
	db, cfg, root := serviceFixture(t)
	g := &serviceGateway{runtimeGateway: &runtimeGateway{}}
	s := &Service{Config: cfg, Store: db, Gateway: g, BotID: "ou_bot"}
	ctx := context.Background()
	opctx, release, handled, err := s.beginVerificationOperation(ctx, "thread")
	if err != nil || handled || opctx != ctx || release == nil {
		t.Fatalf("ordinary operation changed: handled=%v err=%v", handled, err)
	}
	release()
	runtime := s.generation(serviceTestNative(root))
	meta := codexclient.Thread{ID: "thread", CWD: root, Source: json.RawMessage(`"cli"`)}
	if err := runtime.Authorize(ctx, meta, false); err != nil {
		t.Fatal(err)
	}
	b, _ := db.Binding(ctx, cfg.Host.ID, "thread")
	if err := s.canDeliver(ctx, b); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, release, _, err := s.beginVerificationOperation(canceled, "thread"); err == nil {
		if release != nil {
			release()
		}
		t.Fatal("canceled operation returned usable authority")
	}
}

func TestServiceVerificationRejectsManagerForAnotherStore(t *testing.T) {
	s, n, _, _ := bindingServiceFixture(t)
	other, otherNative, _, _ := bindingServiceFixture(t)
	s.VerificationBindings = other.VerificationBindings
	ctx := context.Background()
	joins := otherNative.joins.Load()
	opctx, release, handled, err := s.beginVerificationOperation(ctx, "thread")
	if release != nil {
		release()
	}
	if err == nil || !handled || opctx != nil || otherNative.joins.Load() != joins {
		t.Fatal("another store authorized this service", handled, err)
	}
	runtime := s.generation(n)
	meta, _ := n.Metadata(ctx, "thread")
	if err := runtime.Authorize(ctx, meta, false); err == nil || otherNative.joins.Load() != joins {
		t.Fatal("runtime accepted another store's proof")
	}
	if _, _, err := s.Store.Claim(ctx, store.Receipt{AppID: s.Config.Feishu.AppID, ThreadID: "thread", ChatID: "chat", MessageID: "wrong-manager-pending", Body: "fixture input"}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.ActorTick(ctx, "thread"); err == nil || n.adds.Load() != 0 {
		t.Fatal("wrong manager allowed native write")
	}
	attempts, err := s.Store.SubmissionAttempts(ctx, store.SubmissionScope{HostID: s.Config.Host.ID, AppID: s.Config.Feishu.AppID, ThreadID: "thread"})
	if err != nil || len(attempts) != 0 {
		t.Fatal("wrong manager claimed attempt", err)
	}
}
