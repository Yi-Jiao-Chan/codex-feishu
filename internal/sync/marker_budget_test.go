package sync

import (
	"strings"
	"testing"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
)

func TestLongFinalWireCardsReserveBothRequestMarkerAndConfiguredMentions(t *testing.T) {
	e, _ := engineFixture(t)
	e.MaxCardBytes = 850
	e.MentionTargets = []string{"ou_one_long_identifier", "ou_two_long_identifier"}
	body := "```go\n" + strings.Repeat("中文 code example <tag>\n", 200) + "```"
	turn := codexclient.Turn{ID: "turn", Status: "completed", Items: []codexclient.Item{input("user", "question"), agent("answer", body, "final_answer")}}
	apply(t, e, 200, false, turn)
	bindEngine(t, e)
	g := &cardGateway{}
	drain(t, e, g, 1000)
	if len(g.writes) < 3 {
		t.Fatal("long public result was not segmented")
	}
	for _, w := range g.writes {
		if len(w.Card) > e.MaxCardBytes {
			t.Fatal("marker exceeded total card budget")
		}
		if w.Operation != "reply" {
			continue
		}
		wire, err := cards.WithMentions(w.Card, e.MentionTargets)
		if err != nil || len(wire) > e.MaxCardBytes {
			t.Fatal("mentions or marker exceed configured total card budget", err, len(wire))
		}
	}
}
