package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
)

type nativeFixture struct {
	active, archived []codexclient.Thread
	loaded           []string
	metadata         map[string]codexclient.Thread
	turns            map[string][]codexclient.Turn
	errors           map[string]error
	onList           func(bool)
	listError        error
}

func (n *nativeFixture) ListArchive(_ context.Context, archived bool) ([]codexclient.Thread, error) {
	if n.onList != nil {
		n.onList(archived)
	}
	if archived {
		return n.archived, n.listError
	}
	return n.active, n.listError
}
func (n *nativeFixture) LoadedIDs(context.Context) ([]string, error) { return n.loaded, nil }
func (n *nativeFixture) Metadata(_ context.Context, id string) (codexclient.Thread, error) {
	return n.metadata[id], nil
}
func (n *nativeFixture) Turns(_ context.Context, id string) ([]codexclient.Turn, error) {
	return n.turns[id], n.errors[id]
}
func v7(ms int64) string { return fmt.Sprintf("%08x-%04x-7000-8000-000000000000", ms>>16, ms&0xffff) }
func human(id, cwd string) codexclient.Thread {
	return codexclient.Thread{ID: id, CWD: cwd, Source: json.RawMessage(`"vscode"`)}
}
func userTurn(id string) []codexclient.Turn {
	started := int64(0)
	return []codexclient.Turn{{ID: "turn", StartedAt: &started, Items: []codexclient.Item{{Type: "userMessage", ID: id}}}}
}
func engineFixture(t *testing.T) (*Engine, *nativeFixture, string) {
	t.Helper()
	cwd := t.TempDir()
	s, err := store.Open(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	n := &nativeFixture{metadata: map[string]codexclient.Thread{}, turns: map[string][]codexclient.Turn{}, errors: map[string]error{}}
	e := &Engine{Native: n, Store: s, HostID: "host", AppID: "app", AllowedRoots: []string{cwd}, Now: func() time.Time { return time.UnixMilli(1000) }}
	if _, err = e.Begin(context.Background()); err != nil {
		t.Fatal(err)
	}
	return e, n, cwd
}

func TestBaselineKeepsLiveBirthAndBlankDraftButIgnoresOldResume(t *testing.T) {
	ctx := context.Background()
	e, n, cwd := engineFixture(t)
	old, draft, born := v7(500), v7(600), v7(1500)
	n.active = []codexclient.Thread{human(old, cwd), human(born, cwd)}
	n.loaded = []string{draft}
	for _, id := range []string{old, draft, born} {
		n.metadata[id] = human(id, cwd)
	}
	n.turns[old] = userTurn("old-input")
	n.turns[born] = userTurn("new-input")
	n.errors[draft] = &codexclient.RPCError{Code: -32600, Message: "thread " + draft + " is not materialized yet; thread/turns/list is unavailable before first user message"}
	n.onList = func(archived bool) {
		if !archived {
			if err := e.ObserveBirth(ctx, n.metadata[born]); err != nil {
				t.Fatal(err)
			}
		}
	}
	e.Now = func() time.Time { return time.UnixMilli(2000) }
	if err := e.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{old: "ignored", draft: "pending", born: "pending"} {
		o, err := e.Store.Observation(ctx, "host", id)
		if err != nil || o.State != want {
			t.Fatalf("baseline %s: %+v %v", id, o, err)
		}
	}
	n.onList = nil
	n.turns[old] = append(n.turns[old], userTurn("continued-old-input")...)
	delete(n.errors, draft)
	n.turns[draft] = userTurn("draft-first-input")
	if err := e.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{old: "ignored", draft: "eligible", born: "eligible"} {
		o, err := e.Store.Observation(ctx, "host", id)
		if err != nil || o.State != want {
			t.Fatalf("activation %s: %+v %v", id, o, err)
		}
	}
}

func TestLostBirthOfflineCompensationAndExcludedChildren(t *testing.T) {
	ctx := context.Background()
	e, n, cwd := engineFixture(t)
	if err := e.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	e.Now = func() time.Time { return time.UnixMilli(2000) }
	newID, oldLate, child, future := v7(1500), v7(500), v7(1600), v7(3000)
	for _, id := range []string{newID, oldLate, child, future} {
		n.metadata[id] = human(id, cwd)
		n.turns[id] = userTurn("input-" + id)
		n.active = append(n.active, n.metadata[id])
	}
	parent := newID
	childMetadata := n.metadata[child]
	childMetadata.ParentThreadID = &parent
	n.metadata[child] = childMetadata
	if err := e.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{newID: "eligible", oldLate: "pending", child: "excluded", future: "pending"} {
		o, err := e.Store.Observation(ctx, "host", id)
		if err != nil || o.State != want {
			t.Fatalf("compensation %s: %+v %v", id, o, err)
		}
	}
	// Recovery must reuse the original fence, not move it past offline births.
	if needed, err := e.Begin(ctx); err != nil || needed {
		t.Fatalf("baseline reset: %v %v", needed, err)
	}
	fence, _, err := e.Store.BaselineFence(ctx, "host")
	if err != nil || fence != 1000 {
		t.Fatalf("fence moved: %d %v", fence, err)
	}
}

func TestMutedCandidateNeverReactivatesAfterUnmute(t *testing.T) {
	ctx := context.Background()
	e, n, cwd := engineFixture(t)
	if err := e.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	id := v7(1500)
	n.metadata[id] = human(id, cwd)
	n.active = []codexclient.Thread{n.metadata[id]}
	n.turns[id] = userTurn("input")
	e.Now = func() time.Time { return time.UnixMilli(2000) }
	e.DirectoryRules = func(context.Context) ([]rules.DirectoryRule, error) { return []rules.DirectoryRule{{Path: cwd}}, nil }
	if err := e.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	e.DirectoryRules = nil
	if err := e.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	o, err := e.Store.Observation(ctx, "host", id)
	if err != nil || o.State != "excluded" || o.ExclusionReason != "directory_muted" {
		t.Fatalf("muted history reactivated: %+v %v", o, err)
	}
}

func TestFailedSnapshotDoesNotCompleteBaseline(t *testing.T) {
	ctx := context.Background()
	e, n, _ := engineFixture(t)
	n.listError = errors.New("fixture list unavailable")
	if err := e.Snapshot(ctx); err == nil {
		t.Fatal("failed list became empty baseline")
	}
	_, complete, err := e.Store.BaselineFence(ctx, "host")
	if err != nil || complete {
		t.Fatalf("failed baseline completed: %v %v", complete, err)
	}
}

func TestBirthTimeRequiresVersionVariantAndStrictFence(t *testing.T) {
	for _, tc := range []struct {
		id   string
		now  int64
		want bool
	}{
		{v7(1001), 2000, true}, {v7(1000), 2000, false}, {v7(999), 2000, false}, {v7(3000), 2000, false}, {v7(1001), 500, false},
		{"00000000-03e9-4000-8000-000000000000", 2000, false}, {"00000000-03e9-7000-0000-000000000000", 2000, false}, {"invalid", 2000, false},
	} {
		if got := bornAfterFence(tc.id, 1000, tc.now); got != tc.want {
			t.Fatalf("%s at %d: %v", tc.id, tc.now, got)
		}
	}
}

func TestForkIsDeferredEvenWhenThinListOmitsLineage(t *testing.T) {
	ctx := context.Background()
	e, n, cwd := engineFixture(t)
	if err := e.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	e.Now = func() time.Time { return time.UnixMilli(2000) }
	id, parent := v7(1500), v7(500)
	fork := human(id, cwd)
	fork.ForkedFromID = &parent
	// Thin list loses lineage; metadata read must hydrate it.
	n.active = []codexclient.Thread{human(id, cwd)}
	n.metadata[id] = fork
	n.turns[id] = []codexclient.Turn{{ID: v7(600), Items: []codexclient.Item{{Type: "userMessage", ID: "item-1"}}}}
	if err := e.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	o, err := e.Store.Observation(ctx, "host", id)
	if err != nil || o.State != "excluded" || o.ExclusionReason != "fork_deferred" {
		t.Fatalf("fork was not deferred: %+v %v", o, err)
	}
	n.turns[id] = append(n.turns[id], codexclient.Turn{ID: v7(1600), Items: []codexclient.Item{{Type: "userMessage", ID: "item-1"}}})
	if err := e.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	o, err = e.Store.Observation(ctx, "host", id)
	if err != nil || o.State != "excluded" || o.ExclusionReason != "fork_deferred" {
		t.Fatalf("later input activated a deferred fork: %+v %v", o, err)
	}
}

func TestOneUnreadableCandidateDoesNotBlockOthers(t *testing.T) {
	ctx := context.Background()
	e, n, cwd := engineFixture(t)
	if err := e.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	e.Now = func() time.Time { return time.UnixMilli(2000) }
	blocked, ready := v7(1500), v7(1600)
	for _, id := range []string{blocked, ready} {
		n.metadata[id] = human(id, cwd)
		n.active = append(n.active, n.metadata[id])
		n.turns[id] = userTurn("input")
	}
	n.errors[blocked] = &codexclient.RPCError{Code: -32600, Message: "unavailable fixture"}
	var unavailable *CandidateError
	if err := e.Scan(ctx); !errors.As(err, &unavailable) {
		t.Fatalf("missing isolated problem: %v", err)
	}
	o, err := e.Store.Observation(ctx, "host", ready)
	if err != nil || o.State != "eligible" {
		t.Fatalf("unrelated task blocked: %+v %v", o, err)
	}
	o, err = e.Store.Observation(ctx, "host", blocked)
	if err != nil || o.State != "pending" || o.ExclusionReason != "history_unavailable" {
		t.Fatalf("failed history treated as blank: %+v %v", o, err)
	}
}

func TestFirstInputRacingBlankProofRemainsAmbiguous(t *testing.T) {
	ctx := context.Background()
	e, n, cwd := engineFixture(t)
	oldDraft := v7(500)
	n.active = []codexclient.Thread{human(oldDraft, cwd)}
	n.metadata[oldDraft] = human(oldDraft, cwd)
	started := int64(1)
	n.turns[oldDraft] = []codexclient.Turn{{ID: "turn", StartedAt: &started, Items: []codexclient.Item{{Type: "userMessage", ID: "racing-input"}}}}
	e.Now = func() time.Time { return time.UnixMilli(2000) }
	if err := e.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	o, err := e.Store.Observation(ctx, "host", oldDraft)
	if err != nil || o.Origin != "unknown" || o.State != "pending" || o.ExclusionReason != "first_input_boundary_unknown" {
		t.Fatalf("boundary was silently ignored/activated: %+v %v", o, err)
	}
	if err := e.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	o, _ = e.Store.Observation(ctx, "host", oldDraft)
	if o.State != "pending" {
		t.Fatal("boundary ambiguity became eligible")
	}
}

func TestArchivedCandidateCannotStayEligibleForCreation(t *testing.T) {
	ctx := context.Background()
	e, n, cwd := engineFixture(t)
	if err := e.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	e.Now = func() time.Time { return time.UnixMilli(2000) }
	id := v7(1500)
	n.metadata[id] = human(id, cwd)
	n.active = []codexclient.Thread{n.metadata[id]}
	n.turns[id] = userTurn("input")
	if err := e.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	n.active = nil
	n.archived = []codexclient.Thread{n.metadata[id]}
	if err := e.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	o, err := e.Store.Observation(ctx, "host", id)
	if err != nil || o.State != "pending" || o.ExclusionReason != "archived" {
		t.Fatalf("archived task stayed eligible: %+v %v", o, err)
	}
	if _, err = e.Store.PrepareGroup(ctx, "host", id, "owner", "name", nil); !errors.Is(err, store.ErrIneligible) {
		t.Fatal("archived group prepared")
	}
	n.archived = nil
	n.active = []codexclient.Thread{n.metadata[id]}
	if err := e.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	o, _ = e.Store.Observation(ctx, "host", id)
	if o.State != "eligible" {
		t.Fatal("new candidate could not reactivate after unarchive")
	}
}
