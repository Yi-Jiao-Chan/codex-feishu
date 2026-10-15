package store

import (
	"context"
	"reflect"
	"testing"
)

func TestProgressClockSurvivesReopenAndMetadataFreeUpdates(t *testing.T) {
	ctx := context.Background()
	s, path := discoveryStore(t)
	eligible(t, s, "thread")
	doc := mirrorDoc("progress", "progress", "public action")
	doc.ProgressObservedAt = map[string]int64{"first": 100, "historical": 0}
	stage(t, s, 100, doc)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	m, err := reopened.Mirror(ctx, "host", "thread", doc.Key)
	if err != nil || !reflect.DeepEqual(m.ProgressObservedAt, doc.ProgressObservedAt) {
		t.Fatalf("clock lost on reopen: %+v, %v", m.ProgressObservedAt, err)
	}
	// A callback that updates only its card must preserve all recorded clocks.
	doc.ProgressObservedAt = nil
	doc.Card = `{"body":"control state changed"}`
	stage(t, reopened, 200, doc)
	m, err = reopened.Mirror(ctx, "host", "thread", doc.Key)
	if err != nil || !reflect.DeepEqual(m.ProgressObservedAt, map[string]int64{"first": 100, "historical": 0}) {
		t.Fatal("metadata-free update cleared clock", err)
	}
	revision := m.Revision
	// Identical-card metadata changes may fill a legacy zero once.
	doc.ProgressObservedAt = map[string]int64{"first": 300, "historical": 300, "next": 300}
	stage(t, reopened, 300, doc)
	m, err = reopened.Mirror(ctx, "host", "thread", doc.Key)
	want := map[string]int64{"first": 100, "historical": 300, "next": 300}
	if err != nil || !reflect.DeepEqual(m.ProgressObservedAt, want) || m.Revision != revision {
		t.Fatalf("first clock replaced or metadata alone bumped revision: %+v, %v", m, err)
	}
	// Neither a stale zero nor a newer positive time may replace that value.
	doc.ProgressObservedAt = map[string]int64{"first": 0, "historical": 400, "next": 0}
	stage(t, reopened, 400, doc)
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	verified, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	m, err = verified.Mirror(ctx, "host", "thread", doc.Key)
	if err != nil || !reflect.DeepEqual(m.ProgressObservedAt, want) || m.Revision != revision {
		t.Fatalf("filled clock changed across stale update/reopen: %+v, %v", m, err)
	}
}

func TestBadSnapshotRollsBackProgressClockAndCheckpoint(t *testing.T) {
	ctx := context.Background()
	s, _ := discoveryStore(t)
	eligible(t, s, "thread")
	doc := mirrorDoc("progress", "progress", "public action")
	doc.ProgressObservedAt = map[string]int64{"first": 100}
	stage(t, s, 100, doc)
	doc.ProgressObservedAt = map[string]int64{"first": 100, "new": 200}
	bad := mirrorDoc("bad", "human", "invalid")
	bad.Card = "invalid JSON"
	if err := s.StageSnapshot(ctx, "host", "thread", []Mirror{doc, bad}, nil, Watermark{ObservedAtMS: 200}); err == nil {
		t.Fatal("invalid snapshot committed")
	}
	m, err := s.Mirror(ctx, "host", "thread", doc.Key)
	if err != nil || !reflect.DeepEqual(m.ProgressObservedAt, map[string]int64{"first": 100}) {
		t.Fatal("bad snapshot partially committed clocks", err)
	}
	w, err := s.Watermark(ctx, "host", "thread")
	if err != nil || w.ObservedAtMS != 100 {
		t.Fatal("bad snapshot advanced checkpoint", err)
	}
}

func TestSchema21ProgressClockMigrationDoesNotInventReceptionTimes(t *testing.T) {
	ctx := context.Background()
	s, path := discoveryStore(t)
	eligible(t, s, "thread")
	doc := mirrorDoc("progress", "progress", "legacy public action")
	stage(t, s, 100, doc)
	original, err := s.Mirror(ctx, "host", "thread", doc.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`ALTER TABLE mirrors DROP COLUMN progress_observed_at_json; ALTER TABLE create_intents DROP COLUMN description; PRAGMA user_version=21`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var version int
	if err := upgraded.DB.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatal("migration schema", version, err)
	}
	m, err := upgraded.Mirror(ctx, "host", "thread", doc.Key)
	if err != nil || m.ProgressObservedAt != nil || m.Card != original.Card || m.Revision != original.Revision || m.State != original.State {
		t.Fatalf("migration changed legacy facts: %+v, %v", m, err)
	}
}

func TestExplicitEmptyProgressClockSeedsLegacyWithoutClearingRecordedItems(t *testing.T) {
	ctx := context.Background()
	s, _ := discoveryStore(t)
	eligible(t, s, "thread")
	doc := mirrorDoc("progress", "progress", "legacy public action")
	stage(t, s, 100, doc)
	doc.ProgressObservedAt = map[string]int64{}
	stage(t, s, 200, doc)
	m, err := s.Mirror(ctx, "host", "thread", doc.Key)
	if err != nil || m.ProgressObservedAt == nil || len(m.ProgressObservedAt) != 0 {
		t.Fatal("empty full snapshot did not seed known clock", err)
	}
	doc.ProgressObservedAt = map[string]int64{"new": 300}
	stage(t, s, 300, doc)
	doc.ProgressObservedAt = map[string]int64{}
	stage(t, s, 400, doc)
	m, err = s.Mirror(ctx, "host", "thread", doc.Key)
	if err != nil || !reflect.DeepEqual(m.ProgressObservedAt, map[string]int64{"new": 300}) {
		t.Fatal("explicit empty clock cleared earlier item", err)
	}
}
