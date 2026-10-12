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

type testGateway struct {
	send  func(context.Context, string, string, string, string) (string, error)
	calls atomic.Int32
}

func (g *testGateway) SendCard(ctx context.Context, kind, owner, card, uuid string) (string, error) {
	g.calls.Add(1)
	return g.send(ctx, kind, owner, card, uuid)
}
func managerFixture(t *testing.T) (*Manager, *testGateway, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "verification.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	g := &testGateway{send: func(_ context.Context, kind, owner, card, uuid string) (string, error) {
		if kind != "open_id" || owner != "ou_owner" || uuid == "" || !strings.Contains(card, "verification_confirm") || strings.Contains(card, "<at") {
			t.Fatal("unsafe probe destination or body")
		}
		return "om_card", nil
	}}
	m := &Manager{Store: db, Gateway: g, HostID: "host", AppID: "app", OwnerID: "ou_owner", Commit: strings.Repeat("a", 40), BinarySHA256: strings.Repeat("b", 64), Ready: func() bool { return true }, Now: func() time.Time { return time.UnixMilli(1_000_000) }}
	return m, g, path
}
func TestConcurrentBeginAndCallbackBeforeSendResponse(t *testing.T) {
	m, g, _ := managerFixture(t)
	ctx := context.Background()
	id := store.ID()
	entered, release := make(chan struct{}), make(chan struct{})
	g.send = func(ctx context.Context, kind, owner, card, uuid string) (string, error) {
		c, err := m.Store.VerificationCard(ctx, id, m.HostID, m.AppID, m.OwnerID)
		if err != nil {
			t.Error(err)
			return "", err
		}
		if c.State != "dispatching" || c.Card != card || c.UUID != uuid {
			t.Error("wire left before durable claim")
		}
		ok, err := m.Callback(ctx, owner, c.Token, "om_card", "oc_private")
		if err != nil || !ok {
			t.Error("early callback lost", err)
		}
		ok, err = m.PrivateMessage(ctx, owner, c.Token, "om_ping")
		if err != nil || !ok {
			t.Error("private ping lost", err)
		}
		close(entered)
		<-release
		return "om_card", nil
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if r, err := m.Begin(ctx, id); err != nil || r.CallbackStatus != "pass" || r.PrivateMessageStatus != "pass" {
			t.Error(r, err)
		}
	}()
	<-entered
	replays := make(chan struct{}, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := m.Begin(ctx, id)
			if err != nil || r.State != "unknown" || r.CallbackStatus != "pending" {
				t.Error(r, err)
			}
			replays <- struct{}{}
		}()
	}
	for i := 0; i < 12; i++ {
		<-replays
	}
	r, err := m.Begin(ctx, id)
	if err != nil || r.State != "unknown" {
		t.Fatal(r, err)
	}
	close(release)
	wg.Wait()
	if g.calls.Load() != 1 {
		t.Fatal("duplicate send", g.calls.Load())
	}
	r, err = m.State(ctx, id)
	if err != nil || r.CardStatus != "pass" || r.CallbackStatus != "pass" || r.PrivateMessageStatus != "pass" {
		t.Fatal(r, err)
	}
	b, _ := json.Marshal(r)
	c, _ := m.Store.VerificationCard(ctx, id, m.HostID, m.AppID, m.OwnerID)
	if strings.Contains(string(b), c.Token) || strings.Contains(string(b), "/verify") || strings.Contains(string(b), c.Card) {
		t.Fatal("token or body leaked")
	}
	private, _ := json.Marshal(c)
	if string(private) != "{}" {
		t.Fatal("journal serializes private data")
	}
}

func TestUnknownAndCrashResumeNeverSendAgain(t *testing.T) {
	for _, mode := range []string{"unknown", "cancelled", "crash", "rejected"} {
		t.Run(mode, func(t *testing.T) {
			m, g, path := managerFixture(t)
			ctx := context.Background()
			id := store.ID()
			if mode == "crash" {
				token := store.ID()
				c, err := m.Store.PrepareVerificationCard(ctx, store.VerificationCard{RequestID: id, HostID: m.HostID, AppID: m.AppID, OwnerID: m.OwnerID, BridgeCommit: m.Commit, BinarySHA256: m.BinarySHA256, Token: token, UUID: store.ID(), Card: "frozen", CreatedAtMS: 1_000_000, ExpiresAtMS: 1_300_000})
				if err != nil {
					t.Fatal(err)
				}
				if ok, err := m.Store.ClaimVerificationCard(ctx, c, 1_000_000); err != nil || !ok {
					t.Fatal(ok, err)
				}
			} else {
				callCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				g.send = func(context.Context, string, string, string, string) (string, error) {
					if mode == "cancelled" {
						cancel()
					}
					if mode == "rejected" {
						return "", &feishu.Rejected{Code: 99991672}
					}
					return "", errors.New("PRIVATE API BODY")
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
				if err != nil {
					t.Fatal(err)
				}
				wanted := "unknown"
				if mode == "rejected" {
					wanted = "rejected"
				}
				if r.State != wanted || r.CardStatus == "pass" {
					t.Fatal(r)
				}
			}
			if g.calls.Load() != before {
				t.Fatal("replayed ambiguous send")
			}
		})
	}
}

func TestProbeOwnerIdentityExpiryAndBuildChange(t *testing.T) {
	m, g, _ := managerFixture(t)
	ctx := context.Background()
	id := store.ID()
	if _, err := m.Begin(ctx, id); err != nil {
		t.Fatal(err)
	}
	c, _ := m.Store.VerificationCard(ctx, id, m.HostID, m.AppID, m.OwnerID)
	for _, args := range [][4]string{{"ou_other", c.Token, "om_card", "chat"}, {m.OwnerID, store.ID(), "om_card", "chat"}, {m.OwnerID, c.Token, "other_message", "chat"}} {
		if ok, err := m.Callback(ctx, args[0], args[1], args[2], args[3]); err != nil || ok {
			t.Fatal("bad callback accepted", ok, err)
		}
	}
	if ok, err := m.PrivateMessage(ctx, "ou_other", c.Token, "ping"); err != nil || ok {
		t.Fatal("other user accepted")
	}
	m.Now = func() time.Time { return time.UnixMilli(c.ExpiresAtMS) }
	if ok, err := m.Callback(ctx, m.OwnerID, c.Token, "om_card", "chat"); err != nil || ok {
		t.Fatal("expired callback accepted")
	}
	if ok, err := m.PrivateMessage(ctx, m.OwnerID, c.Token, "ping"); err != nil || ok {
		t.Fatal("expired ping accepted")
	}
	r, err := m.Begin(ctx, id)
	if err != nil || !r.Expired || g.calls.Load() != 1 {
		t.Fatal(r, err)
	}
	m.OwnerID = "ou_other"
	if _, err := m.State(ctx, id); err == nil {
		t.Fatal("cross-owner probe visible")
	}
	if _, err := m.Begin(ctx, id); err == nil {
		t.Fatal("cross-owner ID reused")
	}
	if g.calls.Load() != 1 {
		t.Fatal("cross-owner send")
	}
	if _, err := m.Begin(ctx, "not-a-valid-id"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestNotReadyOrCancelledDoesNotDispatchAndBuildChangePreservesIntent(t *testing.T) {
	m, g, _ := managerFixture(t)
	ctx := context.Background()
	m.Ready = func() bool { return false }
	if _, err := m.Begin(ctx, store.ID()); !errors.Is(err, ErrNotReady) || g.calls.Load() != 0 {
		t.Fatal(err)
	}
	m.Ready = func() bool { return true }
	id := store.ID()
	c, err := m.Store.PrepareVerificationCard(ctx, store.VerificationCard{RequestID: id, HostID: m.HostID, AppID: m.AppID, OwnerID: m.OwnerID, BridgeCommit: "old-build", BinarySHA256: m.BinarySHA256, Token: store.ID(), UUID: store.ID(), Card: "old-frozen-card", CreatedAtMS: 1_000_000, ExpiresAtMS: 1_300_000})
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.Begin(ctx, id)
	if err != nil || r.BridgeCommit != c.BridgeCommit || !r.BuildChanged || r.State != "ready" || g.calls.Load() != 0 {
		t.Fatal(r, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.Begin(cancelled, store.ID()); err == nil || g.calls.Load() != 0 {
		t.Fatal("cancelled request dispatched")
	}
}

func TestDifferentBinaryWithSameCommitCannotDispatchOrPromotePriorProof(t *testing.T) {
	m, g, _ := managerFixture(t)
	m.Commit = "development"
	ctx := context.Background()
	confirmedID := store.ID()
	if _, err := m.Begin(ctx, confirmedID); err != nil {
		t.Fatal(err)
	}
	confirmed, err := m.Store.VerificationCard(ctx, confirmedID, m.HostID, m.AppID, m.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := m.Callback(ctx, m.OwnerID, confirmed.Token, "om_card", "oc_private"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := m.PrivateMessage(ctx, m.OwnerID, confirmed.Token, "om_ping"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	readyID := store.ID()
	_, err = m.Store.PrepareVerificationCard(ctx, store.VerificationCard{RequestID: readyID, HostID: m.HostID, AppID: m.AppID, OwnerID: m.OwnerID, BridgeCommit: m.Commit, BinarySHA256: m.BinarySHA256, Token: store.ID(), UUID: store.ID(), Card: "frozen old build", CreatedAtMS: 1_000_000, ExpiresAtMS: 1_300_000})
	if err != nil {
		t.Fatal(err)
	}
	originalHash := m.BinarySHA256
	m.BinarySHA256 = strings.Repeat("c", 64)
	for _, id := range []string{confirmedID, readyID} {
		r, err := m.Begin(ctx, id)
		if err != nil || !r.BuildChanged || r.BridgeCommit != "development" || r.BinarySHA256 != originalHash {
			t.Fatal("same commit hid binary change", r, err)
		}
		if id == confirmedID && (r.CallbackStatus != "pass" || r.PrivateMessageStatus != "pass") {
			t.Fatal("historical evidence changed", r)
		}
	}
	if g.calls.Load() != 1 {
		t.Fatal("changed binary dispatched old intent")
	}
}
