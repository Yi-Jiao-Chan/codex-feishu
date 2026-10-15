package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

func TestProgressClockKeepsItemIdentityAcrossCompletionReplayAndReopen(t *testing.T) {
	e, path := engineFixture(t)
	e.TimeZone = "Asia/Shanghai"
	var latest cards.ProgressView
	e.ProgressRender = func(_, _, status string, v cards.ProgressView) string {
		latest = v
		return cards.Task(cards.TaskView{Status: status, Progress: v})
	}
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", ItemsView: "full", Items: []codexclient.Item{input("user", "request")}}
	apply(t, e, 200000, false, turn) // An empty modern clock is known, not legacy.
	turn.Items = append(turn.Items, agent("first", "partial", "commentary"))
	apply(t, e, 201000, false, turn)
	turn.Items[1].Text = "completed public text"
	turn.Items = append(turn.Items, agent("second", "completed public text", "commentary"))
	apply(t, e, 202000, false, turn)
	want := []cards.ProgressRecord{{ItemID: "first", Text: "completed public text", ReceivedAtMS: 201000}, {ItemID: "second", Text: "completed public text", ReceivedAtMS: 202000}}
	if !reflect.DeepEqual(latest.Records, want) || latest.TimeZone != e.TimeZone {
		t.Fatalf("item identity/time zone mismatch: %+v", latest)
	}
	e.Store.Close()
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	e.Store = reopened
	apply(t, e, 300000, true, turn)
	if !reflect.DeepEqual(latest.Records, want) {
		t.Fatalf("reopen/replay retimed records: %+v", latest.Records)
	}
	turn.Items = append(turn.Items, agent("new-live", "new item", "partial_answer"))
	apply(t, e, 301000, false, turn)
	if latest.Records[2].ReceivedAtMS != 301000 {
		t.Fatal("new item lacks first live reception time")
	}
}

func TestHistoricalAndLegacyProgressClocksUseFirstRecordedReception(t *testing.T) {
	for _, source := range []string{"historical replay", "legacy existing mirror", "persisted unknown clock"} {
		t.Run(source, func(t *testing.T) {
			e, path := engineFixture(t)
			turn := codexclient.Turn{ID: "turn", Status: "inProgress", ItemsView: "full", Items: []codexclient.Item{input("user", "request"), agent("old", "known text", "commentary")}}
			if source != "historical replay" {
				doc := store.Mirror{Key: mirrorKey("progress", "turn"), TurnID: "turn", Kind: "progress", Card: `{"legacy":true}`, ParentKey: mirrorKey("human", "turn", "user", "0")}
				if source == "persisted unknown clock" {
					doc.ProgressObservedAt = map[string]int64{"old": 0}
				}
				if err := e.Store.StageSnapshot(context.Background(), "host", "thread", []store.Mirror{doc}, nil, store.Watermark{ObservedAtMS: 100000}); err != nil {
					t.Fatal(err)
				}
			}
			apply(t, e, 200000, source == "historical replay", turn)
			turn.Items[1].Text = "updated known text"
			turn.Items = append(turn.Items, agent("new", "new live text", "commentary"))
			apply(t, e, 201000, false, turn)
			want := map[string]int64{"old": 200000, "new": 201000}
			m, err := e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("progress", "turn"))
			if err != nil || !reflect.DeepEqual(m.ProgressObservedAt, want) {
				t.Fatalf("first recorded reception missing: %+v, %v", m.ProgressObservedAt, err)
			}
			e.Store.Close()
			reopened, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			e.Store = reopened
			apply(t, e, 300000, true, turn)
			m, err = e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("progress", "turn"))
			if err != nil || !reflect.DeepEqual(m.ProgressObservedAt, want) {
				t.Fatalf("reopen/replay retimed fallback: %+v, %v", m.ProgressObservedAt, err)
			}
		})
	}
}

func TestProgressBudgetOmissionDoesNotDiscardClock(t *testing.T) {
	e, _ := engineFixture(t)
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", ItemsView: "full", Items: []codexclient.Item{input("user", "request"), agent("old", strings.Repeat("old text", 500), "commentary"), agent("latest", "new text", "commentary")}}
	var rendered cards.ProgressView
	e.ProgressRender = func(_, _, _ string, v cards.ProgressView) string {
		rendered = v
		data, _ := json.Marshal(v)
		return string(data)
	}
	e.MaxCardBytes = 1024 + cards.MaxRequestMarkerBytes
	apply(t, e, 200000, false, turn)
	if rendered.OmittedRecords != 1 || len(rendered.Records) != 1 {
		t.Fatalf("fixture did not omit oldest record: %+v", rendered)
	}
	m, err := e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("progress", "turn"))
	if err != nil || !reflect.DeepEqual(m.ProgressObservedAt, map[string]int64{"old": 200000, "latest": 200000}) {
		t.Fatal("omitted record clock lost", err)
	}
	e.MaxCardBytes = 0
	apply(t, e, 201000, false, turn)
	if len(rendered.Records) != 2 || rendered.Records[0].ReceivedAtMS != 200000 {
		t.Fatalf("record retimed on reappearance: %+v", rendered.Records)
	}
}

func TestInvalidSnapshotDoesNotCommitNewProgressClock(t *testing.T) {
	e, _ := engineFixture(t)
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", ItemsView: "full", Items: []codexclient.Item{input("user", "request"), agent("old", "public text", "commentary")}}
	apply(t, e, 200000, false, turn)
	turn.Items = append(turn.Items, agent("new", "new public text", "commentary"))
	invalid := codexclient.Turn{ID: "invalid", Status: "inProgress", ItemsView: "partial"}
	if err := e.Apply(context.Background(), "thread", []codexclient.Turn{turn, invalid}, false, 201000); err == nil {
		t.Fatal("bad snapshot accepted")
	}
	m, err := e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("progress", "turn"))
	if err != nil || !reflect.DeepEqual(m.ProgressObservedAt, map[string]int64{"old": 200000}) {
		t.Fatal("bad snapshot committed clock", err)
	}
	apply(t, e, 202000, false, turn)
	m, err = e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("progress", "turn"))
	if err != nil || m.ProgressObservedAt["new"] != 202000 {
		t.Fatal("failed snapshot leaked reception time", err)
	}
}

func TestProgressRecordCountCapPersistsAllClocks(t *testing.T) {
	e, _ := engineFixture(t)
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", ItemsView: "full", Items: []codexclient.Item{input("user", "request")}}
	for n := 0; n < 40; n++ {
		turn.Items = append(turn.Items, agent(fmt.Sprintf("record-%02d", n), "short action", "commentary"))
	}
	var rendered cards.ProgressView
	e.ProgressRender = func(_, _, _ string, v cards.ProgressView) string {
		rendered = v
		data, _ := json.Marshal(v)
		return string(data)
	}
	apply(t, e, 200000, false, turn)
	if len(rendered.Records) != cards.MaxProgressRecords || rendered.OmittedRecords != 8 || rendered.Records[0].ItemID != "record-08" {
		t.Fatalf("count cap mismatch: %+v", rendered)
	}
	m, err := e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("progress", "turn"))
	if err != nil || len(m.ProgressObservedAt) != 40 {
		t.Fatal("display cap discarded clocks", err)
	}
	for n := 0; n < 40; n++ {
		if m.ProgressObservedAt[fmt.Sprintf("record-%02d", n)] != 200000 {
			t.Fatal("display cap changed first clock")
		}
	}
}

func TestEmptyFullSnapshotSeedsLegacyClockBeforeNewLiveItem(t *testing.T) {
	e, _ := engineFixture(t)
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", ItemsView: "full", Items: []codexclient.Item{input("user", "request")}}
	legacy := store.Mirror{Key: mirrorKey("progress", "turn"), TurnID: "turn", Kind: "progress", Card: `{"legacy":true}`, ParentKey: mirrorKey("human", "turn", "user", "0")}
	if err := e.Store.StageSnapshot(context.Background(), "host", "thread", []store.Mirror{legacy}, nil, store.Watermark{ObservedAtMS: 100000}); err != nil {
		t.Fatal(err)
	}
	apply(t, e, 200000, false, turn)
	turn.Items = append(turn.Items, agent("new", "new live public text", "commentary"))
	apply(t, e, 201000, false, turn)
	m, err := e.Store.Mirror(context.Background(), "host", "thread", legacy.Key)
	if err != nil || !reflect.DeepEqual(m.ProgressObservedAt, map[string]int64{"new": 201000}) {
		t.Fatalf("known empty history treated new item as historical: %+v, %v", m.ProgressObservedAt, err)
	}
}
