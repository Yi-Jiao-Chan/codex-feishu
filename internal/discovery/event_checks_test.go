package discovery

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

type eventNative struct {
	*windowNative
	activeErr, archiveErr, metadataErr error
	beforeList                         func(bool)
}

func (n *eventNative) ListArchiveSince(ctx context.Context, archived bool, since int64) ([]codexclient.Thread, error) {
	if n.beforeList != nil {
		n.beforeList(archived)
	}
	rows, err := n.windowNative.ListArchiveSince(ctx, archived, since)
	if archived && n.archiveErr != nil {
		return nil, n.archiveErr
	}
	if !archived && n.activeErr != nil {
		return nil, n.activeErr
	}
	return rows, err
}

func (n *eventNative) Metadata(ctx context.Context, id string) (codexclient.Thread, error) {
	if n.metadataErr != nil {
		return codexclient.Thread{}, n.metadataErr
	}
	return n.windowNative.Metadata(ctx, id)
}

func TestCheckEventsSharesListsAndOnlyChecksRequestedCandidates(t *testing.T) {
	ctx := context.Background()
	e, base, cwd := engineFixture(t)
	if err := e.Store.CompleteBaseline(ctx, e.HostID, nil); err != nil {
		t.Fatal(err)
	}
	e.Now = func() time.Time { return time.UnixMilli(2000) }
	n := newWindowNative(base)
	e.Native = n
	ids := []string{v7(1500), v7(1600)}
	for _, id := range append(append([]string(nil), ids...), v7(1700)) {
		base.metadata[id], base.turns[id] = human(id, cwd), userTurn("user-"+id)
		n.recentActive = append(n.recentActive, base.metadata[id])
	}
	checkpoint := store.DiscoveryCheckpoint{LastScanAtMS: 1100, LastFullAtMS: 1100}
	if err := e.Store.SaveDiscoveryCheckpoint(ctx, e.HostID, e.AppID, checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := e.CheckEvents(ctx, []string{ids[0], ids[1], ids[0], ""}, 1); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(n.sinceCalls, []windowListCall{{archived: false, since: 1}, {archived: true, since: 1}}) || len(n.fullCalls) != 0 {
		t.Fatal("event batch did not share one fixed-window list pair")
	}
	for _, id := range ids {
		o, err := e.Store.Observation(ctx, e.HostID, id)
		if err != nil || o.State != "eligible" || o.Proof != "v7" || n.metadataCalls[id] != 1 || n.turnsCalls[id] != 1 {
			t.Fatalf("event candidate not checked once: %+v err=%v", o, err)
		}
	}
	if n.metadataCalls[v7(1700)] != 0 || n.turnsCalls[v7(1700)] != 0 {
		t.Fatal("event batch checked an unrelated active thread")
	}
	after, err := e.Store.DiscoveryCheckpoint(ctx, e.HostID, e.AppID)
	if err != nil || after != checkpoint {
		t.Fatal("partial event check advanced full scan coverage", err)
	}
}

func TestCheckEventsRequiresCompleteActiveEvidenceAndValidHistory(t *testing.T) {
	for _, tc := range []struct {
		name, reason                                                            string
		active, archived, oldArchive                                            bool
		draft, failActive, failArchive, failMetadata, failHistory, cancelLookup bool
	}{
		{name: "absent", reason: "active_unconfirmed"},
		{name: "archived", archived: true, reason: "archived"},
		{name: "both lists", active: true, archived: true, reason: "archived"},
		{name: "old archived hold", oldArchive: true, reason: "archived"},
		{name: "active list failed", active: true, failActive: true, reason: "active_unconfirmed"},
		{name: "archive list failed", active: true, failArchive: true, reason: "active_unconfirmed"},
		{name: "failed lookup preserves archive", oldArchive: true, failActive: true, reason: "archived"},
		{name: "metadata failed", active: true, failMetadata: true, reason: "metadata_unavailable"},
		{name: "history failed", active: true, failHistory: true, reason: "history_unavailable"},
		{name: "empty draft", active: true, draft: true},
		{name: "lookup canceled", active: true, failActive: true, cancelLookup: true, reason: "active_unconfirmed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e, base, cwd := engineFixture(t)
			if err := e.Store.CompleteBaseline(ctx, e.HostID, nil); err != nil {
				t.Fatal(err)
			}
			e.Now = func() time.Time { return time.UnixMilli(2000) }
			id := v7(1500)
			base.metadata[id], base.turns[id] = human(id, cwd), userTurn("input")
			if err := e.ObserveBirth(ctx, base.metadata[id]); err != nil {
				t.Fatal(err)
			}
			if !tc.draft {
				if err := e.Store.MarkEligible(ctx, e.HostID, id, "input", cwd, cwd); err != nil {
					t.Fatal(err)
				}
			}
			if tc.oldArchive {
				if err := e.Store.HoldCandidate(ctx, e.HostID, id, "archived"); err != nil {
					t.Fatal(err)
				}
			}
			n := &eventNative{windowNative: newWindowNative(base)}
			e.Native = n
			if tc.active {
				n.recentActive = []codexclient.Thread{base.metadata[id]}
			}
			if tc.archived {
				n.recentArchived = []codexclient.Thread{base.metadata[id]}
			}
			failure := errors.New("fixture unavailable")
			if tc.failActive {
				n.activeErr = failure
			}
			if tc.failArchive {
				n.archiveErr = failure
			}
			if tc.failMetadata {
				n.metadataErr = failure
			}
			if tc.failHistory {
				base.errors[id] = failure
			}
			if tc.draft {
				base.turns[id] = nil
			}
			if tc.cancelLookup {
				n.beforeList = func(bool) { cancel() }
			}
			err := e.CheckEvents(ctx, []string{id}, 1)
			wantErr := tc.failActive || tc.failArchive || tc.failMetadata || tc.failHistory
			if (err != nil) != wantErr {
				t.Fatalf("check error=%v want error=%v", err, wantErr)
			}
			o, err := e.Store.Observation(context.Background(), e.HostID, id)
			if err != nil || o.State != "pending" || o.ExclusionReason != tc.reason {
				t.Fatalf("unverified event retained eligibility: %+v err=%v", o, err)
			}
			if _, err := e.Store.PrepareGroup(context.Background(), e.HostID, id, "owner", "task", nil); !errors.Is(err, store.ErrIneligible) {
				t.Fatalf("unverified candidate prepared a group: %v", err)
			}
		})
	}
}

func TestCheckEventsKeepsEligibleDuringSuccessfulLookupAndReleasesArchivedHold(t *testing.T) {
	ctx := context.Background()
	e, base, cwd := engineFixture(t)
	if err := e.Store.CompleteBaseline(ctx, e.HostID, nil); err != nil {
		t.Fatal(err)
	}
	e.Now = func() time.Time { return time.UnixMilli(2000) }
	id := v7(1500)
	base.metadata[id], base.turns[id] = human(id, cwd), userTurn("input")
	if err := e.ObserveBirth(ctx, base.metadata[id]); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.MarkEligible(ctx, e.HostID, id, "input", cwd, cwd); err != nil {
		t.Fatal(err)
	}
	n := &eventNative{windowNative: newWindowNative(base)}
	n.recentActive = []codexclient.Thread{base.metadata[id]}
	n.beforeList = func(bool) {
		if _, err := e.Store.PrepareGroup(ctx, e.HostID, id, "owner", "task", nil); err != nil {
			t.Fatal("successful event lookup temporarily revoked eligibility", err)
		}
	}
	e.Native = n
	if err := e.CheckEvents(ctx, []string{id}, 1); err != nil {
		t.Fatal(err)
	}
	n.beforeList = nil
	if err := e.Store.HoldCandidate(ctx, e.HostID, id, "archived"); err != nil {
		t.Fatal(err)
	}
	if err := e.CheckEvents(ctx, []string{id}, 1); err != nil {
		t.Fatal(err)
	}
	o, err := e.Store.Observation(ctx, e.HostID, id)
	if err != nil || o.State != "eligible" || o.ExclusionReason != "" {
		t.Fatal("positive active evidence did not release archived hold", err)
	}
}

func TestCheckEventsPreservesBaselineAndExclusions(t *testing.T) {
	ctx := context.Background()
	e, base, cwd := engineFixture(t)
	n := newWindowNative(base)
	e.Native = n
	if err := e.CheckEvents(ctx, []string{"missing"}, 1); err == nil || len(n.sinceCalls) != 0 {
		t.Fatal("event check bypassed an incomplete baseline")
	}
	if err := e.Store.CompleteBaseline(ctx, e.HostID, []string{"old"}); err != nil {
		t.Fatal(err)
	}
	id := v7(1500)
	if err := e.Store.ObserveNew(ctx, e.HostID, id, cwd, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.Exclude(ctx, e.HostID, id, "directory_muted"); err != nil {
		t.Fatal(err)
	}
	if err := e.CheckEvents(ctx, []string{"old", id}, 1); err != nil || len(n.sinceCalls) != 0 {
		t.Fatal("terminal candidates caused an event list lookup", err)
	}
	e.Now = func() time.Time { return time.UnixMilli(2000) }
	legacy := v7(500)
	base.metadata[legacy], base.turns[legacy] = human(legacy, cwd), userTurn("legacy-input")
	n.recentActive = []codexclient.Thread{base.metadata[legacy]}
	if err := e.CheckEvents(ctx, []string{legacy}, 1); err != nil {
		t.Fatal(err)
	}
	o, err := e.Store.Observation(ctx, e.HostID, legacy)
	if err != nil || o.Origin != "unknown" || o.State != "pending" {
		t.Fatal("ordinary event became new-birth proof", err)
	}
}
