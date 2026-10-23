package creation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-feishu/internal/store"
)

type worktreeRestoreAuditCase struct {
	h                 *managerHarness
	a                 WorktreeAuditor
	oldReport         WorktreeAuditReport
	intent            store.CreationIntent
	scope             store.SubmissionScope
	plan              WorktreeRestorePlan
	result            WorktreeRestoreResult
	oldBirthJournal   store.CreationAttempt
	oldBirthMarker    []byte
	oldBirthIdentity  []byte
	birthMarkerPath   string
	birthIdentityPath string
}

func newWorktreeRestoreAuditCase(t *testing.T) *worktreeRestoreAuditCase {
	t.Helper()
	h, a, oldReport, _ := worktreeAuditFixture(t)
	ctx := context.Background()
	entry := oldReport.Entries[0]
	i, err := h.s.BridgeCreationThread(ctx, oldReport.HostID, oldReport.AppID, entry.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	oldBirthJournal, err := h.s.CreationAttempt(ctx, i.DraftID, "worktree")
	if err != nil {
		t.Fatal(err)
	}
	birthMarkerPath := filepath.Join(filepath.Dir(i.WorktreePath), ".codex-feishu-"+i.DraftID+".json")
	birthIdentityPath := filepath.Join(filepath.Dir(i.WorktreePath), ".codex-feishu-"+i.DraftID+".identity")
	oldBirthMarker, err := os.ReadFile(birthMarkerPath)
	if err != nil {
		t.Fatal(err)
	}
	oldBirthIdentity, err := os.ReadFile(birthIdentityPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(i.WorktreePath, "synthetic-restore-snapshot.txt"), []byte("synthetic restore snapshot\n"), 0600); err != nil {
		t.Fatal(err)
	}
	worktreeTestGit(t, i.WorktreePath, "add", "synthetic-restore-snapshot.txt")
	worktreeTestGit(t, i.WorktreePath, "commit", "-m", "synthetic restore snapshot")
	snapshot := worktreeTestGit(t, i.WorktreePath, "rev-parse", "HEAD")
	worktreeTestGit(t, i.GitRoot, "update-ref", WorktreeSnapshotRef(i.WorktreePath), snapshot)
	worktreeTestGit(t, i.GitRoot, "worktree", "remove", "--force", "--", i.WorktreePath)

	relative, err := filepath.Rel(i.WorktreePath, i.ExecutionDirectory())
	if err != nil || relative == "" {
		t.Fatalf("relative execution directory: %v", err)
	}
	bridgeMarker, err := os.ReadFile(birthMarkerPath)
	if err != nil {
		t.Fatal(err)
	}
	scope := store.SubmissionScope{HostID: i.HostID, AppID: i.AppID, ThreadID: i.NativeThreadID}
	input := WorktreeRestoreInput{
		RestoreID: strings.Repeat("c", 32),
		Identity: store.ThreadWorktreeIdentity{
			HostID: scope.HostID, AppID: scope.AppID, ThreadID: scope.ThreadID,
			RawCWD: i.ExecutionDirectory(), Root: i.WorktreePath, SourceRoot: i.GitRoot,
			CommonDir: i.GitCommonDir, RelativeCWD: relative,
		},
		CodexHome:    h.codexHome,
		BridgeMarker: bridgeMarker,
	}
	plan, err := PlanWorktreeRestore(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	r, fresh, err := h.s.ReceiveSubmissionInput(ctx, scope, store.Receipt{
		AppID: scope.AppID, ThreadID: scope.ThreadID, ChatID: i.ChatID,
		MessageID: "synthetic-restore-audit-input", Body: "synthetic restore audit input",
	})
	if err != nil || !fresh {
		t.Fatalf("restore input was not admitted: fresh=%v err=%v", fresh, err)
	}
	rawPlan, _ := json.Marshal(plan)
	attempt, err := h.s.PrepareWorktreeRestoreAttempt(ctx, scope, r.ID, string(rawPlan), h.clock.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := h.s.ClaimWorktreeRestoreAttempt(ctx, attempt, h.clock.UnixMilli())
	if err != nil || !claimed {
		t.Fatalf("restore attempt was not claimed: claimed=%v err=%v", claimed, err)
	}
	attempt, err = h.s.WorktreeRestoreAttempt(ctx, scope, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	persist := func(ctx context.Context, checkpoint WorktreeRestoreCheckpoint) error {
		raw, err := json.Marshal(checkpoint)
		if err != nil {
			return err
		}
		if err := h.s.CheckpointWorktreeRestoreAttempt(ctx, attempt, checkpoint.Phase, string(raw), h.clock.UnixMilli()); err != nil {
			return err
		}
		attempt, err = h.s.WorktreeRestoreAttempt(ctx, scope, r.ID)
		return err
	}
	result, err := AdvanceWorktreeRestore(ctx, plan, WorktreeRestoreCheckpoint{}, persist)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = h.s.WorktreeRestoreAttempt(ctx, scope, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	rawResult, _ := json.Marshal(result)
	if err := h.s.FinishWorktreeRestoreAttempt(ctx, attempt, "confirmed", string(rawResult), "", h.clock.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if result.Phase != "restored" || result.Generation == "" {
		t.Fatalf("restore did not produce a physical generation: %+v", result)
	}
	return &worktreeRestoreAuditCase{h: h, a: a, oldReport: oldReport, intent: i, scope: scope, plan: plan, result: result,
		oldBirthJournal: oldBirthJournal, oldBirthMarker: oldBirthMarker, oldBirthIdentity: oldBirthIdentity,
		birthMarkerPath: birthMarkerPath, birthIdentityPath: birthIdentityPath}
}

func (f *worktreeRestoreAuditCase) assertBirthUnchanged(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	got, err := f.h.s.CreationAttempt(ctx, f.intent.DraftID, "worktree")
	if err != nil || got != f.oldBirthJournal {
		t.Fatalf("restore rewrote the immutable birth journal: got=%+v err=%v", got, err)
	}
	if raw, err := os.ReadFile(f.birthMarkerPath); err != nil || string(raw) != string(f.oldBirthMarker) {
		t.Fatalf("restore rewrote the original bridge marker: err=%v", err)
	}
	if raw, err := os.ReadFile(f.birthIdentityPath); err != nil || string(raw) != string(f.oldBirthIdentity) {
		t.Fatalf("restore rewrote the original birth identity: err=%v", err)
	}
}

func TestWorktreeRestoreAuditAcceptsConfirmedNewGeneration(t *testing.T) {
	f := newWorktreeRestoreAuditCase(t)
	ctx := context.Background()
	report, err := f.a.Audit(ctx)
	if err != nil || len(report.Entries) != 1 || report.Entries[0].Status != "owner-correct" || report.Entries[0].ObservedOwner != f.intent.NativeThreadID {
		t.Fatalf("confirmed restore generation was not accepted: entries=%+v err=%v", report.Entries, err)
	}
	if report.Entries[0].Identity != f.oldReport.Entries[0].Identity {
		t.Log("audit observed a new physical generation")
	} else {
		t.Fatal("restore reused the original physical generation")
	}
	f.assertBirthUnchanged(t)

	counts, err := f.a.Repair(ctx, f.oldReport)
	if err != nil || counts["refused"] != 1 || counts["confirmed"] != 0 {
		t.Fatalf("old audit plan acted on the restored generation: counts=%v err=%v", counts, err)
	}
	f.assertBirthUnchanged(t)
}

func TestWorktreeRestoreAuditRejectsReplacementGeneration(t *testing.T) {
	t.Run("checkout", func(t *testing.T) {
		f := newWorktreeRestoreAuditCase(t)
		worktreeTestGit(t, f.intent.GitRoot, "worktree", "remove", "--force", "--", f.intent.WorktreePath)
		worktreeTestGit(t, f.intent.GitRoot, "worktree", "add", "--detach", "--", f.intent.WorktreePath, f.plan.SnapshotCommit)
		report, err := f.a.Audit(context.Background())
		if err != nil || len(report.Entries) != 1 || report.Entries[0].Status == "owner-correct" {
			t.Fatalf("replacement checkout was accepted: entries=%+v err=%v", report.Entries, err)
		}
		f.assertBirthUnchanged(t)
	})

	t.Run("owner", func(t *testing.T) {
		f := newWorktreeRestoreAuditCase(t)
		ownerPath := worktreeTestGit(t, f.intent.WorktreePath, "rev-parse", "--path-format=absolute", "--git-path", worktreeOwnerName)
		if err := os.WriteFile(ownerPath, []byte(`{"version":1,"ownerThreadId":"synthetic-foreign-owner"}`), 0600); err != nil {
			t.Fatal(err)
		}
		report, err := f.a.Audit(context.Background())
		if err != nil || len(report.Entries) != 1 || report.Entries[0].Status == "owner-correct" {
			t.Fatalf("foreign owner was accepted: entries=%+v err=%v", report.Entries, err)
		}
		f.assertBirthUnchanged(t)
	})
}
