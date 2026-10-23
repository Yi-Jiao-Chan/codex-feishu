package sync

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
)

func TestLeadingFencedFinalActualOutboundMentionsAndMarkerFitTightBudget(t *testing.T) {
	ctx := context.Background()
	e, _ := engineFixture(t)
	e.MentionTargets = []string{"ou_user"}
	event := time.Date(2026, 10, 13, 1, 0, 0, 0, time.UTC)
	policy, err := rules.DefaultPolicy("UTC")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.InitNotificationPolicyAt(ctx, "app", "ou_user", policy, event.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	body := "```go\n" + strings.Repeat("中文 code example <tag>\n", 200) + "```"
	source := cards.ModelSettingsText("", nil, nil, false)
	unsplit, err := cards.WithMentions(cards.Message("", source, body), e.MentionTargets)
	if err != nil {
		t.Fatal(err)
	}
	unsplit, marked, err := cards.WithRequestMarker(unsplit, "abcdefghijklmnopqrst")
	if err != nil || !marked {
		t.Fatal("fixture could not construct its full wire", err)
	}
	// One byte less than the actual unsplit block+mention+marker wire forces
	// segmentation. Omitting the block separator reserve would accept the
	// unsplit public card and exceed this real wire budget by one byte.
	e.MaxCardBytes = len(unsplit) - 1
	completed := event.Unix()
	turn := codexclient.Turn{ID: "turn", Status: "completed", CompletedAt: &completed, Items: []codexclient.Item{input("user", "request"), agent("answer", body, "final_answer")}}
	apply(t, e, event.Add(time.Second).UnixMilli(), false, turn)
	bindEngine(t, e)
	options := store.MentionOptions{Enabled: true, Targets: e.MentionTargets}
	if err := e.Store.ScheduleMentions(ctx, "host", "thread", options); err != nil {
		t.Fatal(err)
	}
	g := &cardGateway{}
	membership := &mentionMembership{s: e.Store, t: t, at: event.Add(2 * time.Second), ids: []string{"ou_user"}}
	outbound := Outbound{Store: e.Store, Gateway: g, Mentions: &options, Membership: membership}
	for n := 0; n < 1000; n++ {
		result, err := outbound.DispatchOne(ctx, "host", "thread", event.Add(2*time.Second).UnixMilli())
		if err != nil || result.AttemptID != "" && result.State != "confirmed" {
			t.Fatal("owned fixture delivery did not confirm", err)
		}
		if result.AttemptID == "" {
			break
		}
		if n == 999 {
			t.Fatal("owned fixture outbox did not settle")
		}
	}
	budget, err := finalCardBudget(e.MaxCardBytes, e.MentionTargets)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := cards.SplitMessage("", source, body, budget)
	if err != nil || len(parts) < 2 || len(g.writes) != len(parts)+1 {
		t.Fatal("tight native result did not use stable full segments", err)
	}
	var raw strings.Builder
	for n, part := range parts {
		raw.WriteString(part.Raw)
		wire := g.writes[n+1]
		if wire.Operation != "reply" || wire.Parent != g.writes[0].ID || len(wire.Card) > e.MaxCardBytes {
			t.Fatal("actual wire exceeded budget or lost its original input reply")
		}
		var decoded struct {
			Body struct {
				Elements []struct {
					Tag, Content string
					ElementID    string `json:"element_id"`
				} `json:"elements"`
			} `json:"body"`
		}
		if err := json.Unmarshal([]byte(wire.Card), &decoded); err != nil || len(decoded.Body.Elements) < 2 {
			t.Fatal("actual result wire lost its body/source structure", err)
		}
		first, last := decoded.Body.Elements[0], decoded.Body.Elements[len(decoded.Body.Elements)-1]
		if first.Tag != "markdown" || first.ElementID == "" || last.Tag != "markdown" || !strings.HasPrefix(last.Content, "> "+source) || strings.Contains(last.Content, "<at ") {
			t.Fatal("request marker or trailing source was lost")
		}
		want := strings.Trim(cards.SafeMarkdown(part.Body), "\r\n")
		if n == 0 {
			want = cards.Mention("ou_user") + "\n\n" + want
			if e.MaxCardBytes-len(wire.Card) > 100 {
				t.Fatal("leading fenced-code wire did not exercise a tight budget")
			}
		}
		if first.Content != want || (n != 0 && strings.Contains(first.Content, "<at ")) {
			t.Fatal("first-only mention damaged fenced Markdown or appeared on another segment")
		}
	}
	if raw.String() != body {
		t.Fatal("native final content was truncated or lost between Raw segments")
	}
	plain, err := finalCardBudget(e.MaxCardBytes, nil)
	if err != nil || plain != markerCardBudget(e.MaxCardBytes) {
		t.Fatal("mention-free final reserved unnecessary mention separator bytes", err)
	}
}
