package verification

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

func sessionFixture(t *testing.T) (*NativeSessions, *NativeManager, *nativeCreator, string) {
	t.Helper()
	m, n, _, _ := nativeFixture(t)
	root := filepath.Join(t.TempDir(), "sessions")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := NewNativeSessions(context.Background(), m, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, m, n, root
}

func TestNativeSessionsProvisionOnceBeforeCreationAndKeepEvidence(t *testing.T) {
	s, m, n, root := sessionFixture(t)
	ctx := context.Background()
	id := store.ID()
	entered, release := make(chan struct{}), make(chan struct{})
	var sent codexclient.LocalVerificationSpec
	n.start = func(ctx context.Context, spec codexclient.LocalVerificationSpec) (codexclient.Thread, error) {
		sent = spec
		want := filepath.Join(root, id, "cwd")
		if spec.RunID != id || spec.CWD != want {
			t.Error("unexpected resource scope")
		}
		for _, path := range []string{root, filepath.Dir(want), want} {
			if err := privateResourceDirectory(path); err != nil {
				t.Error("resource directory not private", err)
			}
		}
		body, err := os.ReadFile(filepath.Join(root, id, "owner.json"))
		if err != nil {
			t.Error(err)
		}
		var owner workspaceOwner
		if err := json.Unmarshal(body, &owner); err != nil || owner.RequestID != id || owner.BinarySHA256 != m.BinarySHA256 || owner.OwnerID != m.OwnerID {
			t.Error("missing immutable owner evidence", err)
		}
		info, err := os.Stat(filepath.Join(root, id, "owner.json"))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Error("owner marker not private")
		}
		journal, err := m.Store.VerificationNative(ctx, id, m.HostID, m.AppID, m.OwnerID)
		if err != nil || journal.State != "dispatching" || journal.ModelURL != spec.ModelURL || journal.CWD != want {
			t.Error("native API preceded frozen resource request", err)
		}
		u, err := url.Parse(spec.ModelURL)
		if err != nil || u.Hostname() != "127.0.0.1" || u.Path != "/v1" {
			t.Error("model not constrained loopback", err)
		}
		conn, err := net.DialTimeout("tcp", u.Host, time.Second)
		if err != nil {
			t.Error("owned model listener not ready", err)
		} else {
			conn.Close()
		}
		close(entered)
		<-release
		return nativeThread(spec), nil
	}
	first := make(chan error, 1)
	go func() { _, err := s.Begin(ctx, id); first <- err }()
	<-entered
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.Begin(ctx, id)
			if err != nil || !r.ResourcesReady || r.State != "confirmed" && r.State != "unknown" {
				t.Error(r, err)
			}
		}()
	}
	close(release)
	wg.Wait()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if n.calls.Load() != 1 {
		t.Fatal("duplicate native thread", n.calls.Load())
	}
	r, err := s.State(ctx, id)
	if err != nil || !r.ResourcesReady || r.State != "confirmed" || r.ModelRequests != 0 {
		t.Fatal(r, err)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), root) || strings.Contains(string(b), sent.ModelURL) {
		t.Fatal("private resources escaped result")
	}
	if err := s.Release(id); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = s.State(ctx, id)
	if err != nil || r.ResourcesReady || !r.ResourceRecoveryRequired || r.CreationStatus != "pass" {
		t.Fatal("lost historical birth after resources closed", r, err)
	}
	if _, err = os.Stat(filepath.Join(root, id, "owner.json")); err != nil {
		t.Fatal("owner evidence removed", err)
	}
	if err = s.Release(id); !errors.Is(err, ErrResourcesUnavailable) {
		t.Fatal("released nonexistent resource", err)
	}
}

func TestNativeSessionsRestartDoesNotReplaceOldEndpointOrCreate(t *testing.T) {
	for _, outcome := range []string{"ready", "confirmed", "unknown", "partial"} {
		t.Run(outcome, func(t *testing.T) {
			s, m, n, root := sessionFixture(t)
			ctx := context.Background()
			id := store.ID()
			if outcome == "partial" {
				if err := os.Mkdir(filepath.Join(root, id), 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				n.start = func(ctx context.Context, spec codexclient.LocalVerificationSpec) (codexclient.Thread, error) {
					if outcome == "unknown" {
						return codexclient.Thread{}, &codexclient.UnknownError{Method: "thread/start"}
					}
					return nativeThread(spec), nil
				}
				if outcome == "ready" {
					// Drop the current connection between resource availability and
					// durable native claim: the immutable ready row must survive.
					var calls int
					m.Current = func() NativeCreator {
						calls++
						if calls > 1 {
							return nil
						}
						return n
					}
				}
				_, err := s.Begin(ctx, id)
				if outcome == "ready" {
					if !errors.Is(err, ErrNotReady) {
						t.Fatal(err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			before := n.calls.Load()
			m.Current = func() NativeCreator { return n }
			restored, err := NewNativeSessions(ctx, m, root)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			r, err := restored.Begin(ctx, id)
			if outcome == "partial" {
				if !errors.Is(err, ErrResourcesUnavailable) || !r.ResourceRecoveryRequired {
					t.Fatal(r, err)
				}
			} else {
				if err != nil || r.ResourcesReady || !r.ResourceRecoveryRequired || r.State != outcome {
					t.Fatal("restart adopted stale endpoint", r, err)
				}
				if _, err := restored.State(ctx, id); err != nil {
					t.Fatal(err)
				}
			}
			if n.calls.Load() != before || len(restored.active) != 0 {
				t.Fatal("restart created replacement resources/native request")
			}
		})
	}
}

func TestNativeSessionsCloseCancelsInFlightCreation(t *testing.T) {
	s, m, n, _ := sessionFixture(t)
	entered := make(chan struct{})
	n.start = func(ctx context.Context, s codexclient.LocalVerificationSpec) (codexclient.Thread, error) {
		close(entered)
		<-ctx.Done()
		return codexclient.Thread{}, ctx.Err()
	}
	id := store.ID()
	result := make(chan error, 1)
	go func() { _, err := s.Begin(context.Background(), id); result <- err }()
	<-entered
	readCtx, stopRead := context.WithTimeout(context.Background(), time.Second)
	defer stopRead()
	r, err := s.State(readCtx, id)
	if err != nil || r.State != "unknown" || !r.ResourcesReady {
		t.Fatal("State blocked behind pending create", r, err)
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close waited indefinitely for RPC")
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	r.NativeResult, err = m.State(context.Background(), id)
	if err != nil || r.State != "unknown" || n.calls.Load() != 1 {
		t.Fatal("cancelled creation not retained", r, err)
	}
}

func TestNativeSessionsReadOnlyAndInvalidScopes(t *testing.T) {
	s, m, n, root := sessionFixture(t)
	ctx := context.Background()
	id := store.ID()
	if _, err := s.State(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.Begin(ctx, "../../foreign"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := s.Release(id); !errors.Is(err, ErrResourcesUnavailable) {
		t.Fatal(err)
	}
	files, err := os.ReadDir(root)
	if err != nil || len(files) != 0 || n.calls.Load() != 0 {
		t.Fatal("read-only operation provisioned", err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"relative", root + "/../other", alias} {
		if _, err := NewNativeSessions(ctx, m, path); !errors.Is(err, ErrInvalid) {
			t.Fatal("unsafe root accepted", err)
		}
	}
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewNativeSessions(ctx, m, root); !errors.Is(err, ErrInvalid) {
		t.Fatal("shared root accepted", err)
	}
	if _, err := s.Begin(ctx, id); !errors.Is(err, ErrInvalid) {
		t.Fatal("changed root accepted", err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	m.Current = nil
	if _, err := s.Begin(ctx, id); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	files, err = os.ReadDir(root)
	if err != nil || len(files) != 0 || n.calls.Load() != 0 {
		t.Fatal("unready session provisioned", err)
	}
}

func TestNativeSessionsExpiredOrStoppedModelNeverUsesOldReadyEndpoint(t *testing.T) {
	for _, reason := range []string{"expired", "server-stopped", "cwd-missing"} {
		t.Run(reason, func(t *testing.T) {
			s, m, n, _ := sessionFixture(t)
			var query int
			m.Current = func() NativeCreator {
				query++
				if query > 1 {
					return nil
				}
				return n
			}
			id := store.ID()
			if _, err := s.Begin(context.Background(), id); !errors.Is(err, ErrNotReady) {
				t.Fatal(err)
			}
			entry := s.active[id]
			if deadline, ok := entry.life.Deadline(); !ok || time.Until(deadline) > 5*time.Minute {
				t.Fatal("unbounded fixture lifetime")
			}
			if reason == "cwd-missing" {
				if err := os.Rename(entry.spec.CWD, entry.spec.CWD+".retained"); err != nil {
					t.Fatal(err)
				}
			} else if reason == "expired" {
				entry.cancel()
			} else if err := entry.model.Close(); err != nil {
				t.Fatal(err)
			}
			m.Current = func() NativeCreator { return n }
			r, err := s.Begin(context.Background(), id)
			if err != nil || r.State != "ready" || r.ResourcesReady || !r.ResourceRecoveryRequired {
				t.Fatal(r, err)
			}
			if n.calls.Load() != 0 || len(s.active) != 1 {
				t.Fatal("stopped fixture recreated/sent")
			}
			if err := s.Release(id); !errors.Is(err, ErrResourcesUnavailable) {
				t.Fatal(err)
			}
			c, err := m.Store.VerificationNative(context.Background(), id, m.HostID, m.AppID, m.OwnerID)
			if err != nil || c.ModelURL != entry.spec.ModelURL || c.State != "ready" {
				t.Fatal("original endpoint rewritten", err)
			}
		})
	}
}
