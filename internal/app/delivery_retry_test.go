package app

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

type deliveryRetryNative struct {
	*runtimeNative
	reads        atomic.Int32
	denyMetadata atomic.Bool
}

func (n *deliveryRetryNative) Turns(ctx context.Context, id string) ([]codexclient.Turn, error) {
	n.reads.Add(1)
	return n.runtimeNative.Turns(ctx, id)
}

func (n *deliveryRetryNative) Metadata(ctx context.Context, id string) (codexclient.Thread, error) {
	n.reads.Add(1)
	if n.denyMetadata.Load() {
		return codexclient.Thread{}, errors.New("native metadata unavailable")
	}
	return n.runtimeNative.Metadata(ctx, id)
}

func TestRuntimeProgressDeadlinePatchesWithoutEventOrNativeRead(t *testing.T) {
	db, native, g, r := runtimeFixture(t)
	n := &deliveryRetryNative{runtimeNative: native}
	r.Native = n
	r.DiscoveryEnabled = false
	r.MinProgressIntervalMS = 100
	n.set("thread", r.AllowedRoots[0], "inProgress")
	runtimeBind(t, db, r, "thread", "chat")
	var clock atomic.Int64
	initial := time.Now().UnixMilli()
	clock.Store(initial)
	r.Now = func() time.Time { return time.UnixMilli(clock.Load()) }
	var grants atomic.Int32
	r.BeginDelivery = func(ctx context.Context, _ store.Binding) (context.Context, func(), error) {
		grants.Add(1)
		return ctx, func() {}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &threadActor{thread: "thread", wake: make(chan struct{}, 1)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.runActor(ctx, a)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("actor did not stop")
		}
	})
	progress := func() store.Mirror {
		ms, err := db.Mirrors(ctx, "host", "thread")
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range ms {
			if m.Kind == "progress" {
				return m
			}
		}
		return store.Mirror{}
	}
	runtimeWait(t, func() bool { m := progress(); return m.Key != "" && m.Revision == m.DeliveredRevision })
	original := progress()
	clock.Store(initial + 50)
	n.mu.Lock()
	turn := n.turns["thread"][0]
	turn.Items = append(append([]codexclient.Item(nil), turn.Items...), codexclient.Item{ID: "commentary", Type: "agentMessage", Text: "latest public progress", Phase: str("commentary")})
	n.turns["thread"] = []codexclient.Turn{turn}
	n.mu.Unlock()
	a.wake <- struct{}{}
	runtimeWait(t, func() bool { m := progress(); return m.Revision > m.DeliveredRevision })
	readsBeforeRetry := n.reads.Load()
	clock.Store(initial + 99)
	time.Sleep(3 * r.PollInterval)
	g.mu.Lock()
	for _, w := range g.writes {
		if w.op == "update" {
			t.Error("progress patched before its deadline")
		}
	}
	g.mu.Unlock()
	grantsBeforeRetry := grants.Load()
	clock.Store(initial + 100) // No subsequent wake or native event.
	runtimeWait(t, func() bool {
		m := progress()
		return m.Revision > original.Revision && m.Revision == m.DeliveredRevision
	})
	time.Sleep(3 * r.PollInterval)
	g.mu.Lock()
	updates := 0
	for _, w := range g.writes {
		if w.op == "update" {
			updates++
			if w.target != original.RemoteID || !strings.Contains(w.card, "latest public progress") {
				t.Error("retry did not patch the original card with the latest body")
			}
		}
	}
	g.mu.Unlock()
	if updates != 1 || n.reads.Load() != readsBeforeRetry || grants.Load() <= grantsBeforeRetry {
		t.Fatalf("retry violated delivery/native boundaries: updates=%d reads=%d/%d grants=%d/%d", updates, n.reads.Load(), readsBeforeRetry, grants.Load(), grantsBeforeRetry)
	}
}

func TestRuntimeDueDeliveryRechecksAuthorization(t *testing.T) {
	db, native, g, r := runtimeFixture(t)
	n := &deliveryRetryNative{runtimeNative: native}
	r.Native = n
	r.DiscoveryEnabled = false
	n.set("thread", r.AllowedRoots[0], "inProgress")
	runtimeBind(t, db, r, "thread", "chat")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var allowed atomic.Bool
	var checks atomic.Int32
	r.CanDeliver = func(context.Context, store.Binding) error {
		checks.Add(1)
		if !allowed.Load() {
			return errors.New("delivery revoked")
		}
		return nil
	}
	a := &threadActor{thread: "thread", wake: make(chan struct{}, 1)}
	done := make(chan struct{})
	go func() { defer close(done); r.runActor(ctx, a) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("actor did not stop")
		}
	})
	runtimeWait(t, func() bool { return checks.Load() >= 3 })
	g.mu.Lock()
	if len(g.writes) != 0 {
		t.Error("deadline retry bypassed delivery authorization")
	}
	g.mu.Unlock()
	// A later metadata/authorization failure must also revoke retry eligibility,
	// even when the binding-level gate becomes available again.
	reads := n.reads.Load()
	n.denyMetadata.Store(true)
	a.wake <- struct{}{}
	runtimeWait(t, func() bool { return n.reads.Load() > reads })
	allowed.Store(true)
	time.Sleep(3 * r.PollInterval)
	g.mu.Lock()
	if len(g.writes) != 0 {
		t.Error("deadline retry reused authorization after a native failure")
	}
	g.mu.Unlock()
	n.denyMetadata.Store(false)
	a.wake <- struct{}{}
	runtimeWait(t, func() bool { g.mu.Lock(); defer g.mu.Unlock(); return len(g.writes) == 2 })
}
