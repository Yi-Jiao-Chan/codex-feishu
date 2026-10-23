package creation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"

	"codex-feishu/internal/rules"
	"golang.org/x/sys/unix"
)

const (
	worktreeOwnerName    = "codex-thread.json"
	worktreeControlLimit = 16 << 10
)

type nativeWorktreeOwner struct {
	Version       int    `json:"version"`
	OwnerThreadID string `json:"ownerThreadId"`
}

// Keep handles to both directories: a Git administrative slot can be removed
// and reused for a different checkout while an ownership operation is running.
type worktreeOwnerHandle struct {
	checkout, metadata *os.Root
	gitdir             string
	checkoutInfo       os.FileInfo
	metadataInfo       os.FileInfo
	commonInfo         os.FileInfo
	gitfileInfo        os.FileInfo
	backlinkInfo       os.FileInfo
	commondirInfo      os.FileInfo
}

func (w *worktreeOwnerHandle) close() {
	if w.checkout != nil {
		w.checkout.Close()
	}
	if w.metadata != nil {
		w.metadata.Close()
	}
}

func ownerControlInfo(info os.FileInfo, directory bool) bool {
	if info == nil || info.Mode().Perm()&0022 != 0 || info.IsDir() != directory || !directory && !info.Mode().IsRegular() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

// Open through the held directory and refuse symlinks, special files, shared
// write access and unexpectedly large control files. Git's own pointer files
// and App-created owner files may be 0644; new bridge owner files are 0600.
func readOwnerControl(root *os.Root, name string) ([]byte, os.FileInfo, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !ownerControlInfo(before, false) || before.Size() > worktreeControlLimit {
		return nil, nil, ErrWorktreeIdentity
	}
	f, err := root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, ErrWorktreeIdentity
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !ownerControlInfo(info, false) || !os.SameFile(before, info) || info.Size() > worktreeControlLimit {
		return nil, nil, ErrWorktreeIdentity
	}
	raw, err := io.ReadAll(io.LimitReader(f, worktreeControlLimit+1))
	if err != nil || len(raw) > worktreeControlLimit {
		return nil, nil, ErrWorktreeIdentity
	}
	return raw, info, nil
}

func ownerControlPath(base string, raw []byte) (string, error) {
	path := strings.TrimSuffix(string(raw), "\n")
	if path == "" || strings.IndexFunc(path, unicode.IsControl) >= 0 {
		return "", ErrWorktreeIdentity
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	return filepath.Clean(path), nil
}

func openOwnerDirectory(path string) (*os.Root, os.FileInfo, error) {
	real, err := physicalDirectory(path)
	if err != nil || real != path {
		return nil, nil, ErrWorktreeIdentity
	}
	before, err := os.Lstat(path)
	if err != nil || !ownerControlInfo(before, true) {
		return nil, nil, ErrWorktreeIdentity
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, nil, ErrWorktreeIdentity
	}
	info, err := root.Stat(".")
	if err != nil || !ownerControlInfo(info, true) || !os.SameFile(before, info) {
		root.Close()
		return nil, nil, ErrWorktreeIdentity
	}
	return root, info, nil
}

func resolveWorktreeOwner(ctx context.Context, p worktreeRequest) (*worktreeOwnerHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	w := &worktreeOwnerHandle{}
	complete := false
	defer func() {
		if !complete {
			w.close()
		}
	}()
	var err error
	w.checkout, w.checkoutInfo, err = openOwnerDirectory(p.Destination)
	if err != nil {
		return nil, err
	}
	checkout, err := inspectCheckout(ctx, p.Destination)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrWorktreeIdentity
	}
	if checkout.Root != p.Destination || checkout.Common != p.Common {
		return nil, ErrWorktreeIdentity
	}
	records, err := registeredWorktrees(ctx, p.Destination)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrWorktreeIdentity
	}
	registered := 0
	for _, record := range records {
		if record.Path == p.Destination && record.Commit != "" {
			registered++
		}
	}
	if registered != 1 {
		return nil, ErrWorktreeIdentity
	}
	common, info, err := openOwnerDirectory(p.Common)
	if err != nil {
		return nil, err
	}
	w.commonInfo = info
	common.Close()
	// Resolve this particular worktree on every operation. A cached path or a
	// basename-derived worktrees/<name> path can belong to another checkout.
	ownerPath, err := worktreeGit(ctx, p.Destination, "rev-parse", "--path-format=absolute", "--git-path", worktreeOwnerName)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrWorktreeIdentity
	}
	w.gitdir, err = worktreeGit(ctx, p.Destination, "rev-parse", "--absolute-git-dir")
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrWorktreeIdentity
	}
	if filepath.Dir(w.gitdir) != filepath.Join(p.Common, "worktrees") || ownerPath != filepath.Join(w.gitdir, worktreeOwnerName) {
		return nil, ErrWorktreeIdentity
	}
	w.metadata, w.metadataInfo, err = openOwnerDirectory(w.gitdir)
	if err != nil {
		return nil, err
	}
	gitfile, info, err := readOwnerControl(w.checkout, ".git")
	if err != nil || !strings.HasPrefix(string(gitfile), "gitdir: ") {
		return nil, ErrWorktreeIdentity
	}
	w.gitfileInfo = info
	gitfilePath, err := ownerControlPath(p.Destination, gitfile[len("gitdir: "):])
	if err != nil || gitfilePath != w.gitdir {
		return nil, ErrWorktreeIdentity
	}
	backlink, info, err := readOwnerControl(w.metadata, "gitdir")
	if err != nil {
		return nil, ErrWorktreeIdentity
	}
	w.backlinkInfo = info
	backlinkPath, err := ownerControlPath(w.gitdir, backlink)
	if err != nil || backlinkPath != filepath.Join(p.Destination, ".git") {
		return nil, ErrWorktreeIdentity
	}
	commondir, info, err := readOwnerControl(w.metadata, "commondir")
	if err != nil {
		return nil, ErrWorktreeIdentity
	}
	w.commondirInfo = info
	commonPath, err := ownerControlPath(w.gitdir, commondir)
	if err != nil || commonPath != p.Common {
		return nil, ErrWorktreeIdentity
	}
	if p.CWD != "" {
		cwd, err := physicalDirectory(p.CWD)
		if err != nil || cwd != p.CWD || !rules.Inside(p.Destination, cwd) {
			return nil, ErrWorktreeIdentity
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	complete = true
	return w, nil
}

func (w *worktreeOwnerHandle) verify(ctx context.Context, p worktreeRequest) error {
	fresh, err := resolveWorktreeOwner(ctx, p)
	if err != nil {
		return err
	}
	defer fresh.close()
	if w.gitdir != fresh.gitdir || !os.SameFile(w.checkoutInfo, fresh.checkoutInfo) ||
		!os.SameFile(w.metadataInfo, fresh.metadataInfo) || !os.SameFile(w.commonInfo, fresh.commonInfo) ||
		!os.SameFile(w.gitfileInfo, fresh.gitfileInfo) || !os.SameFile(w.backlinkInfo, fresh.backlinkInfo) ||
		!os.SameFile(w.commondirInfo, fresh.commondirInfo) {
		return ErrWorktreeIdentity
	}
	return nil
}

type ownerFileIdentity struct {
	Device uint64 `json:"dev"`
	Inode  uint64 `json:"ino"`
}

func ownerFileIdentityOf(info os.FileInfo) ownerFileIdentity {
	stat := info.Sys().(*syscall.Stat_t) // resolveWorktreeOwner checked the type.
	return ownerFileIdentity{Device: uint64(stat.Dev), Inode: stat.Ino}
}

func (w *worktreeOwnerHandle) identity() (string, error) {
	f, err := w.checkout.OpenFile(".git", os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", ErrWorktreeIdentity
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !ownerControlInfo(info, false) || !os.SameFile(info, w.gitfileInfo) {
		return "", ErrWorktreeIdentity
	}
	var stat unix.Stat_t
	if unix.Fstat(int(f.Fd()), &stat) != nil {
		return "", ErrWorktreeIdentity
	}
	// Directory ctimes change when task files or ownership files are added.
	// Only the immutable .git pointer's ctime supplements device/inode IDs.
	stamp := struct {
		Version     int               `json:"version"`
		GitDir      string            `json:"gitdir"`
		Checkout    ownerFileIdentity `json:"checkout"`
		GitFile     ownerFileIdentity `json:"gitfile"`
		Metadata    ownerFileIdentity `json:"metadata"`
		Common      ownerFileIdentity `json:"common"`
		ChangedSec  int64             `json:"gitfile_ctime_sec"`
		ChangedNsec int64             `json:"gitfile_ctime_nsec"`
	}{1, w.gitdir, ownerFileIdentityOf(w.checkoutInfo), ownerFileIdentityOf(w.gitfileInfo),
		ownerFileIdentityOf(w.metadataInfo), ownerFileIdentityOf(w.commonInfo), stat.Ctim.Sec, stat.Ctim.Nsec}
	raw, _ := json.Marshal(stamp)
	digest := sha256.Sum256(raw)
	return "v1:" + hex.EncodeToString(digest[:]), nil
}

// Persist this opaque identity with the operation intent. It remains stable
// through ordinary edits, commits and owner publication, but not replacement
// of the checkout, its .git pointer or the private administrative directory.
func worktreeOwnerIdentity(ctx context.Context, p worktreeRequest) (string, error) {
	w, err := resolveWorktreeOwner(ctx, p)
	if err != nil {
		return "", err
	}
	defer w.close()
	identity, err := w.identity()
	if err != nil {
		return "", err
	}
	if err := w.verify(ctx, p); err != nil {
		return "", err
	}
	return identity, nil
}

func validOwnerThreadID(id string) bool {
	return id != "" && len(id) <= 512 && strings.TrimSpace(id) == id && strings.IndexFunc(id, unicode.IsControl) < 0
}

func (w *worktreeOwnerHandle) read() (string, error) {
	raw, _, err := readOwnerControl(w.metadata, worktreeOwnerName)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", ErrWorktreeIdentity
	}
	var owner nativeWorktreeOwner
	if json.Unmarshal(raw, &owner) != nil || owner.Version != 1 || !validOwnerThreadID(owner.OwnerThreadID) {
		return "", ErrWorktreeIdentity
	}
	return owner.OwnerThreadID, nil
}

// Missing ownership is distinct from malformed or foreign ownership. Neither
// function repairs Git metadata or takes authorization from a pathname alone.
func readWorktreeOwner(ctx context.Context, p worktreeRequest) (string, error) {
	w, err := resolveWorktreeOwner(ctx, p)
	if err != nil {
		return "", err
	}
	defer w.close()
	owner, err := w.read()
	if err != nil {
		return "", err
	}
	if err := w.verify(ctx, p); err != nil {
		return "", err
	}
	return owner, nil
}

// The caller must durably authorize the registration and protect publication
// from App cleanup. This function only creates an absent owner; it never
// overwrites an existing file, including malformed or differently owned files.
func ensureWorktreeOwner(ctx context.Context, p worktreeRequest, threadID string) error {
	return ensureWorktreeOwnerExpected(ctx, p, threadID, "")
}

// Use the persisted identity for recovery, so a replacement between the
// caller's last read and this operation cannot be adopted as the original.
func ensureWorktreeOwnerWithIdentity(ctx context.Context, p worktreeRequest, threadID, expectedIdentity string) error {
	if expectedIdentity == "" {
		return ErrWorktreeIdentity
	}
	return ensureWorktreeOwnerExpected(ctx, p, threadID, expectedIdentity)
}

func ensureWorktreeOwnerExpected(ctx context.Context, p worktreeRequest, threadID, expectedIdentity string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validOwnerThreadID(threadID) {
		return ErrWorktreeIdentity
	}
	w, err := resolveWorktreeOwner(ctx, p)
	if err != nil {
		return err
	}
	defer w.close()
	if expectedIdentity != "" {
		identity, err := w.identity()
		if err != nil || identity != expectedIdentity {
			return ErrWorktreeIdentity
		}
	}
	owner, err := w.read()
	if err != nil || owner != "" && owner != threadID {
		return ErrWorktreeIdentity
	}
	if owner == "" {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return ErrWorktreeStorage
		}
		temporary := ".codex-thread-" + hex.EncodeToString(nonce[:]) + ".tmp"
		f, err := w.metadata.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, 0600)
		if err != nil {
			return ErrWorktreeStorage
		}
		defer w.metadata.Remove(temporary)
		raw, _ := json.Marshal(nativeWorktreeOwner{Version: 1, OwnerThreadID: threadID})
		raw = append(raw, '\n')
		_, writeErr := f.Write(raw)
		if writeErr == nil {
			writeErr = f.Sync()
		}
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			return ErrWorktreeStorage
		}
		if err := w.verify(ctx, p); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.metadata.Link(temporary, worktreeOwnerName); err != nil && !errors.Is(err, os.ErrExist) {
			return ErrWorktreeStorage
		}
		if err := w.metadata.Remove(temporary); err != nil {
			return ErrWorktreeStorage
		}
	} else {
		// A matching App-created file also needs a durability barrier before
		// this operation can be recorded as confirmed.
		f, err := w.metadata.OpenFile(worktreeOwnerName, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return ErrWorktreeIdentity
		}
		info, statErr := f.Stat()
		if statErr != nil || !ownerControlInfo(info, false) {
			f.Close()
			return ErrWorktreeIdentity
		}
		syncErr := f.Sync()
		closeErr := f.Close()
		if syncErr != nil || closeErr != nil {
			return ErrWorktreeStorage
		}
	}
	dir, err := w.metadata.Open(".")
	if err != nil {
		return ErrWorktreeStorage
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil || closeErr != nil {
		return ErrWorktreeStorage
	}
	if err := w.verify(ctx, p); err != nil {
		return err
	}
	owner, err = w.read()
	if err != nil || owner != threadID {
		return ErrWorktreeIdentity
	}
	return ctx.Err()
}
