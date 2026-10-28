package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/bindings"
	"codex-feishu/internal/store"
)

func stageRuntimeGroupName(t *testing.T, r *Runtime, thread, title string) store.GroupNameRequest {
	t.Helper()
	b, err := r.Store.Binding(context.Background(), r.HostID, thread)
	if err != nil {
		t.Fatal(err)
	}
	request, err := r.Store.StageGroupNameRequest(context.Background(), store.GroupNameScope{AppID: r.AppID, OwnerID: r.OwnerID, Binding: b}, title, r.now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func runGroupNameWorkers(t *testing.T, q *groupNameSync) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < groupNameConcurrency; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); q.run(ctx) }()
	}
	t.Cleanup(func() { cancel(); wg.Wait() })
	return ctx
}

func TestRuntimeSlowGroupNameDoesNotBlockFinalDeliveryOrDormancy(t *testing.T) {
	db, native, _, r := runtimeFixture(t)
	r.DiscoveryEnabled = false
	r.ActorIdleTimeout = 25 * time.Millisecond
	r.IdlePollInterval = 10 * time.Millisecond
	native.set("thread", r.AllowedRoots[0], "inProgress")
	runtimeBind(t, db, r, "thread", "chat")
	started := make(chan struct{}, 1)
	var finished atomic.Bool
	r.SyncGroupName = func(ctx context.Context, _ store.Binding, _ string) error {
		started <- struct{}{}
		<-ctx.Done()
		finished.Store(true)
		return ctx.Err()
	}
	runAdaptiveRuntime(t, r, native)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("group name worker did not start")
	}
	native.set("thread", r.AllowedRoots[0], "completed")
	native.mu.Lock()
	meta := native.threads["thread"]
	meta.Status.Type, meta.UpdatedAt = "idle", time.Now().Add(-time.Hour).Unix()
	native.threads["thread"] = meta
	native.mu.Unlock()
	runtimeNameEvent(native, "thread")
	runtimeWait(t, func() bool {
		mirrors, err := db.Mirrors(context.Background(), r.HostID, "thread")
		for _, m := range mirrors {
			if err == nil && m.Kind == "final" && m.Revision == m.DeliveredRevision && m.RemoteID != "" {
				return true
			}
		}
		return false
	})
	runtimeWait(t, func() bool { return !r.ActorStarted("thread") })
	if finished.Load() {
		t.Fatal("final delivery waited for the slow name request")
	}
	requests, err := db.PendingGroupNameRequests(context.Background(), r.HostID, r.AppID, r.OwnerID)
	if err != nil || len(requests) != 1 {
		t.Fatal("dormant actor lost its durable name task", err)
	}
}

func TestGroupNameWorkersBoundConcurrencySerializeChatsAndCoalesceTitles(t *testing.T) {
	db, _, _, r := runtimeFixture(t)
	for _, id := range []string{"one", "two", "three"} {
		runtimeBind(t, db, r, id, "chat-"+id)
	}
	type call struct{ thread, title string }
	started := make(chan call, 8)
	release := make(chan struct{})
	var mu sync.Mutex
	active := map[string]bool{}
	var calls []call
	var overlap, exceeded bool
	r.SyncGroupName = func(ctx context.Context, b store.Binding, title string) error {
		mu.Lock()
		if active[b.ChatID] {
			overlap = true
		}
		active[b.ChatID] = true
		if len(active) > groupNameConcurrency {
			exceeded = true
		}
		c := call{b.ThreadID, title}
		calls = append(calls, c)
		mu.Unlock()
		started <- c
		select {
		case <-release:
		case <-ctx.Done():
		}
		mu.Lock()
		delete(active, b.ChatID)
		mu.Unlock()
		return ctx.Err()
	}
	q := newGroupNameSync(r)
	for _, id := range []string{"one", "two", "three"} {
		q.enqueue(stageRuntimeGroupName(t, r, id, "A"))
	}
	runGroupNameWorkers(t, q)
	var first call
	for i := 0; i < groupNameConcurrency; i++ {
		select {
		case c := <-started:
			if i == 0 {
				first = c
			}
		case <-time.After(5 * time.Second):
			t.Fatal("name workers did not use their available concurrency")
		}
	}
	q.enqueue(stageRuntimeGroupName(t, r, first.thread, "B"))
	q.enqueue(stageRuntimeGroupName(t, r, first.thread, "C"))
	close(release)
	runtimeWait(t, func() bool {
		requests, err := db.PendingGroupNameRequests(context.Background(), r.HostID, r.AppID, r.OwnerID)
		return err == nil && len(requests) == 0
	})
	mu.Lock()
	defer mu.Unlock()
	if overlap || exceeded || len(calls) != 4 {
		t.Fatalf("name work overlapped or escaped its bound: overlap=%v exceeded=%v calls=%v", overlap, exceeded, calls)
	}
	for _, c := range calls {
		if c.title == "B" {
			t.Fatal("superseded pending title was dispatched")
		}
	}
}

func TestGroupNameWorkerRetriesWithoutActorAndHonorsRateLimitDeadline(t *testing.T) {
	db, _, _, r := runtimeFixture(t)
	runtimeBind(t, db, r, "thread", "chat")
	var clock atomic.Int64
	clock.Store(time.Now().UnixMilli())
	r.Now = func() time.Time { return time.UnixMilli(clock.Load()) }
	deadline := clock.Load() + 60_000
	var calls atomic.Int32
	scheduled := make(chan struct{}, 1)
	r.Notice = func(code string) {
		if code == "group.name_sync_pending" {
			scheduled <- struct{}{}
		}
	}
	r.SyncGroupName = func(context.Context, store.Binding, string) error {
		if calls.Add(1) == 1 {
			return &bindings.GroupNameDeferredError{RetryAtMS: deadline}
		}
		return nil
	}
	q := newGroupNameSync(r)
	q.enqueue(stageRuntimeGroupName(t, r, "thread", "Desired"))
	runGroupNameWorkers(t, q)
	select {
	case <-scheduled:
	case <-time.After(5 * time.Second):
		t.Fatal("name failure was not scheduled independently")
	}
	clock.Store(deadline - 1)
	q.signal()
	time.Sleep(30 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatal("name update bypassed the persisted rate-limit deadline")
	}
	clock.Store(deadline)
	q.signal()
	runtimeWait(t, func() bool {
		requests, err := db.PendingGroupNameRequests(context.Background(), r.HostID, r.AppID, r.OwnerID)
		return err == nil && len(requests) == 0 && calls.Load() == 2
	})
}

func TestRuntimeRecoversGroupNameWorkWithoutActivatingNativeActor(t *testing.T) {
	db, base, _, r := runtimeFixture(t)
	native := &deliveryRetryNative{runtimeNative: base}
	r.Native, r.DiscoveryEnabled = native, false
	settledRuntimeBinding(t, db, base, r, time.Now().Add(-48*time.Hour).Unix())
	stageRuntimeGroupName(t, r, "thread", "Recovered title")
	var calls atomic.Int32
	r.SyncGroupName = func(_ context.Context, b store.Binding, desired string) error {
		if b.ThreadID != "thread" || desired != "Recovered title" {
			return errors.New("unexpected recovered name task")
		}
		calls.Add(1)
		return nil
	}
	runAdaptiveRuntime(t, r, base)
	runtimeWait(t, func() bool { return calls.Load() == 1 })
	if r.ActorStarted("thread") || native.reads.Load() != 0 {
		t.Fatal("name recovery attached or read native thread history")
	}
}

func TestGroupNameLegacyCompletionKeepsNewSameTitleRequest(t *testing.T) {
	db, _, _, r := runtimeFixture(t)
	runtimeBind(t, db, r, "thread", "chat")
	b, _ := db.Binding(context.Background(), r.HostID, "thread")
	scope := store.GroupNameScope{AppID: r.AppID, OwnerID: r.OwnerID, Binding: b}
	attempt, claimed, err := db.ClaimGroupName(context.Background(), store.GroupNamePlan{Scope: scope, Desired: "A", Previous: "Old"}, "", r.now().UnixMilli())
	if err != nil || !claimed {
		t.Fatal("legacy name request fixture failed", err)
	}
	if err := db.FinishGroupName(context.Background(), attempt, "unknown"); err != nil {
		t.Fatal(err)
	}
	requests, err := db.PendingGroupNameRequests(context.Background(), r.HostID, r.AppID, r.OwnerID)
	if err != nil || len(requests) != 1 {
		t.Fatal("legacy task was not recoverable", err)
	}
	q := newGroupNameSync(r)
	q.enqueue(requests[0])
	job, old := q.take()
	latest := stageRuntimeGroupName(t, r, "thread", "A")
	q.enqueue(latest)
	if err := db.CompleteGroupNameRequest(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	q.finish(job, old, nil)
	_, next := q.take()
	if next != latest {
		t.Fatal("completion of a legacy task discarded the new durable request")
	}
}
