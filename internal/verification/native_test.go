package verification

import (
	"context"
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
	"codex-feishu/internal/store"
)

type nativeCreator struct {
	start      func(context.Context, codexclient.LocalVerificationSpec) (codexclient.Thread, error)
	generation uint64
	connection string
	done       chan struct{}
	calls      atomic.Int32
}

func (n *nativeCreator) StartLocalVerification(ctx context.Context, s codexclient.LocalVerificationSpec) (codexclient.Thread, error) {
	n.calls.Add(1)
	return n.start(ctx, s)
}
func (n *nativeCreator) GenerationID() uint64  { return n.generation }
func (n *nativeCreator) ConnectionID() string  { return n.connection }
func (n *nativeCreator) Done() <-chan struct{} { return n.done }

func nativeFixture(t *testing.T) (*NativeManager, *nativeCreator, codexclient.LocalVerificationSpec, string) {
	t.Helper()
	root := t.TempDir()
	cwd := filepath.Join(root, "private")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "verification.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := codexclient.LocalVerificationSpec{RunID: store.ID(), CWD: cwd, ModelURL: "http://127.0.0.1:12345/v1"}
	n := &nativeCreator{generation: 1, connection: store.ID(), done: make(chan struct{})}
	n.start = func(_ context.Context, s codexclient.LocalVerificationSpec) (codexclient.Thread, error) {
		return nativeThread(s), nil
	}
	m := &NativeManager{Store: db, HostID: "host", AppID: "app", OwnerID: "owner", Commit: "development", BinarySHA256: strings.Repeat("b", 64), Current: func() NativeCreator { return n }, Now: func() time.Time { return time.UnixMilli(1_000_000) }}
	return m, n, s, path
}
func nativeThread(s codexclient.LocalVerificationSpec) codexclient.Thread {
	return codexclient.Thread{ID: "owned-thread", CWD: s.CWD, ThreadSource: json.RawMessage(`"feishu.bridge.validation"`)}
}

func TestNativeManagerConcurrentReplayPersistsBeforeSend(t *testing.T) {
	m, n, s, _ := nativeFixture(t)
	ctx := context.Background()
	entered, release := make(chan struct{}), make(chan struct{})
	n.start = func(ctx context.Context, s codexclient.LocalVerificationSpec) (codexclient.Thread, error) {
		c, err := m.Store.VerificationNative(ctx, s.RunID, m.HostID, m.AppID, m.OwnerID)
		request, _ := codexclient.LocalVerificationRequestJSON(s)
		if err != nil || c.State != "dispatching" || c.Generation != 1 || c.ConnectionID != n.connection || c.RequestJSON != request {
			t.Error("RPC preceded durable identity", err)
		}
		close(entered)
		<-release
		return nativeThread(s), nil
	}
	first := make(chan error, 1)
	go func() { _, err := m.Begin(ctx, s.RunID, s); first <- err }()
	<-entered
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := m.Begin(ctx, s.RunID, s)
			if err != nil || r.State != "unknown" {
				t.Error(r, err)
			}
		}()
	}
	wg.Wait()
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	r, err := m.Begin(ctx, s.RunID, s)
	if err != nil || n.calls.Load() != 1 || r.CreationStatus != "pass" || !r.CreatorConnected {
		t.Fatal(r, err, n.calls.Load())
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), s.CWD) || strings.Contains(string(b), s.ModelURL) || strings.Contains(string(b), n.connection) {
		t.Fatal("private request escaped public result")
	}
}

func TestNativeManagerTerminalAndCrashNeverRecreate(t *testing.T) {
	for _, outcome := range []string{"unknown", "rejected", "invalid-proof", "cancel", "crash"} {
		t.Run(outcome, func(t *testing.T) {
			m, n, s, path := nativeFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			n.start = func(context.Context, codexclient.LocalVerificationSpec) (codexclient.Thread, error) {
				switch outcome {
				case "rejected":
					return codexclient.Thread{}, &codexclient.RPCError{Code: -1}
				case "invalid-proof":
					return codexclient.Thread{ID: "foreign", CWD: "/foreign"}, nil
				case "cancel":
					cancel()
					return codexclient.Thread{}, ctx.Err()
				default:
					return codexclient.Thread{}, &codexclient.UnknownError{Method: "thread/start", Cause: errors.New("lost response")}
				}
			}
			if outcome == "crash" {
				request, _ := codexclient.LocalVerificationRequestJSON(s)
				c, err := m.Store.PrepareVerificationNative(ctx, store.VerificationNative{RequestID: s.RunID, HostID: m.HostID, AppID: m.AppID, OwnerID: m.OwnerID, BridgeCommit: m.Commit, BinarySHA256: m.BinarySHA256, CWD: s.CWD, ModelURL: s.ModelURL, RequestJSON: request, CreatedAtMS: m.now().UnixMilli(), ExpiresAtMS: m.now().Add(time.Minute).UnixMilli()})
				if err != nil {
					t.Fatal(err)
				}
				if ok, err := m.Store.ClaimVerificationNative(ctx, c, 1, n.connection, m.now().UnixMilli()); err != nil || !ok {
					t.Fatal(ok, err)
				}
			} else if _, err := m.Begin(ctx, s.RunID, s); err != nil {
				t.Fatal(err)
			}
			if err := m.Store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			m.Store = reopened
			before := n.calls.Load()
			for range 3 {
				r, err := m.Begin(context.Background(), s.RunID, s)
				if err != nil || r.State != "unknown" && r.State != "rejected" || r.ThreadID != "" {
					t.Fatal(r, err)
				}
			}
			if n.calls.Load() != before {
				t.Fatal("terminal attempt recreated")
			}
		})
	}
}

func TestNativeManagerConnectionIdentitySurvivesNumericCollision(t *testing.T) {
	for _, stage := range []string{"before-send", "during-response", "after-response"} {
		t.Run(stage, func(t *testing.T) {
			m, n, s, _ := nativeFixture(t)
			newConnection := &nativeCreator{generation: n.generation, connection: store.ID(), done: make(chan struct{})}
			var current atomic.Pointer[nativeCreator]
			current.Store(n)
			var queries atomic.Int32
			m.Current = func() NativeCreator {
				if stage == "before-send" && queries.Add(1) == 2 {
					current.Store(newConnection)
				}
				return current.Load()
			}
			n.start = func(context.Context, codexclient.LocalVerificationSpec) (codexclient.Thread, error) {
				if stage == "during-response" {
					current.Store(newConnection)
				}
				return nativeThread(s), nil
			}
			r, err := m.Begin(context.Background(), s.RunID, s)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "after-response" {
				current.Store(newConnection)
				r, err = m.State(context.Background(), s.RunID)
			}
			if err != nil || r.CreatorConnected {
				t.Fatal("reused generation treated as same connection", r, err)
			}
			if stage == "before-send" {
				if n.calls.Load() != 0 || r.State != "unknown" {
					t.Fatal(r, n.calls.Load())
				}
			} else if r.State != "confirmed" || n.calls.Load() != 1 {
				t.Fatal("lost historical confirmed birth", r)
			}
			if _, err = m.Begin(context.Background(), s.RunID, s); err != nil || newConnection.calls.Load() != 0 {
				t.Fatal("reconnected attempt resent", err)
			}
		})
	}
}

func TestNativeManagerInvalidOrChangedScopeDoesNotSend(t *testing.T) {
	for _, condition := range []string{"id", "directory", "model", "closed", "no-client", "scope", "payload", "build", "expired"} {
		t.Run(condition, func(t *testing.T) {
			m, n, s, _ := nativeFixture(t)
			ctx := context.Background()
			if condition == "scope" || condition == "payload" || condition == "build" || condition == "expired" {
				m.Current = nil
				if _, err := m.Begin(ctx, s.RunID, s); !errors.Is(err, ErrNotReady) {
					t.Fatal(err)
				}
				m.Current = func() NativeCreator { return n }
			}
			switch condition {
			case "id":
				s.RunID = "wrong"
			case "directory":
				s.CWD = "relative"
			case "model":
				s.ModelURL = "http://localhost:12345/v1"
			case "closed":
				close(n.done)
			case "no-client":
				m.Current = nil
			case "scope":
				m.OwnerID = "another-owner"
			case "payload":
				s.ModelURL = "http://127.0.0.1:23456/v1"
			case "build":
				m.BinarySHA256 = strings.Repeat("c", 64)
			case "expired":
				m.Now = func() time.Time { return time.UnixMilli(2_000_000) }
			}
			r, err := m.Begin(ctx, s.RunID, s)
			if condition == "build" {
				if err != nil || !r.BuildChanged {
					t.Fatal(r, err)
				}
			} else if condition == "expired" {
				if err != nil || !r.Expired {
					t.Fatal(r, err)
				}
			} else if err == nil {
				t.Fatal("invalid scope accepted", condition)
			}
			if n.calls.Load() != 0 {
				t.Fatal("invalid/changed scope sent RPC")
			}
		})
	}
}

func TestNativeManagerStateDoesNotCreate(t *testing.T) {
	m, n, s, _ := nativeFixture(t)
	if _, err := m.State(context.Background(), s.RunID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	var count int
	if err := m.Store.DB.QueryRow(`SELECT count(*) FROM verification_native`).Scan(&count); err != nil || count != 0 || n.calls.Load() != 0 {
		t.Fatal(count, err)
	}
}
