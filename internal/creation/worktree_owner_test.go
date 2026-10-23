package creation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func newWorktreeOwnerFixture(t *testing.T) (worktreeRequest, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(base, "repository")
	if err := os.Mkdir(repository, 0700); err != nil {
		t.Fatal(err)
	}
	commit := initWorktreeTestRepo(t, repository)
	destination := filepath.Join(base, "managed", "aaaa", "project")
	worktreeTestGit(t, repository, "worktree", "add", "--detach", "--", destination, commit)
	common := worktreeTestGit(t, destination, "rev-parse", "--path-format=absolute", "--git-common-dir")
	owner := worktreeTestGit(t, destination, "rev-parse", "--path-format=absolute", "--git-path", worktreeOwnerName)
	return worktreeRequest{
		DraftID: strings.Repeat("a", 32), Source: repository, Repository: repository,
		Common: common, Branch: "refs/heads/main", Commit: commit, Destination: destination, CWD: destination,
	}, owner
}

func TestWorktreeOwnerCreateReadAndSameOwner(t *testing.T) {
	p, path := newWorktreeOwnerFixture(t)
	ctx := context.Background()
	if owner, err := readWorktreeOwner(ctx, p); err != nil || owner != "" {
		t.Fatalf("absent owner = %q, %v", owner, err)
	}
	if err := ensureWorktreeOwner(ctx, p, "synthetic-thread-a"); err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) == p.Destination || filepath.Dir(path) == p.Common {
		t.Fatal("owner was resolved outside the private worktree administrative directory")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("new owner is not private", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var owner nativeWorktreeOwner
	if json.Unmarshal(raw, &owner) != nil || owner.Version != 1 || owner.OwnerThreadID != "synthetic-thread-a" {
		t.Fatal("owner schema or thread ID did not round trip")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := ensureWorktreeOwner(ctx, p, "synthetic-thread-a"); err != nil {
		t.Fatal("same App-compatible owner was rejected", err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(info, after) || after.Mode().Perm() != 0644 {
		t.Fatal("idempotent registration replaced an existing owner", err)
	}
	if got, err := readWorktreeOwner(ctx, p); err != nil || got != owner.OwnerThreadID {
		t.Fatalf("owner readback = %q, %v", got, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".codex-thread-") {
			t.Fatal("successful registration left a temporary file")
		}
	}
}

func TestWorktreeOwnerRefusesExistingConflictOrMalformedFile(t *testing.T) {
	for name, raw := range map[string]string{
		"other-owner":   `{"version":1,"ownerThreadId":"synthetic-other"}`,
		"invalid-json":  `{"version":1,`,
		"wrong-version": `{"version":2,"ownerThreadId":"synthetic-thread-a"}`,
		"missing-owner": `{"version":1}`,
		"trailing-json": `{"version":1,"ownerThreadId":"synthetic-thread-a"} {}`,
		"too-large":     strings.Repeat(" ", worktreeControlLimit+1),
	} {
		t.Run(name, func(t *testing.T) {
			p, path := newWorktreeOwnerFixture(t)
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if err := ensureWorktreeOwner(context.Background(), p, "synthetic-thread-a"); !errors.Is(err, ErrWorktreeIdentity) {
				t.Fatalf("existing owner was not rejected: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != raw {
				t.Fatal("rejected registration changed an existing file", err)
			}
		})
	}
}

func TestWorktreeOwnerConcurrentCreateOnly(t *testing.T) {
	for _, same := range []bool{false, true} {
		t.Run(map[bool]string{false: "different-owners", true: "same-owner"}[same], func(t *testing.T) {
			p, _ := newWorktreeOwnerFixture(t)
			ids := []string{"synthetic-thread-a", "synthetic-thread-b"}
			if same {
				ids[1] = ids[0]
			}
			start := make(chan struct{})
			errs := make([]error, len(ids))
			var wg sync.WaitGroup
			for n := range ids {
				wg.Add(1)
				go func(n int) {
					defer wg.Done()
					<-start
					errs[n] = ensureWorktreeOwner(context.Background(), p, ids[n])
				}(n)
			}
			close(start)
			wg.Wait()
			owner, err := readWorktreeOwner(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			succeeded := 0
			for n, err := range errs {
				if err == nil {
					succeeded++
					if owner != ids[n] {
						t.Fatal("successful writer did not own the resulting file")
					}
				} else if !errors.Is(err, ErrWorktreeIdentity) {
					t.Fatal("unexpected concurrent registration failure", err)
				}
			}
			if same && succeeded != 2 || !same && succeeded != 1 {
				t.Fatalf("successful writers = %d (same owner: %v)", succeeded, same)
			}
		})
	}
}

func TestWorktreeOwnerFreshResolutionAfterAdministrativeSlotReuse(t *testing.T) {
	p, oldOwnerPath := newWorktreeOwnerFixture(t)
	ctx := context.Background()
	if err := ensureWorktreeOwner(ctx, p, "synthetic-old-a"); err != nil {
		t.Fatal(err)
	}
	worktreeTestGit(t, p.Repository, "worktree", "remove", "--", p.Destination)
	b := p
	b.Destination = filepath.Join(filepath.Dir(filepath.Dir(p.Destination)), "bbbb", "project")
	b.CWD = b.Destination
	worktreeTestGit(t, p.Repository, "worktree", "add", "--detach", "--", b.Destination, p.Commit)
	bOwnerPath := worktreeTestGit(t, b.Destination, "rev-parse", "--path-format=absolute", "--git-path", worktreeOwnerName)
	if oldOwnerPath != bOwnerPath {
		t.Fatal("fixture did not reuse the deleted administrative slot")
	}
	if err := ensureWorktreeOwner(ctx, b, "synthetic-thread-b"); err != nil {
		t.Fatal(err)
	}
	worktreeTestGit(t, p.Repository, "worktree", "add", "--detach", "--", p.Destination, p.Commit)
	newOwnerPath := worktreeTestGit(t, p.Destination, "rev-parse", "--path-format=absolute", "--git-path", worktreeOwnerName)
	if oldOwnerPath == newOwnerPath {
		t.Fatal("fixture did not allocate a new administrative slot")
	}
	if owner, err := readWorktreeOwner(ctx, p); err != nil || owner != "" {
		t.Fatal("rebuilt checkout inherited the old or sibling owner", err)
	}
	if err := ensureWorktreeOwner(ctx, p, "synthetic-new-a"); err != nil {
		t.Fatal(err)
	}
	if owner, err := readWorktreeOwner(ctx, b); err != nil || owner != "synthetic-thread-b" {
		t.Fatal("fresh registration changed the sibling owner", err)
	}
}

func TestWorktreeOwnerIdentityPersistsThroughTaskChanges(t *testing.T) {
	p, _ := newWorktreeOwnerFixture(t)
	ctx := context.Background()
	identity, err := worktreeOwnerIdentity(ctx, p)
	if err != nil || !strings.HasPrefix(identity, "v1:") {
		t.Fatal("could not capture the worktree generation", err)
	}
	if err := ensureWorktreeOwnerWithIdentity(ctx, p, "synthetic-thread-a", identity); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Destination, "task.txt"), []byte("synthetic task change\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Destination, "new-task.txt"), []byte("synthetic added file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	worktreeTestGit(t, p.Destination, "add", "task.txt", "new-task.txt")
	worktreeTestGit(t, p.Destination, "commit", "-m", "synthetic task changes")
	after, err := worktreeOwnerIdentity(ctx, p)
	if err != nil || after != identity {
		t.Fatal("task edits, commit or owner publication changed the generation", err)
	}
	if err := ensureWorktreeOwnerWithIdentity(ctx, p, "synthetic-thread-a", identity); err != nil {
		t.Fatal("completed registration did not remain idempotent", err)
	}
}

func TestWorktreeOwnerIdentityRefusesSamePathReplacement(t *testing.T) {
	for _, replacement := range []string{"checkout", "gitfile"} {
		t.Run(replacement, func(t *testing.T) {
			p, path := newWorktreeOwnerFixture(t)
			ctx := context.Background()
			before, err := worktreeOwnerIdentity(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			if replacement == "checkout" {
				worktreeTestGit(t, p.Repository, "worktree", "remove", "--", p.Destination)
				worktreeTestGit(t, p.Repository, "worktree", "add", "--detach", "--", p.Destination, p.Commit)
			} else {
				pointer := filepath.Join(p.Destination, ".git")
				raw, err := os.ReadFile(pointer)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(pointer+".replacement", raw, 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(pointer+".replacement", pointer); err != nil {
					t.Fatal(err)
				}
			}
			after, err := worktreeOwnerIdentity(ctx, p)
			if err != nil || after == before {
				t.Fatal("replacement did not change the generation", err)
			}
			if err := ensureWorktreeOwnerWithIdentity(ctx, p, "synthetic-thread-a", before); !errors.Is(err, ErrWorktreeIdentity) {
				t.Fatal("registration adopted a replacement checkout", err)
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatal("rejected stale registration published an owner", err)
			}
			if err := ensureWorktreeOwnerWithIdentity(ctx, p, "synthetic-new-task", after); err != nil {
				t.Fatal("explicitly authorized replacement could not be registered", err)
			}
		})
	}
}

func TestWorktreeOwnerRejectsGitfilePointingToSibling(t *testing.T) {
	p, ownerPath := newWorktreeOwnerFixture(t)
	sibling := filepath.Join(filepath.Dir(filepath.Dir(p.Destination)), "bbbb", "project")
	worktreeTestGit(t, p.Repository, "worktree", "add", "--detach", "--", sibling, p.Commit)
	siblingGitfile, err := os.ReadFile(filepath.Join(sibling, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Destination, ".git"), siblingGitfile, 0644); err != nil {
		t.Fatal(err)
	}
	// These two checks alone still describe A even though owner resolution now
	// points to B's private directory. The reverse gitdir pointer must disagree.
	if got := worktreeTestGit(t, p.Destination, "rev-parse", "--show-toplevel"); got != p.Destination {
		t.Fatal("fixture no longer demonstrates an apparently valid checkout root")
	}
	if got := worktreeTestGit(t, p.Destination, "rev-parse", "--path-format=absolute", "--git-common-dir"); got != p.Common {
		t.Fatal("fixture no longer shares the expected common directory")
	}
	if err := ensureWorktreeOwner(context.Background(), p, "synthetic-thread-a"); !errors.Is(err, ErrWorktreeIdentity) {
		t.Fatal("sibling Git metadata was accepted", err)
	}
	for _, path := range []string{ownerPath, worktreeTestGit(t, sibling, "rev-parse", "--path-format=absolute", "--git-path", worktreeOwnerName)} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("rejected registration wrote an owner", err)
		}
	}
}

func TestWorktreeOwnerRejectsUnsafeControlFiles(t *testing.T) {
	for _, name := range []string{"checkout-gitfile", "backlink", "commondir", "owner"} {
		t.Run(name+"-symlink", func(t *testing.T) {
			p, ownerPath := newWorktreeOwnerFixture(t)
			path := map[string]string{
				"checkout-gitfile": filepath.Join(p.Destination, ".git"),
				"backlink":         filepath.Join(filepath.Dir(ownerPath), "gitdir"),
				"commondir":        filepath.Join(filepath.Dir(ownerPath), "commondir"),
				"owner":            ownerPath,
			}[name]
			if name == "owner" {
				if err := os.WriteFile(path, []byte(`{"version":1,"ownerThreadId":"synthetic-thread-a"}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Rename(path, path+".saved"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Base(path)+".saved", path); err != nil {
				t.Fatal(err)
			}
			if err := ensureWorktreeOwner(context.Background(), p, "synthetic-thread-a"); !errors.Is(err, ErrWorktreeIdentity) {
				t.Fatal("symlinked control file was accepted", err)
			}
		})
	}
	for _, name := range []string{"checkout-gitfile", "backlink", "commondir", "owner"} {
		t.Run(name+"-shared-write", func(t *testing.T) {
			p, ownerPath := newWorktreeOwnerFixture(t)
			path := map[string]string{
				"checkout-gitfile": filepath.Join(p.Destination, ".git"),
				"backlink":         filepath.Join(filepath.Dir(ownerPath), "gitdir"),
				"commondir":        filepath.Join(filepath.Dir(ownerPath), "commondir"),
				"owner":            ownerPath,
			}[name]
			if name == "owner" {
				if err := os.WriteFile(path, []byte(`{"version":1,"ownerThreadId":"synthetic-thread-a"}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(path, 0660); err != nil {
				t.Fatal(err)
			}
			if err := ensureWorktreeOwner(context.Background(), p, "synthetic-thread-a"); !errors.Is(err, ErrWorktreeIdentity) {
				t.Fatal("shared-writable control file was accepted", err)
			}
		})
	}
	t.Run("special-owner", func(t *testing.T) {
		p, ownerPath := newWorktreeOwnerFixture(t)
		if err := unix.Mkfifo(ownerPath, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readWorktreeOwner(context.Background(), p); !errors.Is(err, ErrWorktreeIdentity) {
			t.Fatal("special owner file was accepted", err)
		}
	})
	t.Run("symlinked-private-gitdir", func(t *testing.T) {
		p, ownerPath := newWorktreeOwnerFixture(t)
		directory := filepath.Dir(ownerPath)
		if err := os.Rename(directory, directory+".saved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Base(directory)+".saved", directory); err != nil {
			t.Fatal(err)
		}
		if err := ensureWorktreeOwner(context.Background(), p, "synthetic-thread-a"); !errors.Is(err, ErrWorktreeIdentity) {
			t.Fatal("symlinked private administrative directory was accepted", err)
		}
	})
}

func TestWorktreeOwnerRejectsSharedOrChangedIdentity(t *testing.T) {
	t.Run("main-checkout", func(t *testing.T) {
		p, _ := newWorktreeOwnerFixture(t)
		p.Destination, p.CWD = p.Repository, p.Repository
		if err := ensureWorktreeOwner(context.Background(), p, "synthetic-thread-a"); !errors.Is(err, ErrWorktreeIdentity) {
			t.Fatal("main/shared Git directory was accepted", err)
		}
	})
	t.Run("wrong-common", func(t *testing.T) {
		p, _ := newWorktreeOwnerFixture(t)
		p.Common = p.Repository
		if err := ensureWorktreeOwner(context.Background(), p, "synthetic-thread-a"); !errors.Is(err, ErrWorktreeIdentity) {
			t.Fatal("changed frozen common directory was accepted", err)
		}
	})
	t.Run("recreated-during-operation", func(t *testing.T) {
		p, _ := newWorktreeOwnerFixture(t)
		original, err := resolveWorktreeOwner(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		defer original.close()
		worktreeTestGit(t, p.Repository, "worktree", "remove", "--", p.Destination)
		worktreeTestGit(t, p.Repository, "worktree", "add", "--detach", "--", p.Destination, p.Commit)
		if err := original.verify(context.Background(), p); !errors.Is(err, ErrWorktreeIdentity) {
			t.Fatal("same-path replacement was accepted as the original checkout", err)
		}
	})
	t.Run("wrong-owner-uid", func(t *testing.T) {
		if os.Geteuid() != 0 {
			t.Skip("changing synthetic file ownership requires root")
		}
		p, path := newWorktreeOwnerFixture(t)
		if err := os.WriteFile(path, []byte(`{"version":1,"ownerThreadId":"synthetic-thread-a"}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(path, 1, -1); err != nil {
			t.Fatal(err)
		}
		if err := ensureWorktreeOwner(context.Background(), p, "synthetic-thread-a"); !errors.Is(err, ErrWorktreeIdentity) {
			t.Fatal("owner file belonging to another UID was accepted", err)
		}
	})
}

func TestWorktreeOwnerCancellationDoesNotPublish(t *testing.T) {
	p, path := newWorktreeOwnerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ensureWorktreeOwner(ctx, p, "synthetic-thread-a"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled registration did not preserve context error", err)
	}
	if _, err := readWorktreeOwner(ctx, p); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled read did not preserve context error", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("cancelled registration published an owner", err)
	}
}

func TestWorktreeOwnerRejectsInvalidThreadID(t *testing.T) {
	p, path := newWorktreeOwnerFixture(t)
	for _, id := range []string{"", " ", " synthetic-thread", "synthetic-thread\n", strings.Repeat("x", 513)} {
		if err := ensureWorktreeOwner(context.Background(), p, id); !errors.Is(err, ErrWorktreeIdentity) {
			t.Fatal("invalid thread identity was accepted", err)
		}
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("invalid thread identity published an owner", err)
	}
}

func TestWorktreeOwnerCancellationDuringFreshResolution(t *testing.T) {
	p, path := newWorktreeOwnerFixture(t)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required for the cancellation scheduling fixture")
	}
	bin := t.TempDir()
	signal := filepath.Join(bin, "owner-resolution-started")
	gitJSON, _ := json.Marshal(git)
	signalJSON, _ := json.Marshal(signal)
	script := "#!" + python + "\nimport os, pathlib, sys, time\nif 'codex-thread.json' in sys.argv:\n pathlib.Path(" + string(signalJSON) + ").touch()\n time.sleep(20)\nos.execv(" + string(gitJSON) + ", ['git'] + sys.argv[1:])\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- ensureWorktreeOwner(ctx, p, "synthetic-thread-a") }()
	deadline := time.NewTimer(4 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		if _, err := os.Stat(signal); err == nil {
			break
		}
		select {
		case err := <-finished:
			t.Fatal("registration ended before the scheduled cancellation", err)
		case <-deadline.C:
			t.Fatal("registration did not reach fresh owner resolution")
		case <-poll.C:
		}
	}
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal("in-flight cancellation did not preserve context error", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("cancelled resolution published an owner", err)
	}
}
