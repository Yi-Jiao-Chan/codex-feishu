package creation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
	"golang.org/x/sys/unix"
)

var (
	ErrWorktreeRepository = errors.New("creation requires a registered Git checkout")
	ErrWorktreeBranch     = errors.New("creation base branch is unavailable")
	ErrWorktreeStorage    = errors.New("creation worktree storage is unavailable")
	ErrWorktreeIdentity   = errors.New("creation worktree identity cannot be verified")
	ErrWorktreeScope      = errors.New("creation worktree requires an authorized repository root")
)

type gitCheckout struct{ Root, Common string }
type worktreeRecord struct {
	Path, Commit, Branch string
	Incomplete           bool
}

// Git output and errors never enter logs or cards: they can contain paths,
// remote URLs and repository content. Inherited Git overrides cannot redirect
// a command away from the selected physical checkout.
type gitBuffer struct {
	bytes.Buffer
	overflow bool
}

func (b *gitBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := (512 << 10) - b.Len()
	if len(p) > remaining {
		p, b.overflow = p[:remaining], true
	}
	b.Buffer.Write(p)
	return n, nil
}

func worktreeGit(ctx context.Context, directory string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}, args...)...)
	cmd.Dir, cmd.WaitDelay = directory, time.Second
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		if !strings.HasPrefix(key, "GIT_") && key != "LC_ALL" {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var output gitBuffer
	cmd.Stdout, cmd.Stderr = &output, io.Discard
	if err := cmd.Run(); err != nil || output.overflow {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", ErrWorktreeRepository
	}
	return strings.TrimRight(output.String(), "\r\n"), nil
}

func registeredWorktrees(ctx context.Context, directory string) ([]worktreeRecord, error) {
	raw, err := worktreeGit(ctx, directory, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	var records []worktreeRecord
	for _, block := range strings.Split(raw, "\x00\x00") {
		var r worktreeRecord
		for _, field := range strings.Split(block, "\x00") {
			switch {
			case strings.HasPrefix(field, "worktree "):
				r.Path = strings.TrimPrefix(field, "worktree ")
			case strings.HasPrefix(field, "HEAD "):
				r.Commit = strings.TrimPrefix(field, "HEAD ")
			case strings.HasPrefix(field, "branch "):
				r.Branch = strings.TrimPrefix(field, "branch ")
			case field == "locked" || strings.HasPrefix(field, "locked ") || field == "prunable" || strings.HasPrefix(field, "prunable "):
				r.Incomplete = true
			}
		}
		if r.Path != "" {
			records = append(records, r)
		}
	}
	return records, nil
}

func inspectCheckout(ctx context.Context, directory string) (gitCheckout, error) {
	root, err := worktreeGit(ctx, directory, "rev-parse", "--show-toplevel")
	if err != nil {
		return gitCheckout{}, err
	}
	root, err = physicalDirectory(root)
	if err != nil || !rules.Inside(root, directory) {
		return gitCheckout{}, ErrWorktreeRepository
	}
	common, err := worktreeGit(ctx, directory, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return gitCheckout{}, err
	}
	common, err = physicalDirectory(common)
	if err != nil {
		return gitCheckout{}, ErrWorktreeRepository
	}
	records, err := registeredWorktrees(ctx, directory)
	if err != nil {
		return gitCheckout{}, err
	}
	for _, r := range records {
		if actual, err := rules.CanonicalDirectory(r.Path); err == nil && actual == root && r.Commit != "" {
			return gitCheckout{Root: root, Common: common}, nil
		}
	}
	return gitCheckout{}, ErrWorktreeRepository
}

func resolveWorktreeCommit(ctx context.Context, directory, ref string) (string, error) {
	if ref != "HEAD" && !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/remotes/") || strings.IndexFunc(ref, unicode.IsControl) >= 0 {
		return "", ErrWorktreeBranch
	}
	var commit string
	var err error
	if ref == "HEAD" {
		commit, err = worktreeGit(ctx, directory, "rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
	} else {
		if _, err := worktreeGit(ctx, directory, "check-ref-format", ref); err != nil {
			return "", ErrWorktreeBranch
		}
		commit, err = worktreeGit(ctx, directory, "show-ref", "--verify", "--hash", ref)
	}
	if err != nil || len(commit) != 40 && len(commit) != 64 || strings.Trim(commit, "0123456789abcdef") != "" {
		return "", ErrWorktreeBranch
	}
	if kind, err := worktreeGit(ctx, directory, "cat-file", "-t", commit); err != nil || kind != "commit" {
		return "", ErrWorktreeBranch
	}
	return commit, nil
}

func worktreeBranches(ctx context.Context, directory, selected string, roots []string) ([]cards.CreationOption, string, error) {
	checkout, err := inspectCheckout(ctx, directory)
	if err != nil {
		return nil, "", err
	}
	if !(DirectoryService{Roots: roots}).allowed(checkout.Root) {
		return nil, "", ErrWorktreeScope
	}
	if _, err := resolveWorktreeCommit(ctx, directory, "HEAD"); err != nil {
		return nil, "", err
	}
	current, _ := worktreeGit(ctx, directory, "symbolic-ref", "--quiet", "HEAD")
	if selected == "" {
		selected = current
		if selected == "" {
			selected = "HEAD"
		}
	}
	var selectionErr error
	if _, err := resolveWorktreeCommit(ctx, directory, selected); err != nil {
		// Still offer live branches after the previously selected ref was
		// deleted. Never silently replace that explicit choice with another.
		selected, selectionErr = "", ErrWorktreeBranch
	}
	raw, err := worktreeGit(ctx, directory, "for-each-ref", "--sort=refname", "--format=%(refname)", "refs/heads/", "refs/remotes/")
	if err != nil {
		return nil, "", err
	}
	refs := strings.Split(raw, "\n")
	if len(refs) > maxDirectoryCandidates {
		return nil, "", ErrWorktreeBranch
	}
	var options []cards.CreationOption
	seen := map[string]bool{}
	for _, ref := range append([]string{selected, current, "refs/heads/main", "refs/heads/master", "HEAD"}, refs...) {
		if ref == "" || seen[ref] || ref != "HEAD" && !strings.Contains("\n"+raw+"\n", "\n"+ref+"\n") {
			continue
		}
		seen[ref] = true
		label := strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/remotes/")
		if ref == "HEAD" {
			label = "当前提交（HEAD）"
		}
		options = append(options, cards.CreationOption{Label: label, Value: ref})
		if len(options) == 100 {
			break
		}
	}
	return options, selected, selectionErr
}

func legacyWorktreePath(database, draftID string) (string, error) {
	if !filepath.IsAbs(database) || len(draftID) != 32 || strings.Trim(draftID, "0123456789abcdef") != "" {
		return "", ErrWorktreeStorage
	}
	parent, err := rules.CanonicalDirectory(filepath.Dir(database))
	if err != nil {
		return "", ErrWorktreeStorage
	}
	return filepath.Join(parent, "worktrees", draftID, "checkout"), nil
}

func nativeWorktreeHome(n Native) (string, error) {
	provider, ok := n.(interface{ CodexHome() string })
	if !ok {
		return "", ErrWorktreeStorage
	}
	home, err := physicalDirectory(provider.CodexHome())
	if err != nil {
		return "", ErrWorktreeStorage
	}
	return home, nil
}

func worktreeName(root, draftID string) string {
	name := filepath.Base(root)
	if name == "." || name == string(filepath.Separator) || name == ".codex-worktree-name" || name == ".codex-feishu-"+draftID+".json" {
		return "workspace"
	}
	return name
}

func plannedWorktreePath(home, draftID, root string) (string, error) {
	if !filepath.IsAbs(home) || len(draftID) != 32 || strings.Trim(draftID, "0123456789abcdef") != "" {
		return "", ErrWorktreeStorage
	}
	for attempt := 0; attempt < 256; attempt++ {
		bucket := draftID[:4]
		if attempt > 0 {
			digest := sha256.Sum256([]byte(draftID + ":" + strconv.Itoa(attempt)))
			bucket = hex.EncodeToString(digest[:2])
		}
		parent := filepath.Join(home, "worktrees", bucket)
		if _, err := os.Lstat(parent); os.IsNotExist(err) {
			return filepath.Join(parent, worktreeName(root, draftID)), nil
		} else if err != nil {
			return "", ErrWorktreeStorage
		}
	}
	return "", ErrWorktreeStorage
}

// Planning reads only Git metadata. The exact commit and destination are
// frozen with the creation intent before any filesystem or native writes.
func (m *Manager) planWorktree(ctx context.Context, n Native, d store.CreationDraft, i *store.CreationIntent) error {
	checkout, err := inspectCheckout(ctx, d.Directory)
	if err != nil {
		return err
	}
	if !m.directories(nil).allowed(checkout.Root) {
		return ErrWorktreeScope
	}
	commit, err := resolveWorktreeCommit(ctx, d.Directory, d.BaseBranch)
	if err != nil {
		return err
	}
	home, err := nativeWorktreeHome(n)
	if err != nil {
		return err
	}
	path, err := plannedWorktreePath(home, d.ID, checkout.Root)
	if err != nil || rules.Inside(checkout.Common, path) {
		return ErrWorktreeStorage
	}
	rel, _ := filepath.Rel(checkout.Root, d.Directory)
	if rel != "." {
		kind, err := worktreeGit(ctx, d.Directory, "cat-file", "-t", commit+":"+filepath.ToSlash(rel))
		if err != nil || kind != "tree" {
			return ErrWorktreeBranch
		}
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return ErrWorktreeStorage
	}
	i.BaseCommit, i.GitRoot, i.GitCommonDir, i.WorktreePath = commit, checkout.Root, checkout.Common, path
	return nil
}

type worktreeRequest struct {
	DraftID, Source, Repository, Common, Branch, Commit, Destination, CWD string
}

func frozenWorktree(i store.CreationIntent) worktreeRequest {
	return worktreeRequest{i.DraftID, i.Directory, i.GitRoot, i.GitCommonDir, i.BaseBranch, i.BaseCommit, i.WorktreePath, i.ExecutionDirectory()}
}

func privateWorktreeDirectory(path string, create bool) error {
	if create {
		if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
			return ErrWorktreeStorage
		}
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return ErrWorktreeStorage
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return ErrWorktreeStorage
	}
	return nil
}

func worktreeMarker(p worktreeRequest, create bool) error {
	parent := filepath.Dir(p.Destination)
	if err := privateWorktreeDirectory(filepath.Dir(parent), create); err != nil {
		return err
	}
	legacy := filepath.Base(parent) == p.DraftID && filepath.Base(p.Destination) == "checkout"
	if create && !legacy {
		// A short bucket is shared with the App's allocator. Never add a
		// marker to an existing bucket unless it already proves our intent.
		if err := os.Mkdir(parent, 0700); err != nil {
			if !os.IsExist(err) {
				return ErrWorktreeStorage
			}
			create = false
		}
	}
	if err := privateWorktreeDirectory(parent, create); err != nil {
		return err
	}
	raw, _ := json.Marshal(p)
	name := ".codex-feishu-" + p.DraftID + ".json"
	if legacy {
		name = "creation.json"
	}
	path := filepath.Join(parent, name)
	if create {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			_, err = f.Write(raw)
			if err == nil {
				err = f.Sync()
			}
			closeErr := f.Close()
			if err != nil || closeErr != nil {
				return ErrWorktreeStorage
			}
			dir, err := os.Open(parent)
			if err != nil {
				return ErrWorktreeStorage
			}
			err = dir.Sync()
			dir.Close()
			if err != nil {
				return ErrWorktreeStorage
			}
		} else if !os.IsExist(err) {
			return ErrWorktreeStorage
		}
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() != int64(len(raw)) {
		return ErrWorktreeIdentity
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, raw) {
		return ErrWorktreeIdentity
	}
	return nil
}

func verifyWorktree(ctx context.Context, p worktreeRequest, birth bool) error {
	if err := worktreeMarker(p, false); err != nil {
		return err
	}
	root, err := rules.CanonicalDirectory(p.Destination)
	if err != nil || root != p.Destination {
		return ErrWorktreeIdentity
	}
	checkout, err := inspectCheckout(ctx, root)
	if err != nil || checkout.Root != p.Destination || checkout.Common != p.Common {
		return ErrWorktreeIdentity
	}
	if birth {
		records, err := registeredWorktrees(ctx, p.Repository)
		if err != nil {
			return err
		}
		found := false
		for _, r := range records {
			if r.Path == p.Destination && r.Commit == p.Commit && r.Branch == "" && !r.Incomplete {
				found = true
			}
		}
		if !found {
			return ErrWorktreeIdentity
		}
		// HEAD and registration exist before checkout finishes. A missing or
		// partial index/files cannot prove an interrupted add completed.
		status, err := worktreeGit(ctx, root, "status", "--porcelain=v1", "--untracked-files=normal")
		if err != nil || status != "" {
			return ErrWorktreeIdentity
		}
	}
	cwd, err := physicalDirectory(p.CWD)
	if err != nil || cwd != p.CWD || !rules.Inside(root, cwd) || unix.Access(cwd, unix.R_OK|unix.W_OK|unix.X_OK) != nil {
		return ErrWorktreeIdentity
	}
	return nil
}

// ValidateCreatedWorktree checks a persisted checkout without creating or
// repairing anything. Normal edits and commits after creation remain valid.
func ValidateCreatedWorktree(ctx context.Context, i store.CreationIntent) error {
	if i.WorkspaceMode != "worktree" || i.WorktreePath == "" || i.ExecutionDirectory() == "" {
		return ErrWorktreeIdentity
	}
	return verifyWorktree(ctx, frozenWorktree(i), false)
}

func (m *Manager) ensureWorktree(ctx context.Context, n Native, i store.CreationIntent) (bool, error) {
	legacy, err := legacyWorktreePath(m.Config.Storage.DatabaseFile, i.DraftID)
	if err != nil {
		return false, err
	}
	if i.WorktreePath != legacy {
		home, err := nativeWorktreeHome(n)
		if err != nil {
			return false, err
		}
		parent := filepath.Dir(i.WorktreePath)
		bucket := filepath.Base(parent)
		if filepath.Dir(parent) != filepath.Join(home, "worktrees") || len(bucket) != 4 || strings.Trim(bucket, "0123456789abcdef") != "" || filepath.Base(i.WorktreePath) != worktreeName(i.GitRoot, i.DraftID) {
			return false, ErrWorktreeStorage
		}
	}
	checkout, err := inspectCheckout(ctx, i.Directory)
	if err != nil || checkout.Root != i.GitRoot || checkout.Common != i.GitCommonDir {
		return false, ErrWorktreeIdentity
	}
	if !m.directories(nil).allowed(checkout.Root) {
		return false, ErrWorktreeScope
	}
	p := frozenWorktree(i)
	raw, _ := json.Marshal(p)
	a, err := m.Store.PrepareCreationAttempt(ctx, i.DraftID, "worktree", string(raw), m.now().UnixMilli())
	if err != nil {
		return false, err
	}
	if a.State == "confirmed" {
		return true, verifyWorktree(ctx, p, false)
	}
	if a.State == "unknown" {
		if verifyWorktree(ctx, p, true) != nil {
			return false, nil // An absent checkout is not permission to repeat add.
		}
		return true, m.Store.FinishCreationAttempt(ctx, a, "confirmed", p.Destination, `{}`)
	}
	if a.State == "rejected" {
		return false, nil
	}
	ok, err := m.Store.DispatchCreationAttempt(ctx, a)
	if err != nil || !ok {
		return false, err
	}
	// These checks precede git worktree add, so failure is a known rejection.
	if err = worktreeMarker(p, true); err == nil {
		if _, statErr := os.Lstat(p.Destination); !os.IsNotExist(statErr) {
			err = ErrWorktreeStorage
		}
	}
	if err != nil {
		return false, m.Store.FinishCreationAttempt(ctx, a, "rejected", "", `{}`)
	}
	_, runErr := worktreeGit(ctx, p.Repository, "worktree", "add", "--detach", "--", p.Destination, p.Commit)
	outcome := "unknown"
	if runErr == nil && verifyWorktree(ctx, p, true) == nil {
		outcome = "confirmed"
	}
	remote := ""
	if outcome == "confirmed" {
		remote = p.Destination
	}
	// A cancelled call may no longer write SQLite. Recovery leaves its durable
	// dispatch unknown and proves the checkout; it never runs git add again.
	return outcome == "confirmed", m.Store.FinishCreationAttempt(ctx, a, outcome, remote, `{}`)
}
