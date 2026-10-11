package app

import (
	"encoding/json"
	"testing"

	"codex-feishu/internal/codexclient"
)

func TestRequestTrackerPreservesAppOwnershipAndTypedRequestIdentity(t *testing.T) {
	tracker := &RequestTracker{}
	event := func(method, thread, turn, id string) codexclient.Event {
		b, _ := json.Marshal(map[string]any{"threadId": thread, "turnId": turn, "requestId": json.RawMessage(id)})
		return codexclient.Event{Method: method, Params: b, Generation: 3, RequestID: json.RawMessage(id)}
	}
	numeric := event("item/tool/requestUserInput", "thread", "turn", "19")
	text := event("item/commandExecution/requestApproval", "thread", "turn", "\"19\"")
	tracker.Observe(numeric)
	tracker.Observe(numeric)
	tracker.Observe(text)
	if !tracker.Waiting("thread", "turn") || len(tracker.requests) != 2 {
		t.Fatal("request replay or typed ID lost")
	}
	tracker.Observe(event("item/agentMessage/delta", "thread", "turn", "19"))
	tracker.Observe(event("serverRequest/resolved", "other-thread", "", "19"))
	if !tracker.Waiting("thread", "turn") {
		t.Fatal("ordinary output/foreign resolution answered request")
	}
	tracker.Observe(event("serverRequest/resolved", "thread", "", "19"))
	if !tracker.Waiting("thread", "turn") || len(tracker.requests) != 1 {
		t.Fatal("numeric resolution consumed string request")
	}
	tracker.Reconcile("thread", []codexclient.Turn{{ID: "turn", Status: "inProgress"}})
	if !tracker.Waiting("thread", "turn") {
		t.Fatal("nonterminal snapshot cleared pending")
	}
	tracker.Reconcile("thread", []codexclient.Turn{{ID: "turn", Status: "interrupted"}})
	if tracker.Waiting("thread", "turn") {
		t.Fatal("cancelled turn retained pending request")
	}
	tracker.Observe(numeric)
	newGeneration := &RequestTracker{}
	if newGeneration.Waiting("thread", "turn") {
		t.Fatal("new connection reused stale request IDs")
	}
}
