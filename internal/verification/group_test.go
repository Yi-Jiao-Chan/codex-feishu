package verification

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

type groupGateway struct {
	create       func(context.Context, string, string, []string, string) (string, error)
	members      []string
	memberErr    error
	bot          bool
	calls, reads atomic.Int32
}

func (g *groupGateway) CreateGroup(ctx context.Context, name, owner string, members []string, uuid string) (string, error) {
	g.calls.Add(1)
	return g.create(ctx, name, owner, members, uuid)
}
func (g *groupGateway) BotInChat(context.Context, string) (bool, error) {
	g.reads.Add(1)
	return g.bot, nil
}
func (g *groupGateway) MemberIDs(context.Context, string) ([]string, error) {
	g.reads.Add(1)
	return g.members, g.memberErr
}
func groupManagerFixture(t *testing.T) (*GroupManager, *groupGateway, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "group.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	gateway := &groupGateway{bot: true, members: []string{"ou_owner"}, create: func(_ context.Context, name, owner string, members []string, uuid string) (string, error) {
		if !strings.HasPrefix(name, "Codex Feishu · 验证 ") || owner != "ou_owner" || len(members) != 1 || members[0] != owner || uuid == "" {
			t.Error("unsafe verification group")
		}
		return "oc_test", nil
	}}
	manager := &GroupManager{Store: db, Gateway: gateway, HostID: "host", AppID: "app", OwnerID: "ou_owner", BotID: "ou_bot", Commit: "development", BinarySHA256: strings.Repeat("b", 64), Ready: func() bool { return true }, Now: func() time.Time { return time.UnixMilli(1_000_000) }}
	return manager, gateway, path
}

func TestManagedGroupConcurrentReplayAndScopedIncomingProof(t *testing.T) {
	m, g, _ := groupManagerFixture(t)
	ctx := context.Background()
	id := store.ID()
	entered, release := make(chan struct{}), make(chan struct{})
	g.create = func(ctx context.Context, name, owner string, members []string, uuid string) (string, error) {
		record, err := m.Store.VerificationGroup(ctx, id, m.HostID, m.AppID, m.OwnerID)
		if err != nil || record.State != "dispatching" || record.UUID != uuid || record.Name != name || record.MembersJSON != `["ou_owner"]` {
			t.Error("group API preceded frozen claim", record, err)
		}
		close(entered)
		<-release
		return "oc_test", nil
	}
	first := make(chan error, 1)
	go func() { _, err := m.Begin(ctx, id); first <- err }()
	<-entered
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := m.Begin(ctx, id)
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
	if g.calls.Load() != 1 {
		t.Fatal("duplicate group")
	}
	r, err := m.State(ctx, id)
	if err != nil || r.GroupStatus != "pass" || r.MembershipStatus != "pass" || r.GroupMessageStatus != "pending" {
		t.Fatal(r, err)
	}
	for _, args := range [][4]string{{"ou_other", id, "message", "oc_test"}, {m.OwnerID, id, "message", "other-chat"}, {m.OwnerID, store.ID(), "message", "oc_test"}} {
		if ok, err := m.Message(ctx, args[0], args[1], args[2], args[3]); err != nil || ok {
			t.Fatal("bad group input accepted", ok, err)
		}
	}
	for i := 0; i < 2; i++ {
		if ok, err := m.Message(ctx, m.OwnerID, id, "om_group", "oc_test"); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	r, err = m.State(ctx, id)
	if err != nil || r.GroupMessageStatus != "pass" {
		t.Fatal(r, err)
	}
	before := g.reads.Load()
	if _, err := m.State(ctx, id); err != nil || g.reads.Load() != before {
		t.Fatal("read-only state issued external request")
	}
	record, _ := m.Store.VerificationGroup(ctx, id, m.HostID, m.AppID, m.OwnerID)
	body, _ := json.Marshal(record)
	if string(body) != "{}" {
		t.Fatal("private roster/name/uuid leaked")
	}
}

func TestGroupUnknownCancellationAndCrashNeverCreateAgain(t *testing.T) {
	for _, mode := range []string{"unknown", "cancelled", "crash", "rejected"} {
		t.Run(mode, func(t *testing.T) {
			m, g, path := groupManagerFixture(t)
			ctx := context.Background()
			id := store.ID()
			if mode == "crash" {
				record, err := m.Store.PrepareVerificationGroup(ctx, store.VerificationGroup{RequestID: id, HostID: m.HostID, AppID: m.AppID, OwnerID: m.OwnerID, BridgeCommit: m.Commit, BinarySHA256: m.BinarySHA256, UUID: store.ID(), Name: "Codex Feishu · 验证 " + id[:8], MembersJSON: `["ou_owner"]`, CreatedAtMS: 1_000_000, ExpiresAtMS: 1_300_000})
				if err != nil {
					t.Fatal(err)
				}
				if ok, err := m.Store.ClaimVerificationGroup(ctx, record, 1_000_000); err != nil || !ok {
					t.Fatal(ok, err)
				}
			} else {
				callCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				g.create = func(context.Context, string, string, []string, string) (string, error) {
					if mode == "cancelled" {
						cancel()
					}
					if mode == "rejected" {
						return "", &feishu.Rejected{Code: 99991672}
					}
					return "", errors.New("PRIVATE SERVER BODY")
				}
				if _, err := m.Begin(callCtx, id); err != nil {
					t.Fatal(err)
				}
			}
			before := g.calls.Load()
			m.Store.Close()
			db, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			m.Store = db
			for i := 0; i < 2; i++ {
				r, err := m.Begin(ctx, id)
				expected := "unknown"
				if mode == "rejected" {
					expected = "rejected"
				}
				if err != nil || r.State != expected || r.GroupStatus == "pass" {
					t.Fatal(r, err)
				}
			}
			if g.calls.Load() != before || g.reads.Load() != 0 {
				t.Fatal("ambiguous intent reopened")
			}
		})
	}
}

func TestGroupMembershipReadsMayResumeButDoNotRecreate(t *testing.T) {
	for _, mode := range []string{"error", "missing-owner", "extra-person", "bot-absent"} {
		t.Run(mode, func(t *testing.T) {
			m, g, _ := groupManagerFixture(t)
			ctx := context.Background()
			id := store.ID()
			switch mode {
			case "error":
				g.memberErr = errors.New("private")
			case "missing-owner":
				g.members = []string{m.BotID}
			case "extra-person":
				g.members = []string{m.OwnerID, "ou_other"}
			case "bot-absent":
				g.bot = false
			}
			r, err := m.Begin(ctx, id)
			if err != nil || r.GroupStatus != "pass" || r.MembershipStatus != "pending" {
				t.Fatal(r, err)
			}
			if ok, err := m.Message(ctx, m.OwnerID, id, "early-ping", "oc_test"); err != nil || !ok {
				t.Fatal(ok, err)
			}
			r, _ = m.State(ctx, id)
			if r.GroupMessageStatus != "pending" {
				t.Fatal("unproved membership passed inbound")
			}
			g.bot = true
			g.members = []string{m.OwnerID, m.BotID}
			g.memberErr = nil
			r, err = m.Begin(ctx, id)
			if err != nil || r.MembershipStatus != "pass" || r.GroupMessageStatus != "pass" || g.calls.Load() != 1 {
				t.Fatal(r, err)
			}
		})
	}
}

func TestGroupBuildIdentityAndExpiryBlockMutation(t *testing.T) {
	m, g, _ := groupManagerFixture(t)
	ctx := context.Background()
	id := store.ID()
	if _, err := m.Begin(ctx, id); err != nil {
		t.Fatal(err)
	}
	m.BinarySHA256 = strings.Repeat("c", 64)
	r, err := m.Begin(ctx, id)
	if err != nil || !r.BuildChanged || g.calls.Load() != 1 {
		t.Fatal(r, err)
	}
	ready := store.ID()
	_, err = m.Store.PrepareVerificationGroup(ctx, store.VerificationGroup{RequestID: ready, HostID: m.HostID, AppID: m.AppID, OwnerID: m.OwnerID, BridgeCommit: m.Commit, BinarySHA256: strings.Repeat("b", 64), UUID: store.ID(), Name: "Codex Feishu · 验证 " + ready[:8], MembersJSON: `["ou_owner"]`, CreatedAtMS: 1_000_000, ExpiresAtMS: 1_300_000})
	if err != nil {
		t.Fatal(err)
	}
	r, err = m.Begin(ctx, ready)
	if err != nil || !r.BuildChanged || r.State != "ready" || g.calls.Load() != 1 {
		t.Fatal(r, err)
	}
	m.Now = func() time.Time { return time.UnixMilli(1_300_000) }
	if ok, err := m.Message(ctx, m.OwnerID, id, "late-ping", "oc_test"); err != nil || ok {
		t.Fatal("expired input accepted")
	}
	r, err = m.Begin(ctx, id)
	if err != nil || !r.Expired || g.calls.Load() != 1 {
		t.Fatal(r, err)
	}
	if _, err := m.State(ctx, store.ID()); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing journal was not identified", err)
	}
}
