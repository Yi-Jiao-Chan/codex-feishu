package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/control"
	"codex-feishu/internal/store"
	"codex-feishu/internal/verification"
)

type verificationServiceNative struct {
	*serviceNative
	done      chan struct{}
	closeOnce sync.Once
	starts    atomic.Int32
}

func (n *verificationServiceNative) Done() <-chan struct{} { return n.done }
func (n *verificationServiceNative) GenerationID() uint64  { return 7 }
func (n *verificationServiceNative) ConnectionID() string  { return strings.Repeat("b", 32) }
func (n *verificationServiceNative) Close() {
	n.closeOnce.Do(func() { close(n.done); n.serviceNative.Close() })
}
func (n *verificationServiceNative) StartLocalVerification(ctx context.Context, s codexclient.LocalVerificationSpec) (codexclient.Thread, error) {
	if _, err := codexclient.LocalVerificationRequestJSON(s); err != nil {
		return codexclient.Thread{}, err
	}
	n.starts.Add(1)
	t := codexclient.Thread{ID: "probe-" + s.RunID, CWD: s.CWD, ThreadSource: json.RawMessage(`"feishu.bridge.validation"`), Source: json.RawMessage(`"cli"`), CreatedAt: time.Now().Unix()}
	n.mu.Lock()
	n.threads[t.ID] = t
	n.mu.Unlock()
	params, _ := json.Marshal(map[string]any{"thread": t})
	select {
	case n.events <- codexclient.Event{Generation: 7, Method: "thread/started", Params: params}:
	case <-ctx.Done():
		return codexclient.Thread{}, ctx.Err()
	}
	return t, nil
}

func TestVerificationCreatorReturnsOnlyExistingConnectedGeneration(t *testing.T) {
	n := &verificationServiceNative{serviceNative: &serviceNative{runtimeNative: &runtimeNative{}}, done: make(chan struct{})}
	s := &Service{Connect: func(context.Context) (ServiceNative, error) {
		t.Fatal("getter opened another connection")
		return nil, nil
	}}
	if s.VerificationCreator() != nil {
		t.Fatal("stopped service exposed creator")
	}
	s.running.Store(true)
	s.nativeConnected.Store(true)
	if s.VerificationCreator() != nil {
		t.Fatal("missing runtime exposed creator")
	}
	s.current = &Runtime{Native: n}
	if s.VerificationCreator() != n {
		t.Fatal("getter did not reuse exact existing client")
	}
	s.nativeConnected.Store(false)
	if s.VerificationCreator() != nil {
		t.Fatal("disconnected runtime exposed creator")
	}
	s.nativeConnected.Store(true)
	n.Close()
	if s.VerificationCreator() != nil {
		t.Fatal("closed reader exposed creator")
	}
	s.current = &Runtime{Native: &runtimeNative{}}
	if s.VerificationCreator() != nil {
		t.Fatal("legacy native without creation capability exposed")
	}
}

func TestServiceManagedThreadCreationReusesNativeAndNeverCreatesGroupOrInput(t *testing.T) {
	db, cfg, _ := serviceFixture(t)
	g := &serviceGateway{runtimeGateway: &runtimeGateway{}, listening: make(chan struct{})}
	n := &verificationServiceNative{serviceNative: &serviceNative{runtimeNative: &runtimeNative{threads: map[string]codexclient.Thread{}, turns: map[string][]codexclient.Turn{}, events: make(chan codexclient.Event, 16)}}, done: make(chan struct{})}
	var connections atomic.Int32
	s := &Service{Config: cfg, Store: db, Gateway: g, BotID: "ou_bot", Connect: func(context.Context) (ServiceNative, error) { connections.Add(1); return n, nil }}
	life, stop := context.WithCancel(context.Background())
	defer stop()
	root := filepath.Join(t.TempDir(), "verification-native")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	manager := &verification.NativeManager{Store: db, HostID: cfg.Host.ID, AppID: cfg.Feishu.AppID, OwnerID: cfg.Access.OwnerOpenID, Commit: "test-build", BinarySHA256: strings.Repeat("c", 64), Current: s.VerificationCreator}
	sessions, err := verification.NewNativeSessions(life, manager, root)
	if err != nil {
		t.Fatal(err)
	}
	defer sessions.Close()
	s.VerificationThreads = sessions
	serviceCtx, cancelService := context.WithCancel(life)
	defer cancelService()
	serviceDone := make(chan error, 1)
	go func() { serviceDone <- s.Run(serviceCtx) }()
	select {
	case <-g.listening:
	case err := <-serviceDone:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("receiver did not start")
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for s.VerificationCreator() == nil {
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("existing generation missing")
		}
	}
	dir, err := os.MkdirTemp("", "cftprobe-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	endpoint := filepath.Join(dir, "control.sock")
	identity := strings.Repeat("a", 64)
	server, err := control.Listen(life, endpoint, identity, s.Health)
	if err != nil {
		t.Fatal(err)
	}
	server.VerificationThreads = sessions
	controlDone := make(chan error, 1)
	go func() { controlDone <- server.Serve(life) }()
	defer func() { stop(); server.Close(); <-controlDone; cancelService(); <-serviceDone }()
	id := store.ID()
	for _, begin := range []bool{true, true, false} {
		p, err := control.VerifyThreadCreation(life, endpoint, identity, id, begin)
		if err != nil || p.CreationStatus != "pass" || !p.CreatorConnected || !p.ResourcesReady || p.ModelRequests != 0 {
			t.Fatal(p, err)
		}
	}
	c, err := db.VerificationNative(life, id, cfg.Host.ID, cfg.Feishu.AppID, cfg.Access.OwnerOpenID)
	if err != nil || c.Generation != 7 || c.ConnectionID != n.ConnectionID() {
		t.Fatal("journal lost service generation", err)
	}
	if _, err := db.Binding(life, cfg.Host.ID, c.ThreadID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("creation inferred binding", err)
	}
	health, err := control.Query(life, endpoint, identity)
	if err != nil || !health.VerificationThreadCreationSupported || !health.NativeConnected {
		t.Fatal(health, err)
	}
	if connections.Load() != 1 || n.starts.Load() != 1 || n.adds.Load() != 0 || g.listens.Load() != 1 {
		t.Fatal("probe opened second connection,receiver or input", connections.Load(), n.starts.Load(), n.adds.Load(), g.listens.Load())
	}
	g.mu.Lock()
	groups, writes := g.groups, len(g.writes)
	g.mu.Unlock()
	if groups != 0 || writes != 0 {
		t.Fatal("isolated birth sent Feishu content", groups, writes)
	}
}
