package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
)

type handoffRuntimeNative struct {
	*runtimeNative
	reads atomic.Int32
}

func (n *handoffRuntimeNative) Turns(ctx context.Context, thread string) ([]codexclient.Turn, error) {
	n.reads.Add(1)
	return n.runtimeNative.Turns(ctx, thread)
}

func TestRuntimeUsesInputHistoryOnceAndFallsBackAfterInputError(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "handoff", true: "input-error"}[fail], func(t *testing.T) {
			db, native, _, r := runtimeFixture(t)
			n := &handoffRuntimeNative{runtimeNative: native}
			r.Native, r.DiscoveryEnabled = n, false
			r.Now = func() time.Time { return time.Unix(200, 0) }
			n.set("thread", r.AllowedRoots[0], "completed")
			runtimeBind(t, db, r, "thread", "chat")
			r.ActorTick = func(ctx context.Context, thread string) ([]codexclient.Turn, error) {
				if fail {
					return []codexclient.Turn{{ID: "stale-prewrite"}}, errors.New("input not reconciled")
				}
				return n.Turns(ctx, thread)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				r.runActor(ctx, &threadActor{thread: "thread", wake: make(chan struct{}, 1)})
			}()
			t.Cleanup(func() { cancel(); <-done })
			waitIdleMirrorsDelivered(t, ctx, db, "thread")
			if n.reads.Load() != 1 {
				t.Fatal("actor read full history more than once", n.reads.Load())
			}
			metrics := r.timings.snapshot()
			if !fail && metrics.HistoryReuses != 1 || fail && metrics.HistoryReuses != 0 {
				t.Fatal("failed input tick reused stale history or successful handoff was lost", metrics)
			}
		})
	}
}

func TestDiscoveryScansDoNotReactivateSettledBoundCandidates(t *testing.T) {
	db, base, _, r := runtimeFixture(t)
	n := &deliveryRetryNative{runtimeNative: base}
	r.Native = n
	r.ScanInterval = 15 * time.Millisecond
	r.ActorIdleTimeout = 30 * time.Millisecond
	r.IdlePollInterval = 10 * time.Millisecond
	r.IdleHistoryInterval = time.Hour
	settledRuntimeBinding(t, db, base, r, time.Now().Add(-48*time.Hour).Unix())
	runAdaptiveRuntime(t, r, base)
	time.Sleep(80 * time.Millisecond)
	if n.reads.Load() != 0 || r.ActorStarted("thread") {
		t.Fatal("discovery activated a settled bound candidate without new activity")
	}
	r.Wake("thread")
	runtimeWait(t, func() bool { return n.reads.Load() > 0 })
	runtimeWait(t, func() bool { return !r.ActorStarted("thread") })
	reads := n.reads.Load()
	time.Sleep(80 * time.Millisecond)
	if n.reads.Load() != reads || r.ActorStarted("thread") {
		t.Fatal("discovery repeatedly reactivated the dormant binding")
	}
}

func TestReasoningDeltasDoNotReloadHistoryAndPublicEventsStillWake(t *testing.T) {
	db, base, _, r := runtimeFixture(t)
	n := &handoffRuntimeNative{runtimeNative: base}
	r.Native, r.DiscoveryEnabled = n, false
	r.ScanInterval = time.Hour
	r.IdlePollInterval = 20 * time.Millisecond
	r.IdleHistoryInterval = time.Hour
	settledRuntimeBinding(t, db, base, r, time.Now().Add(-48*time.Hour).Unix())
	runAdaptiveRuntime(t, r, base)
	// Deltas alone must not activate a dormant route.
	params := json.RawMessage(`{"threadId":"thread","delta":"synthetic hidden fragment"}`)
	for _, method := range []string{"item/reasoning/summaryTextDelta", "item/reasoning/summaryPartAdded", "item/reasoning/textDelta"} {
		base.events <- codexclient.Event{Method: method, Params: params}
	}
	runtimeWait(t, func() bool { return r.timings.snapshot().IgnoredReasoningEvents == 3 })
	if n.reads.Load() != 0 || r.ActorStarted("thread") {
		t.Fatal("hidden token deltas activated a public history reader")
	}
	for i, method := range []string{"turn/started", "item/agentMessage/delta", "item/completed", "turn/completed", "item/tool/requestUserInput"} {
		reads := n.reads.Load()
		base.events <- codexclient.Event{Method: method, Params: params}
		runtimeWait(t, func() bool { return n.reads.Load() > reads })
		// Wait for the current projection before using the next read baseline.
		runtimeWait(t, func() bool { return r.timings.snapshot().Stages["projection"].Count >= uint64(i+1) })
	}
	reads := n.reads.Load()
	base.events <- codexclient.Event{Method: "item/reasoning/textDelta", Params: params}
	runtimeWait(t, func() bool { return r.timings.snapshot().IgnoredReasoningEvents == 4 })
	time.Sleep(2 * r.PollInterval)
	if n.reads.Load() != reads {
		t.Fatal("hidden delta reloaded an already active history reader")
	}
}
