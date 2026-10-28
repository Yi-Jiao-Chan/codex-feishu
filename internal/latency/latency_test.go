package latency

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRecordHashesScopedIdentitiesAndDropsErrorText(t *testing.T) {
	var events []Event
	observe := Observer(func(e Event) { events = append(events, e) })
	observe.Record("stage", "private-host", "private-thread", "private-turn", "private-work", 1500*time.Microsecond, Result(errors.New("private-error")))
	observe.Record("stage", "private-host", "private-thread", "private-turn", "private-work", -time.Second, Result(nil))
	observe.Record("stage", "another-host", "private-thread", "private-turn", "private-work", 0, "ok")
	data, err := json.Marshal(events)
	if err != nil || strings.Contains(string(data), "private-") {
		t.Fatal("private identity or error text reached diagnostics")
	}
	first, second := events[0], events[1]
	if first.ThreadRef == "" || first.TurnRef == "" || first.WorkRef == "" || first.AtMS <= 0 || first.DurationMS != 1.5 || first.Outcome != "error" {
		t.Fatal("missing trace identity, timestamp, duration or outcome")
	}
	if first.ThreadRef != second.ThreadRef || first.TurnRef != second.TurnRef || first.WorkRef != second.WorkRef || second.DurationMS != 0 || second.Outcome != "ok" {
		t.Fatal("trace correlation is not stable")
	}
	if first.ThreadRef == events[2].ThreadRef || first.TurnRef == events[2].TurnRef || first.WorkRef == events[2].WorkRef {
		t.Fatal("trace identities cross deployment scopes")
	}
	var disabled Observer
	disabled.Record("stage", "", "", "", "", 0, "ok")
}
