package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
)

func pollingDeployment(t *testing.T, s *Store, host, app string, startedAt int64) {
	t.Helper()
	if needed, err := s.BeginBaseline(context.Background(), host, app, startedAt); err != nil || !needed {
		t.Fatalf("begin deployment %s/%s: needed=%v err=%v", host, app, needed, err)
	}
}

func TestPollingCheckpointsRoundTripOverwriteScopeAndFence(t *testing.T) {
	ctx := context.Background()
	s, path := discoveryStore(t)
	if _, err := s.DiscoveryCheckpoint(ctx, "host", "app"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing discovery checkpoint error = %v", err)
	}
	if _, err := s.PinnedSnapshot(ctx, "host", "app"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing pinned checkpoint error = %v", err)
	}
	if err := s.SaveDiscoveryCheckpoint(ctx, "host", "app", DiscoveryCheckpoint{LastScanAtMS: 2, LastFullAtMS: 1}); !errors.Is(err, ErrConflict) {
		t.Fatalf("checkpoint without deployment error = %v", err)
	}

	pollingDeployment(t, s, "host", "app", 1000)
	fence, complete, err := s.BaselineFence(ctx, "host")
	if err != nil || fence != 1000 || complete {
		t.Fatalf("initial baseline fence = %d complete=%v err=%v", fence, complete, err)
	}
	if _, err := s.BeginBaseline(ctx, "host", "app", 9000); err != nil {
		t.Fatalf("baseline replay failed: %v", err)
	}
	fence, _, err = s.BaselineFence(ctx, "host")
	if err != nil || fence != 1000 {
		t.Fatalf("baseline fence changed: %d %v", fence, err)
	}

	firstCheckpoint := DiscoveryCheckpoint{LastScanAtMS: 2000, LastFullAtMS: 1500}
	if err := s.SaveDiscoveryCheckpoint(ctx, "host", "app", firstCheckpoint); err != nil {
		t.Fatal(err)
	}
	gotCheckpoint, err := s.DiscoveryCheckpoint(ctx, "host", "app")
	if err != nil || !reflect.DeepEqual(gotCheckpoint, firstCheckpoint) {
		t.Fatalf("saved discovery checkpoint = %+v, err=%v", gotCheckpoint, err)
	}
	secondCheckpoint := DiscoveryCheckpoint{LastScanAtMS: 3000, LastFullAtMS: 2500}
	if err := s.SaveDiscoveryCheckpoint(ctx, "host", "app", secondCheckpoint); err != nil {
		t.Fatal(err)
	}
	gotCheckpoint, err = s.DiscoveryCheckpoint(ctx, "host", "app")
	if err != nil || !reflect.DeepEqual(gotCheckpoint, secondCheckpoint) {
		t.Fatalf("overwritten discovery checkpoint = %+v, err=%v", gotCheckpoint, err)
	}

	firstPinned := PinnedSnapshot{SectionID: "pinned", ThreadIDs: []string{"thread-z", "thread-a"}, ObservedAtMS: 4000}
	if err := s.SavePinnedSnapshot(ctx, "host", "app", firstPinned); err != nil {
		t.Fatal(err)
	}
	gotPinned, err := s.PinnedSnapshot(ctx, "host", "app")
	if err != nil || gotPinned.SectionID != "pinned" || !reflect.DeepEqual(gotPinned.ThreadIDs, []string{"thread-a", "thread-z"}) || gotPinned.ObservedAtMS != 4000 {
		t.Fatalf("saved pinned checkpoint = %+v, err=%v", gotPinned, err)
	}
	emptyPinned := PinnedSnapshot{ObservedAtMS: 5000}
	if err := s.SavePinnedSnapshot(ctx, "host", "app", emptyPinned); err != nil {
		t.Fatal(err)
	}
	gotPinned, err = s.PinnedSnapshot(ctx, "host", "app")
	if err != nil || gotPinned.SectionID != "" || len(gotPinned.ThreadIDs) != 0 || gotPinned.ObservedAtMS != 5000 {
		t.Fatalf("empty pinned checkpoint = %+v, err=%v", gotPinned, err)
	}

	if _, err := s.DiscoveryCheckpoint(ctx, "host", "other-app"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("wrong app checkpoint error = %v", err)
	}
	if _, err := s.PinnedSnapshot(ctx, "other-host", "app"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("wrong host pinned checkpoint error = %v", err)
	}
	pollingDeployment(t, s, "other-host", "other-app", 6000)
	otherCheckpoint := DiscoveryCheckpoint{LastScanAtMS: 8000, LastFullAtMS: 7000}
	if err := s.SaveDiscoveryCheckpoint(ctx, "other-host", "other-app", otherCheckpoint); err != nil {
		t.Fatal(err)
	}
	if got, err := s.DiscoveryCheckpoint(ctx, "other-host", "other-app"); err != nil || !reflect.DeepEqual(got, otherCheckpoint) {
		t.Fatalf("other deployment checkpoint = %+v, err=%v", got, err)
	}
	if got, err := s.DiscoveryCheckpoint(ctx, "host", "app"); err != nil || !reflect.DeepEqual(got, secondCheckpoint) {
		t.Fatalf("deployment checkpoint crossed scope = %+v, err=%v", got, err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, err := reopened.DiscoveryCheckpoint(ctx, "host", "app"); err != nil || !reflect.DeepEqual(got, secondCheckpoint) {
		t.Fatalf("discovery checkpoint lost after reopen = %+v, err=%v", got, err)
	}
	if got, err := reopened.PinnedSnapshot(ctx, "host", "app"); err != nil || got.ObservedAtMS != 5000 || got.SectionID != "" || len(got.ThreadIDs) != 0 {
		t.Fatalf("pinned checkpoint lost after reopen = %+v, err=%v", got, err)
	}
	fence, complete, err = reopened.BaselineFence(ctx, "host")
	if err != nil || fence != 1000 || complete {
		t.Fatalf("reopened baseline fence = %d complete=%v err=%v", fence, complete, err)
	}
}

func TestPollingCheckpointValidationRejectsWithoutOverwriting(t *testing.T) {
	ctx := context.Background()
	s, _ := discoveryStore(t)
	pollingDeployment(t, s, "host", "app", 1000)

	validCheckpoint := DiscoveryCheckpoint{LastScanAtMS: 3000, LastFullAtMS: 2000}
	if err := s.SaveDiscoveryCheckpoint(ctx, "host", "app", validCheckpoint); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []DiscoveryCheckpoint{
		{},
		{LastScanAtMS: 0, LastFullAtMS: 1},
		{LastScanAtMS: 1, LastFullAtMS: 0},
		{LastScanAtMS: 1000, LastFullAtMS: 2000},
		{LastScanAtMS: -1, LastFullAtMS: -1},
	} {
		if err := s.SaveDiscoveryCheckpoint(ctx, "host", "app", invalid); !errors.Is(err, ErrConflict) {
			t.Errorf("invalid discovery checkpoint %+v error = %v", invalid, err)
		}
		got, err := s.DiscoveryCheckpoint(ctx, "host", "app")
		if err != nil || !reflect.DeepEqual(got, validCheckpoint) {
			t.Fatalf("invalid discovery checkpoint overwrote value: %+v, err=%v", got, err)
		}
	}

	validPinned := PinnedSnapshot{SectionID: "pinned", ThreadIDs: []string{"thread-a", "thread-b"}, ObservedAtMS: 4000}
	if err := s.SavePinnedSnapshot(ctx, "host", "app", validPinned); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []PinnedSnapshot{
		{},
		{SectionID: "pinned", ThreadIDs: []string{"thread-a"}},
		{ThreadIDs: []string{"thread-a"}, ObservedAtMS: 5000},
		{SectionID: "pinned", ThreadIDs: []string{""}, ObservedAtMS: 5000},
		{SectionID: "pinned", ThreadIDs: []string{"thread-a", "thread-a"}, ObservedAtMS: 5000},
	} {
		if err := s.SavePinnedSnapshot(ctx, "host", "app", invalid); !errors.Is(err, ErrConflict) {
			t.Errorf("invalid pinned snapshot %+v error = %v", invalid, err)
		}
		got, err := s.PinnedSnapshot(ctx, "host", "app")
		if err != nil || !reflect.DeepEqual(got.ThreadIDs, []string{"thread-a", "thread-b"}) || got.SectionID != "pinned" || got.ObservedAtMS != 4000 {
			t.Fatalf("invalid pinned snapshot overwrote value: %+v, err=%v", got, err)
		}
	}
}

func TestPollingCheckpointsRejectMalformedAndWrongState(t *testing.T) {
	ctx := context.Background()
	s, _ := discoveryStore(t)
	pollingDeployment(t, s, "host", "app", 1000)
	checkpoint := DiscoveryCheckpoint{LastScanAtMS: 3000, LastFullAtMS: 2000}
	pinned := PinnedSnapshot{SectionID: "pinned", ThreadIDs: []string{"thread"}, ObservedAtMS: 4000}
	if err := s.SaveDiscoveryCheckpoint(ctx, "host", "app", checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePinnedSnapshot(ctx, "host", "app", pinned); err != nil {
		t.Fatal(err)
	}

	discoveryKey := pollingKey(discoveryCheckpointKind, "host", "app")
	if _, err := s.DB.ExecContext(ctx, "UPDATE m0_operations SET remote_id=? WHERE key=?", "not-json", discoveryKey); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DiscoveryCheckpoint(ctx, "host", "app"); !errors.Is(err, ErrConflict) {
		t.Fatalf("malformed discovery checkpoint error = %v", err)
	}
	if err := s.SaveDiscoveryCheckpoint(ctx, "host", "app", checkpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, "UPDATE m0_operations SET state=? WHERE key=?", "sending", discoveryKey); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DiscoveryCheckpoint(ctx, "host", "app"); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong-state discovery checkpoint error = %v", err)
	}

	pinnedKey := pollingKey(pinnedSnapshotKind, "host", "app")
	if _, err := s.DB.ExecContext(ctx, "UPDATE m0_operations SET remote_id=? WHERE key=?", "not-json", pinnedKey); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PinnedSnapshot(ctx, "host", "app"); !errors.Is(err, ErrConflict) {
		t.Fatalf("malformed pinned checkpoint error = %v", err)
	}
	if err := s.SavePinnedSnapshot(ctx, "host", "app", pinned); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, "UPDATE m0_operations SET state=? WHERE key=?", "unknown", pinnedKey); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PinnedSnapshot(ctx, "host", "app"); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong-state pinned checkpoint error = %v", err)
	}
}
