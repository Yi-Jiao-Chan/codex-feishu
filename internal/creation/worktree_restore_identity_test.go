package creation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"codex-feishu/internal/store"
)

type restoreIdentityFixture struct {
	source, home, destination, common, commit string
}

func newRestoreIdentityFixture(t *testing.T) restoreIdentityFixture {
	t.Helper()
	base := t.TempDir()
	source := filepath.Join(base, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	commit := initWorktreeTestRepo(t, source)
	home := filepath.Join(base, "daemon-home")
	destination := filepath.Join(home, "worktrees", "abcd", "project")
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		t.Fatal(err)
	}
	worktreeTestGit(t, source, "worktree", "add", "--detach", "--", destination, commit)
	common := worktreeTestGit(t, destination, "rev-parse", "--path-format=absolute", "--git-common-dir")
	return restoreIdentityFixture{source: source, home: home, destination: destination, common: common, commit: commit}
}

func TestInspectWorktreeRestoreIdentityKeepsManagedRootAndRelativeCWD(t *testing.T) {
	fixture := newRestoreIdentityFixture(t)
	scope := store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}
	ctx := context.Background()

	root, err := InspectWorktreeRestoreIdentity(ctx, scope, fixture.destination, fixture.source, fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	if root.RawCWD != fixture.destination || root.Root != fixture.destination || root.SourceRoot != fixture.source || root.CommonDir != fixture.common || root.RelativeCWD != "." {
		t.Fatalf("root identity widened or changed: %+v", root)
	}

	subdir := filepath.Join(fixture.destination, "sub")
	sub, err := InspectWorktreeRestoreIdentity(ctx, scope, subdir, fixture.source, fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	if sub.RawCWD != subdir || sub.Root != fixture.destination || sub.SourceRoot != fixture.source || sub.CommonDir != fixture.common || sub.RelativeCWD != "sub" {
		t.Fatalf("subdirectory identity was not exact: %+v", sub)
	}
	if err := AuthorizeWorktreeRestoreSource(ctx, sub, []string{fixture.source}); err != nil {
		t.Fatalf("source root did not authorize its original child: %v", err)
	}
	if err := AuthorizeWorktreeRestoreSource(ctx, sub, []string{subdir}); err != nil {
		t.Fatalf("original subdirectory was not authorized: %v", err)
	}
	sibling := filepath.Join(fixture.source, "sibling")
	if err := os.Mkdir(sibling, 0700); err != nil {
		t.Fatal(err)
	}
	if err := AuthorizeWorktreeRestoreSource(ctx, sub, []string{sibling}); !errors.Is(err, ErrWorktreeScope) {
		t.Fatalf("authorization expanded from the original child to a sibling: %v", err)
	}

	outside := filepath.Join(filepath.Dir(fixture.home), "outside", "project")
	if err := os.MkdirAll(filepath.Dir(outside), 0700); err != nil {
		t.Fatal(err)
	}
	worktreeTestGit(t, fixture.source, "worktree", "add", "--detach", "--", outside, fixture.commit)
	if _, err := InspectWorktreeRestoreIdentity(ctx, scope, outside, fixture.source, fixture.home); !errors.Is(err, ErrWorktreeScope) {
		t.Fatalf("worktree outside managed root was accepted: %v", err)
	}

	p := worktreeRequest{Source: fixture.source, Repository: fixture.source, Common: fixture.common, Destination: fixture.destination, CWD: fixture.destination}
	if err := ensureWorktreeOwner(ctx, p, "foreign-thread"); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectWorktreeRestoreIdentity(ctx, scope, fixture.destination, fixture.source, fixture.home); !errors.Is(err, ErrWorktreeIdentity) {
		t.Fatalf("foreign owner was accepted: %v", err)
	}
}

func TestLocateWorktreeSnapshotIdentityRequiresExactRefAndTree(t *testing.T) {
	fixture := newRestoreIdentityFixture(t)
	scope := store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}
	ctx := context.Background()
	subdir := filepath.Join(fixture.destination, "sub")
	if err := os.RemoveAll(fixture.destination); err != nil {
		t.Fatal(err)
	}
	ref := WorktreeSnapshotRef(fixture.destination)
	worktreeTestGit(t, fixture.source, "update-ref", ref, fixture.commit)

	located, err := LocateWorktreeSnapshotIdentity(ctx, scope, subdir, fixture.source, fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	if located.RawCWD != subdir || located.Root != fixture.destination || located.SourceRoot != fixture.source || located.CommonDir != fixture.common || located.RelativeCWD != "sub" {
		t.Fatalf("snapshot identity did not preserve exact paths: %+v", located)
	}
	sep := string(os.PathSeparator)
	alias := fixture.home + sep + "worktrees" + sep + "abcd" + sep + ".." + sep + "abcd" + sep + "project"
	if ref == WorktreeSnapshotRef(alias) {
		t.Fatal("snapshot reference canonicalized a raw path alias")
	}

	worktreeTestGit(t, fixture.source, "update-ref", "-d", ref)
	if _, err := LocateWorktreeSnapshotIdentity(ctx, scope, subdir, fixture.source, fixture.home); !errors.Is(err, ErrWorktreeSnapshotMissing) {
		t.Fatalf("missing snapshot ref did not fail closed: %v", err)
	}
	worktreeTestGit(t, fixture.source, "update-ref", ref, fixture.commit)
	fileCWD := filepath.Join(fixture.destination, "task.txt")
	if _, err := LocateWorktreeSnapshotIdentity(ctx, scope, fileCWD, fixture.source, fixture.home); !errors.Is(err, ErrWorktreeIdentity) {
		t.Fatalf("snapshot accepted a blob where a subdirectory tree was required: %v", err)
	}
}
