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
	path, cwd string
	mu        sync.Mutex
	conn      *websocket.Conn
	methods   map[string]int
	accepted  atomic.Int32
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
			var result any
			switch in.Method {
			case "initialized":
				continue
			case "initialize":
				result = map[string]any{"userAgent": "local-version-fixture"}
			case "thread/resume", "thread/read":
				result = map[string]any{"thread": map[string]any{"id": "local-owned-thread", "cwd": f.cwd, "source": "cli", "status": map[string]string{"type": "idle"}}}
			case "thread/turns/list", "thread/queue/list", "thread/list", "thread/loaded/list":
				result = map[string]any{"data": []any{}, "nextCursor": nil}
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

// This receiver remains available during native incompatibility, matching the
// production offline-receive contract. Its methods have no remote side effects.
type serveVersionGateway struct {
	ready           atomic.Bool
	listens, groups atomic.Int32
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
func (*serveVersionGateway) MemberIDs(context.Context, string) ([]string, error) {
	return []string{"ou_fixture_owner_valid"}, nil
}
func (*serveVersionGateway) AddReaction(context.Context, string, string) (string, error) {
	return "local-reaction", nil
}
func (*serveVersionGateway) DeleteReaction(context.Context, string, string) error { return nil }
func (*serveVersionGateway) SendCard(context.Context, string, string, string, string) (string, error) {
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

func TestServeNativeFactoryRejectsInitialUnverifiedBeforeConnect(t *testing.T) {
	var discovers, dials atomic.Int32
	factory := serveNativeConnect("configured-local-cli", func(ctx context.Context, cli string) (codexclient.Daemon, error) {
		discovers.Add(1)
		if cli != "configured-local-cli" {
			t.Error("ignored configured CLI")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("discovery is unbounded")
		}
		return codexclient.Daemon{Status: "running", AppServerVersion: "0.163.0", SocketPath: "never-dial-this"}, nil
	}, func(context.Context, string) (*codexclient.Client, error) {
		dials.Add(1)
		return nil, errors.New("unexpected dial")
	})
	native, err := factory(context.Background())
	if native != nil || err == nil || !strings.Contains(err.Error(), "verified compatibility list") || discovers.Load() != 1 || dials.Load() != 0 {
		t.Fatal("initial incompatible daemon reached a native connection or lost rejection")
	}
}

func TestServeNativeFactoryAcceptsVerifiedDotOneWithLocalUnixWebSocket(t *testing.T) {
	root := t.TempDir()
	socket := newServeVersionSocket(t, root)
	var dials atomic.Int32
	factory := serveNativeConnect("local-cli", func(context.Context, string) (codexclient.Daemon, error) {
		return codexclient.Daemon{Status: "running", AppServerVersion: "0.162.1", SocketPath: socket.path}, nil
	}, func(ctx context.Context, path string) (*codexclient.Client, error) {
		dials.Add(1)
		return codexclient.Connect(ctx, path)
	})
	native, err := factory(context.Background())
	if err != nil || native == nil || dials.Load() != 1 || socket.accepted.Load() != 1 {
		t.Fatalf("verified 0.162.1 was not accepted by the local Unix WebSocket gate: native=%v err=%v dials=%d accepted=%d", native != nil, err, dials.Load(), socket.accepted.Load())
	}
	versioned, ok := native.(interface{ AppServerVersion() string })
	if !ok {
		t.Fatal("native wrapper does not expose the discovered daemon version")
	}
	if version := versioned.AppServerVersion(); version != "0.162.1" {
		t.Fatalf("native wrapper lost the discovered daemon version: %q", version)
	}
	native.Close()
}

func TestServeNativeFactoryCreationRequiresVerifiedDotOneBeforeConnect(t *testing.T) {
	root := t.TempDir()
	socket := newServeVersionSocket(t, root)
	var dials atomic.Int32
	factory := serveNativeConnect("local-cli", func(context.Context, string) (codexclient.Daemon, error) {
		return codexclient.Daemon{Status: "running", AppServerVersion: "0.162.0", SocketPath: socket.path}, nil
	}, func(ctx context.Context, path string) (*codexclient.Client, error) {
		dials.Add(1)
		return codexclient.Connect(ctx, path)
	}, true)
	native, err := factory(context.Background())
	if native != nil || err == nil || !strings.Contains(err.Error(), "creation protocol version is not verified") || dials.Load() != 0 || socket.accepted.Load() != 0 {
		t.Fatalf("creation-incompatible daemon reached native connect: native=%v err=%v dials=%d accepted=%d", native != nil, err, dials.Load(), socket.accepted.Load())
	}
}

func TestServeNativeFactoryRechecksVersionAfterDisconnectWithoutNativeWrites(t *testing.T) {
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
	var discovers, dials, rejections atomic.Int32
	factory := serveNativeConnect(cfg.Codex.CLIPath, func(_ context.Context, cli string) (codexclient.Daemon, error) {
		if cli != cfg.Codex.CLIPath {
			t.Error("ignored configured discovery CLI")
		}
		version := "0.163.0"
		if discovers.Add(1) == 1 {
			version = "0.162.0"
		}
		return codexclient.Daemon{Status: "running", AppServerVersion: version, SocketPath: socket.path}, nil
	}, func(ctx context.Context, path string) (*codexclient.Client, error) {
		dials.Add(1)
		return codexclient.Connect(ctx, path)
	})
	gateway := &serveVersionGateway{}
	service := &app.Service{Config: cfg, Store: db, Gateway: gateway, BotID: "ou_fixture_bot_valid", RetryInterval: 5 * time.Millisecond, Connect: func(ctx context.Context) (app.ServiceNative, error) {
		native, err := factory(ctx)
		if err != nil {
			if !strings.Contains(err.Error(), "verified compatibility list") {
				t.Error("connection factory lost version rejection")
			}
			rejections.Add(1)
		}
		return native, err
	}}
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
	socket.disconnect()
	serveVersionWait(t, ctx, func() bool { return rejections.Load() >= 2 && !service.NativeConnected() })
	attaches := socket.count("thread/resume")
	receipt, fresh, err := db.Claim(ctx, store.Receipt{AppID: cfg.Feishu.AppID, ThreadID: "local-owned-thread", ChatID: "local-chat", MessageID: "local-offline-input", Body: "owned local fixture input"})
	if err != nil || !fresh {
		t.Fatal("local offline input not persisted")
	}
	service.Wake("local-owned-thread")
	rejected := rejections.Load()
	serveVersionWait(t, ctx, func() bool { return rejections.Load() > rejected })
	health, err := service.Health(ctx)
	if err != nil || !health.Running || health.NativeConnected || health.HydratedBindings != 0 || !health.FeishuConnected {
		t.Fatal("incompatible generation reported native readiness or stopped offline receiver")
	}
	if dials.Load() != 1 || socket.accepted.Load() != 1 || socket.count("thread/resume") != attaches || gateway.groups.Load() != 0 || gateway.listens.Load() != 1 {
		t.Fatal("incompatible reconnect dialed, attached, created group or duplicated receiver")
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
