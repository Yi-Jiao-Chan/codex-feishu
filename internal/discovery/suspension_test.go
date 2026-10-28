package discovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
)

func suspendedFixture(t *testing.T) (*Engine, *windowNative, string, string, store.Observation) {
	t.Helper()
	ctx := context.Background()
	e, base, root := engineFixture(t)
	if err := e.Store.CompleteBaseline(ctx, e.HostID, nil); err != nil {
		t.Fatal(err)
	}
	e.Now = func() time.Time { return time.UnixMilli(2000) }
	cwd := filepath.Join(root, "worktree")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	id := v7(1500)
	base.metadata[id], base.turns[id] = human(id, cwd), userTurn("input")
	base.active, base.loaded = []codexclient.Thread{base.metadata[id]}, []string{id}
	n := newWindowNative(base)
	n.recentActive = base.active
	e.Native = n
	if err := e.ObserveBirth(ctx, base.metadata[id]); err != nil {
		t.Fatal(err)
	}
	if err := e.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := e.Store.Observation(ctx, e.HostID, id)
	if err != nil || before.State != "eligible" {
		t.Fatalf("fixture not eligible: %+v %v", before, err)
	}
	if err := os.Remove(cwd); err != nil {
		t.Fatal(err)
	}
	n.resetCalls()
	if err := e.ScanSince(ctx, 1); err != nil {
		t.Fatal(err)
	}
	after, err := e.Store.Observation(ctx, e.HostID, id)
	if err != nil || !after.Suspended() || after.State != "pending" || after.Origin != before.Origin || after.Proof != before.Proof || after.CWD != before.CWD || after.CreatedAt != before.CreatedAt || after.FirstUserItemID != before.FirstUserItemID || after.ProjectRoot != before.ProjectRoot {
		t.Fatalf("suspension lost candidate evidence: before=%+v after=%+v err=%v", before, after, err)
	}
	if n.metadataCalls[id] != 1 || n.turnsCalls[id] != 0 {
		t.Fatal("missing cwd was not detected before history")
	}
	n.resetCalls()
	return e, n, id, cwd, before
}

func TestSuspendedCandidateSkipsAllPeriodicSourcesUntilNativeActivity(t *testing.T) {
	ctx := context.Background()
	e, n, id, cwd, before := suspendedFixture(t)
	for _, scan := range []func() error{func() error { return e.ScanSince(ctx, 1) }, func() error { return e.Scan(ctx) }} {
		if err := scan(); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := e.Store.PendingObservations(ctx, e.HostID)
	if err != nil || len(pending) != 0 || n.metadataCalls[id] != 0 || n.turnsCalls[id] != 0 {
		t.Fatalf("suspended listed/loaded candidate was retried: pending=%+v err=%v", pending, err)
	}
	if _, err := e.Store.PrepareGroup(ctx, e.HostID, id, "owner", "fixture", nil); !errors.Is(err, store.ErrIneligible) {
		t.Fatal("suspended candidate remained eligible", err)
	}
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	if err := e.Scan(ctx); err != nil || n.metadataCalls[id] != 0 {
		t.Fatal("periodic scan retried a restored candidate", err)
	}
	if err := e.CheckEvents(ctx, []string{id}, 1); err != nil {
		t.Fatal(err)
	}
	after, err := e.Store.Observation(ctx, e.HostID, id)
	if err != nil || after != before || n.metadataCalls[id] != 1 || n.turnsCalls[id] != 1 {
		t.Fatalf("native activity did not fully revalidate original evidence: %+v %v", after, err)
	}
}

func TestSuspendedCandidateExplicitCheckRequiresActiveProof(t *testing.T) {
	ctx := context.Background()
	e, n, id, cwd, _ := suspendedFixture(t)
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	if err := e.Check(ctx, id, 1000); err != nil {
		t.Fatal(err)
	}
	o, err := e.Store.Observation(ctx, e.HostID, id)
	if err != nil || o.Suspended() || o.State != "pending" || o.ExclusionReason != "active_unconfirmed" || n.metadataCalls[id] != 1 || n.turnsCalls[id] != 1 {
		t.Fatalf("explicit check bypassed active-list evidence: %+v %v", o, err)
	}
	if err := e.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	o, _ = e.Store.Observation(ctx, e.HostID, id)
	if o.State != "eligible" {
		t.Fatal("active proof could not restore eligibility")
	}
}

func TestRepeatedMissingActivityAndFailedListsPreserveSuspension(t *testing.T) {
	ctx := context.Background()
	e, n, id, _, _ := suspendedFixture(t)
	for range 2 {
		if err := e.CheckEvents(ctx, []string{id}, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Check(ctx, id, 1000); err != nil {
		t.Fatal(err)
	}
	o, _ := e.Store.Observation(ctx, e.HostID, id)
	if !o.Suspended() || n.metadataCalls[id] != 3 || n.turnsCalls[id] != 0 {
		t.Fatal("repeated missing events lost suspension or read history")
	}
	failing := &eventNative{windowNative: n, activeErr: errors.New("fixture unavailable")}
	e.Native = failing
	if err := e.CheckEvents(ctx, []string{id}, 1); err == nil {
		t.Fatal("failed list accepted")
	}
	o, _ = e.Store.Observation(ctx, e.HostID, id)
	if !o.Suspended() {
		t.Fatal("failed activity list released suspension")
	}
	e.Native = n
	n.resetCalls()
	if err := e.Scan(ctx); err != nil || n.metadataCalls[id] != 0 {
		t.Fatal("periodic retries resumed after missing activity", err)
	}
}

func TestSuspendedCandidatePreservesArchiveHoldUntilPositiveActiveEvidence(t *testing.T) {
	ctx := context.Background()
	e, n, id, cwd, _ := suspendedFixture(t)
	n.active, n.recentActive = nil, nil
	n.archived = []codexclient.Thread{n.metadata[id]}
	if err := e.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	o, _ := e.Store.Observation(ctx, e.HostID, id)
	if !o.Suspended() || !o.Archived() {
		t.Fatalf("archive evidence replaced missing cwd hold: %+v", o)
	}
	n.archived = nil
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	if err := e.Check(ctx, id, 1000); err != nil {
		t.Fatal(err)
	}
	if err := e.CheckEvents(ctx, []string{id}, 1); err != nil {
		t.Fatal(err)
	}
	o, _ = e.Store.Observation(ctx, e.HostID, id)
	if !o.Suspended() || !o.Archived() || n.metadataCalls[id] != 0 {
		t.Fatal("absent archive row or explicit check released archive hold")
	}
	n.recentActive = []codexclient.Thread{n.metadata[id]}
	if err := e.CheckEvents(ctx, []string{id}, 1); err != nil {
		t.Fatal(err)
	}
	o, _ = e.Store.Observation(ctx, e.HostID, id)
	if o.State != "eligible" || o.Suspended() || o.Archived() {
		t.Fatalf("positive active event could not restore candidate: %+v", o)
	}
}

func TestRestoredSuspendedCandidateStillHonorsMuteAndFirstInput(t *testing.T) {
	for _, policy := range []string{"muted", "draft", "excluded", "child", "outside_roots"} {
		t.Run(policy, func(t *testing.T) {
			ctx := context.Background()
			e, n, id, cwd, _ := suspendedFixture(t)
			if err := os.Mkdir(cwd, 0700); err != nil {
				t.Fatal(err)
			}
			want := "pending"
			switch policy {
			case "muted":
				want = "excluded"
				e.DirectoryRules = func(context.Context) ([]rules.DirectoryRule, error) { return []rules.DirectoryRule{{Path: cwd}}, nil }
			case "draft":
				n.turns[id] = nil
			case "excluded":
				want = "excluded"
				if err := e.Store.Exclude(ctx, e.HostID, id, "directory_muted"); err != nil {
					t.Fatal(err)
				}
			case "child":
				want = "excluded"
				metadata, parent := n.metadata[id], "parent"
				metadata.ParentThreadID = &parent
				n.metadata[id] = metadata
			case "outside_roots":
				want = "excluded"
				e.AllowedRoots = []string{t.TempDir()}
			}
			if err := e.CheckEvents(ctx, []string{id}, 1); err != nil {
				t.Fatal(err)
			}
			o, _ := e.Store.Observation(ctx, e.HostID, id)
			if o.State != want || o.Suspended() || o.Proof != "birth" {
				t.Fatalf("restoration bypassed policy or lost birth evidence: %+v", o)
			}
			if policy == "excluded" && n.metadataCalls[id] != 0 {
				t.Fatal("mute exclusion reawakened")
			}
		})
	}
}

func TestBaselineMissingCWDRemainsAmbiguousAfterRestoration(t *testing.T) {
	ctx := context.Background()
	e, base, root := engineFixture(t)
	id, cwd := v7(500), filepath.Join(root, "missing")
	base.active = []codexclient.Thread{human(id, cwd)}
	base.metadata[id], base.turns[id] = base.active[0], userTurn("old-input")
	n := newWindowNative(base)
	e.Native = n
	if err := e.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	o, _ := e.Store.Observation(ctx, e.HostID, id)
	if !o.Suspended() || o.Origin != "unknown" {
		t.Fatalf("missing baseline cwd not preserved ambiguously: %+v", o)
	}
	n.resetCalls()
	if err := e.Scan(ctx); err != nil || n.metadataCalls[id] != 0 {
		t.Fatal("baseline missing cwd was retried", err)
	}
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	n.recentActive = base.active
	if err := e.CheckEvents(ctx, []string{id}, 1); err != nil {
		t.Fatal(err)
	}
	o, _ = e.Store.Observation(ctx, e.HostID, id)
	if o.State != "pending" || o.Origin != "unknown" || o.Suspended() {
		t.Fatalf("restoration invented new-birth evidence: %+v", o)
	}
}

func TestConfiguredRootFailureKeepsCandidateRetryable(t *testing.T) {
	ctx := context.Background()
	e, base, cwd := engineFixture(t)
	if err := e.Store.CompleteBaseline(ctx, e.HostID, nil); err != nil {
		t.Fatal(err)
	}
	e.Now = func() time.Time { return time.UnixMilli(2000) }
	id := v7(1500)
	base.metadata[id], base.turns[id] = human(id, cwd), userTurn("input")
	base.active = []codexclient.Thread{base.metadata[id]}
	n := newWindowNative(base)
	e.Native = n
	e.AllowedRoots = []string{filepath.Join(cwd, "temporarily-unavailable-root")}
	for range 2 {
		var candidate *CandidateError
		if err := e.Scan(ctx); !errors.As(err, &candidate) || candidate.Stage != "cwd" {
			t.Fatalf("root failure not retryable: %v", err)
		}
	}
	o, err := e.Store.Observation(ctx, e.HostID, id)
	if err != nil || o.Suspended() || o.State != "pending" || n.metadataCalls[id] != 2 {
		t.Fatalf("root failure suspended an existing cwd: %+v %v", o, err)
	}
}
