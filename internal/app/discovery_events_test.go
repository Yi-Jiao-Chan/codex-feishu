package app

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/discovery"
)

type eventRuntimeNative struct {
	*runtimeNative
	recentCalls   atomic.Int32
	turnReads     atomic.Int32
	blockActive   atomic.Bool
	activeStarted chan struct{}
	releaseActive chan struct{}
	callMu        sync.Mutex
	activeAt      []time.Time
}

func (n *eventRuntimeNative) ListArchiveSince(ctx context.Context, archived bool, _ int64) ([]codexclient.Thread, error) {
	n.recentCalls.Add(1)
	if !archived {
		n.callMu.Lock()
		n.activeAt = append(n.activeAt, time.Now())
		n.callMu.Unlock()
		if n.blockActive.Load() {
			select {
			case n.activeStarted <- struct{}{}:
			default:
			}
			select {
			case <-n.releaseActive:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return n.runtimeNative.ListArchive(ctx, archived)
}

func (n *eventRuntimeNative) Turns(ctx context.Context, id string) ([]codexclient.Turn, error) {
	turns, err := n.runtimeNative.Turns(ctx, id)
	n.turnReads.Add(1)
	return turns, err
}

func TestRuntimeBirthAndFirstInputBypassPeriodicDiscovery(t *testing.T) {
	for _, draft := range []bool{false, true} {
		name := "birth with input"
		if draft {
			name = "first input after draft birth"
		}
		t.Run(name, func(t *testing.T) {
			db, base, gateway, r := runtimeFixture(t)
			n := &eventRuntimeNative{runtimeNative: base}
			r.Native, r.ScanInterval = n, time.Hour
			runAdaptiveRuntime(t, r, base)
			ctx := context.Background()
			runtimeWait(t, func() bool {
				_, err := db.DiscoveryCheckpoint(ctx, r.HostID, r.AppID)
				return err == nil
			})
			before, err := db.DiscoveryCheckpoint(ctx, r.HostID, r.AppID)
			if err != nil {
				t.Fatal(err)
			}
			base.set("new", r.AllowedRoots[0], "completed")
			base.mu.Lock()
			meta := base.threads["new"]
			turns := base.turns["new"]
			if draft {
				base.turns["new"] = nil
			}
			base.mu.Unlock()
			birth, _ := json.Marshal(map[string]any{"thread": meta})
			base.events <- codexclient.Event{Method: "thread/started", Params: birth}
			if draft {
				runtimeWait(t, func() bool { return n.turnReads.Load() > 0 })
				gateway.mu.Lock()
				if gateway.groups != 0 {
					t.Error("empty draft created a group")
				}
				gateway.mu.Unlock()
				base.mu.Lock()
				base.turns["new"] = turns
				base.mu.Unlock()
				input, _ := json.Marshal(map[string]any{"threadId": "new", "item": map[string]string{"type": "userMessage", "id": "user-new"}})
				base.events <- codexclient.Event{Method: "item/started", Params: input}
			}
			runtimeWait(t, func() bool {
				_, err := db.Binding(ctx, r.HostID, "new")
				return err == nil
			})
			after, err := db.DiscoveryCheckpoint(ctx, r.HostID, r.AppID)
			if err != nil || after != before {
				t.Fatal("event discovery changed periodic scan coverage", err)
			}
			gateway.mu.Lock()
			defer gateway.mu.Unlock()
			if gateway.groups != 1 || n.recentCalls.Load() == 0 {
				t.Fatal("event path did not create exactly one group before the hourly scan")
			}
		})
	}
}

func TestDiscoveryEventBatchesKeepInflightEventsAndDoNotStarve(t *testing.T) {
	db, base, _, r := runtimeFixture(t)
	n := &eventRuntimeNative{runtimeNative: base, activeStarted: make(chan struct{}, 1), releaseActive: make(chan struct{})}
	r.Native, r.ScanInterval = n, time.Hour
	r.checks = make(chan string, 256)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := &discovery.Engine{Store: db, Native: n, HostID: r.HostID, AppID: r.AppID, AllowedRoots: r.AllowedRoots}
	if _, err := e.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteBaseline(ctx, r.HostID, nil); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); r.runDiscovery(ctx, e, make(chan struct{}, 1)) }()
	t.Cleanup(func() { cancel(); <-done })
	runtimeWait(t, func() bool {
		_, err := db.DiscoveryCheckpoint(ctx, r.HostID, r.AppID)
		return err == nil
	})
	base.set("first", r.AllowedRoots[0], "completed")
	meta, _ := base.Metadata(ctx, "first")
	if err := e.ObserveBirth(ctx, meta); err != nil {
		t.Fatal(err)
	}
	n.blockActive.Store(true)
	r.checks <- "first"
	select {
	case <-n.activeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("event lookup did not start")
	}
	// A new candidate and a burst arrive after the first batch has been taken.
	base.set("second", r.AllowedRoots[0], "completed")
	meta, _ = base.Metadata(ctx, "second")
	if err := e.ObserveBirth(ctx, meta); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		r.checks <- []string{"first", "second"}[i%2]
	}
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				select {
				case r.checks <- "second":
				default:
				}
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-producerDone })
	n.blockActive.Store(false)
	close(n.releaseActive)
	runtimeWait(t, func() bool {
		for _, id := range []string{"first", "second"} {
			o, err := db.Observation(ctx, r.HostID, id)
			if err != nil || o.State != "eligible" {
				return false
			}
		}
		return true
	})
	select {
	case <-producerDone:
		t.Fatal("event checks waited for continuous events to stop")
	default:
	}
	cancel()
	<-done
	<-producerDone
	n.callMu.Lock()
	defer n.callMu.Unlock()
	if len(n.activeAt) < 2 || n.recentCalls.Load() != int32(2*len(n.activeAt)) {
		t.Fatal("event batches lost an in-flight candidate or repeated per-ID listings")
	}
	for i := 1; i < len(n.activeAt); i++ {
		if n.activeAt[i].Sub(n.activeAt[i-1]) < discoveryEventCheckInterval {
			t.Fatal("continuous events bypassed list query rate limiting")
		}
	}
}
