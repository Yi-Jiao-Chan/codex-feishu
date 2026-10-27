package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

func settledRuntimeBinding(t *testing.T, db *store.Store, native *runtimeNative, runtime *Runtime, updatedAt int64) {
	t.Helper()
	native.set("thread", runtime.AllowedRoots[0], "completed")
	native.mu.Lock()
	meta := native.threads["thread"]
	meta.UpdatedAt = updatedAt
	meta.Status.Type = "idle"
	native.threads["thread"] = meta
	native.mu.Unlock()
	runtimeBind(t, db, runtime, "thread", "chat")
	if err := db.StageMirrors(context.Background(), runtime.HostID, "thread", nil, store.Watermark{ObservedAtMS: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
}

func runAdaptiveRuntime(t *testing.T, runtime *Runtime, native *runtimeNative) (context.CancelFunc, <-chan error) {
	t.Helper()
	ready := make(chan struct{}, 1)
	runtime.EventObserved = func(event codexclient.Event) {
		if event.Method == "fixture/adaptive-ready" {
			ready <- struct{}{}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	native.events <- codexclient.Event{Method: "fixture/adaptive-ready"}
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("runtime did not reach its event loop")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("runtime stopped unexpectedly: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("runtime did not join")
		}
	})
	return cancel, done
}

func TestRuntimeDormantBindingStartsOnDemandAndEvicts(t *testing.T) {
	db, base, _, runtime := runtimeFixture(t)
	native := &deliveryRetryNative{runtimeNative: base}
	runtime.Native = native
	runtime.DiscoveryEnabled = false
	runtime.ActorIdleTimeout = 30 * time.Millisecond
	runtime.IdlePollInterval = 10 * time.Millisecond
	runtime.IdleHistoryInterval = time.Hour
	var released atomic.Int32
	runtime.ActorReleased = func(string) { released.Add(1) }
	settledRuntimeBinding(t, db, base, runtime, time.Now().Add(-48*time.Hour).Unix())
	runAdaptiveRuntime(t, runtime, base)

	time.Sleep(50 * time.Millisecond)
	if runtime.ActorStarted("thread") || native.reads.Load() != 0 {
		t.Fatal("settled historical binding retained a startup actor")
	}
	runtime.Wake("thread")
	runtimeWait(t, func() bool { return native.reads.Load() > 0 })
	runtimeWait(t, func() bool { return !runtime.ActorStarted("thread") })
	if released.Load() != 1 {
		t.Fatalf("dormant actor retained generation-local state: releases=%d", released.Load())
	}
	projectionCache := runtime.cotProjectionCache
	if projectionCache == nil {
		t.Fatal("runtime did not retain its bounded projection cache")
	}
	reads := native.reads.Load()
	runtime.Wake("thread")
	runtimeWait(t, func() bool { return native.reads.Load() > reads })
	if runtime.cotProjectionCache != projectionCache {
		t.Fatal("dormant actor activation replaced the generation-wide projection cache")
	}
}

func TestRuntimeBoundScanActivatesNativeChangeAfterRestart(t *testing.T) {
	db, base, _, runtime := runtimeFixture(t)
	native := &deliveryRetryNative{runtimeNative: base}
	runtime.Native = native
	runtime.DiscoveryEnabled = false
	runtime.ActorIdleTimeout = time.Hour
	settledRuntimeBinding(t, db, base, runtime, time.Now().Unix())
	mark, err := db.Watermark(context.Background(), runtime.HostID, "thread")
	if err != nil {
		t.Fatal(err)
	}
	// The native update is newer than the persisted complete-history read.
	base.mu.Lock()
	meta := base.threads["thread"]
	meta.UpdatedAt = mark.ObservedAtMS/1000 + 2
	base.threads["thread"] = meta
	base.mu.Unlock()
	runAdaptiveRuntime(t, runtime, base)
	runtimeWait(t, func() bool { return native.reads.Load() > 0 && runtime.ActorStarted("thread") })
}

type activationFailureNative struct {
	*runtimeNative
	fail  atomic.Bool
	calls atomic.Int32
}

func (n *activationFailureNative) Metadata(ctx context.Context, id string) (codexclient.Thread, error) {
	n.calls.Add(1)
	if n.fail.Load() {
		return codexclient.Thread{}, errors.New("metadata temporarily unavailable")
	}
	return n.runtimeNative.Metadata(ctx, id)
}

func TestRuntimeActivationFailureUsesCentralBackoffAndExplicitWakeBypassesIt(t *testing.T) {
	db, base, _, runtime := runtimeFixture(t)
	native := &activationFailureNative{runtimeNative: base}
	native.fail.Store(true)
	runtime.Native = native
	runtime.DiscoveryEnabled = false
	runtime.ActivationRetryInitial = 20 * time.Millisecond
	runtime.ActivationRetryMax = 40 * time.Millisecond
	var retryNotices atomic.Int32
	runtime.ActorRetrying = func(string) { retryNotices.Add(1) }
	settledRuntimeBinding(t, db, base, runtime, time.Now().Add(-48*time.Hour).Unix())
	runAdaptiveRuntime(t, runtime, base)

	runtime.Wake("thread")
	runtimeWait(t, func() bool { return native.calls.Load() >= 2 })
	active, retrying, managed := runtime.bindingLifecycle()
	if !managed || active["thread"] || !retrying["thread"] {
		t.Fatal("failed activation retained an actor or lost its central retry")
	}
	if retryNotices.Load() == 0 {
		t.Fatal("failed activation did not invalidate current control readiness")
	}
	before := native.calls.Load()
	native.fail.Store(false)
	runtime.Wake("thread")
	runtimeWait(t, func() bool { return runtime.ActorStarted("thread") && native.calls.Load() > before })
}

func TestRuntimeCandidateRetryRetainsGroupCreation(t *testing.T) {
	db, native, _, runtime := runtimeFixture(t)
	runtime.ScanInterval = time.Hour
	runtime.ActivationRetryInitial = 20 * time.Millisecond
	runtime.ActivationRetryMax = 40 * time.Millisecond
	var preparations atomic.Int32
	var retryNotices atomic.Int32
	runtime.ActorRetrying = func(string) { retryNotices.Add(1) }
	runtime.HistoryPreparing = func(context.Context, string, []codexclient.Turn) error {
		if preparations.Add(1) == 1 {
			return errors.New("temporary history preparation failure")
		}
		return nil
	}
	now := time.Now()
	if _, err := db.BeginBaseline(context.Background(), runtime.HostID, runtime.AppID, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	native.set("candidate", runtime.AllowedRoots[0], "completed")
	if err := db.ObserveNew(context.Background(), runtime.HostID, "candidate", runtime.AllowedRoots[0], now.Unix()); err != nil {
		t.Fatal(err)
	}
	runAdaptiveRuntime(t, runtime, native)
	runtimeWait(t, func() bool {
		_, err := db.Binding(context.Background(), runtime.HostID, "candidate")
		return err == nil
	})
	if preparations.Load() < 2 {
		t.Fatal("candidate did not retry history preparation")
	}
	if retryNotices.Load() == 0 {
		t.Fatal("actor retry did not invalidate current control readiness")
	}
}

func TestRuntimeCarryoverKeepsOnlyDurableRoutes(t *testing.T) {
	runtime := &Runtime{
		running:    true,
		actors:     map[string]*threadActor{"bound-active": {}, "candidate": {}},
		retries:    map[string]actorRetry{"bound-retry": {}, "candidate-retry": {}},
		pending:    map[string]bool{"bound-pending": false, "candidate-pending": false},
		activating: map[string]bool{"bound-activating": true, "candidate-activating": true},
		bindings: map[string]bool{
			"bound-active": true, "bound-retry": true, "bound-pending": true, "bound-activating": true,
		},
	}
	runtime.captureCarryover()
	got := runtime.carryoverThreads()
	want := []string{"bound-activating", "bound-active", "bound-pending", "bound-retry"}
	if len(got) != len(want) {
		t.Fatalf("carryover=%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("carryover=%v", got)
		}
	}
}
