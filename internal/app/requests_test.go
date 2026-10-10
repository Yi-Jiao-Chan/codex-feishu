package app

import (
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func appRequestEvent(generation uint64, method, id, thread, turn string) codexclient.Event {
	params, _ := json.Marshal(map[string]string{"threadId": thread, "turnId": turn})
	return codexclient.Event{Generation: generation, Method: method, RequestID: json.RawMessage(id), Params: params}
}

func resolvedEvent(generation uint64, id, thread string) codexclient.Event {
	params, _ := json.Marshal(map[string]any{"threadId": thread, "requestId": json.RawMessage(id)})
	return codexclient.Event{Generation: generation, Method: "serverRequest/resolved", Params: params}
}

func TestAppWaitingStateKeepsTurnCardAndExactRequestIdentity(t *testing.T) {
	m, g := harness(t)
	ctx := context.Background()
	turn := codexclient.Turn{ID: "turn", Status: "inProgress"}
	request := appRequestEvent(7, "item/tool/requestUserInput", `3`, "thread", "turn")
	m.trackAppRequest(request)
	m.trackAppRequest(request) // warm resume can replay the same pending request.
	m.trackAppRequest(appRequestEvent(7, "item/tool/requestUserInput", `4`, "other-thread", "turn"))
	m.trackAppRequest(appRequestEvent(7, "item/tool/call", `5`, "thread", "turn"))
	if len(m.appRequests) != 1 {
		t.Fatal("replay duplicated or unrelated request tracked")
	}
	if err := m.syncTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	if len(g.sends) != 1 || g.parents[0] != "original" || !strings.Contains(g.sends[0], "等待你在 Codex App 中处理") {
		t.Fatal("waiting must use the quoted turn progress card")
	}
	m.trackAppRequest(resolvedEvent(6, `3`, "thread"))
	m.trackAppRequest(resolvedEvent(7, `3`, "other-thread"))
	m.trackAppRequest(resolvedEvent(7, `"3"`, "thread"))
	if !m.waitingInApp("turn") {
		t.Fatal("stale or different request resolved pending interaction")
	}
	m.trackAppRequest(appRequestEvent(7, "item/fileChange/requestApproval", `"3"`, "thread", "turn"))
	m.trackAppRequest(resolvedEvent(7, `3`, "thread"))
	if !m.waitingInApp("turn") {
		t.Fatal("one of multiple callbacks was mistaken for all resolved")
	}
	m.trackAppRequest(resolvedEvent(7, `"3"`, "thread"))
	m.progress[turn.ID] = progressState{updated: time.Now().Add(-3 * time.Second)}
	if err := m.syncTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	if len(g.sends) != 1 || len(g.updates) != 1 || strings.Contains(g.updates[0], "等待你") {
		t.Fatal("resolved state did not update the same card")
	}
	m.trackAppRequest(request)
	turn.Status = "interrupted"
	if err := m.syncTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	if m.waitingInApp("turn") {
		t.Fatal("terminal turn kept its pending interaction")
	}
}

func TestAppRequestIDNormalization(t *testing.T) {
	if appRequestKey(1, json.RawMessage(`"\u0061"`)) != appRequestKey(1, json.RawMessage(`"a"`)) {
		t.Fatal("equivalent string IDs differ")
	}
	if appRequestKey(1, json.RawMessage(`9223372036854775807`)) == appRequestKey(1, json.RawMessage(`9223372036854775806`)) {
		t.Fatal("integer IDs lost precision")
	}
	for _, invalid := range []string{"", "null", " null ", "{}", "[]", "1.5", "true"} {
		if appRequestKey(1, json.RawMessage(invalid)) != "" {
			t.Fatalf("invalid request ID accepted: %q", invalid)
		}
	}
}

func TestAppWaitingReactionReturnsToExecutionWithoutRevivingRemovedInput(t *testing.T) {
	m, g := harness(t)
	ctx := context.Background()
	turn := codexclient.Turn{ID: "turn", Status: "inProgress"}
	r := store.Receipt{ID: "receipt", MessageID: "original", State: "consumed"}
	request := appRequestEvent(7, "item/tool/requestUserInput", `3`, "thread", "turn")
	m.trackAppRequest(request)
	m.syncReaction(ctx, r, turn)
	m.trackAppRequest(resolvedEvent(7, `3`, "thread"))
	m.syncReaction(ctx, r, turn)
	if strings.Join(g.added, ",") != "THINKING,OnIt" {
		t.Fatal("waiting/resolved reaction did not follow native request")
	}
	m.trackAppRequest(request)
	r.State = "removed"
	m.syncReaction(ctx, r, turn)
	if len(g.added) != 2 {
		t.Fatal("pending request revived reaction on removed input")
	}
}
