package creation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-feishu/internal/store"
)

func restoreGitFixture(t *testing.T, legacy bool) (WorktreeRestoreInput, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(base, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	initWorktreeTestRepo(t, source)
	home := filepath.Join(base, "codex")
	managed := filepath.Join(home, "worktrees")
	if err := os.MkdirAll(managed, 0700); err != nil {
		t.Fatal(err)
	}
	bucket := "codex-feishu-" + strings.Repeat("a", 32)
	if legacy {
		bucket = "abcd"
	}
	root := filepath.Join(managed, bucket, "project")
	worktreeTestGit(t, source, "worktree", "add", "--detach", "--", root, "HEAD")
	if err := os.Chmod(filepath.Dir(root), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(root), ".codex-worktree-name"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "code.txt"), []byte("synthetic dirty snapshot\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "captured-untracked.txt"), []byte("synthetic captured file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	worktreeTestGit(t, root, "add", "sub/code.txt", "captured-untracked.txt")
	tree := worktreeTestGit(t, root, "write-tree")
	snapshot := worktreeTestGit(t, root, "commit-tree", tree, "-p", "HEAD", "-m", "synthetic snapshot")
	worktreeTestGit(t, source, "update-ref", WorktreeSnapshotRef(root), snapshot)
	common := worktreeTestGit(t, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	in := WorktreeRestoreInput{RestoreID: strings.Repeat("b", 32), CodexHome: home,
		Identity: store.ThreadWorktreeIdentity{HostID: "host", AppID: "app", ThreadID: "synthetic-original-thread",
			Root: root, RawCWD: filepath.Join(root, "sub"), SourceRoot: source, CommonDir: common, RelativeCWD: "sub"}}
	worktreeTestGit(t, source, "worktree", "remove", "--force", "--", root)
	return in, snapshot
}

func restoreCheckpointRecorder(target *WorktreeRestoreCheckpoint) WorktreeRestorePersist {
	return func(_ context.Context, c WorktreeRestoreCheckpoint) error { *target = c; return nil }
}

func TestWorktreeRestoreLegacyPublicationClassification(t *testing.T) {
	for _, tc := range []struct {
		name, bucket, child string
		delayed             bool
	}{
		{"lowercase bucket", "abcd", "project", false},
		{"uppercase bucket", "ABCD", "project", false},
		{"mixed bucket", "aBcD", "project", false},
		{"uppercase timestamp", "named", "261023-1234-ABCDEF09", false},
		{"mixed timestamp", "named", "261023-1234-aBcDeF09", false},
		{"named bucket", "named", "project", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, snapshot := restoreGitFixture(t, false)
			in.Identity.Root = filepath.Join(in.CodexHome, "worktrees", tc.bucket, tc.child)
			in.Identity.RawCWD = filepath.Join(in.Identity.Root, in.Identity.RelativeCWD)
			if err := os.Mkdir(filepath.Dir(in.Identity.Root), 0700); err != nil {
				t.Fatal(err)
			}
			worktreeTestGit(t, in.Identity.SourceRoot, "update-ref", WorktreeSnapshotRef(in.Identity.Root), snapshot)
			plan, err := PlanWorktreeRestore(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if plan.DelayedPublication != tc.delayed {
				t.Fatalf("DelayedPublication = %v, want %v", plan.DelayedPublication, tc.delayed)
			}
		})
	}
}

func TestWorktreeRestoreSnapshotSubdirectoryOwnerAndPublication(t *testing.T) {
	in, snapshot := restoreGitFixture(t, false)
	p, err := PlanWorktreeRestore(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	var durable WorktreeRestoreCheckpoint
	result, err := AdvanceWorktreeRestore(context.Background(), p, WorktreeRestoreCheckpoint{}, restoreCheckpointRecorder(&durable))
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != "restored" || result.Generation == "" || result.Generation != result.StageIdentity || !result.PublicationPending {
		t.Fatal("restore did not confirm the physical generation")
	}
	if got := worktreeTestGit(t, in.Identity.Root, "rev-parse", "HEAD"); got != snapshot {
		t.Fatal("restore guessed the source branch instead of snapshot")
	}
	for path, want := range map[string]string{"sub/code.txt": "synthetic dirty snapshot\n", "captured-untracked.txt": "synthetic captured file\n"} {
		raw, err := os.ReadFile(filepath.Join(in.Identity.Root, path))
		if err != nil || string(raw) != want {
			t.Fatal("snapshot content was not recovered", err)
		}
	}
	if owner, err := readWorktreeOwner(context.Background(), restoreRootRequest(p, false)); err != nil || owner != in.Identity.ThreadID {
		t.Fatal("original owner was not registered", err)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(in.Identity.Root), ".codex-worktree-name")); !os.IsNotExist(err) {
		t.Fatal("restored idle checkout was prematurely published", err)
	}
	if err := os.WriteFile(filepath.Join(in.Identity.Root, "task.txt"), []byte("synthetic later task edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	worktreeTestGit(t, in.Identity.Root, "commit", "-am", "synthetic later task commit")
	if err := ValidateRestoredWorktree(context.Background(), p, result); err != nil {
		t.Fatal("normal task changes invalidated restored generation", err)
	}
	if err := PublishRestoredWorktree(context.Background(), p, result); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(in.Identity.Root), ".codex-worktree-name")); err != nil {
		t.Fatal("restored worktree was not published", err)
	}
	if durable.Generation != result.Generation {
		t.Fatal("result was returned before its durable checkpoint")
	}
}

func TestWorktreeRestoreFreezesSnapshotCommit(t *testing.T) {
	in, old := restoreGitFixture(t, false)
	p, err := PlanWorktreeRestore(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	newer := worktreeTestGit(t, in.Identity.SourceRoot, "rev-parse", "HEAD")
	if newer == old {
		t.Fatal("fixture snapshot equals original base")
	}
	worktreeTestGit(t, in.Identity.SourceRoot, "update-ref", p.SnapshotRef, newer)
	var durable WorktreeRestoreCheckpoint
	if _, err := AdvanceWorktreeRestore(context.Background(), p, WorktreeRestoreCheckpoint{}, restoreCheckpointRecorder(&durable)); err != nil {
		t.Fatal(err)
	}
	if got := worktreeTestGit(t, in.Identity.Root, "rev-parse", "HEAD"); got != old {
		t.Fatal("a moving snapshot ref changed the frozen restore")
	}
}

func TestWorktreeRestoreMaterializesSnapshotCWDExcludedBySourceSparseProfile(t *testing.T) {
	in, _ := restoreGitFixture(t, false)
	worktreeTestGit(t, in.Identity.SourceRoot, "sparse-checkout", "set", "--no-cone", "task.txt")
	profile := worktreeTestGit(t, in.Identity.SourceRoot, "rev-parse", "--path-format=absolute", "--git-path", "info/sparse-checkout")
	before, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	p, err := PlanWorktreeRestore(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	var durable WorktreeRestoreCheckpoint
	if _, err := AdvanceWorktreeRestore(context.Background(), p, WorktreeRestoreCheckpoint{}, restoreCheckpointRecorder(&durable)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(in.Identity.RawCWD, "code.txt"))
	if err != nil || string(data) != "synthetic dirty snapshot\n" {
		t.Fatal("snapshot cwd remained sparse/missing", err)
	}
	after, err := os.ReadFile(profile)
	if err != nil || string(after) != string(before) {
		t.Fatal("restore changed the source sparse profile", err)
	}
}

func TestWorktreeRestorePreservesOriginalBridgeBirth(t *testing.T) {
	in, _ := restoreGitFixture(t, false)
	bridge := worktreeRequest{DraftID: strings.Repeat("a", 32), Source: filepath.Join(in.Identity.SourceRoot, "sub"), Repository: in.Identity.SourceRoot,
		Common: in.Identity.CommonDir, Branch: "refs/heads/main", Commit: worktreeTestGit(t, in.Identity.SourceRoot, "rev-parse", "HEAD"), Destination: in.Identity.Root, CWD: in.Identity.RawCWD}
	in.BridgeMarker, _ = json.Marshal(bridge)
	marker := filepath.Join(filepath.Dir(in.Identity.Root), ".codex-feishu-"+bridge.DraftID+".json")
	birth := filepath.Join(filepath.Dir(in.Identity.Root), ".codex-feishu-"+bridge.DraftID+".identity")
	oldBirth := []byte(`{"version":1,"identity":"synthetic-original-birth"}`)
	if err := os.WriteFile(marker, in.BridgeMarker, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(birth, oldBirth, 0600); err != nil {
		t.Fatal(err)
	}
	p, err := PlanWorktreeRestore(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	var durable WorktreeRestoreCheckpoint
	if _, err := AdvanceWorktreeRestore(context.Background(), p, WorktreeRestoreCheckpoint{}, restoreCheckpointRecorder(&durable)); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(birth); err != nil || string(raw) != string(oldBirth) {
		t.Fatal("restore rewrote the original creation birth", err)
	}
	if raw, err := os.ReadFile(marker); err != nil || string(raw) != string(in.BridgeMarker) {
		t.Fatal("restore rewrote the original creation request", err)
	}
}

func TestWorktreeRestoreRefusesMissingSnapshotAndOccupiedRoot(t *testing.T) {
	t.Run("missing-snapshot", func(t *testing.T) {
		in, _ := restoreGitFixture(t, false)
		worktreeTestGit(t, in.Identity.SourceRoot, "update-ref", "-d", WorktreeSnapshotRef(in.Identity.Root))
		if _, err := PlanWorktreeRestore(context.Background(), in); !errors.Is(err, ErrWorktreeSnapshotMissing) {
			t.Fatal("missing snapshot was not refused", err)
		}
		if _, err := os.Stat(in.Identity.Root); !os.IsNotExist(err) {
			t.Fatal("snapshot absence rebuilt the base", err)
		}
	})
	t.Run("empty-root", func(t *testing.T) {
		in, _ := restoreGitFixture(t, false)
		if err := os.Mkdir(in.Identity.Root, 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := PlanWorktreeRestore(context.Background(), in); !errors.Is(err, ErrWorktreeRestoreConflict) {
			t.Fatal("existing empty directory was adopted", err)
		}
	})
	t.Run("symlink-parent", func(t *testing.T) {
		in, _ := restoreGitFixture(t, false)
		parent := filepath.Dir(in.Identity.Root)
		if err := os.Rename(parent, parent+".saved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(parent+".saved", parent); err != nil {
			t.Fatal(err)
		}
		if _, err := PlanWorktreeRestore(context.Background(), in); err == nil {
			t.Fatal("symlinked parent was adopted")
		}
	})
}

func TestWorktreeRestoreDoesNotReplaceConcurrentEmptyDirectory(t *testing.T) {
	in, _ := restoreGitFixture(t, false)
	p, err := PlanWorktreeRestore(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	var durable WorktreeRestoreCheckpoint
	var foreign os.FileInfo
	record := func(_ context.Context, c WorktreeRestoreCheckpoint) error {
		durable = c
		if c.Phase == "move-dispatching" {
			if err := os.Mkdir(in.Identity.Root, 0700); err != nil {
				t.Fatal(err)
			}
			foreign, err = os.Stat(in.Identity.Root)
			if err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	_, err = AdvanceWorktreeRestore(context.Background(), p, WorktreeRestoreCheckpoint{}, record)
	if !errors.Is(err, ErrWorktreeRestoreConflict) {
		t.Fatal("concurrent destination was not refused", err)
	}
	after, err := os.Stat(in.Identity.Root)
	if err != nil || !os.SameFile(foreign, after) {
		t.Fatal("concurrent empty directory was replaced", err)
	}
	entries, err := os.ReadDir(in.Identity.Root)
	if err != nil || len(entries) != 0 {
		t.Fatal("foreign directory was modified", err)
	}
	if owner, err := readWorktreeOwner(context.Background(), restoreRootRequest(p, true)); err != nil || owner != in.Identity.ThreadID {
		t.Fatal("owned staging checkout was lost", err)
	}
	if _, err := ReconcileWorktreeRestore(context.Background(), p, durable); !errors.Is(err, ErrWorktreeRestoreConflict) {
		t.Fatal("unknown move adopted a competing root", err)
	}
}

func TestWorktreeRestoreDoesNotMoveReplacedStage(t *testing.T) {
	in, _ := restoreGitFixture(t, false)
	p, err := PlanWorktreeRestore(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	var foreign os.FileInfo
	_, err = AdvanceWorktreeRestore(context.Background(), p, WorktreeRestoreCheckpoint{}, func(_ context.Context, c WorktreeRestoreCheckpoint) error {
		if c.Phase == "move-dispatching" {
			worktreeTestGit(t, in.Identity.SourceRoot, "worktree", "remove", "--", p.StageRoot)
			worktreeTestGit(t, in.Identity.SourceRoot, "worktree", "add", "--detach", "--", p.StageRoot, p.SnapshotCommit)
			foreign, err = os.Stat(p.StageRoot)
			if err != nil {
				t.Fatal(err)
			}
		}
		return nil
	})
	if !errors.Is(err, ErrWorktreeRestoreConflict) {
		t.Fatal("replacement staging checkout was not refused", err)
	}
	after, err := os.Stat(p.StageRoot)
	if err != nil || !os.SameFile(after, foreign) {
		t.Fatal("replacement staging directory was moved", err)
	}
	if _, err := os.Lstat(in.Identity.Root); !os.IsNotExist(err) {
		t.Fatal("foreign staging checkout was published", err)
	}
}

func TestWorktreeRestoreUnknownRenameRepairsOnlyItsOwnBacklink(t *testing.T) {
	in, _ := restoreGitFixture(t, false)
	p, err := PlanWorktreeRestore(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	var durable WorktreeRestoreCheckpoint
	stopped := errors.New("synthetic checkpoint interruption")
	_, err = AdvanceWorktreeRestore(context.Background(), p, WorktreeRestoreCheckpoint{}, func(_ context.Context, c WorktreeRestoreCheckpoint) error {
		if c.Phase == "moved" {
			return stopped
		}
		durable = c
		return nil
	})
	if !errors.Is(err, stopped) || durable.Phase != "move-dispatching" {
		t.Fatal("fixture did not stop after rename before repair", err)
	}
	if ownerPath := worktreeTestGit(t, in.Identity.Root, "rev-parse", "--path-format=absolute", "--git-path", worktreeOwnerName); ownerPath != filepath.Join(durable.Physical.GitDir, worktreeOwnerName) {
		t.Fatal("first exposed root did not point to preowned metadata")
	}
	raw, err := os.ReadFile(filepath.Join(durable.Physical.GitDir, worktreeOwnerName))
	if err != nil {
		t.Fatal(err)
	}
	var owner nativeWorktreeOwner
	if json.Unmarshal(raw, &owner) != nil || owner.OwnerThreadID != in.Identity.ThreadID {
		t.Fatal("first exposed root had missing/wrong owner")
	}
	if _, err := os.Lstat(p.StageRoot); !os.IsNotExist(err) {
		t.Fatal("rename did not move the original inode", err)
	}
	proof, err := ReconcileWorktreeRestore(context.Background(), p, durable)
	if err != nil || proof.Phase != "moved" {
		t.Fatal("readonly reconciliation rejected its own old backlink", err)
	}
	result, err := AdvanceWorktreeRestore(context.Background(), p, proof, restoreCheckpointRecorder(&durable))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRestoredWorktree(context.Background(), p, result); err != nil {
		t.Fatal(err)
	}
	records, err := registeredWorktrees(context.Background(), in.Identity.SourceRoot)
	if err != nil || len(records) != 2 {
		t.Fatal("unknown recovery duplicated a worktree", err)
	}
}

func TestWorktreeRestorePreservesRunningOwnerWorkBeforeRepair(t *testing.T) {
	in, _ := restoreGitFixture(t, false)
	p, err := PlanWorktreeRestore(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	var durable WorktreeRestoreCheckpoint
	result, err := AdvanceWorktreeRestore(context.Background(), p, WorktreeRestoreCheckpoint{}, func(_ context.Context, c WorktreeRestoreCheckpoint) error {
		durable = c
		if c.Phase == "moved" {
			if err := os.WriteFile(filepath.Join(in.Identity.Root, "owner-new-work.txt"), []byte("synthetic running owner work\n"), 0600); err != nil {
				t.Fatal(err)
			}
			worktreeTestGit(t, in.Identity.Root, "add", "owner-new-work.txt")
			worktreeTestGit(t, in.Identity.Root, "commit", "-m", "synthetic running owner commit")
		}
		return nil
	})
	if err != nil {
		t.Fatal("running owner work blocked a physically verified recovery", err)
	}
	if data, err := os.ReadFile(filepath.Join(in.Identity.Root, "owner-new-work.txt")); err != nil || string(data) != "synthetic running owner work\n" {
		t.Fatal("restore discarded running owner work", err)
	}
	if err := ValidateRestoredWorktree(context.Background(), p, result); err != nil {
		t.Fatal(err)
	}
	if durable.Generation != result.Generation {
		t.Fatal("running-owner restore was not checkpointed")
	}
}

func TestWorktreeRestoreUnknownWithoutCheckoutNeverRepeatsAdd(t *testing.T) {
	in, _ := restoreGitFixture(t, false)
	p, err := PlanWorktreeRestore(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	c := WorktreeRestoreCheckpoint{Version: 1, Phase: "stage-dispatching", PlanDigest: restorePlanDigest(p)}
	if _, err := ReconcileWorktreeRestore(context.Background(), p, c); err == nil {
		t.Fatal("unknown absent stage was treated as ready")
	}
	if _, err := AdvanceWorktreeRestore(context.Background(), p, c, func(context.Context, WorktreeRestoreCheckpoint) error { return nil }); !errors.Is(err, ErrWorktreeRestoreUnknown) {
		t.Fatal("unknown add was dispatched again", err)
	}
	if _, err := os.Lstat(filepath.Dir(p.StageRoot)); !os.IsNotExist(err) {
		t.Fatal("unknown recovery allocated a replacement bucket", err)
	}
	records, err := registeredWorktrees(context.Background(), in.Identity.SourceRoot)
	if err != nil || len(records) != 1 {
		t.Fatal("unknown recovery registered a new worktree", err)
	}
}

func TestWorktreeRestoreStageBirthReceiptGuardsUnknownGeneration(t *testing.T) {
	for _, change := range []string{"none", "missing-receipt", "recreated-stage"} {
		t.Run(change, func(t *testing.T) {
			in, _ := restoreGitFixture(t, false)
			p, err := PlanWorktreeRestore(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			var durable WorktreeRestoreCheckpoint
			stop := errors.New("synthetic lost stage checkpoint")
			_, err = AdvanceWorktreeRestore(context.Background(), p, WorktreeRestoreCheckpoint{}, func(_ context.Context, c WorktreeRestoreCheckpoint) error {
				if c.Phase == "stage-ready" {
					return stop
				}
				durable = c
				return nil
			})
			if !errors.Is(err, stop) || durable.Phase != "stage-dispatching" {
				t.Fatal("fixture did not interrupt the stage checkpoint", err)
			}
			if change == "missing-receipt" {
				if err := os.Remove(filepath.Join(filepath.Dir(p.StageRoot), ".codex-feishu-restore.identity")); err != nil {
					t.Fatal(err)
				}
			}
			if change == "recreated-stage" {
				worktreeTestGit(t, in.Identity.SourceRoot, "worktree", "remove", "--", p.StageRoot)
				worktreeTestGit(t, in.Identity.SourceRoot, "worktree", "add", "--detach", "--", p.StageRoot, p.SnapshotCommit)
			}
			proof, err := ReconcileWorktreeRestore(context.Background(), p, durable)
			if change == "none" {
				if err != nil || proof.Phase != "stage-ready" || proof.StageIdentity == "" {
					t.Fatal("complete private birth receipt was not recovered", err)
				}
			} else if !errors.Is(err, ErrWorktreeRestoreUnknown) {
				t.Fatal("missing/replaced generation was adopted", err)
			}
			if owner, err := readWorktreeOwner(context.Background(), restoreRootRequest(p, true)); err != nil || owner != "" {
				t.Fatal("readonly reconciliation wrote an owner", err)
			}
		})
	}
}

func TestWorktreeRestoreClaimBeforeCheckpointCanResumeReadOnly(t *testing.T) {
	in, _ := restoreGitFixture(t, false)
	p, err := PlanWorktreeRestore(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := ReconcileWorktreeRestore(context.Background(), p, WorktreeRestoreCheckpoint{})
	if err != nil || proof.Phase != "planned" || proof.PlanDigest == "" {
		t.Fatal("unissued claim could not be verified", err)
	}
	if _, err := os.Lstat(filepath.Dir(p.StageRoot)); !os.IsNotExist(err) {
		t.Fatal("read-only reconciliation allocated staging", err)
	}
	var durable WorktreeRestoreCheckpoint
	if _, err := AdvanceWorktreeRestore(context.Background(), p, proof, restoreCheckpointRecorder(&durable)); err != nil {
		t.Fatal("verified unissued claim could not advance", err)
	}
}

func TestWorktreeRestoreStaleRegistrationIsNotPruned(t *testing.T) {
	for _, locked := range []bool{false, true} {
		t.Run(map[bool]string{false: "unlocked", true: "locked"}[locked], func(t *testing.T) {
			in, _ := restoreGitFixture(t, false)
			worktreeTestGit(t, in.Identity.SourceRoot, "worktree", "add", "--detach", "--", in.Identity.Root, "HEAD")
			if locked {
				worktreeTestGit(t, in.Identity.SourceRoot, "worktree", "lock", "--reason", "synthetic external lock", in.Identity.Root)
			}
			gd := worktreeTestGit(t, in.Identity.Root, "rev-parse", "--absolute-git-dir")
			if err := os.RemoveAll(in.Identity.Root); err != nil {
				t.Fatal(err)
			}
			if _, err := PlanWorktreeRestore(context.Background(), in); !errors.Is(err, ErrWorktreeRestoreStale) {
				t.Fatal("stale registration was not refused", err)
			}
			if _, err := os.Stat(gd); err != nil {
				t.Fatal("stale metadata was removed", err)
			}
		})
	}
}

func TestWorktreeRestoreDoesNotWriteReusedSiblingOwnerSlot(t *testing.T) {
	in, _ := restoreGitFixture(t, true)
	sibling := filepath.Join(in.CodexHome, "worktrees", "cdef", "project")
	worktreeTestGit(t, in.Identity.SourceRoot, "worktree", "add", "--detach", "--", sibling, "HEAD")
	pSibling := worktreeRequest{Repository: in.Identity.SourceRoot, Common: in.Identity.CommonDir, Destination: sibling, CWD: sibling}
	if err := ensureWorktreeOwner(context.Background(), pSibling, "synthetic-sibling-thread"); err != nil {
		t.Fatal(err)
	}
	p, err := PlanWorktreeRestore(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	var durable WorktreeRestoreCheckpoint
	result, err := AdvanceWorktreeRestore(context.Background(), p, WorktreeRestoreCheckpoint{}, restoreCheckpointRecorder(&durable))
	if err != nil {
		t.Fatal(err)
	}
	if result.PublicationPending || len(result.Limitations) < 2 {
		t.Fatal("legacy publication limits were hidden")
	}
	if owner, err := readWorktreeOwner(context.Background(), pSibling); err != nil || owner != "synthetic-sibling-thread" {
		t.Fatal("reused sibling slot was overwritten", err)
	}
	if owner, err := readWorktreeOwner(context.Background(), restoreRootRequest(p, false)); err != nil || owner != in.Identity.ThreadID {
		t.Fatal("restored owner is incorrect", err)
	}
}

func TestWorktreeRestoreCancelledOrUnjournaledDoesNotWrite(t *testing.T) {
	in, _ := restoreGitFixture(t, false)
	p, err := PlanWorktreeRestore(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := AdvanceWorktreeRestore(ctx, p, WorktreeRestoreCheckpoint{}, func(context.Context, WorktreeRestoreCheckpoint) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation was not preserved", err)
	}
	denied := errors.New("synthetic journal failure")
	if _, err := AdvanceWorktreeRestore(context.Background(), p, WorktreeRestoreCheckpoint{}, func(context.Context, WorktreeRestoreCheckpoint) error { return denied }); !errors.Is(err, denied) {
		t.Fatal("missing durable dispatch was ignored", err)
	}
	if _, err := os.Lstat(filepath.Dir(p.StageRoot)); !os.IsNotExist(err) {
		t.Fatal("unclaimed restore modified staging", err)
	}
}
