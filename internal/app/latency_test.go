package app

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/latency"
)

func TestNativeLatencyOnlyRecordsPublicLifecycleMetadata(t *testing.T) {
	var events []latency.Event
	r := &Runtime{HostID: "private-host", Latency: func(e latency.Event) { events = append(events, e) }}
	for _, tc := range []struct{ method, params string }{
		{"thread/started", `{"thread":{"id":"private-thread","name":"private-title","cwd":"private-path"}}`},
		{"turn/started", `{"threadId":"private-thread","turn":{"id":"private-turn","items":[{"text":"private-content"}]}}`},
		{"item/completed", `{"threadId":"private-thread","turnId":"private-turn","item":{"id":"private-input","type":"userMessage","content":[{"text":"private-content"}]}}`},
		{"item/completed", `{"threadId":"private-thread","turnId":"private-turn","item":{"id":"private-answer","type":"agentMessage","phase":"final_answer","text":"private-content"}}`},
		{"turn/completed", `{"threadId":"private-thread","turn":{"id":"private-turn","error":{"message":"private-error"}}}`},
		{"item/completed", `{"threadId":"private-thread","item":{"id":"private-reasoning","type":"reasoning","text":"private-reasoning-content"}}`},
		{"item/reasoning/textDelta", `{"threadId":"private-thread","delta":"private-reasoning-content"}`},
		{"item/completed", `{"threadId":"private-thread","item":{"id":"private-tool","type":"commandExecution","command":"private-command"}}`},
		{"item/completed", `{"threadId":"private-thread","item":{"type":"private-type","phase":"private-phase"}}`},
		{"turn/completed", `invalid`},
	} {
		r.observeNativeLatency(codexclient.Event{Method: tc.method, Params: json.RawMessage(tc.params)})
	}
	var stages []string
	for _, e := range events {
		stages = append(stages, e.Stage)
		if e.ThreadRef != events[0].ThreadRef || e.Outcome != "observed" || e.DurationMS != 0 {
			t.Fatal("native checkpoints lost correlation or became duration estimates")
		}
	}
	if !reflect.DeepEqual(stages, []string{"native.thread.started", "native.turn.started", "native.user.completed", "native.answer.completed", "native.turn.completed"}) {
		t.Fatalf("unexpected public lifecycle stages: %v", stages)
	}
	data, _ := json.Marshal(events)
	if strings.Contains(string(data), "private-") || events[1].TurnRef != events[2].TurnRef || events[2].TurnRef != events[4].TurnRef {
		t.Fatal("trace exposed private data or lost turn correlation")
	}
}

func TestRuntimeLatencyCorrelatesDiscoveryGroupAndFinalDelivery(t *testing.T) {
	db, native, _, r := runtimeFixture(t)
	var mu sync.Mutex
	var events []latency.Event
	r.Latency = func(event latency.Event) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}
	runAdaptiveRuntime(t, r, native)
	runtimeWait(t, func() bool {
		_, err := db.DiscoveryCheckpoint(context.Background(), r.HostID, r.AppID)
		return err == nil
	})
	native.set("new", r.AllowedRoots[0], "completed")
	meta, _ := native.Metadata(context.Background(), "new")
	params, _ := json.Marshal(map[string]any{"thread": meta})
	native.events <- codexclient.Event{Method: "thread/started", Params: params}
	runtimeWait(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, event := range events {
			if event.Stage == "delivery.final.reply.finished" && event.Outcome == "confirmed" {
				return true
			}
		}
		return false
	})
	mu.Lock()
	defer mu.Unlock()
	want := map[string]bool{
		"native.thread.started": false, "discovery.eligible": false, "actor.attach": false,
		"group.create": false, "group.bound": false, "mirror.final.staged": false,
		"delivery.final.reply.claimed": false, "delivery.final.reply.rpc": false, "delivery.final.reply.finished": false,
	}
	threadRef := ""
	for _, event := range events {
		if _, needed := want[event.Stage]; !needed {
			continue
		}
		want[event.Stage] = true
		if threadRef == "" {
			threadRef = event.ThreadRef
		}
		if event.ThreadRef == "" || event.ThreadRef != threadRef {
			t.Fatal("trace lost correlation across components")
		}
	}
	for stage, found := range want {
		if !found {
			t.Errorf("runtime trace missing %s", stage)
		}
	}
}
