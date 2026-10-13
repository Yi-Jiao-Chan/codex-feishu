package app

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

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
	"github.com/coder/websocket"
)

// A local protocol peer exercises real Client framing, socket loss, Service
// generations and history pagination. It is not the shared native daemon;
// the Feishu gateway remains the existing in-memory service fixture.
type serviceRecoveryWire struct {
	path, cwd string
	mu        sync.Mutex
	conn      *websocket.Conn
	turns     []codexclient.Turn
	methods   map[string]int
	accepts   atomic.Int32
}

func newServiceRecoveryWire(t *testing.T, cwd string, turns []codexclient.Turn) *serviceRecoveryWire {
	t.Helper()
	f := &serviceRecoveryWire{path: filepath.Join(t.TempDir(), "native.sock"), cwd: cwd, turns: turns, methods: map[string]int{}}
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
		f.accepts.Add(1)
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
				Params struct {
					ThreadID     string `json:"threadId"`
					Cursor       string `json:"cursor"`
					ExcludeTurns bool   `json:"excludeTurns"`
					ItemsView    string `json:"itemsView"`
				} `json:"params"`
			}
			if json.Unmarshal(b, &in) != nil {
				return
			}
			f.mu.Lock()
			f.methods[in.Method]++
			turns := append([]codexclient.Turn(nil), f.turns...)
			f.mu.Unlock()
			var result any
			switch in.Method {
			case "initialize":
				result = map[string]any{"userAgent": "service-wire-recovery-fixture"}
			case "initialized":
				continue
			case "thread/resume", "thread/read":
				if in.Params.ThreadID != "thread" || in.Method == "thread/resume" && !in.Params.ExcludeTurns {
					t.Error("local protocol attach/read scope mismatch")
					return
				}
				result = map[string]any{"thread": map[string]any{"id": "thread", "cwd": f.cwd, "source": "cli", "status": map[string]string{"type": "idle"}}}
			case "thread/turns/list":
				if in.Params.ThreadID != "thread" || in.Params.ItemsView != "full" {
					t.Error("local protocol history scope or view mismatch")
					return
				}
				// One turn per page: reconnect must read beyond the old first page.
				if in.Params.Cursor == "offline-turn-page" && len(turns) == 2 {
					result = map[string]any{"data": turns[1:], "nextCursor": nil}
				} else if in.Params.Cursor == "" {
					var next any
					if len(turns) > 1 {
						next = "offline-turn-page"
					}
					result = map[string]any{"data": turns[:1], "nextCursor": next}
				} else {
					t.Error("unexpected local history cursor")
					return
				}
			case "thread/queue/list":
				if in.Params.ThreadID != "thread" {
					t.Error("local queue scope mismatch")
					return
				}
				result = map[string]any{"data": []any{}, "nextCursor": nil}
			case "thread/list", "thread/loaded/list":
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
func (f *serviceRecoveryWire) disconnect() {
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if conn != nil {
		conn.CloseNow()
	}
}
func (f *serviceRecoveryWire) history(turns []codexclient.Turn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.turns = append([]codexclient.Turn(nil), turns...)
}
func (f *serviceRecoveryWire) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.methods[method]
}

func serviceRecoveryTurn(id, human, final string) codexclient.Turn {
	text, _ := json.Marshal(map[string]string{"type": "text", "text": human})
	phase := "final_answer"
	at := time.Now().Add(-time.Minute).Unix()
	return codexclient.Turn{ID: id, Status: "completed", ItemsView: "full", StartedAt: &at, CompletedAt: &at, Items: []codexclient.Item{
		{ID: id + "-human", Type: "userMessage", Content: []json.RawMessage{text}},
		{ID: id + "-final", Type: "agentMessage", Text: final, Phase: &phase},
	}}
}

func serviceRecoveryMirrors(db *store.Store, ctx context.Context, expected int) (map[string]store.Mirror, bool) {
	rows, err := db.Mirrors(ctx, "host", "thread")
	if err != nil || len(rows) != expected {
		return nil, false
	}
	mirrors := make(map[string]store.Mirror, len(rows))
	for _, m := range rows {
		if m.State != "confirmed" || m.RemoteID == "" || m.Revision != m.DeliveredRevision {
			return nil, false
		}
		mirrors[m.Key] = m
	}
	return mirrors, true
}

func TestServiceWireDisconnectRehydratesOfflinePublicHistoryOnce(t *testing.T) {
	db, cfg, root := serviceFixture(t)
	old := serviceRecoveryTurn("owned-old-turn", "owned old public input", "owned old final reply")
	added := serviceRecoveryTurn("owned-offline-turn", "owned offline public input", "owned offline final reply")
	wire := newServiceRecoveryWire(t, root, []codexclient.Turn{old})
	gateway := &serviceGateway{runtimeGateway: &runtimeGateway{}, listening: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	var allowConnect atomic.Bool
	allowConnect.Store(true)
	var dials atomic.Int32
	service := &Service{Config: cfg, Store: db, Gateway: gateway, BotID: "ou_bot", RetryInterval: 5 * time.Millisecond, Connect: func(ctx context.Context) (ServiceNative, error) {
		if !allowConnect.Load() {
			return nil, errors.New("local reconnect gate held")
		}
		dials.Add(1)
		return codexclient.Connect(ctx, wire.path)
	}}
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	stop := func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Error("local service did not stop through cancellation")
			}
		case <-time.After(3 * time.Second):
			t.Error("local wire service workers did not join")
		}
	}
	var stopOnce sync.Once
	t.Cleanup(func() { stopOnce.Do(stop) })
	runtimeWait(t, func() bool {
		_, ok := serviceRecoveryMirrors(db, ctx, 2)
		return ok && service.NativeConnected() && service.actionAvailable("thread")
	})
	originals, ok := serviceRecoveryMirrors(db, ctx, 2)
	if !ok {
		t.Fatal("initial public history not confirmed")
	}
	binding, err := db.Binding(ctx, cfg.Host.ID, "thread")
	if err != nil {
		t.Fatal("initial fixture binding missing")
	}
	gateway.mu.Lock()
	initialWrites := append([]runtimeWrite(nil), gateway.writes...)
	gateway.mu.Unlock()
	if len(initialWrites) != 2 || initialWrites[0].op != "send" || initialWrites[1].op != "reply" || initialWrites[1].parent != originals[mirrorForRecoveryKind(t, originals, "human")].RemoteID {
		t.Fatal("initial human/final relation was not delivered once")
	}
	allowConnect.Store(false)
	wire.disconnect()
	runtimeWait(t, func() bool { return !service.NativeConnected() && !service.actionAvailable("thread") })
	// Add history only while the real socket is down. No notification or Wake
	// supplies the new body: the next generation must recover it from history.
	wire.history([]codexclient.Turn{old, added})
	if _, ok := serviceRecoveryMirrors(db, ctx, 2); !ok {
		t.Fatal("socket loss changed already confirmed public history")
	}
	gateway.mu.Lock()
	offlineWrites := len(gateway.writes)
	gateway.mu.Unlock()
	if offlineWrites != 2 || !gateway.Ready() || gateway.listens.Load() != 1 {
		t.Fatal("offline period reposted completed content or replaced receiver")
	}
	allowConnect.Store(true)
	runtimeWait(t, func() bool {
		_, ok := serviceRecoveryMirrors(db, ctx, 4)
		return ok && service.NativeConnected() && service.actionAvailable("thread") && wire.count("thread/resume") == 2
	})
	mark, err := db.Watermark(ctx, cfg.Host.ID, "thread")
	if err != nil || mark.TurnID != added.ID || mark.ItemID != added.Items[1].ID {
		t.Fatal("recovery checkpoint missed the offline turn or final item")
	}
	// Repeat the full snapshot while connected, then join workers before final
	// count assertions so no late delivery can escape the evidence boundary.
	reads := wire.count("thread/turns/list")
	service.Wake("thread")
	runtimeWait(t, func() bool {
		next, err := db.Watermark(ctx, cfg.Host.ID, "thread")
		return err == nil && next.TurnID == added.ID && next.ItemID == added.Items[1].ID && next.ObservedAtMS > mark.ObservedAtMS && wire.count("thread/turns/list") > reads
	})
	stopOnce.Do(stop)
	checkCtx := context.Background()
	mirrors, ok := serviceRecoveryMirrors(db, checkCtx, 4)
	if !ok {
		t.Fatal("recovered public outbox is not fully confirmed")
	}
	for key, before := range originals {
		after, ok := mirrors[key]
		if !ok || after.RemoteID != before.RemoteID || after.Revision != before.Revision || after.DeliveredRevision != before.DeliveredRevision || after.Card != before.Card {
			t.Fatal("reconnect changed or reposted original public content")
		}
	}
	afterBinding, err := db.Binding(checkCtx, cfg.Host.ID, "thread")
	if err != nil || afterBinding != binding {
		t.Fatal("reconnect changed the existing group binding")
	}
	gateway.mu.Lock()
	writes := append([]runtimeWrite(nil), gateway.writes...)
	groups := gateway.groups
	gateway.mu.Unlock()
	if len(writes) != 4 || groups != 0 || gateway.listens.Load() != 1 || dials.Load() != 2 || wire.accepts.Load() != 2 || wire.count("thread/resume") != 2 || wire.count("initialize") != 2 {
		t.Fatal("socket recovery duplicated content, receiver, group or native generation")
	}
	for _, body := range []string{"owned old public input", "owned old final reply", "owned offline public input", "owned offline final reply"} {
		count := 0
		for _, write := range writes {
			if strings.Contains(write.card, body) {
				count++
			}
			if write.op == "update" {
				t.Fatal("unchanged completed history was patched")
			}
		}
		if count != 1 {
			t.Fatal("public body was not delivered exactly once")
		}
	}
	newHuman, newFinal := store.Mirror{}, store.Mirror{}
	for _, m := range mirrors {
		if m.TurnID == added.ID {
			switch m.Kind {
			case "human":
				newHuman = m
			case "final":
				newFinal = m
			}
		}
	}
	if newHuman.RemoteID == "" || newFinal.RemoteID == "" || newFinal.ParentKey != newHuman.Key || !strings.Contains(newHuman.Card, "owned offline public input") || !strings.Contains(newFinal.Card, "owned offline final reply") {
		t.Fatal("offline turn content or reply anchor was not restored")
	}
	newReplies := 0
	for _, write := range writes {
		if strings.Contains(write.card, "owned offline final reply") {
			if write.op != "reply" || write.parent != newHuman.RemoteID {
				t.Fatal("offline final did not reply to its restored human anchor")
			}
			newReplies++
		}
	}
	if newReplies != 1 {
		t.Fatal("offline final reply relation was not delivered once")
	}
	for _, method := range []string{"thread/start", "thread/queue/add", "thread/queue/delete", "thread/queue/start", "turn/steer", "turn/interrupt"} {
		if wire.count(method) != 0 {
			t.Fatal("history recovery issued a native mutation")
		}
	}
	t.Log("local Unix WebSocket Client→Service disconnect/reconnect→two-page history→SQLite/outbox; old and offline human/final each once; receiver=1 generations=2 groups=0 native_mutations=0; fake Feishu gateway, not a network-outage or platform-limit claim")
}

func mirrorForRecoveryKind(t *testing.T, mirrors map[string]store.Mirror, kind string) string {
	t.Helper()
	for key, m := range mirrors {
		if m.Kind == kind {
			return key
		}
	}
	t.Fatal("fixture mirror kind missing")
	return ""
}
