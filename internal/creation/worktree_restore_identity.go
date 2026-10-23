package creation

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
)

var ErrWorktreeSnapshotMissing = errors.New("managed worktree snapshot is unavailable")

// Preserve the original creation proof without changing its birth generation.
func CreationWorktreeRestoreMarker(i store.CreationIntent) json.RawMessage {
	raw, _ := json.Marshal(frozenWorktree(i))
	return raw
}

// The desktop hashes the original root string, including any path alias.
// Canonicalizing it before hashing would select a different snapshot.
func WorktreeSnapshotRef(root string) string {
	sum := sha1.Sum([]byte(root))
	return "refs/codex/snapshots/" + hex.EncodeToString(sum[:])
}

func restorePathString(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && strings.IndexFunc(path, unicode.IsControl) < 0
}

// This establishes the managed-root boundary, not the checkout identity. The
// caller must additionally prove a live registration or an exact snapshot ref.
func restoreManagedLocation(home, root string) bool {
	if !restorePathString(root) || !restorePathString(home) {
		return false
	}
	actualHome, err := rules.CanonicalDirectory(home)
	if err != nil {
		return false
	}
	managed, err := rules.CanonicalDirectory(filepath.Dir(filepath.Dir(root)))
	return err == nil && managed == filepath.Join(actualHome, "worktrees")
}

// InspectWorktreeRestoreIdentity is read-only. It records the precise root
// while a previously authorized checkout is still available, including a
// thread whose cwd is a subdirectory of that checkout.
func InspectWorktreeRestoreIdentity(ctx context.Context, scope store.SubmissionScope, rawCWD, source, home string) (store.ThreadWorktreeIdentity, error) {
	var out store.ThreadWorktreeIdentity
	if !restorePathString(rawCWD) {
		return out, ErrWorktreeIdentity
	}
	actual, err := rules.CanonicalDirectory(rawCWD)
	if err != nil {
		return out, ErrWorktreeIdentity
	}
	checkout, err := inspectCheckout(ctx, actual)
	if err != nil {
		return out, err
	}
	rel, err := filepath.Rel(checkout.Root, actual)
	if err != nil || !rules.Inside(checkout.Root, actual) {
		return out, ErrWorktreeIdentity
	}
	root := rawCWD
	if rel != "." {
		suffix := string(filepath.Separator) + rel
		if !strings.HasSuffix(rawCWD, suffix) {
			return out, ErrWorktreeIdentity
		}
		root = strings.TrimSuffix(rawCWD, suffix)
	}
	if !restoreManagedLocation(home, root) {
		return out, ErrWorktreeScope
	}
	source, err = rules.CanonicalDirectory(source)
	if err != nil {
		return out, ErrWorktreeIdentity
	}
	original, err := inspectCheckout(ctx, source)
	if err != nil || original.Root != source || original.Common != checkout.Common || source == checkout.Root {
		return out, ErrWorktreeIdentity
	}
	p := worktreeRequest{Repository: source, Common: checkout.Common, Destination: root, CWD: rawCWD}
	owner, err := readWorktreeOwner(ctx, p)
	if err != nil || owner != "" && owner != scope.ThreadID {
		return out, ErrWorktreeIdentity
	}
	return store.ThreadWorktreeIdentity{HostID: scope.HostID, AppID: scope.AppID, ThreadID: scope.ThreadID,
		RawCWD: rawCWD, Root: root, SourceRoot: source, CommonDir: checkout.Common, RelativeCWD: rel}, nil
}

// LocateWorktreeSnapshotIdentity backfills older bound threads using their
// previously authorized source repository. A path shape alone is insufficient:
// exactly one ancestor must have a commit snapshot under its original hash.
// It never scans unrelated repositories or substitutes the branch's HEAD.
func LocateWorktreeSnapshotIdentity(ctx context.Context, scope store.SubmissionScope, rawCWD, source, home string) (store.ThreadWorktreeIdentity, error) {
	var out store.ThreadWorktreeIdentity
	if !restorePathString(rawCWD) {
		return out, ErrWorktreeIdentity
	}
	source, err := rules.CanonicalDirectory(source)
	if err != nil {
		return out, ErrWorktreeIdentity
	}
	original, err := inspectCheckout(ctx, source)
	if err != nil || original.Root != source {
		return out, ErrWorktreeIdentity
	}
	found, managed := 0, false
	for root, depth := rawCWD, 0; root != filepath.Dir(root) && depth < 128; root, depth = filepath.Dir(root), depth+1 {
		if !restoreManagedLocation(home, root) {
			continue
		}
		managed = true
		commit, err := worktreeGit(ctx, source, "rev-parse", "--verify", "--end-of-options", WorktreeSnapshotRef(root)+"^{commit}")
		if err != nil || !validRestoreCommit(commit) {
			continue
		}
		if _, err := os.Lstat(root); !os.IsNotExist(err) {
			return out, ErrWorktreeIdentity
		}
		rel, err := filepath.Rel(root, rawCWD)
		if err != nil || !rules.Inside(root, rawCWD) {
			return out, ErrWorktreeIdentity
		}
		if rel != "." {
			kind, err := worktreeGit(ctx, source, "cat-file", "-t", commit+":"+filepath.ToSlash(rel))
			if err != nil || kind != "tree" {
				return out, ErrWorktreeIdentity
			}
		}
		found++
		out = store.ThreadWorktreeIdentity{HostID: scope.HostID, AppID: scope.AppID, ThreadID: scope.ThreadID,
			RawCWD: rawCWD, Root: root, SourceRoot: source, CommonDir: original.Common, RelativeCWD: rel}
	}
	if found != 1 {
		if found == 0 && managed {
			return out, ErrWorktreeSnapshotMissing
		}
		return out, ErrWorktreeIdentity
	}
	return out, nil
}

func validRestoreCommit(commit string) bool {
	return (len(commit) == 40 || len(commit) == 64) && strings.Trim(commit, "0123456789abcdef") == ""
}

// AuthorizeWorktreeRestoreSource checks the original relative directory under
// today's policy even when that subdirectory exists only in the snapshot.
// Registered alternate checkouts follow the same mapping as ResolveScope.
func AuthorizeWorktreeRestoreSource(ctx context.Context, identity store.ThreadWorktreeIdentity, roots []string) error {
	checkout, err := inspectCheckout(ctx, identity.SourceRoot)
	if err != nil || checkout.Root != identity.SourceRoot || checkout.Common != identity.CommonDir ||
		filepath.IsAbs(identity.RelativeCWD) || !rules.Inside(identity.SourceRoot, filepath.Join(identity.SourceRoot, identity.RelativeCWD)) {
		return ErrWorktreeIdentity
	}
	for _, configured := range roots {
		allowed, err := rules.CanonicalDirectory(configured)
		if err != nil {
			continue
		}
		if rules.Inside(allowed, filepath.Join(identity.SourceRoot, identity.RelativeCWD)) {
			return nil
		}
		other, err := inspectCheckout(ctx, allowed)
		if err == nil && other.Common == checkout.Common && rules.Inside(allowed, filepath.Join(other.Root, identity.RelativeCWD)) {
			return nil
		}
	}
	return ErrWorktreeScope
}
