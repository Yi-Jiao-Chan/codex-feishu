package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/app"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/store"
	"github.com/coder/websocket"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// Local protocol fixture: real Unix WebSocket Client/Service, no daemon CLI,
// real credentials, native user threads, or Feishu connections.
type serveVersionSocket struct {
	path, cwd        string
	mu               sync.Mutex
	conn             *websocket.Conn
	methods          map[string]int
	accepted         atomic.Int32
	queueUnavailable atomic.Bool
}

func newServeVersionSocket(t *testing.T, cwd string) *serveVersionSocket {
	t.Helper()
	f := &serveVersionSocket{path: filepath.Join(t.TempDir(), "native.sock"), cwd: cwd, methods: map[string]int{}}
	listener, err := net.Listen("unix", f.path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		f.accepted.Add(1)
		f.mu.Lock()
		f.conn = conn
		f.mu.Unlock()
		for {
			_, b, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var in struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if json.Unmarshal(b, &in) != nil {
				return
			}
			f.mu.Lock()
			f.methods[in.Method]++
			f.mu.Unlock()
			if in.Method == "thread/queue/list" && f.queueUnavailable.Load() {
				reply, _ := json.Marshal(map[string]any{"id": in.ID, "error": map[string]any{"code": -32601, "message": "fixture method not found"}})
				if conn.Write(r.Context(), websocket.MessageText, reply) != nil {
					return
				}
				continue
			}
			var result any
			switch in.Method {
			case "initialized":
				continue
			case "initialize":
				result = map[string]any{"userAgent": "local-version-fixture"}
			case "thread/resume", "thread/read":
				result = map[string]any{"thread": map[string]any{"id": "local-owned-thread", "cwd": f.cwd, "source": "cli", "status": map[string]string{"type": "idle"}}}
			case "thread/turns/list", "thread/queue/list", "thread/list", "thread/loaded/list", "threadSection/list":
				result = map[string]any{"data": []any{}, "nextCursor": nil}
			case "thread/queue/add":
				result = map[string]any{"queuedSubmission": map[string]string{"id": "fixture-queue-id"}}
			default:
				t.Errorf("unexpected native fixture method %s", in.Method)
				return
			}
			b, _ = json.Marshal(map[string]any{"id": in.ID, "result": result})
			if conn.Write(r.Context(), websocket.MessageText, b) != nil {
				return
			}
		}
	})}
	go server.Serve(listener)
	t.Cleanup(func() { f.disconnect(); server.Close() })
	return f
}
func (f *serveVersionSocket) disconnect() {
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if conn != nil {
		conn.CloseNow()
	}
}
func (f *serveVersionSocket) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.methods[method]
}

// This receiver and its in-memory deliveries have no remote side effects.
type serveVersionGateway struct {
	ready                    atomic.Bool
	listens, groups, notices atomic.Int32
}

func (g *serveVersionGateway) Ready() bool { return g.ready.Load() }
func (g *serveVersionGateway) Listen(ctx context.Context, _ func(context.Context, *im.P2MessageReceiveV1) error, _ func(context.Context, *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error)) error {
	g.listens.Add(1)
	g.ready.Store(true)
	defer g.ready.Store(false)
	<-ctx.Done()
	return ctx.Err()
}
func (g *serveVersionGateway) CreateGroupWithDescription(context.Context, string, string, []string, string, string) (string, error) {
	g.groups.Add(1)
	return "", errors.New("unexpected group mutation")
}
func (*serveVersionGateway) BotInChat(context.Context, string) (bool, error) { return true, nil }
func (*serveVersionGateway) IsOneToOneGroup(context.Context, string, string) (bool, error) {
	return false, nil
}
func (*serveVersionGateway) MemberIDs(context.Context, string) ([]string, error) {
	return []string{"ou_fixture_owner_valid"}, nil
}
func (*serveVersionGateway) AddReaction(context.Context, string, string) (string, error) {
	return "local-reaction", nil
}
func (*serveVersionGateway) DeleteReaction(context.Context, string, string) error { return nil }
func (g *serveVersionGateway) SendCard(_ context.Context, kind, target, card, _ string) (string, error) {
	if kind == "open_id" {
		if target != "ou_fixture_owner_valid" || !strings.Contains(card, "任务队列") || !strings.Contains(card, "0.169.1") {
			return "", errors.New("unexpected compatibility notice")
		}
		g.notices.Add(1)
	}
	return "local-send", nil
}
func (*serveVersionGateway) ReplyCard(context.Context, string, string, string) (string, error) {
	return "local-reply", nil
}
func (*serveVersionGateway) UpdateCard(context.Context, string, string) error { return nil }

func serveVersionWait(t *testing.T, ctx context.Context, predicate func() bool) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !predicate() {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("local serve version fixture did not reach expected boundary")
		}
	}
}

func TestServeNativeFactoryConnectsAnyDiscoveredVersion(t *testing.T) {
	for _, version := range []string{"0.162.0", "0.162.1", "0.169.1"} {
		t.Run(version, func(t *testing.T) {
			socket := newServeVersionSocket(t, t.TempDir())
			factory := serveNativeConnect("local-cli", func(ctx context.Context, cli string) (codexclient.Daemon, error) {
				if _, bounded := ctx.Deadline(); cli != "local-cli" || !bounded {
					t.Error("ignored configured CLI or discovery deadline")
				}
				return codexclient.Daemon{Status: "running", AppServerVersion: version, SocketPath: socket.path}, nil
			}, codexclient.Connect)
			native, err := factory(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer native.Close()
			if socket.accepted.Load() != 1 || native.(interface{ AppServerVersion() string }).AppServerVersion() != version {
				t.Fatal("lost connected server version")
			}
			if queue, err := native.Queue(context.Background(), "local-owned-thread"); err != nil || len(queue) != 0 {
				t.Fatal("read-only queue check failed", err)
			}
			if queued, err := native.Add(context.Background(), "local-owned-thread", "fixture", "fixture-client"); err != nil || queued.ID != "fixture-queue-id" {
				t.Fatal("compatible queue submission was version-gated", err)
			}
		})
	}
}

func TestServeReconnectsAfterUpgradeAndNotifiesOnceWhenQueueIsMissing(t *testing.T) {
	path, _, _, _ := doctorFixture(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Discovery.Enabled = false
	db, err := store.Open(cfg.Storage.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	root := filepath.Dir(path)
	if _, err := db.BeginBaseline(ctx, cfg.Host.ID, cfg.Feishu.AppID, 100); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteBaseline(ctx, cfg.Host.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.ObserveNew(ctx, cfg.Host.ID, "local-owned-thread", root, 101); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkEligible(ctx, cfg.Host.ID, "local-owned-thread", "fixture", root, root); err != nil {
		t.Fatal(err)
	}
	intent, err := db.PrepareGroup(ctx, cfg.Host.ID, "local-owned-thread", cfg.Access.OwnerOpenID, "local fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ClaimGroupDispatch(ctx, intent.ID); err != nil || !ok {
		t.Fatal("local group seed claim failed")
	}
	if err := db.ConfirmGroup(ctx, intent.ID, "local-chat"); err != nil {
		t.Fatal(err)
	}
	if err := db.BindConfirmedGroup(ctx, intent.ID); err != nil {
		t.Fatal(err)
	}
	socket := newServeVersionSocket(t, root)
	var discovers, dials atomic.Int32
	factory := serveNativeConnect(cfg.Codex.CLIPath, func(_ context.Context, cli string) (codexclient.Daemon, error) {
		if cli != cfg.Codex.CLIPath {
			t.Error("ignored configured discovery CLI")
		}
		version := "0.169.1"
		if discovers.Add(1) == 1 {
			version = "0.162.0"
		}
		return codexclient.Daemon{Status: "running", AppServerVersion: version, SocketPath: socket.path}, nil
	}, func(ctx context.Context, path string) (*codexclient.Client, error) {
		dials.Add(1)
		return codexclient.Connect(ctx, path)
	})
	gateway := &serveVersionGateway{}
	service := &app.Service{Config: cfg, Store: db, Gateway: gateway, BotID: "ou_fixture_bot_valid", RetryInterval: 5 * time.Millisecond, Connect: factory}
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("local service workers did not join")
		}
	})
	serveVersionWait(t, ctx, func() bool {
		health, err := service.Health(ctx)
		return err == nil && health.NativeConnected && health.HydratedBindings == 1 && socket.count("thread/resume") == 1
	})
	socket.queueUnavailable.Store(true)
	initialHistory := socket.count("thread/turns/list")
	socket.disconnect()
	receipt, fresh, err := db.Claim(ctx, store.Receipt{AppID: cfg.Feishu.AppID, ThreadID: "local-owned-thread", ChatID: "local-chat", MessageID: "local-offline-input", Body: "owned local fixture input"})
	if err != nil || !fresh {
		t.Fatal("local input not persisted")
	}
	service.Wake("local-owned-thread")
	serveVersionWait(t, ctx, func() bool {
		return dials.Load() == 2 && service.NativeConnected() && socket.count("thread/turns/list") > initialHistory && gateway.notices.Load() == 1
	})
	historyBeforeRetry := socket.count("thread/turns/list")
	socket.disconnect()
	serveVersionWait(t, ctx, func() bool {
		return dials.Load() == 3 && service.NativeConnected() && socket.count("thread/turns/list") > historyBeforeRetry
	})
	var notices int
	if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM command_replies WHERE kind=?`, store.CompatibilityNoticeKind).Scan(&notices); err != nil || notices != 1 {
		t.Fatal("reconnect staged more than one notice", err)
	}
	health, err := service.Health(ctx)
	if err != nil || !health.NativeConnected || !health.FeishuConnected || gateway.notices.Load() != 1 || gateway.groups.Load() != 0 || gateway.listens.Load() != 1 {
		t.Fatal("queue failure stopped sync, duplicated the receiver, or repeated its notice")
	}
	for _, method := range []string{"thread/start", "thread/queue/add", "thread/queue/delete", "thread/queue/start", "turn/steer", "turn/interrupt"} {
		if socket.count(method) != 0 {
			t.Fatal("incompatible generation issued a native mutation")
		}
	}
	rows, err := db.Receipts(ctx, "local-owned-thread")
	if err != nil || len(rows) != 1 || rows[0].ID != receipt.ID || rows[0].State != "received" {
		t.Fatal("offline input was lost or falsely submitted")
	}
}
