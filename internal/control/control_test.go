//go:build linux

package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/config"
	"codex-feishu/internal/store"
)

func controlFixture(t *testing.T) (string, string) {
	t.Helper()
	// A short private pathname stays within the OS Unix socket limit even
	// when go test's ordinary temporary directory contains a long test name.
	dir, err := os.MkdirTemp("", "cfctl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "bridge.sock"), strings.Repeat("a", 64)
}

func runControl(t *testing.T, path, identity string, provider func(context.Context) (Snapshot, error)) (*Server, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	server, err := Listen(ctx, path, identity, provider)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		server.Close()
	})
	return server, cancel, done
}

func snapshot() Snapshot {
	return Snapshot{Running: true, NativeConnected: true, FeishuConnected: true,
		StartedAtMS: time.Now().Add(-time.Second).UnixMilli(), ObservedAtMS: time.Now().UnixMilli(),
		Persistent: store.PersistentHealth{Schema: store.SchemaVersion, Bindings: 2, Inputs: map[string]int64{"queued": 1}}}
}

func TestRealUnixQueryIdentityAndPrivacy(t *testing.T) {
	path, identity := controlFixture(t)
	var calls atomic.Int32
	_, cancel, done := runControl(t, path, identity, func(context.Context) (Snapshot, error) { calls.Add(1); return snapshot(), nil })
	got, err := Query(context.Background(), path, identity)
	if err != nil || !got.NativeConnected || got.Persistent.Inputs["queued"] != 1 || calls.Load() != 1 {
		t.Fatal("same-user real Unix query failed", got, err)
	}
	if _, err := Query(context.Background(), path, strings.Repeat("b", 64)); !errors.Is(err, ErrIdentity) || calls.Load() != 1 {
		t.Fatal("other deployment inspected provider", err)
	}
	if _, err := Listen(context.Background(), path, identity, func(context.Context) (Snapshot, error) { return snapshot(), nil }); !errors.Is(err, ErrActive) {
		t.Fatal("active endpoint replaced", err)
	}
	b, _ := json.Marshal(got)
	for _, forbidden := range []string{"thread_id", "message_id", "body", "secret", "command", "question", "answer"} {
		if strings.Contains(string(b), `"`+forbidden+`":`) {
			t.Fatalf("private field %q in snapshot", forbidden)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("server did not stop")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned endpoint not cleaned up", err)
	}
}

func TestEndpointRestrictionsAndStaleRecovery(t *testing.T) {
	path, identity := controlFixture(t)
	provider := func(context.Context) (Snapshot, error) { return snapshot(), nil }
	if _, err := Query(context.Background(), filepath.Join(filepath.Dir(path), "missing", "bridge.sock"), identity); !errors.Is(err, ErrUnavailable) {
		t.Fatal("missing endpoint directory is not unavailable", err)
	}
	if err := os.WriteFile(path, []byte("preserve this file"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(context.Background(), path, identity, provider); !errors.Is(err, ErrUnsafe) {
		t.Fatal("regular file replaced", err)
	}
	os.Remove(path)
	os.Symlink("elsewhere", path)
	if _, err := Listen(context.Background(), path, identity, provider); !errors.Is(err, ErrUnsafe) {
		t.Fatal("symlink accepted", err)
	}
	os.Remove(path)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	l.Close()
	os.Chmod(path, 0600)
	server, cancel, done := runControl(t, path, identity, provider)
	if _, err := Query(context.Background(), path, identity); err != nil {
		t.Fatal("stale socket not recovered", err)
	}
	os.Chmod(path, 0666)
	if _, err := Query(context.Background(), path, identity); !errors.Is(err, ErrUnsafe) {
		t.Fatal("world-accessible socket inspected", err)
	}
	os.Chmod(path, 0600)
	// Cleanup must not remove a replacement path it does not own.
	os.Remove(path)
	os.WriteFile(path, []byte("replacement"), 0600)
	server.Close()
	cancel()
	<-done
	if b, err := os.ReadFile(path); err != nil || string(b) != "replacement" {
		t.Fatal("cleanup removed a replacement", err)
	}
}

func wire(t *testing.T, path, body string) []byte {
	t.Helper()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(conn, body); err != nil {
		t.Fatal(err)
	}
	conn.CloseWrite()
	b, err := io.ReadAll(conn)
	if err != nil {
		// Oversized/malformed requests may be closed without a response.
		return nil
	}
	return b
}

func TestRestrictedMethodsMalformedRequestsAndProviderFailure(t *testing.T) {
	path, identity := controlFixture(t)
	var calls atomic.Int32
	_, cancel, done := runControl(t, path, identity, func(context.Context) (Snapshot, error) {
		calls.Add(1)
		return Snapshot{}, errors.New("PRIVATE BODY MUST NEVER REACH WIRE")
	})
	base := `{"schema_version":1,"method":"health","identity":"` + identity + `"}`
	for _, body := range []string{base + `{}`, strings.Replace(base, `"health"`, `"turn/start"`, 1), strings.Replace(base, `:1`, `:2`, 1), strings.Replace(base, `"method"`, `"body"`, 1), strings.Repeat(" ", maxWireBytes+1) + base} {
		b := wire(t, path, body)
		if calls.Load() != 0 || strings.Contains(string(b), "PRIVATE") {
			t.Fatal("invalid input reached provider", string(b))
		}
	}
	b := wire(t, path, base)
	if calls.Load() != 1 || !strings.Contains(string(b), "health-unavailable") || strings.Contains(string(b), "PRIVATE") {
		t.Fatal("provider error leaked or misreported", string(b))
	}
	cancel()
	<-done
}

func TestCancellationClosesIdleClientsAndCancelsHealth(t *testing.T) {
	path, identity := controlFixture(t)
	entered := make(chan struct{})
	_, cancel, done := runControl(t, path, identity, func(ctx context.Context) (Snapshot, error) {
		close(entered)
		<-ctx.Done()
		return Snapshot{}, ctx.Err()
	})
	idle, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	queryDone := make(chan error, 1)
	go func() { _, err := Query(context.Background(), path, identity); queryDone <- err }()
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown held by idle/health clients")
	}
	if err := <-queryDone; err == nil {
		t.Fatal("cancelled health claimed success")
	}
}

func TestIdentityTracksConfigurationAndCanonicalPath(t *testing.T) {
	path, _ := controlFixture(t)
	if err := os.WriteFile(path, []byte("placeholder"), 0600); err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	cfg.Host.ID, cfg.Feishu.AppID = "host", "cli_app"
	first, err := Identity(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	alias := path + ".alias"
	os.Symlink(path, alias)
	second, err := Identity(alias, cfg)
	if err != nil || first != second {
		t.Fatal("path alias changed identity", err)
	}
	cfg.Discovery.AllowedRoots = []string{"/new/authorized/root"}
	changed, err := Identity(path, cfg)
	if err != nil || changed == first {
		t.Fatal("changed config matched running identity", err)
	}
}

func TestQueryRejectsStaleOrFutureHealth(t *testing.T) {
	for _, drift := range []time.Duration{-time.Hour, time.Hour} {
		t.Run(drift.String(), func(t *testing.T) {
			path, identity := controlFixture(t)
			_, cancel, done := runControl(t, path, identity, func(context.Context) (Snapshot, error) {
				s := snapshot()
				s.ObservedAtMS = time.Now().Add(drift).UnixMilli()
				return s, nil
			})
			if _, err := Query(context.Background(), path, identity); !errors.Is(err, ErrUnavailable) {
				t.Fatal("stale/corrupt cached health accepted", err)
			}
			cancel()
			<-done
		})
	}
}
