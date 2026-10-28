package sync

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/feishu"
	"codex-feishu/internal/latency"
	"codex-feishu/internal/store"
)

func checkDeliveryLatency(t *testing.T, events []latency.Event, stage, turn, work, rpcOutcome, finishOutcome string) {
	t.Helper()
	wantStages := []string{stage + ".claimed", stage + ".rpc", stage + ".finished"}
	if len(events) != len(wantStages) {
		t.Fatalf("delivery latency event count: got %d, want %d", len(events), len(wantStages))
	}
	var expected latency.Event
	latency.Observer(func(event latency.Event) { expected = event }).Record("expected", "host", "thread", turn, work, 0, "ok")
	for i, event := range events {
		if event.Stage != wantStages[i] || event.AtMS <= 0 || event.DurationMS < 0 ||
			event.ThreadRef == "" || event.ThreadRef != expected.ThreadRef || event.TurnRef != expected.TurnRef || event.WorkRef != expected.WorkRef {
			t.Fatalf("unexpected checkpoint %d: %+v", i, event)
		}
	}
	if events[0].Outcome != "ok" || events[1].Outcome != rpcOutcome || events[2].Outcome != finishOutcome {
		t.Fatalf("incorrect checkpoint outcomes: %+v", events)
	}
	wire, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{`"host"`, `"thread"`, `"turn"`, `"document"`, "original-input", "public text", "remote-message", "/project", "PRIVATE_TRANSPORT_ERROR"} {
		if strings.Contains(string(wire), private) {
			t.Fatalf("checkpoint exposed private input %q: %s", private, wire)
		}
	}
}

func TestOutboundLatencyFinalAndNoReadyWork(t *testing.T) {
	ctx := context.Background()
	s, _, m := outboundFixture(t, "final")
	g := &outboundGateway{s: s, t: t}
	var events []latency.Event
	o := Outbound{Store: s, Gateway: g, Latency: func(event latency.Event) { events = append(events, event) }}

	result, err := o.DispatchOne(ctx, "host", "thread", 1000)
	if err != nil || result.State != "confirmed" {
		t.Fatalf("final delivery: %+v %v", result, err)
	}
	checkDeliveryLatency(t, events, "delivery.final.reply", m.TurnID, m.Key, "ok", "confirmed")

	events = nil
	result, err = o.DispatchOne(ctx, "host", "thread", 1001)
	if err != nil || result.AttemptID != "" || len(events) != 0 {
		t.Fatalf("no work emitted delivery checkpoints: %+v %v events=%+v", result, err, events)
	}

	m.Card = `{"body":"updated public text"}`
	if err := s.StageMirrors(ctx, "host", "thread", []store.Mirror{m}, store.Watermark{ObservedAtMS: 103}); err != nil {
		t.Fatal(err)
	}
	result, err = o.DispatchOne(ctx, "host", "thread", 3000)
	if err != nil || result.State != "confirmed" {
		t.Fatalf("updated final delivery: %+v %v", result, err)
	}
	checkDeliveryLatency(t, events, "delivery.final.update", m.TurnID, m.Key, "ok", "confirmed")
}

func TestOutboundLatencyDeliveryOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, state, rpcOutcome string
		gatewayErr              error
		missingID, closeDB      bool
	}{
		{name: "unknown transport", state: "unknown", rpcOutcome: "error", gatewayErr: errors.New("PRIVATE_TRANSPORT_ERROR")},
		{name: "unknown identity", state: "unknown", rpcOutcome: "ok", missingID: true},
		{name: "deferred", state: "deferred", rpcOutcome: "error", gatewayErr: &feishu.RateLimited{Rejection: &feishu.Rejected{Code: 99991400, HTTPStatus: 429}, RetryAfter: time.Second}},
		{name: "confirmation failure", state: "unknown", rpcOutcome: "ok", closeDB: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, m := outboundFixture(t, "human")
			g := &outboundGateway{s: s, t: t, err: tc.gatewayErr, missingID: tc.missingID, closeDB: tc.closeDB}
			var events []latency.Event
			o := Outbound{Store: s, Gateway: g, Latency: func(event latency.Event) { events = append(events, event) }}
			result, err := o.DispatchOne(context.Background(), "host", "thread", 1000)
			if result.State != tc.state || (err != nil) != tc.closeDB {
				t.Fatalf("delivery outcome: %+v %v", result, err)
			}
			checkDeliveryLatency(t, events, "delivery.human.send", m.TurnID, m.Key, tc.rpcOutcome, tc.state)
		})
	}
}

func TestOutboundLatencyTopicPersistenceFailureStillRecordsRPC(t *testing.T) {
	s, _, m := outboundFixture(t, "human")
	makeTopic(t, s)
	g := &topicResultGateway{topicGateway: &topicGateway{outboundGateway: &outboundGateway{s: s, t: t}}, threadID: "other-platform"}
	var events []latency.Event
	o := Outbound{Store: s, Gateway: g, Latency: func(event latency.Event) { events = append(events, event) }}
	result, err := o.DispatchOne(context.Background(), "host", "thread", 1000)
	if !errors.Is(err, store.ErrConflict) || result.State != "unknown" {
		t.Fatalf("topic persistence failure: %+v %v", result, err)
	}
	checkDeliveryLatency(t, events, "delivery.human.reply", m.TurnID, m.Key, "ok", "unknown")
}

func TestOutboundLatencyMembershipRead(t *testing.T) {
	_, _, _, membership, o, event := mentionOutboundFixture(t)
	membership.err = errors.New("PRIVATE_TRANSPORT_ERROR")
	var events []latency.Event
	o.Latency = func(event latency.Event) { events = append(events, event) }
	result, err := o.DispatchOne(context.Background(), "host", "thread", event.Add(2*time.Minute).UnixMilli())
	if err != nil || result.State != "confirmed" {
		t.Fatalf("delivery after membership error: %+v %v", result, err)
	}
	if len(events) != 4 || events[0].Stage != "delivery.membership" || events[0].Outcome != "error" {
		t.Fatalf("membership checkpoint missing or mislabeled: %+v", events)
	}
	checkDeliveryLatency(t, events[1:], "delivery.final.reply", result.TurnID, result.Key, "ok", "confirmed")
	if events[0].ThreadRef != events[1].ThreadRef || events[0].TurnRef != events[1].TurnRef || events[0].WorkRef != events[1].WorkRef {
		t.Fatalf("membership refs do not match delivery: %+v", events)
	}
}

func TestOutboundLatencyCOTOperations(t *testing.T) {
	for _, step := range []string{"create", "append", "complete"} {
		t.Run(step, func(t *testing.T) {
			f := prepareCOTOutboundStep(t, step, false)
			var events []latency.Event
			f.o.Latency = func(event latency.Event) { events = append(events, event) }
			result := f.dispatch(t, step)
			checkDeliveryLatency(t, events, "delivery.progress.cot_"+step, result.TurnID, result.Key, "ok", "confirmed")
		})
	}
}

func TestDeliveryLatencyLabelsDoNotExposeUnexpectedValues(t *testing.T) {
	got := []string{
		deliveryLatencyStage("PRIVATE_KIND", "PRIVATE_OPERATION", ""),
		deliveryLatencyStage("progress", "reply", "PRIVATE_STEP"),
	}
	want := []string{"delivery.unknown.unknown", "delivery.progress.unknown"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unsafe labels: %v", got)
	}
}
