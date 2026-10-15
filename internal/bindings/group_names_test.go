package bindings

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

type nameGatewayFixture struct {
	mu               sync.Mutex
	info             feishu.GroupInfo
	present          bool
	gets, bots, puts atomic.Int32
	getErr           error
	update           func(context.Context, string) error
}

func (g *nameGatewayFixture) BotInChat(context.Context, string) (bool, error) {
	g.bots.Add(1)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.present, nil
}
func (g *nameGatewayFixture) GetGroupInfo(context.Context, string) (feishu.GroupInfo, error) {
	g.gets.Add(1)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.info, g.getErr
}
func (g *nameGatewayFixture) UpdateGroupName(ctx context.Context, _ string, name string) error {
	g.puts.Add(1)
	if g.update != nil {
		return g.update(ctx, name)
	}
	g.setName(name)
	return nil
}
func (g *nameGatewayFixture) setName(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.info.Name = name
}

func nameManager(t *testing.T) (*GroupNames, *nameGatewayFixture, store.Binding, string) {
	t.Helper()
	m, _, path := groupManager(t)
	b, err := m.Ensure(context.Background(), "thread", "A")
	if err != nil {
		t.Fatal(err)
	}
	g := &nameGatewayFixture{present: true, info: feishu.GroupInfo{Name: "A", OwnerID: "owner", ChatMode: "group", Description: "original description"}}
	names := &GroupNames{Store: m.Store, Gateway: g, AppID: "app", OwnerID: "owner", Now: func() time.Time { return time.UnixMilli(1000) }, BeginWrite: func(ctx context.Context, b store.Binding) (context.Context, func(), error) {
		return ctx, func() {}, nil
	}}
	return names, g, b, path
}

func nameJournalCount(t *testing.T, m *GroupNames) int {
	t.Helper()
	var n int
	if err := m.Store.DB.QueryRow(`SELECT COUNT(*) FROM m0_operations WHERE kind=?`, store.GroupNameOperationKind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestGroupNamesIdempotentAndAToBToA(t *testing.T) {
	m, g, b, _ := nameManager(t)
	ctx := context.Background()
	if err := m.Reconcile(ctx, b, "A"); err != nil {
		t.Fatal(err)
	}
	if nameJournalCount(t, m) != 0 || g.puts.Load() != 0 {
		t.Fatal("same name dispatched")
	}
	full := strings.Repeat("完整会话名", 30)
	for _, desired := range []string{"B", "B", "A", full} {
		if err := m.Reconcile(ctx, b, desired); err != nil {
			t.Fatal(err)
		}
	}
	if g.puts.Load() != 3 || nameJournalCount(t, m) != 3 || g.info.Name != full || g.info.OwnerID != "owner" || g.info.Description != "original description" {
		t.Fatal("idempotence/scope/name lost", g.puts.Load(), g.info)
	}
	i, err := m.Store.GroupIntent(ctx, b.IntentID)
	if err != nil || i.Name != "A" {
		t.Fatal("creation intent modified", i, err)
	}
}

func TestGroupNamesRejectScopeOwnerMembershipBeforeJournal(t *testing.T) {
	for _, tc := range []string{"missing-chat", "stale-chat", "stale-intent", "app", "owner", "nongroup", "bot", "get-failure", "empty-title", "missing-guard"} {
		t.Run(tc, func(t *testing.T) {
			m, g, b, _ := nameManager(t)
			desired := "B"
			switch tc {
			case "missing-chat":
				b.ChatID = ""
			case "stale-chat":
				b.ChatID = "other"
			case "stale-intent":
				b.IntentID = "other"
			case "app":
				m.AppID = "other"
			case "owner":
				g.info.OwnerID = "other"
			case "nongroup":
				g.info.ChatMode = "p2p"
			case "bot":
				g.present = false
			case "get-failure":
				g.getErr = &feishu.Unknown{Operation: "read"}
			case "empty-title":
				desired = " "
			case "missing-guard":
				m.BeginWrite = nil
			}
			if err := m.Reconcile(context.Background(), b, desired); err == nil {
				t.Fatal("unauthorized mutation accepted")
			}
			if g.puts.Load() != 0 || nameJournalCount(t, m) != 0 {
				t.Fatal("invalid scope journaled/written")
			}
		})
	}
}

func TestGroupNamesWriteGuardRejectsAndCancellationDoesNotSubstituteContext(t *testing.T) {
	for _, tc := range []string{"reject", "cancel-before", "cancel-after", "changed-binding", "invalid-guard"} {
		t.Run(tc, func(t *testing.T) {
			m, g, b, _ := nameManager(t)
			ctx := context.Background()
			var released atomic.Int32
			guardErr := errors.New("guard rejected")
			m.BeginWrite = func(ctx context.Context, _ store.Binding) (context.Context, func(), error) {
				release := func() { released.Add(1) }
				switch tc {
				case "reject":
					return nil, release, guardErr
				case "cancel-before":
					c, cancel := context.WithCancel(ctx)
					cancel()
					return c, release, nil
				case "cancel-after":
					c, cancel := context.WithCancel(ctx)
					g.update = func(c context.Context, _ string) error { cancel(); return c.Err() }
					return c, release, nil
				case "changed-binding":
					if _, err := m.Store.DB.Exec(`UPDATE bindings SET chat_id='other' WHERE host_id=? AND thread_id=?`, b.HostID, b.ThreadID); err != nil {
						t.Fatal(err)
					}
				case "invalid-guard":
					return nil, release, nil
				}
				return ctx, release, nil
			}
			err := m.Reconcile(ctx, b, "B")
			if err == nil || released.Load() != 1 {
				t.Fatal("guard rejection/release lost", err, released.Load())
			}
			if tc == "cancel-after" {
				a, err := m.Store.GroupNameAttempt(ctx, store.GroupNameScope{AppID: "app", OwnerID: "owner", Binding: b})
				if err != nil || a.State != "dispatching" || g.puts.Load() != 1 {
					t.Fatal("cancel switched context to finish/retry", a, err)
				}
			} else if g.puts.Load() != 0 || nameJournalCount(t, m) != 0 {
				t.Fatal("rejected guard dispatched")
			}
		})
	}
}

func TestGroupNamesUnknownNeverRedispatchesEvenWhenSourceChanges(t *testing.T) {
	for _, applied := range []bool{true, false} {
		t.Run(map[bool]string{true: "applied", false: "unapplied"}[applied], func(t *testing.T) {
			m, g, b, path := nameManager(t)
			ctx := context.Background()
			g.update = func(_ context.Context, name string) error {
				if applied {
					g.setName(name)
				}
				return &feishu.Unknown{Operation: "lost response"}
			}
			if err := m.Reconcile(ctx, b, "B"); !errors.Is(err, ErrGroupNameUnknown) {
				t.Fatal(err)
			}
			m.Store.Close()
			reopened, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			m.Store = reopened
			g.update = nil
			err = m.Reconcile(ctx, b, "C")
			if applied && !errors.Is(err, ErrGroupNameBusy) || !applied && !errors.Is(err, ErrGroupNameUnknown) {
				t.Fatal("wrong readback/title-cache result", err)
			}
			if g.puts.Load() != 1 {
				t.Fatal("unknown redispatched")
			}
			if applied {
				if err := m.Reconcile(ctx, b, "C"); err != nil || g.puts.Load() != 2 {
					t.Fatal("latest title did not resume after confirmation", err)
				}
			} else {
				for n := 0; n < 3; n++ {
					if err := m.Reconcile(ctx, b, "D"); !errors.Is(err, ErrGroupNameUnknown) {
						t.Fatal(err)
					}
				}
				if g.puts.Load() != 1 || nameJournalCount(t, m) != 1 {
					t.Fatal("new title bypassed unknown")
				}
			}
		})
	}
}

func TestGroupNamesDispatchCrashAndIncompleteReadback(t *testing.T) {
	for _, tc := range []string{"dispatching-unapplied", "dispatching-applied", "success-read-error", "success-read-mismatch"} {
		t.Run(tc, func(t *testing.T) {
			m, g, b, path := nameManager(t)
			ctx := context.Background()
			if strings.HasPrefix(tc, "dispatching") {
				_, claimed, err := m.Store.ClaimGroupName(ctx, store.GroupNamePlan{Scope: store.GroupNameScope{AppID: "app", OwnerID: "owner", Binding: b}, Desired: "B", Previous: "A"}, "", 1000)
				if err != nil || !claimed {
					t.Fatal(err)
				}
				if tc == "dispatching-applied" {
					g.setName("B")
				}
				m.Store.Close()
				reopened, err := store.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				m.Store = reopened
				if err := m.Reconcile(ctx, b, "B"); tc == "dispatching-applied" && err != nil || tc == "dispatching-unapplied" && !errors.Is(err, ErrGroupNameUnknown) {
					t.Fatal(err)
				}
				if g.puts.Load() != 0 {
					t.Fatal("crash redispatched")
				}
			} else {
				g.update = func(_ context.Context, name string) error {
					if tc == "success-read-error" {
						g.setName(name)
						g.mu.Lock()
						g.getErr = &feishu.Unknown{Operation: "read"}
						g.mu.Unlock()
					}
					return nil
				}
				if err := m.Reconcile(ctx, b, "B"); !errors.Is(err, ErrGroupNameUnknown) {
					t.Fatal("unverified PUT called confirmed", err)
				}
				if g.puts.Load() != 1 {
					t.Fatal("unexpected PUT count")
				}
			}
		})
	}
}

func TestGroupNamesRateLimitDeadlineDurableAndLatestTitle(t *testing.T) {
	m, g, b, path := nameManager(t)
	ctx := context.Background()
	at := int64(1000)
	m.Now = func() time.Time { return time.UnixMilli(at) }
	g.update = func(context.Context, string) error {
		at = 3000
		return &feishu.RateLimited{RetryAfter: 5 * time.Second}
	}
	err := m.Reconcile(ctx, b, "B")
	var deferred *GroupNameDeferredError
	if !errors.As(err, &deferred) || deferred.RetryAtMS != 8000 {
		t.Fatal("response-time delay missing", err)
	}
	m.Store.Close()
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	m.Store = reopened
	gets, bots := g.gets.Load(), g.bots.Load()
	for n := 0; n < 4; n++ {
		if err := m.Reconcile(ctx, b, "C"); !errors.Is(err, ErrGroupNameDeferred) {
			t.Fatal(err)
		}
	}
	if g.gets.Load() != gets || g.bots.Load() != bots || g.puts.Load() != 1 {
		t.Fatal("waiting calls API")
	}
	at = 8000
	g.update = nil
	if err := m.Reconcile(ctx, b, "C"); err != nil || g.puts.Load() != 2 || g.info.Name != "C" || nameJournalCount(t, m) != 2 {
		t.Fatal("due retry did not use latest title", err)
	}
}

func TestGroupNamesPermanentRejectionSameTitleAndConcurrentClaim(t *testing.T) {
	m, g, b, _ := nameManager(t)
	ctx := context.Background()
	g.update = func(context.Context, string) error { return &feishu.Rejected{Code: 99991672} }
	if err := m.Reconcile(ctx, b, "B"); !errors.Is(err, ErrGroupNameRejected) {
		t.Fatal(err)
	}
	gets, bots := g.gets.Load(), g.bots.Load()
	if err := m.Reconcile(ctx, b, "B"); !errors.Is(err, ErrGroupNameRejected) || g.gets.Load() != gets || g.bots.Load() != bots || g.puts.Load() != 1 {
		t.Fatal("permanent same title retried", err)
	}
	g.update = nil
	var wg sync.WaitGroup
	for n := 0; n < 20; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.Reconcile(ctx, b, "C"); err != nil && !errors.Is(err, ErrGroupNameBusy) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if g.puts.Load() != 2 || nameJournalCount(t, m) != 2 {
		t.Fatal("parallel calls double-dispatched", g.puts.Load())
	}
}

func TestGroupNamesPersistsBeforePUTAndJournalFailureDoesNotWrite(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "journal-first", true: "journal-fails"}[fail], func(t *testing.T) {
			m, g, b, _ := nameManager(t)
			ctx := context.Background()
			if fail {
				if _, err := m.Store.DB.Exec(`CREATE TRIGGER fail_group_name BEFORE INSERT ON m0_operations WHEN NEW.kind='feishu.chat.name_sync' BEGIN SELECT RAISE(FAIL,'fixture journal failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			g.update = func(_ context.Context, desired string) error {
				a, err := m.Store.GroupNameAttempt(ctx, store.GroupNameScope{AppID: "app", OwnerID: "owner", Binding: b})
				if err != nil || a.State != "dispatching" || a.Plan.Desired != desired || a.Plan.Previous != "A" {
					t.Fatal("PUT preceded frozen dispatch journal", a, err)
				}
				g.setName(desired)
				return nil
			}
			err := m.Reconcile(ctx, b, "B")
			if fail {
				if err == nil || g.puts.Load() != 0 || nameJournalCount(t, m) != 0 {
					t.Fatal("journal failure dispatched", err)
				}
			} else if err != nil || g.puts.Load() != 1 {
				t.Fatal(err)
			}
		})
	}
}
