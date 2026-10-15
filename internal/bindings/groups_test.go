package bindings

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

type nativeGroupFixture struct {
	calls atomic.Int32
	cwd   string
}

func (n *nativeGroupFixture) Attach(_ context.Context, id string) (codexclient.Thread, error) {
	n.calls.Add(1)
	cwd := n.cwd
	if cwd == "" {
		cwd = "/project"
	}
	return codexclient.Thread{ID: id, CWD: cwd}, nil
}

type groupFixture struct {
	calls       atomic.Int32
	create      func(string, string, []string, string) (string, error)
	description string
}

func (g *groupFixture) CreateGroupWithDescription(_ context.Context, name, owner string, members []string, uuid, description string) (string, error) {
	g.calls.Add(1)
	g.description = description
	return g.create(name, owner, members, uuid)
}

func groupManager(t *testing.T) (*Manager, *groupFixture, string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "groups.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err = s.BeginBaseline(ctx, "host", "app", 1000); err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteBaseline(ctx, "host", nil); err != nil {
		t.Fatal(err)
	}
	if err = s.ObserveNew(ctx, "host", "thread", "/project", 2); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkEligible(ctx, "host", "thread", "actual-input", "/project", "/project"); err != nil {
		t.Fatal(err)
	}
	g := &groupFixture{}
	m := &Manager{Store: s, Native: &nativeGroupFixture{}, Groups: g, HostID: "host", OwnerID: "owner", Members: []string{"owner"}, Authorize: func(context.Context, codexclient.Thread) error { return nil }}
	g.create = func(name, owner string, members []string, uuid string) (string, error) {
		i, err := m.Store.GroupIntent(ctx, uuid)
		if err != nil || i.State != "dispatching" || i.Name != name || i.OwnerID != owner || i.Description != g.description || i.Description == "" {
			t.Errorf("remote write lacked journal: %+v %v", i, err)
		}
		return "chat", nil
	}
	return m, g, path
}

func TestGroupDescriptionUsesAttachedWorkingDirectory(t *testing.T) {
	m, g, _ := groupManager(t)
	// Discovery still records /project, while the actual attached session has
	// moved into a worktree. Keep its full path, including non-ASCII characters.
	cwd := "/home/operator/.codex/worktrees/worktree/" + strings.Repeat("目录", 60)
	m.Native.(*nativeGroupFixture).cwd = cwd
	if _, err := m.Ensure(context.Background(), "thread", "project · task"); err != nil {
		t.Fatal(err)
	}
	if g.description != "工作目录："+cwd {
		t.Fatalf("actual working directory lost: %q", g.description)
	}
}

func TestMissingAbsoluteWorkingDirectoryDoesNotCreateGroup(t *testing.T) {
	m, g, _ := groupManager(t)
	m.Native.(*nativeGroupFixture).cwd = "relative/project"
	if _, err := m.Ensure(context.Background(), "thread", "name"); !errors.Is(err, store.ErrIneligible) {
		t.Fatal("unavailable actual cwd accepted", err)
	}
	if g.calls.Load() != 0 {
		t.Fatal("group created without actual cwd")
	}
}

func TestConcurrentEnsureCreatesExactlyOneGroup(t *testing.T) {
	ctx := context.Background()
	m, g, _ := groupManager(t)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := m.Ensure(ctx, "thread", "project · task")
			if err != nil && !errors.Is(err, ErrGroupBusy) {
				t.Error(err)
			}
			if err == nil && b.ChatID != "chat" {
				t.Errorf("wrong binding: %+v", b)
			}
		}()
	}
	wg.Wait()
	if g.calls.Load() != 1 {
		t.Fatalf("groups created: %d", g.calls.Load())
	}
	b, err := m.Ensure(ctx, "thread", "changed after restart")
	if err != nil || b.ChatID != "chat" || g.calls.Load() != 1 {
		t.Fatalf("replay created again: %+v %v", b, err)
	}
}

func TestUnknownAndExplicitRejectionAreNotBlindlyRetried(t *testing.T) {
	for _, tc := range []struct {
		name      string
		remoteErr error
		want      error
		state     string
	}{
		{"lost response", &feishu.Unknown{Operation: "create"}, ErrGroupUnknown, "unknown"},
		{"missing remote id", nil, ErrGroupUnknown, "unknown"},
		{"permission rejected", &feishu.Rejected{Code: 99991672}, ErrGroupRejected, "rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			m, g, _ := groupManager(t)
			g.create = func(string, string, []string, string) (string, error) { return "", tc.remoteErr }
			for i := 0; i < 3; i++ {
				if _, err := m.Ensure(ctx, "thread", "name"); !errors.Is(err, tc.want) {
					t.Fatalf("wrong result: %v", err)
				}
			}
			if g.calls.Load() != 1 {
				t.Fatalf("ambiguous/rejected dispatch retried: %d", g.calls.Load())
			}
			i, err := m.Store.PrepareGroup(ctx, "host", "thread", "owner", "name", nil)
			if err != nil || i.State != tc.state {
				t.Fatalf("attempt lost: %+v %v", i, err)
			}
		})
	}
}

func TestSuccessBeforeLocalSaveCrashRecoversUnknownWithoutResend(t *testing.T) {
	ctx := context.Background()
	m, g, path := groupManager(t)
	g.create = func(string, string, []string, string) (string, error) {
		m.Store.Close()
		return "remote-created-chat", nil
	}
	if _, err := m.Ensure(ctx, "thread", "name"); err == nil {
		t.Fatal("closed local DB reported success")
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	m.Store = reopened
	if err = reopened.RecoverGroupDispatches(ctx, "host"); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Ensure(ctx, "thread", "name"); !errors.Is(err, ErrGroupUnknown) {
		t.Fatalf("lost confirmed response was not unknown: %v", err)
	}
	if g.calls.Load() != 1 {
		t.Fatal("created second group after lost local save")
	}
}

func TestAuthorizationRecheckedBeforeRemoteCreation(t *testing.T) {
	ctx := context.Background()
	m, g, _ := groupManager(t)
	m.Authorize = func(context.Context, codexclient.Thread) error { return store.ErrIneligible }
	if _, err := m.Ensure(ctx, "thread", "name"); !errors.Is(err, store.ErrIneligible) {
		t.Fatalf("stale authorization allowed: %v", err)
	}
	if g.calls.Load() != 0 {
		t.Fatal("unauthorized group dispatched")
	}
	i, err := m.Store.PrepareGroup(ctx, "host", "thread", "owner", "name", nil)
	if err != nil || i.State != "ready" {
		t.Fatalf("unsent intent corrupted: %+v %v", i, err)
	}
}
