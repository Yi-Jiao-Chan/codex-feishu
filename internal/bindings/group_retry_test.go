package bindings

import (
	"context"
	"errors"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

func TestGroupManagerDefersBeforeAttachThenKeepsOriginalRequest(t *testing.T) {
	m, g, path := groupManager(t)
	ctx := context.Background()
	at := int64(1000)
	m.Now = func() time.Time { return time.UnixMilli(at) }
	var uuid string
	g.create = func(name, owner string, members []string, id string) (string, error) {
		if uuid == "" {
			uuid = id
		} else if uuid != id || name != "original" || owner != "owner" || len(members) != 1 {
			t.Error("retry changed frozen request")
		}
		if g.calls.Load() == 1 {
			at = 4000
			return "", &feishu.RateLimited{RetryAfter: 5 * time.Second}
		}
		return "chat", nil
	}
	_, err := m.Ensure(ctx, "thread", "original")
	var deferred *GroupDeferredError
	if !errors.As(err, &deferred) || deferred.RetryAtMS != 9000 {
		t.Fatal("no response-time wait", err)
	}
	initialCalls := m.Native.(*nativeGroupFixture).calls.Load()
	m.Store.Close()
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	m.Store = reopened
	if err := m.Store.RecoverGroupDispatches(ctx, m.HostID); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 5; n++ {
		if _, err := m.Ensure(ctx, "thread", "changed"); !errors.Is(err, ErrGroupDeferred) {
			t.Fatal("pending deadline forgotten", err)
		}
	}
	if g.calls.Load() != 1 || m.Native.(*nativeGroupFixture).calls.Load() != initialCalls {
		t.Fatal("waiting performs create/attach")
	}
	m.Members = []string{"owner", "owner"} // Same authorized set; request stays frozen.
	at = 9000
	b, err := m.Ensure(ctx, "thread", "changed")
	if err != nil || b.ChatID != "chat" || g.calls.Load() != 2 {
		t.Fatal("retry lost intent", err)
	}
	if _, err := m.Ensure(ctx, "thread", "different"); err != nil || g.calls.Load() != 2 {
		t.Fatal("bound group duplicated", err)
	}
}

func TestGroupManagerRechecksAuthorizationAtRetry(t *testing.T) {
	m, g, _ := groupManager(t)
	ctx := context.Background()
	at := int64(1000)
	m.Now = func() time.Time { return time.UnixMilli(at) }
	g.create = func(string, string, []string, string) (string, error) {
		return "", &feishu.RateLimited{RetryAfter: time.Second}
	}
	if _, err := m.Ensure(ctx, "thread", "name"); !errors.Is(err, ErrGroupDeferred) {
		t.Fatal(err)
	}
	at = 2000
	m.Authorize = func(ctx context.Context, _ codexclient.Thread) error {
		if err := m.Store.Exclude(ctx, m.HostID, "thread", "directory_muted"); err != nil {
			return err
		}
		return store.ErrIneligible
	}
	if _, err := m.Ensure(ctx, "thread", "name"); !errors.Is(err, store.ErrIneligible) {
		t.Fatal("authorization bypassed", err)
	}
	if g.calls.Load() != 1 {
		t.Fatal("muted group created")
	}
	m.Authorize = func(context.Context, codexclient.Thread) error { return nil }
	if _, err := m.Ensure(ctx, "thread", "name"); !errors.Is(err, store.ErrIneligible) {
		t.Fatal("excluded history revived", err)
	}
}

func TestGroupManagerDoesNotCreateFrozenRequestAfterMemberScopeChanges(t *testing.T) {
	m, g, _ := groupManager(t)
	ctx := context.Background()
	at := int64(1000)
	m.Now = func() time.Time { return time.UnixMilli(at) }
	m.Members = []string{"owner", "extra"}
	g.create = func(string, string, []string, string) (string, error) {
		if g.calls.Load() == 1 {
			return "", &feishu.RateLimited{RetryAfter: time.Second}
		}
		return "chat", nil
	}
	if _, err := m.Ensure(ctx, "thread", "name"); !errors.Is(err, ErrGroupDeferred) {
		t.Fatal(err)
	}
	at = 2000
	m.OwnerID = "different-owner"
	if _, err := m.Ensure(ctx, "thread", "name"); !errors.Is(err, store.ErrConflict) {
		t.Fatal("changed owner bypassed frozen intent", err)
	}
	m.OwnerID = "owner"
	m.Members = []string{"owner"}
	if _, err := m.Ensure(ctx, "thread", "name"); !errors.Is(err, store.ErrConflict) {
		t.Fatal("revoked member was included", err)
	}
	if g.calls.Load() != 1 {
		t.Fatal("changed access scope reached gateway")
	}
	m.Members = []string{"extra", "owner"}
	if _, err := m.Ensure(ctx, "thread", "name"); err != nil || g.calls.Load() != 2 {
		t.Fatal("restored equivalent member set not accepted", err)
	}
}
