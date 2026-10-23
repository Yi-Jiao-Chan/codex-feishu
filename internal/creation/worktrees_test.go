package creation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"codex-feishu/internal/store"
)

func worktreeTestGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=Worktree Test", "-c", "user.email=worktree@example.invalid", "-C", directory}, args...)...)
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture git %v failed: %v: %s", args, err, raw)
	}
	return strings.TrimSpace(string(raw))
}

func initWorktreeTestRepo(t *testing.T, directory string) string {
	t.Helper()
	worktreeTestGit(t, directory, "init", "--initial-branch=main")
	if err := os.MkdirAll(filepath.Join(directory, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"task.txt", "sub/code.txt"} {
		if err := os.WriteFile(filepath.Join(directory, path), []byte("main\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	worktreeTestGit(t, directory, "add", ".")
	worktreeTestGit(t, directory, "commit", "-m", "initial")
	return worktreeTestGit(t, directory, "rev-parse", "HEAD")
}

func worktreeDraft(t *testing.T, h *managerHarness, source string) store.CreationDraft {
	t.Helper()
	h.m.Config.Storage.DatabaseFile = h.db
	d := h.receive(source, "/new inspect the independent checkout")
	if err := h.action(d, "creation_worktree", map[string]any{"creation_body": "updated task"}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft(source)
	if d.WorkspaceMode != "worktree" || d.BaseBranch != "refs/heads/main" || d.Body != "updated task" {
		t.Fatalf("workspace action did not preserve the draft: %+v", d)
	}
	if len(h.n.starts) != 0 || h.g.groups != 0 {
		t.Fatal("mode selection created remote resources")
	}
	return d
}

func claimWorktreeTestIntent(t *testing.T, h *managerHarness, d store.CreationDraft, form map[string]any) store.CreationIntent {
	t.Helper()
	form = localCreationForm(form)
	form["creation_use_worktree"] = true
	if err := h.action(d, "creation_submit", form); err != nil {
		t.Fatal(err)
	}
	if err := h.m.tickUI(context.Background()); err != nil {
		t.Fatal(err)
	}
	i := h.intent(d)
	if i.WorkspaceMode != "worktree" || i.WorktreePath == "" || i.BaseCommit == "" {
		t.Fatalf("worktree plan was not frozen with intent: %+v", i)
	}
	h.g.expectedDirectory = i.ExecutionDirectory()
	return i
}

func TestWorktreeCreationPinsBranchAndKeepsSourceChanges(t *testing.T) {
	h := newManagerHarness(t)
	main := initWorktreeTestRepo(t, h.root)
	worktreeTestGit(t, h.root, "switch", "-c", "feature/测试")
	if err := os.WriteFile(filepath.Join(h.root, "task.txt"), []byte("feature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	worktreeTestGit(t, h.root, "commit", "-am", "feature")
	feature := worktreeTestGit(t, h.root, "rev-parse", "HEAD")
	worktreeTestGit(t, h.root, "switch", "main")
	if err := os.WriteFile(filepath.Join(h.root, "task.txt"), []byte("private unstaged change\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.root, "untracked.txt"), []byte("private untracked\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before := worktreeTestGit(t, h.root, "status", "--porcelain=v1", "--untracked-files=all")
	d := worktreeDraft(t, h, "worktree-input")
	// New planning is independent of the native/App Codex home. Keep the
	// provider path unavailable so a hidden handshake dependency is caught.
	h.codexHome = filepath.Join(t.TempDir(), "missing-codex-home")
	i := claimWorktreeTestIntent(t, h, d, map[string]any{"creation_branch": "refs/heads/feature/测试"})
	expected, err := legacyWorktreePath(h.db, d.ID)
	if err != nil || i.WorktreePath != expected || filepath.Dir(filepath.Dir(filepath.Dir(i.WorktreePath))) != filepath.Dir(h.db) || strings.Contains(i.WorktreePath, filepath.Join(h.codexHome, "worktrees")) {
		t.Fatal("new worktree does not use the bridge database sibling storage")
	}
	if i.BaseCommit != feature || i.BaseBranch != "refs/heads/feature/测试" {
		t.Fatal("chosen branch was not resolved before dispatch")
	}
	worktreeTestGit(t, h.root, "update-ref", "refs/heads/feature/测试", main)
	h.tick()
	i = h.intent(d)
	if i.State != "confirmed" || len(h.n.starts) != 1 || len(h.n.adds) != 1 || h.n.starts[0].CWD != i.ExecutionDirectory() || h.n.starts[0].CWD == h.root {
		t.Fatalf("native session did not use the independent checkout: state=%s starts=%d adds=%d", i.State, len(h.n.starts), len(h.n.adds))
	}
	if got := worktreeTestGit(t, i.WorktreePath, "rev-parse", "HEAD"); got != feature {
		t.Fatal("branch movement changed the frozen base commit")
	}
	if got := worktreeTestGit(t, i.WorktreePath, "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
		t.Fatal("new checkout is not detached")
	}
	if got := worktreeTestGit(t, h.root, "status", "--porcelain=v1", "--untracked-files=all"); got != before {
		t.Fatal("worktree creation changed source files or index")
	}
	if got := worktreeTestGit(t, h.root, "rev-parse", "HEAD"); got != main {
		t.Fatal("worktree creation moved the source branch")
	}
	if _, err := os.Stat(filepath.Join(i.WorktreePath, "untracked.txt")); !os.IsNotExist(err) {
		t.Fatal("committed-branch mode copied untracked source material")
	}
	var cwd, description string
	if err := h.s.DB.QueryRow(`SELECT cwd FROM thread_observations WHERE host_id='host' AND thread_id=?`, i.NativeThreadID).Scan(&cwd); err != nil || cwd != i.ExecutionDirectory() {
		t.Fatal("persisted binding observation lost the execution directory", err)
	}
	if err := h.s.DB.QueryRow(`SELECT description FROM create_intents WHERE intent_id=?`, i.ID).Scan(&description); err != nil || description != "工作目录："+cwd {
		t.Fatal("group metadata used the source checkout", err)
	}
	h.tick()
	h.tick()
	if len(h.n.starts) != 1 || h.g.groups != 1 {
		t.Fatal("completed workflow duplicated native resources")
	}
}

func TestWorktreeRecoveryProvesExistingCheckoutWithoutRecreating(t *testing.T) {
	h := newManagerHarness(t)
	initWorktreeTestRepo(t, h.root)
	d := worktreeDraft(t, h, "lost-worktree-result")
	i := claimWorktreeTestIntent(t, h, d, nil)
	p := frozenWorktree(i)
	raw, _ := json.Marshal(p)
	a, err := h.s.PrepareCreationAttempt(context.Background(), d.ID, "worktree", string(raw), h.clock.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := h.s.DispatchCreationAttempt(context.Background(), a); err != nil || !ok {
		t.Fatal("could not claim worktree dispatch", err)
	}
	if err := worktreeMarker(p, true); err != nil {
		t.Fatal(err)
	}
	worktreeTestGit(t, h.root, "worktree", "add", "--detach", "--", p.Destination, p.Commit)
	// The process exits after Git commits the checkout but before the SQLite
	// outcome. The new worker must use the journal and exact registration.
	if err := h.s.Close(); err != nil {
		t.Fatal(err)
	}
	h.s, err = store.Open(h.db)
	if err != nil {
		t.Fatal(err)
	}
	h.m.Store = h.s
	h.tick()
	i = h.intent(d)
	if i.State != "confirmed" || len(h.n.starts) != 1 {
		t.Fatal("existing checkout was not recovered", i.State)
	}
	records, err := registeredWorktrees(context.Background(), h.root)
	if err != nil || len(records) != 2 {
		t.Fatal("recovery created an extra checkout", len(records), err)
	}
	a, err = h.s.CreationAttempt(context.Background(), d.ID, "worktree")
	if err != nil || a.State != "confirmed" || a.RemoteID != p.Destination {
		t.Fatal("exact worktree readback did not close the journal", err)
	}
}

func TestWorktreeUnknownWithoutCheckoutNeverRetriesAdd(t *testing.T) {
	h := newManagerHarness(t)
	initWorktreeTestRepo(t, h.root)
	d := worktreeDraft(t, h, "unknown-without-checkout")
	i := claimWorktreeTestIntent(t, h, d, nil)
	raw, _ := json.Marshal(frozenWorktree(i))
	a, err := h.s.PrepareCreationAttempt(context.Background(), d.ID, "worktree", string(raw), h.clock.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.s.DispatchCreationAttempt(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	for j := 0; j < 2; j++ {
		h.tick()
	}
	if _, err := os.Lstat(i.WorktreePath); !os.IsNotExist(err) {
		t.Fatal("unknown dispatch was repeated")
	}
	if got := h.intent(d); got.State != "unknown" || len(h.n.starts) != 0 || h.g.groups != 0 {
		t.Fatal("uncertain checkout created downstream resources", got.State)
	}
}

func TestWorktreeSelectionValidationAndSubdirectory(t *testing.T) {
	h := newManagerHarness(t)
	initWorktreeTestRepo(t, h.root)
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "invalid.git"))
	options, selected, err := worktreeBranches(context.Background(), h.root, "", []string{h.root})
	if err != nil || selected != "refs/heads/main" || len(options) < 2 {
		t.Fatal("branch listing used an inherited Git override", selected, err)
	}
	for _, ref := range []string{"--help", "main", "refs/heads/missing", "refs/heads/main\n", "refs/heads/main~0", "refs/heads/main@{0}"} {
		if _, err := resolveWorktreeCommit(context.Background(), h.root, ref); !errors.Is(err, ErrWorktreeBranch) {
			t.Fatal("unverified branch accepted", ref, err)
		}
	}
	options, selected, err = worktreeBranches(context.Background(), h.root, "refs/heads/deleted", []string{h.root})
	if !errors.Is(err, ErrWorktreeBranch) || selected != "" || len(options) == 0 {
		t.Fatal("deleted branch left the user without replacement choices")
	}
	t.Setenv("GIT_DIR", "")
	h.m.Config.Storage.DatabaseFile = h.db
	d := h.receive("subdirectory-worktree", "/new inspect subdirectory")
	d.Directory = filepath.Join(h.root, "sub")
	d.WorkspaceMode, d.BaseBranch = "worktree", "refs/heads/main"
	d, err = h.s.UpdateCreationDraft(context.Background(), d, d.Revision, h.clock.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	h.tick()
	i := claimWorktreeTestIntent(t, h, d, nil)
	h.tick()
	if i.ExecutionDirectory() != filepath.Join(i.WorktreePath, "sub") || len(h.n.starts) != 1 || h.n.starts[0].CWD != i.ExecutionDirectory() {
		t.Fatal("selected subdirectory was widened to repository root")
	}
}

func TestWorktreeRejectsNonGitAndSymlinkStorage(t *testing.T) {
	h := newManagerHarness(t)
	d := h.receive("non-git-worktree", "/new original task")
	if err := h.action(d, "creation_submit", map[string]any{"creation_body": "keep edited task", "creation_use_worktree": true}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft("non-git-worktree")
	if d.WorkspaceMode != "worktree" || d.Body != "keep edited task" || d.ErrorCode != "worktree-repository" || h.g.groups != 0 {
		t.Fatal("non-Git worktree submission lost the draft or created a task")
	}
	initWorktreeTestRepo(t, h.root)
	d = worktreeDraft(t, h, "symlink-storage")
	i := claimWorktreeTestIntent(t, h, d, nil)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(filepath.Dir(h.db), "worktrees")); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if got := h.intent(d); got.State != "rejected" || h.g.groups != 0 || len(h.n.starts) != 0 {
		t.Fatal("worktree storage symlink was followed", got.State)
	}
	if _, err := os.Stat(i.WorktreePath); !os.IsNotExist(err) {
		t.Fatal("unsafe destination was created")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("storage validation wrote through a symlink", err)
	}
}

func TestWorktreeAvailabilityBeforeSwitchAndAfterDirectoryChange(t *testing.T) {
	h := newManagerHarness(t)
	workspaceControls := func(card string) (checked, branchDisabled bool) {
		t.Helper()
		var parsed any
		if err := json.Unmarshal([]byte(card), &parsed); err != nil {
			t.Fatal(err)
		}
		var find func(any)
		foundChecker, foundBranch := false, false
		find = func(value any) {
			switch v := value.(type) {
			case map[string]any:
				if v["tag"] == "checker" && v["name"] == "creation_use_worktree" {
					foundChecker = true
					checked, _ = v["checked"].(bool)
				}
				if v["tag"] == "select_static" && v["name"] == "creation_branch" {
					foundBranch = true
					branchDisabled, _ = v["disabled"].(bool)
				}
				for _, child := range v {
					find(child)
				}
			case []any:
				for _, child := range v {
					find(child)
				}
			}
		}
		find(parsed)
		if !foundChecker || !foundBranch {
			t.Fatal("creation card is missing workspace checker or branch selector")
		}
		return checked, branchDisabled
	}
	d := h.receive("preflight-worktree", "/new preserve task while selecting a repository")
	card := h.g.cards[d.CardID].Content
	checked, branchDisabled := workspaceControls(card)
	if !checked || !branchDisabled || !strings.Contains(card, creationErrorText("worktree-repository")) {
		t.Fatal("non-Git directory did not show a checked unavailable worktree")
	}
	if d.WorkspaceMode != "worktree" || d.ErrorCode != "" {
		t.Fatal("display preflight changed the durable draft")
	}
	repo := directory(t, h.root, "actual-repository")
	initWorktreeTestRepo(t, repo)
	if err := h.action(d, "creation_path", map[string]any{"creation_path": repo}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft("preflight-worktree")
	card = h.g.cards[d.CardID].Content
	checked, branchDisabled = workspaceControls(card)
	if d.Directory != repo || !checked || branchDisabled || strings.Contains(card, creationErrorText("worktree-repository")) {
		t.Fatal("selecting a real repository did not restore worktree mode")
	}
	if err := h.action(d, "creation_worktree", map[string]any{"creation_body": "edited task"}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft("preflight-worktree")
	if d.WorkspaceMode != "worktree" || d.BaseBranch != "refs/heads/main" || d.Body != "edited task" || len(h.n.starts) != 0 || h.g.groups != 0 {
		t.Fatal("valid workspace switch lost edits or created resources before submission")
	}
}

func TestWorktreeRequiresWholeRepositoryScope(t *testing.T) {
	h := newManagerHarness(t)
	initWorktreeTestRepo(t, h.root)
	sub := filepath.Join(h.root, "sub")
	h.m.Config.Creation.AllowedRoots = []string{sub}
	h.m.Config.Creation.CommonDirectories = []string{sub}
	h.m.Config.Storage.DatabaseFile = h.db
	d := h.receive("narrow-worktree-scope", "/new only this subdirectory")
	if err := h.action(d, "creation_worktree", nil); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft("narrow-worktree-scope")
	if d.WorkspaceMode != "worktree" || d.ErrorCode != "worktree-scope" {
		t.Fatal("failed worktree selection changed the durable mode or bypassed repository scope")
	}
	d.WorkspaceMode, d.BaseBranch = "worktree", "refs/heads/main"
	var plan store.CreationIntent
	if err := h.m.planWorktree(context.Background(), h.n, d, &plan); !errors.Is(err, ErrWorktreeScope) {
		t.Fatal("final submit could bypass repository scope", err)
	}
}

func TestWorktreeRepositoryNameCannotCollideWithMarker(t *testing.T) {
	h := newManagerHarness(t)
	h.root = directory(t, h.root, "creation.json")
	h.m.Config.Creation.AllowedRoots = []string{h.root}
	h.m.Config.Creation.CommonDirectories = []string{h.root}
	initWorktreeTestRepo(t, h.root)
	d := worktreeDraft(t, h, "marker-name-repository")
	i := claimWorktreeTestIntent(t, h, d, nil)
	h.tick()
	expected, err := legacyWorktreePath(h.db, d.ID)
	if err != nil || h.intent(d).State != "confirmed" || i.WorktreePath != expected {
		t.Fatal("repository basename collided with the private marker")
	}
}

func TestWorktreeUnknownDoesNotConfirmPartialOrLockedCheckout(t *testing.T) {
	for _, partial := range []bool{true, false} {
		t.Run(map[bool]string{true: "partial-files", false: "initializing-lock"}[partial], func(t *testing.T) {
			h := newManagerHarness(t)
			initWorktreeTestRepo(t, h.root)
			d := worktreeDraft(t, h, "incomplete-checkout")
			i := claimWorktreeTestIntent(t, h, d, nil)
			p := frozenWorktree(i)
			raw, _ := json.Marshal(p)
			a, err := h.s.PrepareCreationAttempt(context.Background(), d.ID, "worktree", string(raw), h.clock.UnixMilli())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.s.DispatchCreationAttempt(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			if err := worktreeMarker(p, true); err != nil {
				t.Fatal(err)
			}
			if partial {
				worktreeTestGit(t, h.root, "worktree", "add", "--detach", "--no-checkout", p.Destination, p.Commit)
			} else {
				worktreeTestGit(t, h.root, "worktree", "add", "--detach", p.Destination, p.Commit)
				worktreeTestGit(t, h.root, "worktree", "lock", "--reason", "initializing", p.Destination)
			}
			h.tick()
			if h.intent(d).State != "unknown" || h.g.groups != 0 || len(h.n.starts) != 0 {
				t.Fatal("incomplete checkout started a native session")
			}
		})
	}
}

func TestWorktreeAllocationLeavesAppBucketsUntouched(t *testing.T) {
	h := newManagerHarness(t)
	initWorktreeTestRepo(t, h.root)
	d := worktreeDraft(t, h, "native-bucket-collision")
	foreign := filepath.Join(h.codexHome, "worktrees", d.ID[:4])
	if err := os.MkdirAll(foreign, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(foreign, ".codex-worktree-name")
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	i := claimWorktreeTestIntent(t, h, d, nil)
	if filepath.Dir(i.WorktreePath) == foreign {
		t.Fatal("existing App bucket was reused")
	}
	h.tick()
	entries, err := os.ReadDir(foreign)
	if err != nil || len(entries) != 1 || entries[0].Name() != ".codex-worktree-name" || h.intent(d).State != "confirmed" {
		t.Fatal("App bucket changed during bridge creation", err)
	}
}

func TestWorktreeFrozenCodexHomeForeignBucketWithoutBridgeMarkerIsRejected(t *testing.T) {
	h := newManagerHarness(t)
	initWorktreeTestRepo(t, h.root)
	d := worktreeDraft(t, h, "foreign-codexhome-bucket")
	i := claimWorktreeTestIntent(t, h, d, nil)
	foreign := filepath.Join(h.codexHome, "worktrees", d.ID[:4])
	if err := os.MkdirAll(foreign, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, ".codex-worktree-name"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	frozen := filepath.Join(foreign, worktreeName(h.root, d.ID))
	if _, err := h.s.DB.Exec(`UPDATE creation_workflows SET worktree_path=? WHERE draft_id=?`, frozen, d.ID); err != nil {
		t.Fatal(err)
	}
	i.WorktreePath = frozen
	ready, err := h.m.ensureWorktree(context.Background(), h.n, i)
	if err != nil || ready {
		t.Fatalf("foreign App bucket was accepted: ready=%v err=%v", ready, err)
	}
	entries, err := os.ReadDir(foreign)
	if err != nil || len(entries) != 1 || entries[0].Name() != ".codex-worktree-name" {
		t.Fatalf("foreign App bucket was modified: entries=%v err=%v", entries, err)
	}
	if got := h.intent(d); got.State != "rejected" || len(h.n.starts) != 0 || h.g.groups != 0 {
		t.Fatalf("foreign App bucket rejection created downstream resources: %+v", got)
	}
}

func TestWorktreeLegacyUnknownKeepsOriginalPathAndJournal(t *testing.T) {
	h := newManagerHarness(t)
	initWorktreeTestRepo(t, h.root)
	d := worktreeDraft(t, h, "legacy-checkout-recovery")
	i := claimWorktreeTestIntent(t, h, d, nil)
	legacy, err := legacyWorktreePath(h.db, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.Exec(`UPDATE creation_workflows SET worktree_path=? WHERE draft_id=?`, legacy, d.ID); err != nil {
		t.Fatal(err)
	}
	i.WorktreePath = legacy
	h.g.expectedDirectory = i.ExecutionDirectory()
	p := frozenWorktree(i)
	raw, _ := json.Marshal(p)
	a, err := h.s.PrepareCreationAttempt(context.Background(), d.ID, "worktree", string(raw), h.clock.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := h.s.DispatchCreationAttempt(context.Background(), a); err != nil || !ok {
		t.Fatal(err)
	}
	if err := worktreeMarker(p, true); err != nil {
		t.Fatal(err)
	}
	worktreeTestGit(t, h.root, "worktree", "add", "--detach", p.Destination, p.Commit)
	h.tick()
	got := h.intent(d)
	if got.State != "confirmed" || got.WorktreePath != legacy || len(h.n.starts) != 1 || h.n.starts[0].CWD != legacy {
		t.Fatal("legacy workflow was relocated or recreated", got.State)
	}
	actual, err := h.s.CreationAttempt(context.Background(), d.ID, "worktree")
	if err != nil || actual.Params != string(raw) || actual.RemoteID != legacy {
		t.Fatal("legacy journal was rewritten", err)
	}
}

func TestWorktreeFrozenCodexHomeUnknownKeepsOriginalPathAndJournal(t *testing.T) {
	h := newManagerHarness(t)
	initWorktreeTestRepo(t, h.root)
	d := worktreeDraft(t, h, "legacy-checkout-recovery")
	i := claimWorktreeTestIntent(t, h, d, nil)
	frozen := filepath.Join(h.codexHome, "worktrees", d.ID[:4], worktreeName(h.root, d.ID))
	if _, err := h.s.DB.Exec(`UPDATE creation_workflows SET worktree_path=? WHERE draft_id=?`, frozen, d.ID); err != nil {
		t.Fatal(err)
	}
	i.WorktreePath = frozen
	h.g.expectedDirectory = i.ExecutionDirectory()
	p := frozenWorktree(i)
	raw, _ := json.Marshal(p)
	a, err := h.s.PrepareCreationAttempt(context.Background(), d.ID, "worktree", string(raw), h.clock.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := h.s.DispatchCreationAttempt(context.Background(), a); err != nil || !ok {
		t.Fatal(err)
	}
	if err := worktreeMarker(p, true); err != nil {
		t.Fatal(err)
	}
	worktreeTestGit(t, h.root, "worktree", "add", "--detach", p.Destination, p.Commit)
	h.tick()
	got := h.intent(d)
	if got.State != "confirmed" || got.WorktreePath != frozen || len(h.n.starts) != 1 || h.n.starts[0].CWD != frozen {
		t.Fatal("frozen CodexHome workflow was relocated or recreated", got.State)
	}
	actual, err := h.s.CreationAttempt(context.Background(), d.ID, "worktree")
	if err != nil || actual.Params != string(raw) || actual.RemoteID != frozen {
		t.Fatal("legacy journal was rewritten", err)
	}
}

func TestWorktreeFrozenCodexHomeConfirmedPathIsVerifiedInPlace(t *testing.T) {
	h := newManagerHarness(t)
	initWorktreeTestRepo(t, h.root)
	d := worktreeDraft(t, h, "confirmed-codexhome-recovery")
	i := claimWorktreeTestIntent(t, h, d, nil)
	frozen := filepath.Join(h.codexHome, "worktrees", d.ID[:4], worktreeName(h.root, d.ID))
	if _, err := h.s.DB.Exec(`UPDATE creation_workflows SET worktree_path=? WHERE draft_id=?`, frozen, d.ID); err != nil {
		t.Fatal(err)
	}
	i.WorktreePath = frozen
	p := frozenWorktree(i)
	raw, _ := json.Marshal(p)
	a, err := h.s.PrepareCreationAttempt(context.Background(), d.ID, "worktree", string(raw), h.clock.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := h.s.DispatchCreationAttempt(context.Background(), a); err != nil || !ok {
		t.Fatal("could not claim frozen CodexHome worktree", err)
	}
	if err := worktreeMarker(p, true); err != nil {
		t.Fatal(err)
	}
	worktreeTestGit(t, h.root, "worktree", "add", "--detach", p.Destination, p.Commit)
	if err := h.s.FinishCreationAttempt(context.Background(), a, "confirmed", p.Destination, `{}`); err != nil {
		t.Fatal(err)
	}
	ready, err := h.m.ensureWorktree(context.Background(), h.n, i)
	if err != nil || !ready {
		t.Fatalf("confirmed frozen CodexHome worktree was not verified in place: %v", err)
	}
	if got := h.intent(d); got.WorktreePath != frozen {
		t.Fatal("confirmed frozen worktree path changed")
	}
}
