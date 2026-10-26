package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

const runtimeVerificationSource = `"feishu.bridge.validation"`

type runtimeVerificationNative struct {
	*runtimeNative
	attachCount atomic.Int32
	turnsErr    error
}

func (n *runtimeVerificationNative) Attach(ctx context.Context, id string) (codexclient.Thread, error) {
	n.attachCount.Add(1)
	return n.runtimeNative.Attach(ctx, id)
}

func (n *runtimeVerificationNative) Turns(ctx context.Context, id string) ([]codexclient.Turn, error) {
	if n.turnsErr != nil {
		return nil, n.turnsErr
	}
	return n.runtimeNative.Turns(ctx, id)
}

// Build a private, durable route without inventing a native first user item.
func runtimeVerificationBind(t *testing.T, r *Runtime, thread string) {
	t.Helper()
	ctx := context.Background()
	s := r.Store
	if _, err := s.BeginBaseline(ctx, r.HostID, r.AppID, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteBaseline(ctx, r.HostID, nil); err != nil {
		t.Fatal(err)
	}
	request := store.ID()
	connection := strings.Repeat("a", 32)
	n := store.VerificationNative{RequestID: request, HostID: r.HostID, AppID: r.AppID, OwnerID: r.OwnerID, BridgeCommit: "fixture-build", BinarySHA256: strings.Repeat("b", 64), ConnectionID: connection, CWD: r.AllowedRoots[0], ModelURL: "http://127.0.0.1:1234/v1", RequestJSON: `{"model":"fixture-model"}`, CreatedAtMS: 100_000, ExpiresAtMS: 1_000_000}
	if _, err := s.PrepareVerificationNative(ctx, n); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimVerificationNative(ctx, n, 7, connection, 200_000); err != nil || !ok {
		t.Fatalf("native claim: %v %v", ok, err)
	}
	n.Generation, n.State = 7, "dispatching"
	if err := s.FinishVerificationNative(ctx, n, "confirmed", thread); err != nil {
		t.Fatal(err)
	}
	n, err := s.VerificationNative(ctx, request, r.HostID, r.AppID, r.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	g := store.VerificationGroup{RequestID: request, HostID: r.HostID, AppID: r.AppID, OwnerID: r.OwnerID, BridgeCommit: n.BridgeCommit, BinarySHA256: n.BinarySHA256, UUID: "group-" + request, Name: "fixture group", MembersJSON: `["owner"]`, CreatedAtMS: 100_000, ExpiresAtMS: 1_000_000}
	if _, err := s.PrepareVerificationGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimVerificationGroup(ctx, g, 200_000); err != nil || !ok {
		t.Fatalf("group claim: %v %v", ok, err)
	}
	g.State = "dispatching"
	if err := s.FinishVerificationGroup(ctx, g, "confirmed", "chat-"+thread); err != nil {
		t.Fatal(err)
	}
	g, err = s.VerificationGroup(ctx, request, r.HostID, r.AppID, r.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ObserveVerificationGroupMembership(ctx, g, 220_000); err != nil || !ok {
		t.Fatalf("membership: %v %v", ok, err)
	}
	if ok, err := s.ObserveVerificationGroupMessage(ctx, r.HostID, r.AppID, r.OwnerID, request, "fixture-message", g.ChatID, 220_000); err != nil || !ok {
		t.Fatalf("incoming observation: %v %v", ok, err)
	}
	g, err = s.VerificationGroup(ctx, request, r.HostID, r.AppID, r.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterVerificationBinding(ctx, n, g, "bot", 300_000); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeVerificationAuthorizePrecedesOrdinaryClassification(t *testing.T) {
	_, n, _, r := runtimeFixture(t)
	n.set("verified", r.AllowedRoots[0], "completed")
	meta, _ := n.Metadata(context.Background(), "verified")
	meta.ThreadSource = json.RawMessage(runtimeVerificationSource)
	if err := r.Authorize(context.Background(), meta, false); !errors.Is(err, store.ErrIneligible) {
		t.Fatalf("validation without callback accepted: %v", err)
	}
	rejected := errors.New("fixture marker rejected")
	calls := 0
	r.VerificationAuthorize = func(_ context.Context, current codexclient.Thread, automatic bool) (bool, error) {
		calls++
		if current.ID != "verified" {
			return false, nil
		}
		if automatic || string(current.ThreadSource) != runtimeVerificationSource {
			return true, rejected
		}
		return true, nil
	}
	if err := r.Authorize(context.Background(), meta, false); err != nil {
		t.Fatal(err)
	}
	if err := r.Authorize(context.Background(), meta, true); !errors.Is(err, rejected) {
		t.Fatalf("automatic verification fell through: %v", err)
	}
	meta.ThreadSource = json.RawMessage(`"user"`)
	if !meta.InteractiveMain() {
		t.Fatal("ordinary-source fixture must otherwise be eligible")
	}
	if err := r.Authorize(context.Background(), meta, false); !errors.Is(err, rejected) {
		t.Fatalf("failed marker fell back to ordinary authorization: %v", err)
	}
	meta.ID = "ordinary"
	if err := r.Authorize(context.Background(), meta, false); err != nil {
		t.Fatalf("unhandled ordinary thread changed: %v", err)
	}
	if calls != 4 {
		t.Fatalf("callback calls=%d", calls)
	}
	r.VerificationAuthorize = func(context.Context, codexclient.Thread, bool) (bool, error) {
		return false, rejected
	}
	if err := r.Authorize(context.Background(), meta, false); !errors.Is(err, rejected) {
		t.Fatal("failed marker lookup fell through ordinary classification", err)
	}
}

func TestRuntimeVerificationActivateSerializesValidRoutesAndShutdown(t *testing.T) {
	_, n, g, r := runtimeFixture(t)
	r.DiscoveryEnabled = false
	n.set("verified", r.AllowedRoots[0], "completed")
	n.mu.Lock()
	meta := n.threads["verified"]
	meta.ThreadSource = json.RawMessage(runtimeVerificationSource)
	n.threads["verified"] = meta
	n.mu.Unlock()
	wrapped := &runtimeVerificationNative{runtimeNative: n, turnsErr: &codexclient.RPCError{Code: -32600, Message: "thread/turns/list is unavailable before first user message"}}
	r.Native = wrapped
	var valid atomic.Bool
	r.VerificationAuthorize = func(_ context.Context, current codexclient.Thread, automatic bool) (bool, error) {
		if current.ID != "verified" {
			return false, nil
		}
		if !valid.Load() || automatic || string(current.ThreadSource) != runtimeVerificationSource {
			return true, store.ErrIneligible
		}
		return true, nil
	}
	observed := make(chan struct{}, 1)
	r.EventObserved = func(codexclient.Event) { observed <- struct{}{} }
	notices := make(chan string, 256)
	r.Notice = func(code string) { notices <- code }
	hydrated := make(chan int, 256)
	r.HistoryHydrated = func(_ context.Context, _ string, turns []codexclient.Turn) error {
		hydrated <- len(turns)
		return nil
	}
	r.Activate("verified") // Before Run is intentionally inert.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("runtime did not join")
		}
	})
	n.events <- codexclient.Event{Method: "fixture/barrier", Params: json.RawMessage(`{}`)}
	select {
	case <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not enter main select")
	}
	r.Activate("verified") // No durable route yet.
	select {
	case <-notices:
	case <-time.After(5 * time.Second):
		t.Fatal("missing binding was not rejected")
	}
	runtimeVerificationBind(t, r, "verified")
	r.Wake("verified") // A newly persisted route needs its explicit activation handoff.
	r.mu.Lock()
	actors := len(r.actors)
	r.mu.Unlock()
	if actors != 0 {
		t.Fatal("unknown Wake activated a route before its handoff")
	}
	r.Activate("verified") // Durable marker is currently invalid.
	select {
	case <-notices:
	case <-time.After(5 * time.Second):
		t.Fatal("invalid activation was not rejected")
	}
	if wrapped.attachCount.Load() != 0 {
		t.Fatal("invalid activation attached a native thread")
	}
	valid.Store(true)
	r.Activate("verified")
	select {
	case turns := <-hydrated:
		if turns != 0 {
			t.Fatalf("empty native history invented %d turns", turns)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("valid empty verification thread did not hydrate")
	}
	if o, err := r.Store.Observation(ctx, r.HostID, "verified"); err != nil || o.FirstUserItemID != "" {
		t.Fatalf("native first user item invented: %+v %v", o, err)
	}
	var senders sync.WaitGroup
	for i := 0; i < 8; i++ {
		senders.Add(1)
		go func() {
			defer senders.Done()
			for j := 0; j < 100; j++ {
				r.Activate("verified")
				r.Activate("")
			}
		}()
	}
	senders.Wait()
	r.mu.Lock()
	actors = len(r.actors)
	r.mu.Unlock()
	if actors != 1 || wrapped.attachCount.Load() != 1 {
		t.Fatalf("duplicate actors=%d attaches=%d", actors, wrapped.attachCount.Load())
	}
	for i := 0; i < 8; i++ {
		senders.Add(1)
		go func() {
			defer senders.Done()
			for j := 0; j < 100; j++ {
				r.Activate("verified")
				r.Wake("verified")
			}
		}()
	}
	cancel()
	senders.Wait()
	runtimeWait(t, func() bool { r.mu.Lock(); defer r.mu.Unlock(); return !r.running })
	r.Activate("verified")
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.groups != 0 || len(g.writes) != 0 {
		t.Fatalf("empty activation caused external writes: groups=%d writes=%d", g.groups, len(g.writes))
	}
}

func TestRuntimeVerificationEmptyHistoryIsExclusiveAndExistingRouteCannotCreateGroup(t *testing.T) {
	for _, mode := range []string{"verification", "ordinary", "no_callback", "other_rpc_error"} {
		t.Run(mode, func(t *testing.T) {
			_, n, g, r := runtimeFixture(t)
			n.set("thread", r.AllowedRoots[0], "completed")
			if mode == "ordinary" {
				runtimeBind(t, r.Store, r, "thread", "chat-thread")
			} else {
				n.mu.Lock()
				meta := n.threads["thread"]
				meta.ThreadSource = json.RawMessage(runtimeVerificationSource)
				n.threads["thread"] = meta
				n.mu.Unlock()
				runtimeVerificationBind(t, r, "thread")
			}
			before := &codexclient.RPCError{Code: -32600, Message: "thread/turns/list is unavailable before first user message"}
			if mode == "other_rpc_error" {
				before.Message = "thread has no rollout"
			}
			r.Native = &runtimeVerificationNative{runtimeNative: n, turnsErr: before}
			if mode != "no_callback" {
				r.VerificationAuthorize = func(_ context.Context, current codexclient.Thread, automatic bool) (bool, error) {
					if mode == "ordinary" {
						return false, nil
					}
					if automatic || string(current.ThreadSource) != runtimeVerificationSource {
						return true, store.ErrIneligible
					}
					return true, nil
				}
			}
			var hydrations atomic.Int32
			var preparations atomic.Int32
			r.HistoryPreparing = func(context.Context, string, []codexclient.Turn) error {
				preparations.Add(1)
				return nil
			}
			r.HistoryHydrated = func(_ context.Context, _ string, turns []codexclient.Turn) error {
				if len(turns) != 0 {
					t.Error("empty history fabricated turns")
				}
				hydrations.Add(1)
				return nil
			}
			notices := make(chan string, 256)
			r.Notice = func(code string) { notices <- code }
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- r.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("runtime did not stop")
				}
			})
			if mode == "verification" {
				runtimeWait(t, func() bool { return hydrations.Load() >= 1 })
				if _, err := r.Store.DB.ExecContext(ctx, `DELETE FROM bindings WHERE host_id=? AND thread_id=?`, r.HostID, "thread"); err != nil {
					t.Fatal(err)
				}
				prepared := preparations.Load()
				r.Wake("thread")
				runtimeWait(t, func() bool { return preparations.Load() > prepared })
				// An excluded verification observation without its route cannot
				// stage history; a previously hydrated actor must still never
				// create a replacement group after that local rejection.
				select {
				case <-notices:
				case <-time.After(5 * time.Second):
					t.Fatal("missing route did not block staging")
				}
				if _, err := r.Store.Binding(ctx, r.HostID, "thread"); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("binding unexpectedly replaced: %v", err)
				}
			} else {
				select {
				case <-notices:
				case <-time.After(5 * time.Second):
					t.Fatal("expected authorization/history rejection")
				}
				if hydrations.Load() != 0 {
					t.Fatal("ordinary or invalid history hydrated")
				}
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.groups != 0 || len(g.writes) != 0 {
				t.Fatalf("unexpected group/message write: %d/%d", g.groups, len(g.writes))
			}
		})
	}
}
