package bindings

import (
	"context"
	"errors"
	"testing"

	"codex-feishu/internal/latency"
)

func TestGroupLatencyDistinguishesUnknownFromBound(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		m, gateway, _ := groupManager(t)
		var events []latency.Event
		m.Latency = func(e latency.Event) { events = append(events, e) }
		if unknown {
			gateway.create = func(string, string, []string, string) (string, error) {
				return "", errors.New("synthetic private response")
			}
		}
		_, err := m.Ensure(context.Background(), "thread", "synthetic title")
		if unknown != errors.Is(err, ErrGroupUnknown) {
			t.Fatal("group result changed")
		}
		foundRPC, foundBound := false, false
		for _, event := range events {
			if event.ThreadRef == "" || event.ThreadRef != events[0].ThreadRef {
				t.Fatal("group phases lost their route correlation")
			}
			if event.Stage == "group.create" {
				foundRPC = true
				if unknown && event.Outcome != "unknown" || !unknown && event.Outcome != "confirmed" {
					t.Fatal("RPC outcome misclassified")
				}
			}
			foundBound = foundBound || event.Stage == "group.bound" && event.Outcome == "ok"
		}
		if !foundRPC || foundBound == unknown || events[len(events)-1].Stage != "group.ensure" {
			t.Fatal("group creation checkpoints missing or falsely confirmed")
		}
		if unknown {
			if _, err := m.Ensure(context.Background(), "thread", "synthetic title"); !errors.Is(err, ErrGroupUnknown) || gateway.calls.Load() != 1 {
				t.Fatal("instrumentation retried an unknown creation")
			}
		}
	}
}
