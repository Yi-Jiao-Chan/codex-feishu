package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCreationWorktreeMigrationPreservesExistingWorkflow(t *testing.T) {
	s, path := discoveryStore(t)
	ctx := context.Background()
	if _, err := s.BeginBaseline(ctx, creationTestHost, creationTestApp, 100); err != nil {
		t.Fatal(err)
	}
	d, i := creationReadyForMaterialize(t, s, "old-local-workflow", 100)
	b, receipt, err := s.MaterializeCreation(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	d, err = s.CreationDraft(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	i, err = s.CreationIntent(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct the exact previous schema, leaving its real workflow,
	// immutable attempts, receipt and binding in place for the upgrade.
	for table, columns := range map[string][]string{
		"creation_drafts":    {"workspace_mode", "base_branch"},
		"creation_workflows": {"workspace_mode", "base_branch", "base_commit", "git_root", "git_common_dir", "worktree_path"},
	} {
		for _, column := range columns {
			if _, err := s.DB.Exec(`ALTER TABLE ` + table + ` DROP COLUMN ` + column); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := s.DB.Exec(`PRAGMA user_version=28`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	gotDraft, err := upgraded.CreationDraft(ctx, d.ID)
	if err != nil || !reflect.DeepEqual(d, gotDraft) || gotDraft.WorkspaceMode != "local" {
		t.Fatal("old draft changed during migration", err)
	}
	gotIntent, err := upgraded.CreationIntent(ctx, d.ID)
	if err != nil || !reflect.DeepEqual(i, gotIntent) || gotIntent.WorkspaceMode != "local" {
		t.Fatal("old workflow changed during migration", err)
	}
	gotBinding, gotReceipt, err := upgraded.MaterializeCreation(ctx, d.ID)
	if err != nil || gotBinding != b || !reflect.DeepEqual(gotReceipt, receipt) {
		t.Fatal("migration changed the existing binding or first input", err)
	}
	var integrity string
	if err := upgraded.DB.QueryRow(`PRAGMA quick_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatal("upgraded database is not intact", err)
	}
}

func TestCreationWorktreeIntentAndAttemptAreFrozen(t *testing.T) {
	s := creationTestStore(t)
	ctx := context.Background()
	d := creationTestDraft("worktree-intent", 100)
	d.WorkspaceMode, d.BaseBranch = "worktree", "refs/heads/main"
	d = receiveCreationTestDraft(t, s, d)
	plan := CreationIntent{OwnerID: "owner", Members: []string{"owner"}, GitRoot: "/project", GitCommonDir: "/project/.git", BaseCommit: strings.Repeat("1", 40), WorktreePath: "/private/worktrees/draft/project"}
	i, claimed, err := s.ClaimCreationIntent(ctx, d, plan, 101)
	if err != nil || !claimed || i.WorkspaceMode != "worktree" || i.BaseCommit != plan.BaseCommit {
		t.Fatal("worktree plan was not claimed atomically", err)
	}
	plan.BaseCommit = strings.Repeat("2", 40)
	again, claimed, err := s.ClaimCreationIntent(ctx, d, plan, 102)
	if err != nil || claimed || !reflect.DeepEqual(again, i) {
		t.Fatal("duplicate submission changed the frozen worktree", err)
	}
	a, err := s.PrepareCreationAttempt(ctx, d.ID, "worktree", `{"frozen":true}`, 103)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.DispatchCreationAttempt(ctx, a); err != nil || !ok {
		t.Fatal(err)
	}
	if err := s.FinishCreationAttempt(ctx, a, "confirmed", "/other/checkout", `{}`); !errors.Is(err, ErrConflict) {
		t.Fatal("another checkout confirmed the worktree", err)
	}
	if err := s.RecoverCreations(ctx, creationTestHost, creationTestApp); err != nil {
		t.Fatal(err)
	}
	if err := s.RetryCreation(ctx, d); !errors.Is(err, ErrSubmissionStale) {
		t.Fatal("unknown worktree dispatch allowed a retry", err)
	}
	if err := s.FinishCreationAttempt(ctx, a, "confirmed", i.WorktreePath, `{}`); err != nil {
		t.Fatal(err)
	}
}
