package app

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

type pinNativeFixture struct {
	*runtimeNative
	pinMu       sync.Mutex
	sections    []codexclient.ThreadSection
	catalogErr  error
	archiveErr  error
	archived    map[string]bool
	sectionRead atomic.Int32
}

func (n *pinNativeFixture) Sections(context.Context) ([]codexclient.ThreadSection, error) {
	n.pinMu.Lock()
	defer n.pinMu.Unlock()
	return append([]codexclient.ThreadSection(nil), n.sections...), n.catalogErr
}

func (n *pinNativeFixture) ListSection(_ context.Context, section string, archived bool) ([]codexclient.Thread, error) {
	n.sectionRead.Add(1)
	n.pinMu.Lock()
	defer n.pinMu.Unlock()
	err := n.archiveErr
	if archived && err != nil {
		return nil, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	var threads []codexclient.Thread
	for _, thread := range n.threads {
		if thread.Section != nil && thread.Section.ID == section && n.archived[thread.ID] == archived {
			threads = append(threads, thread)
		}
	}
	return threads, nil
}

func pinMeta(n *runtimeNative, id, section, name string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	thread := n.threads[id]
	thread.UpdatedAt = time.Now().Add(-48 * time.Hour).Unix()
	thread.Status.Type = "idle"
	thread.Name = &name
	thread.Section = nil
	if section != "" {
		thread.Section = &codexclient.ThreadSection{ID: section, Name: "Pinned"}
	}
	n.threads[id] = thread
}

func TestPinnedSectionIdentityAndChanges(t *testing.T) {
	for _, tc := range []struct {
		name, previous, want string
		sections             []codexclient.ThreadSection
		invalid              bool
	}{
		{name: "new catalog", sections: []codexclient.ThreadSection{{ID: "pin", Name: "Pinned"}, {ID: "other", Name: "Work"}}, want: "pin"},
		{name: "renamed stable identity", previous: "pin", sections: []codexclient.ThreadSection{{ID: "pin", Name: "Favorites"}, {ID: "other", Name: "Pinned"}}, want: "pin"},
		{name: "deleted section", previous: "pin", sections: []codexclient.ThreadSection{{ID: "other", Name: "Work"}}},
		{name: "replacement section", previous: "old", sections: []codexclient.ThreadSection{{ID: "new", Name: "Pinned"}}, want: "new"},
		{name: "ambiguous name", sections: []codexclient.ThreadSection{{ID: "one", Name: "Pinned"}, {ID: "two", Name: "Pinned"}}, invalid: true},
		{name: "duplicate identity", sections: []codexclient.ThreadSection{{ID: "one", Name: "Pinned"}, {ID: "one", Name: "Pinned"}}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pinnedSectionID(tc.sections, tc.previous)
			if got != tc.want || (err != nil) != tc.invalid {
				t.Fatalf("identity=%q error=%v", got, err)
			}
		})
	}
	if got := pinChanges([]string{"same", "removed"}, []string{"same", "added"}); !reflect.DeepEqual(got, map[string]bool{"removed": true, "added": true}) {
		t.Fatal("pin changes did not include both directions")
	}
	name := "📌 literal title"
	meta := codexclient.Thread{Name: &name}
	if threadGroupTitleWithPin(meta, nil, true) != "📌 🤖 📌 literal title" || threadGroupTitleWithPin(meta, nil, false) != "🤖 📌 literal title" {
		t.Fatal("bridge marker modified the original title")
	}
}

func TestRuntimePinnedBoundThreadStaysResidentUntilUnpinnedOrArchived(t *testing.T) {
	db, base, gateway, r := runtimeFixture(t)
	n := &pinNativeFixture{
		runtimeNative: base,
		sections:      []codexclient.ThreadSection{{ID: "pin", Name: "Pinned"}},
		archived:      map[string]bool{"archived": true},
	}
	r.Native, r.DiscoveryEnabled = n, false
	r.ScanInterval, r.ActivationSweepInterval = time.Hour, time.Hour
	r.PinScanInterval = 10 * time.Millisecond
	r.IdlePollInterval = 10 * time.Millisecond
	r.ActorIdleTimeout = 30 * time.Millisecond
	r.IdleHistoryInterval = time.Hour
	settledRuntimeBinding(t, db, base, r, time.Now().Add(-48*time.Hour).Unix())
	pinMeta(base, "thread", "pin", "Task")
	for _, id := range []string{"archived", "unbound"} {
		base.set(id, r.AllowedRoots[0], "completed")
		pinMeta(base, id, "pin", "Task")
	}
	runtimeBind(t, db, r, "archived", "archived-chat")
	if err := db.StageMirrors(context.Background(), r.HostID, "archived", nil, store.Watermark{ObservedAtMS: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	// An unchanged persisted pin set must still activate the current generation.
	if err := db.SavePinnedSnapshot(context.Background(), r.HostID, r.AppID, store.PinnedSnapshot{
		SectionID: "pin", ThreadIDs: []string{"thread", "archived", "unbound"}, ObservedAtMS: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	var hydrated, released, failures atomic.Int32
	r.HistoryHydrated = func(_ context.Context, id string, _ []codexclient.Turn) error {
		if id == "thread" {
			hydrated.Add(1)
		}
		return nil
	}
	r.ActorReleased = func(id string) {
		if id == "thread" {
			released.Add(1)
		}
	}
	r.Notice = func(code string) {
		if code == "pins.scan_pending" {
			failures.Add(1)
		}
	}
	// Residency does not depend on group renaming being enabled.
	runAdaptiveRuntime(t, r, base)
	runtimeWait(t, func() bool { return hydrated.Load() > 0 && r.ActorStarted("thread") })
	reads, scans := hydrated.Load(), n.sectionRead.Load()
	runtimeWait(t, func() bool { return n.sectionRead.Load() >= scans+24 })
	if !r.ActorStarted("thread") || released.Load() != 0 || hydrated.Load() != reads {
		t.Fatal("idle pin was evicted or unchanged pin scans repeatedly read history")
	}
	if r.ActorStarted("archived") || r.ActorStarted("unbound") {
		t.Fatal("pin polling activated an archived or unbound thread")
	}
	// A partial scan cannot drop the previously confirmed resident set.
	n.pinMu.Lock()
	n.archiveErr = errors.New("fixture incomplete archived page")
	n.pinMu.Unlock()
	pinMeta(base, "thread", "", "Task")
	runtimeWait(t, func() bool { return failures.Load() >= 12 })
	if !r.pinnedKeepAlive("thread") || !r.ActorStarted("thread") || released.Load() != 0 {
		t.Fatal("incomplete pin scan evicted a resident thread")
	}
	// Archiving changes residency even though the persisted union stays equal.
	pinMeta(base, "thread", "pin", "Task")
	n.pinMu.Lock()
	n.archived["thread"], n.archiveErr = true, nil
	n.pinMu.Unlock()
	runtimeWait(t, func() bool { return !r.pinnedKeepAlive("thread") && !r.ActorStarted("thread") })
	previous, err := db.PinnedSnapshot(context.Background(), r.HostID, r.AppID)
	if err != nil || !reflect.DeepEqual(previous.ThreadIDs, []string{"archived", "thread", "unbound"}) {
		t.Fatal("archiving changed the complete pin checkpoint", err)
	}
	n.pinMu.Lock()
	n.archived["thread"] = false
	n.pinMu.Unlock()
	runtimeWait(t, func() bool { return hydrated.Load() > reads && r.ActorStarted("thread") })
	pinMeta(base, "thread", "", "Task")
	runtimeWait(t, func() bool { return !r.pinnedKeepAlive("thread") && !r.ActorStarted("thread") })
	if released.Load() != 2 {
		t.Fatalf("archive and unpin did not release their actors: %d", released.Load())
	}
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	if gateway.groups != 0 {
		t.Fatal("pin polling created a group")
	}
}

func TestRuntimePinnedActivationFailureKeepsCentralBackoff(t *testing.T) {
	db, base, _, r := runtimeFixture(t)
	n := &pinNativeFixture{runtimeNative: base, sections: []codexclient.ThreadSection{{ID: "pin", Name: "Pinned"}}}
	r.Native, r.DiscoveryEnabled = n, false
	r.PinScanInterval = 10 * time.Millisecond
	r.ScanInterval, r.ActivationSweepInterval = time.Hour, time.Hour
	r.ActivationRetryInitial, r.ActivationRetryMax = time.Hour, time.Hour
	settledRuntimeBinding(t, db, base, r, time.Now().Add(-48*time.Hour).Unix())
	pinMeta(base, "thread", "pin", "Task")
	var attempts atomic.Int32
	r.CanAttach = func(context.Context, string) error {
		attempts.Add(1)
		return errors.New("fixture authorization unavailable")
	}
	runAdaptiveRuntime(t, r, base)
	runtimeWait(t, func() bool {
		active, retrying, _ := r.bindingLifecycle()
		return attempts.Load() > 0 && !active["thread"] && retrying["thread"]
	})
	scans := n.sectionRead.Load()
	runtimeWait(t, func() bool { return n.sectionRead.Load() >= scans+12 })
	if attempts.Load() != 1 || r.ActorStarted("thread") {
		t.Fatal("unchanged pin scans bypassed activation backoff")
	}
}

func TestRuntimePinAndUnpinOldBoundThreadWithoutActivityOrNativeEvent(t *testing.T) {
	db, base, gateway, r := runtimeFixture(t)
	n := &pinNativeFixture{runtimeNative: base, sections: []codexclient.ThreadSection{{ID: "pin", Name: "Pinned"}}}
	r.Native, r.DiscoveryEnabled = n, false
	r.PinScanInterval = 10 * time.Millisecond
	r.IdlePollInterval = 10 * time.Millisecond
	r.ActorIdleTimeout = 30 * time.Millisecond
	base.set("thread", r.AllowedRoots[0], "completed")
	pinMeta(base, "thread", "", "Task")
	runtimeBind(t, db, r, "thread", "chat")
	var namesMu sync.Mutex
	var names []string
	r.SyncGroupName = func(_ context.Context, binding store.Binding, name string) error {
		if binding.ThreadID != "thread" || binding.ChatID != "chat" {
			return store.ErrConflict
		}
		namesMu.Lock()
		names = append(names, name)
		namesMu.Unlock()
		return nil
	}
	var failures atomic.Int32
	r.Notice = func(code string) {
		if code == "pins.scan_pending" {
			failures.Add(1)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitName := func(want string) {
		t.Helper()
		runtimeWait(t, func() bool {
			namesMu.Lock()
			defer namesMu.Unlock()
			return len(names) > 0 && names[len(names)-1] == want
		})
	}
	waitName("🤖 Task")
	runtimeWait(t, func() bool { return !r.ActorStarted("thread") })
	pinMeta(base, "thread", "pin", "Task")
	// A pin in an unbound old thread must not start discovery or create a group.
	base.set("unbound", r.AllowedRoots[0], "completed")
	pinMeta(base, "unbound", "pin", "Unbound")
	waitName("📌 🤖 Task")
	if r.ActorStarted("unbound") {
		t.Fatal("pin polling activated an unbound thread")
	}
	n.pinMu.Lock()
	n.sections[0].Name = "Favorites"
	n.pinMu.Unlock()
	pinMeta(base, "thread", "pin", "Renamed task")
	runtimeNameEvent(base, "thread")
	waitName("📌 🤖 Renamed task")
	// A failed archived page must not publish the already-read active half.
	n.pinMu.Lock()
	n.archiveErr = errors.New("fixture incomplete archived page")
	n.pinMu.Unlock()
	pinMeta(base, "thread", "", "Renamed task")
	runtimeWait(t, func() bool { return failures.Load() >= 2 })
	previous, err := db.PinnedSnapshot(ctx, r.HostID, r.AppID)
	if err != nil || !reflect.DeepEqual(previous.ThreadIDs, []string{"thread", "unbound"}) {
		t.Fatal("incomplete scan replaced the last complete pin set", err)
	}
	namesMu.Lock()
	if names[len(names)-1] != "📌 🤖 Renamed task" {
		t.Error("a failed scan removed the marker")
	}
	namesMu.Unlock()
	n.pinMu.Lock()
	n.archiveErr = nil
	n.pinMu.Unlock()
	waitName("🤖 Renamed task")
	// Unchanged successful snapshots must not issue duplicate renames.
	reads := n.sectionRead.Load()
	runtimeWait(t, func() bool { return n.sectionRead.Load() >= reads+4 })
	namesMu.Lock()
	if len(names) != 4 {
		t.Errorf("unchanged pin/title state renamed repeatedly: %d", len(names))
	}
	namesMu.Unlock()
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	if gateway.groups != 0 {
		t.Fatal("pin changes created another group")
	}
}

func TestPinCatalogFailureDoesNotRenameUntilIdentityIsKnown(t *testing.T) {
	db, base, _, r := runtimeFixture(t)
	n := &pinNativeFixture{runtimeNative: base, sections: []codexclient.ThreadSection{{ID: "pin", Name: "Pinned"}}, catalogErr: errors.New("fixture unavailable")}
	r.Native, r.DiscoveryEnabled = n, false
	r.PinScanInterval = 10 * time.Millisecond
	base.set("thread", r.AllowedRoots[0], "completed")
	pinMeta(base, "thread", "pin", "Task")
	runtimeBind(t, db, r, "thread", "chat")
	var calls, failures atomic.Int32
	r.SyncGroupName = func(context.Context, store.Binding, string) error { calls.Add(1); return nil }
	r.Notice = func(code string) {
		if code == "pins.scan_pending" {
			failures.Add(1)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	runtimeWait(t, func() bool { return failures.Load() >= 2 })
	if calls.Load() != 0 {
		t.Fatal("unknown pin identity was treated as unpinned")
	}
	n.pinMu.Lock()
	n.catalogErr = nil
	n.pinMu.Unlock()
	runtimeWait(t, func() bool { return calls.Load() == 1 })
}
