package sync

import (
	"context"
	"testing"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/latency"
)

func TestMirrorLatencyMarksCommittedChangesWithoutReplayDuplicates(t *testing.T) {
	e, _ := engineFixture(t)
	var events []latency.Event
	e.Latency = func(event latency.Event) { events = append(events, event) }
	turns := []codexclient.Turn{{ID: "turn", Status: "inProgress", Items: []codexclient.Item{input("user", "synthetic input"), agent("answer", "synthetic answer", "final_answer")}}}
	apply := func() {
		t.Helper()
		if err := e.Apply(context.Background(), "thread", turns, false, 5000); err != nil {
			t.Fatal(err)
		}
	}
	apply()
	for _, event := range events {
		if event.Stage == "mirror.final.staged" {
			t.Fatal("an in-progress turn was reported as a staged final reply")
		}
	}
	turns[0].Status = "completed"
	events = nil
	apply()
	var final latency.Event
	for _, event := range events {
		if event.Stage == "mirror.final.staged" {
			final = event
		}
	}
	if final.WorkRef == "" || final.TurnRef == "" || final.Outcome != "ok" {
		t.Fatal("committed final reply has no correlation checkpoint")
	}
	events = nil
	apply()
	for _, event := range events {
		if event.Stage != "mirror.commit" {
			t.Fatal("unchanged snapshot repeated its staged checkpoint")
		}
	}
}
