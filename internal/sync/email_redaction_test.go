package sync

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"codex-feishu/internal/codexclient"
)

func TestEmailRedactionIsFrozenForPublicDeliveryWithoutChangingHistory(t *testing.T) {
	e, _ := engineFixture(t)
	bindEngine(t, e)
	turn := codexclient.Turn{ID: "turn", Status: "completed", ItemsView: "full", Items: []codexclient.Item{
		input("user", "检查 alice@example.invalid 的提交"),
		agent("final", "作者为 bob+ci@example.invalid。", "final_answer"),
	}}
	before, _ := json.Marshal(turn)
	apply(t, e, 200, false, turn)
	g := &cardGateway{}
	o := Outbound{Store: e.Store, Gateway: g}
	for n, address := range []string{`alice\u003cA\u003eexample.invalid`, `bob+ci\u003cA\u003eexample.invalid`} {
		result, err := o.DispatchOne(context.Background(), "host", "thread", 1000)
		if err != nil || result.State != "confirmed" {
			t.Fatalf("public delivery failed: %+v %v", result, err)
		}
		attempt, err := e.Store.MirrorAttempt(context.Background(), result.AttemptID)
		if err != nil || attempt.Card != g.writes[n].Card || strings.Contains(attempt.Card, "@example.invalid") ||
			!strings.Contains(attempt.Card, address) || strings.Contains(attempt.Card, `\u0026lt;A\u0026gt;`) {
			t.Fatal("delivery differed from the frozen, masked card", err)
		}
	}
	if g.writes[1].Operation != "reply" || g.writes[1].Parent != g.writes[0].ID {
		t.Fatal("email masking changed the final reply anchor")
	}
	after, _ := json.Marshal(turn)
	if string(after) != string(before) {
		t.Fatal("display masking rewrote native input or history")
	}
	apply(t, e, 201, true, turn)
	drain(t, e, g, 2000)
	if len(g.writes) != 2 {
		t.Fatal("unchanged masked history caused duplicate delivery")
	}
}

func TestCOTProjectionMasksEmailBeforePlanningEvents(t *testing.T) {
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", ItemsView: "full", Items: []codexclient.Item{
		agent("progress", "正在检查 alice@example.invalid", "commentary"),
		{ID: "file", Type: "fileChange", Status: "completed", Changes: []codexclient.FileChange{{Path: "/synthetic/bob@example.invalid", Kind: json.RawMessage(`{"type":"update"}`)}}},
	}}
	before, _ := json.Marshal(turn)
	raw, err := cotDocument("thread", turn, nil, 1000)
	doc := parseProjection(t, raw, err)
	if len(doc.Items) != 2 || doc.Items[0].Text != "正在检查 alice<A>example.invalid" || doc.Items[1].Title != "编辑文件" || strings.Contains(raw, "@example.invalid") {
		t.Fatalf("unexpected masked progress: %+v", doc.Items)
	}
	after, _ := json.Marshal(turn)
	if string(before) != string(after) {
		t.Fatal("progress projection rewrote native history")
	}
}
