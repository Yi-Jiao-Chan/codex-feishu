package sync

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"codex-feishu/internal/cards"
)

func TestRetryControlSharesFinalCardByteBudgetAndFirstSegmentOnly(t *testing.T) {
	e, _ := engineFixture(t)
	e.MaxCardBytes = 850
	e.FinalRender = func(_, _, status, card string) string {
		if status == "failed" {
			return cards.WithRetryControl(card, strings.Repeat("a", 32), "available", 7)
		}
		return card
	}
	turn := completedTurn("failed", "user", "synthetic request", "answer", "```go\n"+strings.Repeat("fmt.Println(\"中文😀\")\n", 70)+"```\n")
	turn.Status = "failed"
	original := turn
	apply(t, e, 1000, false, turn)
	mirrors, err := e.Store.Mirrors(context.Background(), "host", "thread")
	if err != nil {
		t.Fatal(err)
	}
	finals, controls := 0, 0
	for _, mirror := range mirrors {
		if mirror.Kind != "final" {
			continue
		}
		finals++
		if len(mirror.Card) > markerCardBudget(e.MaxCardBytes) || !json.Valid([]byte(mirror.Card)) {
			t.Fatal("retry control exceeded the final card's real serialized budget")
		}
		if strings.Contains(mirror.Card, `"action":"retry"`) {
			controls++
			if mirror.Key != mirrorKey("final", turn.ID, "0") || !mirror.HasCallback {
				t.Fatal("retry control escaped the first final segment")
			}
		}
	}
	if finals < 2 || controls != 1 || !reflect.DeepEqual(turn, original) {
		t.Fatal("retry decoration duplicated controls or changed native history")
	}
}
