package rules

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestMissingDirectoryClassification(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{filepath.Join(base, "missing"), file, filepath.Join(file, "child")} {
		if _, err := ResolveScope(context.Background(), cwd, []string{base}); !errors.Is(err, ErrDirectoryMissing) {
			t.Fatalf("missing/non-directory cwd did not suspend: %v", err)
		}
	}
	if _, err := ResolveScope(context.Background(), base, []string{filepath.Join(base, "missing")}); err == nil || errors.Is(err, ErrDirectoryMissing) {
		t.Fatalf("configured root failure classified as missing cwd: %v", err)
	}
	for _, cause := range []error{syscall.EACCES, syscall.EPERM, syscall.EIO, syscall.ESTALE} {
		if err := directoryError(&os.PathError{Op: "stat", Path: "/private/fixture", Err: cause}); errors.Is(err, ErrDirectoryMissing) || err.Error() != "directory unavailable" {
			t.Fatalf("transient/permission error suspended or leaked path: %v", err)
		}
	}
}

func testGit(t *testing.T, cwd string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func TestRegisteredWorktreeRespectsSubdirectoryAndProjectMute(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	project := filepath.Join(base, "original project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	testGit(t, project, "init", "-q")
	testGit(t, project, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "fixture")
	worktree := filepath.Join(base, "worktree")
	testGit(t, project, "worktree", "add", "--detach", worktree)
	for _, path := range []string{filepath.Join(project, "a"), filepath.Join(worktree, "a"), filepath.Join(worktree, "abc")} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	// Ambient Git variables must not fabricate this candidate's repository.
	t.Setenv("GIT_DIR", filepath.Join(project, ".git"))
	t.Setenv("GIT_WORK_TREE", project)
	scope, err := ResolveScope(ctx, filepath.Join(worktree, "a"), []string{filepath.Join(project, "a")})
	if err != nil || !scope.Allowed || !scope.Worktree || scope.ProjectCWD != filepath.Join(project, "a") {
		t.Fatalf("registered worktree rejected: %+v %v", scope, err)
	}
	if _, muted := Muted(scope, []DirectoryRule{{Path: project}}); muted {
		t.Fatal("exact root mute widened to descendants")
	}
	if _, muted := Muted(scope, []DirectoryRule{{Path: project, Recursive: true}}); !muted {
		t.Fatal("worktree bypassed recursive project mute")
	}
	if _, muted := Muted(scope, []DirectoryRule{{Path: filepath.Join(project, "a")}}); !muted {
		t.Fatal("worktree bypassed mapped exact mute")
	}
	denied, err := ResolveScope(ctx, filepath.Join(worktree, "abc"), []string{filepath.Join(project, "a")})
	if err != nil || denied.Allowed {
		t.Fatalf("allowed subdir widened: %+v %v", denied, err)
	}
	// Copying a .git backlink is insufficient without a registered worktree.
	forged := filepath.Join(base, "unregistered")
	if err := os.Mkdir(forged, 0700); err != nil {
		t.Fatal(err)
	}
	backlink, err := os.ReadFile(filepath.Join(worktree, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(forged, ".git"), backlink, 0600); err != nil {
		t.Fatal(err)
	}
	denied, err = ResolveScope(ctx, forged, []string{project})
	if err != nil || denied.Allowed {
		t.Fatalf("unregistered checkout allowed: %+v %v", denied, err)
	}
}

func TestCanonicalRulesAndBoundary(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "a")
	child := filepath.Join(project, "child")
	if err := os.MkdirAll(child, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(project, alias); err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalDirectory(alias)
	if err != nil || canonical != project {
		t.Fatalf("symlink not canonical: %s %v", canonical, err)
	}
	scope, err := ResolveScope(context.Background(), filepath.Join(alias, "child"), []string{project})
	if err != nil || !scope.Allowed || scope.CWD != child {
		t.Fatalf("physical scope: %+v %v", scope, err)
	}
	if _, muted := Muted(scope, []DirectoryRule{{Path: canonical}}); muted {
		t.Fatal("exact mute matched child")
	}
	if _, muted := Muted(scope, []DirectoryRule{{Path: canonical, Recursive: true}}); !muted {
		t.Fatal("recursive mute missed child")
	}
	if Inside(project, filepath.Join(base, "abc")) {
		t.Fatal("prefix collision")
	}
	if _, err := CanonicalDirectory("relative"); err == nil {
		t.Fatal("relative rule accepted")
	}
}
