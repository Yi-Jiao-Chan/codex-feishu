package latency

import (
	"fmt"
	"testing"
	"time"
)

func TestSamplerCoalescesRepeatedSetupButKeepsMessagesAndRecovery(t *testing.T) {
	var sampler Sampler
	now := time.Unix(100, 0)
	event := Event{Stage: "actor.attach", ThreadRef: "route", Outcome: "error"}
	if allow, omitted := sampler.Allow(event, now); !allow || omitted != 0 {
		t.Fatal("first setup checkpoint was lost")
	}
	if allow, _ := sampler.Allow(event, now.Add(time.Millisecond)); allow {
		t.Fatal("duplicate setup checkpoint was not coalesced")
	}
	event.Outcome = "ok"
	if allow, _ := sampler.Allow(event, now.Add(time.Millisecond)); !allow {
		t.Fatal("recovery checkpoint was lost")
	}
	for _, stage := range []string{"native.user.completed", "native.answer.completed", "mirror.final.staged", "delivery.final.reply.claimed", "delivery.final.reply.rpc", "delivery.final.reply.finished"} {
		event.Stage = stage
		for i := 0; i < 2; i++ {
			if allow, _ := sampler.Allow(event, now.Add(time.Second)); !allow {
				t.Fatalf("message/delivery checkpoint coalesced: %s", stage)
			}
		}
	}
	event.Stage, event.Outcome = "actor.attach", "error"
	if allow, omitted := sampler.Allow(event, now.Add(sampleWindow)); !allow || omitted != 1 {
		t.Fatal("new window did not retain the omitted count and next checkpoint")
	}
}

func TestSamplerBoundsSetupKeysAndReclaimsWindow(t *testing.T) {
	var sampler Sampler
	now := time.Unix(100, 0)
	for i := 0; i < maxSampleKeys+10; i++ {
		sampler.Allow(Event{Stage: "actor.started", ThreadRef: fmt.Sprint(i), Outcome: "ok"}, now)
	}
	if len(sampler.seen) != maxSampleKeys {
		t.Fatal("setup checkpoint keys were not bounded")
	}
	if allow, omitted := sampler.Allow(Event{Stage: "actor.started", ThreadRef: "next", Outcome: "ok"}, now.Add(sampleWindow)); !allow || omitted != 10 || len(sampler.seen) != 1 {
		t.Fatal("setup checkpoint window was not reclaimed")
	}
}
