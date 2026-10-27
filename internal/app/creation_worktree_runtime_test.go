package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/creation"
	"codex-feishu/internal/store"
	"codex-feishu/internal/submissions"
)

func creationRuntimeGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=Creation Runtime", "-c", "user.email=creation@example.invalid", "-C", directory}, args...)...)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GIT_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture Git failed: %v: %s", err, raw)
	}
	return strings.TrimSpace(string(raw))
}

func creationRuntimeWorktree(t *testing.T, source string, subdirectory bool) store.CreationIntent {
	t.Helper()
	creationRuntimeGit(t, source, "init", "--initial-branch=main")
	if err := os.Mkdir(filepath.Join(source, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "sub", "task.txt"), []byte("fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	creationRuntimeGit(t, source, "add", ".")
	creationRuntimeGit(t, source, "commit", "-m", "fixture")
	commit := creationRuntimeGit(t, source, "rev-parse", "HEAD")
	storage := t.TempDir()
	if err := os.Chmod(storage, 0700); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(storage, "checkout")
	creationRuntimeGit(t, source, "worktree", "add", "--detach", checkout, commit)
	directory := source
	if subdirectory {
		directory = filepath.Join(source, "sub")
	}
	return store.CreationIntent{Directory: directory, WorkspaceMode: "worktree", BaseBranch: "refs/heads/main", BaseCommit: commit, GitRoot: source, GitCommonDir: filepath.Join(source, ".git"), WorktreePath: checkout}
}

func TestCreationRuntimeWorktreeDeliversAndProcessesPendingInput(t *testing.T) {
	for _, subdirectory := range []bool{false, true} {
		t.Run(map[bool]string{false: "repository", true: "subdirectory"}[subdirectory], func(t *testing.T) {
			db, base, g, r := runtimeFixture(t)
			n := &creationRuntimeNative{runtimeNative: base}
			r.Native = n
			s := creationRuntimeService(r)
			plan := creationRuntimeWorktree(t, r.AllowedRoots[0], subdirectory)
			d := prepareRuntimeCreationWorkspace(t, r, n, "created-thread", plan)
			_, receipt, err := db.MaterializeCreation(context.Background(), d.ID)
			if err != nil {
				t.Fatal(err)
			}
			acceptRuntimeCreation(t, r, n, d, receipt)
			i, err := db.CreationIntent(context.Background(), d.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := creation.ValidateCreatedWorktree(context.Background(), i); err != nil {
				t.Fatal("worktree identity fixture invalid", err)
			}
			followup, fresh, err := db.Claim(context.Background(), store.Receipt{AppID: r.AppID, MessageID: "followup-message", ThreadID: receipt.ThreadID, ChatID: "created-chat", Body: "next task"})
			if err != nil || !fresh {
				t.Fatal("followup receipt failed", err)
			}
			controller := &submissions.Controller{Store: db, Native: n, HostID: r.HostID, AppID: r.AppID, ThreadID: receipt.ThreadID}
			r.ActorTick = func(ctx context.Context, thread string) ([]codexclient.Turn, error) {
				meta, err := n.Metadata(ctx, thread)
				if err != nil {
					return nil, err
				}
				if _, err := s.authorizeCreation(ctx, meta, false); err != nil {
					return nil, err
				}
				return controller.TickWithHistory(ctx)
			}
			runCreationRuntime(t, r, n)
			runtimeWait(t, func() bool {
				current, err := db.Submission(context.Background(), store.SubmissionScope{HostID: r.HostID, AppID: r.AppID, ThreadID: receipt.ThreadID}, followup.ID)
				return err == nil && current.State == "accepted" && n.adds.Load() == 2 && creationRuntimeDelivered(g)
			})
			g.mu.Lock()
			groups := g.groups
			g.mu.Unlock()
			if groups != 0 {
				t.Fatal("recovery created a replacement group")
			}
		})
	}
}

func TestCreationWorktreeAuthorizationRejectsDirectoryAndScopeDrift(t *testing.T) {
	db, base, _, r := runtimeFixture(t)
	n := &creationRuntimeNative{runtimeNative: base}
	r.Native = n
	s := creationRuntimeService(r)
	plan := creationRuntimeWorktree(t, r.AllowedRoots[0], true)
	d := prepareRuntimeCreationWorkspace(t, r, n, "created-thread", plan)
	_, receipt, err := db.MaterializeCreation(context.Background(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	acceptRuntimeCreation(t, r, n, d, receipt)
	meta, err := n.Metadata(context.Background(), receipt.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{d.Directory, plan.WorktreePath, t.TempDir()} {
		changed := meta
		changed.CWD = cwd
		if handled, err := s.authorizeCreation(context.Background(), changed, false); !handled || !errors.Is(err, store.ErrIneligible) {
			t.Fatal("unconfirmed execution directory was authorized", err)
		}
	}
	s.Config.Creation.AllowedRoots = []string{plan.Directory}
	if handled, err := s.authorizeCreation(context.Background(), meta, false); !handled || !errors.Is(err, store.ErrIneligible) {
		t.Fatal("selected subdirectory authorized the entire source repository", err)
	}
	s.Config.Creation.AllowedRoots = []string{t.TempDir()}
	if handled, err := s.authorizeCreation(context.Background(), meta, false); !handled || !errors.Is(err, store.ErrIneligible) {
		t.Fatal("removed source authorization was ignored", err)
	}
	s.Config.Creation.AllowedRoots = r.AllowedRoots
	creationRuntimeGit(t, plan.GitRoot, "worktree", "remove", "--force", plan.WorktreePath)
	if err := os.MkdirAll(meta.CWD, 0700); err != nil {
		t.Fatal(err)
	}
	if handled, err := s.authorizeCreation(context.Background(), meta, false); !handled || !errors.Is(err, store.ErrIneligible) {
		t.Fatal("replacement directory without registered checkout was authorized", err)
	}
	if err := os.RemoveAll(plan.WorktreePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(filepath.Dir(plan.WorktreePath), ".codex-feishu-"+d.ID+".json")); err != nil {
		t.Fatal(err)
	}
	creationRuntimeGit(t, plan.GitRoot, "worktree", "add", "--detach", plan.WorktreePath, plan.BaseCommit)
	if handled, err := s.authorizeCreation(context.Background(), meta, false); !handled || !errors.Is(err, store.ErrIneligible) {
		t.Fatal("registered replacement checkout without bridge ownership was authorized", err)
	}
}
