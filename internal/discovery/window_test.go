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

type windowListCall struct {
	archived bool
	since    int64
}

type windowNative struct {
	*nativeFixture
	recentActive, recentArchived []codexclient.Thread
	sinceCalls                   []windowListCall
	fullCalls                    []bool
	metadataCalls                map[string]int
	turnsCalls                   map[string]int
}

func newWindowNative(base *nativeFixture) *windowNative {
	return &windowNative{
		nativeFixture: base,
		metadataCalls: map[string]int{},
		turnsCalls:    map[string]int{},
	}
}

func (n *windowNative) ListArchive(ctx context.Context, archived bool) ([]codexclient.Thread, error) {
	n.fullCalls = append(n.fullCalls, archived)
	return n.nativeFixture.ListArchive(ctx, archived)
}

func (n *windowNative) ListArchiveSince(_ context.Context, archived bool, since int64) ([]codexclient.Thread, error) {
	n.sinceCalls = append(n.sinceCalls, windowListCall{archived: archived, since: since})
	if archived {
		return n.recentArchived, nil
	}
	return n.recentActive, nil
}

func (n *windowNative) Metadata(ctx context.Context, id string) (codexclient.Thread, error) {
	n.metadataCalls[id]++
	return n.nativeFixture.Metadata(ctx, id)
}

func (n *windowNative) Turns(ctx context.Context, id string) ([]codexclient.Turn, error) {
	n.turnsCalls[id]++
	return n.nativeFixture.Turns(ctx, id)
}

func (n *windowNative) resetCalls() {
	n.sinceCalls = nil
	n.fullCalls = nil
	n.metadataCalls = map[string]int{}
	n.turnsCalls = map[string]int{}
}

func TestScanSinceUsesOneFixedWindowAndScanUsesFullLists(t *testing.T) {
	ctx := context.Background()
	e, base, _ := engineFixture(t)
	n := newWindowNative(base)
	e.Native = n
	if err := e.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	n.resetCalls()

	const since = int64(123)
	if err := e.ScanSince(ctx, since); err != nil {
		t.Fatal(err)
	}
	if want := []windowListCall{{archived: false, since: since}, {archived: true, since: since}}; !reflect.DeepEqual(n.sinceCalls, want) {
		t.Fatalf("window calls = %#v, want %#v", n.sinceCalls, want)
	}
	if len(n.fullCalls) != 0 {
		t.Fatalf("window scan made full calls: %#v", n.fullCalls)
	}

	n.resetCalls()
	if err := e.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(n.fullCalls, []bool{false, true}) {
		t.Fatalf("full scan calls = %#v", n.fullCalls)
	}
	if len(n.sinceCalls) != 0 {
		t.Fatalf("full scan used window calls: %#v", n.sinceCalls)
	}
}

func TestScanSinceChecksLoadedAndPendingOutsideWindowAndHoldsArchived(t *testing.T) {
	ctx := context.Background()
	e, base, cwd := engineFixture(t)
	n := newWindowNative(base)
	e.Native = n
	if err := e.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	n.resetCalls()
	e.Now = func() time.Time { return time.UnixMilli(2000) }

	loadedID := "loaded-old"
	pendingID := "pending-old"
	archivedID := "archived-old"
	bornID := v7(1500)
	for _, id := range []string{loadedID, pendingID, bornID} {
		base.metadata[id] = human(id, cwd)
		base.turns[id] = userTurn("input-" + id)
	}
	base.loaded = []string{loadedID}
	n.recentActive = []codexclient.Thread{human(bornID, cwd)}
	n.recentArchived = []codexclient.Thread{human(archivedID, cwd)}
	if err := e.Store.ObserveCandidate(ctx, "host", pendingID, cwd, 0, "ambiguous"); err != nil {
		t.Fatal(err)
	}

	const since = int64(456)
	if err := e.ScanSince(ctx, since); err != nil {
		t.Fatal(err)
	}
	if want := []windowListCall{{archived: false, since: since}, {archived: true, since: since}}; !reflect.DeepEqual(n.sinceCalls, want) {
		t.Fatalf("window calls = %#v, want %#v", n.sinceCalls, want)
	}
	for _, id := range []string{loadedID, pendingID, bornID} {
		if n.metadataCalls[id] == 0 || n.turnsCalls[id] == 0 {
			t.Fatalf("candidate %q was not explicitly checked: metadata=%d turns=%d", id, n.metadataCalls[id], n.turnsCalls[id])
		}
	}
	if n.metadataCalls[archivedID] != 0 || n.turnsCalls[archivedID] != 0 {
		t.Fatalf("archived candidate was read through history: metadata=%d turns=%d", n.metadataCalls[archivedID], n.turnsCalls[archivedID])
	}

	for _, id := range []string{loadedID, pendingID} {
		o, err := e.Store.Observation(ctx, "host", id)
		if err != nil || o.Origin != "unknown" || o.State != "pending" {
			t.Fatalf("old candidate %q changed state: %+v err=%v", id, o, err)
		}
	}
	o, err := e.Store.Observation(ctx, "host", archivedID)
	if err != nil || o.State != "pending" || o.ExclusionReason != "archived" {
		t.Fatalf("archived candidate was not held: %+v err=%v", o, err)
	}
	o, err = e.Store.Observation(ctx, "host", bornID)
	if err != nil || o.Origin != "new" || o.Proof != "v7" || o.State != "eligible" {
		t.Fatalf("new birth lost fence proof or eligibility: %+v err=%v", o, err)
	}
	fence, complete, err := e.Store.BaselineFence(ctx, "host")
	if err != nil || fence != 1000 || !complete {
		t.Fatalf("baseline fence changed during window scan: fence=%d complete=%v err=%v", fence, complete, err)
	}
}

func TestArchivedPendingCandidateRequiresPositiveActiveEvidenceToReactivate(t *testing.T) {
	ctx := context.Background()
	e, base, cwd := engineFixture(t)
	n := newWindowNative(base)
	e.Native = n
	if err := e.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	e.Now = func() time.Time { return time.UnixMilli(2000) }
	fence, complete, err := e.Store.BaselineFence(ctx, "host")
	if err != nil || !complete || fence != 1000 {
		t.Fatalf("baseline fence = %d complete=%v err=%v", fence, complete, err)
	}

	id := v7(1500)
	base.metadata[id] = human(id, cwd)
	base.turns[id] = userTurn("archived-input")
	n.recentActive = nil
	n.recentArchived = []codexclient.Thread{human(id, cwd)}
	if err := e.ScanSince(ctx, 100); err != nil {
		t.Fatal(err)
	}
	assertHeld := func(threadID string) {
		t.Helper()
		o, err := e.Store.Observation(ctx, "host", threadID)
		if err != nil || o.State != "pending" || o.ExclusionReason != "archived" {
			t.Fatalf("archived candidate %q was not held: %+v err=%v", threadID, o, err)
		}
	}
	assertHeld(id)

	// The archived thread remains in persisted pending observations with a
	// readable input, but it ages out of both recent listings.
	n.recentActive = nil
	n.recentArchived = nil
	if err := e.ScanSince(ctx, 101); err != nil {
		t.Fatal(err)
	}
	assertHeld(id)
	if err := e.Check(ctx, id, fence); err != nil {
		t.Fatal(err)
	}
	assertHeld(id)

	// A positive active listing is the evidence that the archived hold may be
	// rechecked and activated.
	n.recentActive = []codexclient.Thread{human(id, cwd)}
	if err := e.ScanSince(ctx, 102); err != nil {
		t.Fatal(err)
	}
	o, err := e.Store.Observation(ctx, "host", id)
	if err != nil || o.Origin != "new" || o.Proof != "v7" || o.State != "eligible" || o.ExclusionReason != "" {
		t.Fatalf("active evidence did not reactivate candidate: %+v err=%v", o, err)
	}
	fenceAfter, completeAfter, err := e.Store.BaselineFence(ctx, "host")
	if err != nil || fenceAfter != fence || !completeAfter {
		t.Fatalf("active recheck changed baseline fence: %d complete=%v err=%v", fenceAfter, completeAfter, err)
	}

	// Full compensation also must retain an archived hold when that candidate
	// disappears from the full archived listing.
	secondID := v7(1600)
	base.metadata[secondID] = human(secondID, cwd)
	base.turns[secondID] = userTurn("second-archived-input")
	base.active = nil
	base.archived = []codexclient.Thread{human(secondID, cwd)}
	if err := e.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	assertHeld(secondID)
	base.archived = nil
	if err := e.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	assertHeld(secondID)
}

func TestWindowMissingActiveEvidencePreventsPendingAndEligibleActivation(t *testing.T) {
	for _, historyUnavailable := range []bool{true, false} {
		name := "eligible"
		if historyUnavailable {
			name = "pending"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			e, base, cwd := engineFixture(t)
			n := newWindowNative(base)
			e.Native = n
			if err := e.Snapshot(ctx); err != nil {
				t.Fatal(err)
			}
			e.Now = func() time.Time { return time.UnixMilli(2000) }
			id := v7(1500)
			base.metadata[id] = human(id, cwd)
			base.turns[id] = userTurn("pending-input")
			n.recentActive = []codexclient.Thread{base.metadata[id]}
			if historyUnavailable {
				base.errors[id] = errors.New("history temporarily unavailable")
			}
			err := e.ScanSince(ctx, 1)
			var unavailable *CandidateError
			if historyUnavailable && !errors.As(err, &unavailable) || !historyUnavailable && err != nil {
				t.Fatalf("initial scan: %v", err)
			}

			// First archive need not update the activity timestamp. Both recent
			// lists can omit it even though it was never held as archived.
			e.Now = func() time.Time { return time.UnixMilli(2000).Add(26 * time.Hour) }
			delete(base.errors, id)
			base.archived = []codexclient.Thread{base.metadata[id]}
			n.recentActive = nil
			n.recentArchived = nil
			assertPending := func() {
				t.Helper()
				o, err := e.Store.Observation(ctx, "host", id)
				if err != nil || o.Origin != "new" || o.State != "pending" || o.ExclusionReason != "active_unconfirmed" {
					t.Fatalf("unconfirmed candidate activated: %+v err=%v", o, err)
				}
				if _, err := e.Store.PrepareGroup(ctx, "host", id, "owner", "name", nil); !errors.Is(err, store.ErrIneligible) {
					t.Fatalf("unconfirmed candidate prepared group: %v", err)
				}
			}
			// An event must be safe even before the next window scan persists
			// an unconfirmed-active hold.
			if err := e.Check(ctx, id, 1000); err != nil {
				t.Fatal(err)
			}
			assertPending()
			if err := e.ScanSince(ctx, e.Now().Add(-24*time.Hour).Unix()); err != nil {
				t.Fatal(err)
			}
			assertPending()
			if err := e.Check(ctx, id, 1000); err != nil {
				t.Fatal(err)
			}
			assertPending()

			// Positive active evidence permits retry with the original birth
			// proof; no new baseline or terminal exclusion is introduced.
			n.recentActive = []codexclient.Thread{base.metadata[id]}
			if err := e.ScanSince(ctx, e.Now().Add(-24*time.Hour).Unix()); err != nil {
				t.Fatal(err)
			}
			o, err := e.Store.Observation(ctx, "host", id)
			if err != nil || o.Origin != "new" || o.State != "eligible" || o.ExclusionReason != "" {
				t.Fatalf("active evidence did not reactivate candidate: %+v err=%v", o, err)
			}
		})
	}
}
