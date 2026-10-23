package creation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
	"golang.org/x/sys/unix"
)

var (
	ErrWorktreeRestoreUnsupported = errors.New("worktree restore layout is unsupported")
	ErrWorktreeRestoreConflict    = errors.New("worktree restore destination or ownership conflicts")
	ErrWorktreeRestoreStale       = errors.New("worktree restore destination has a stale Git registration")
	ErrWorktreeRestoreUnknown     = errors.New("worktree restore needs physical verification")
	ErrWorktreeRestoreIncomplete  = errors.New("worktree snapshot does not contain the required checkout")
	ErrWorktreeRestoreSource      = errors.New("worktree restore source identity is unavailable")
	ErrWorktreeRestoreStorage     = errors.New("worktree restore private storage is unavailable")
)

func WorktreeRestoreErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrWorktreeSnapshotMissing):
		return "worktree-snapshot-missing"
	case errors.Is(err, ErrWorktreeRestoreUnsupported):
		return "worktree-restore-unsupported"
	case errors.Is(err, ErrWorktreeRestoreStale):
		return "worktree-restore-stale"
	case errors.Is(err, ErrWorktreeRestoreConflict), errors.Is(err, ErrWorktreeIdentity):
		return "worktree-restore-conflict"
	case errors.Is(err, ErrWorktreeRestoreIncomplete):
		return "worktree-restore-incomplete"
	case errors.Is(err, ErrWorktreeRestoreSource):
		return "worktree-restore-source"
	case errors.Is(err, ErrWorktreeRestoreStorage):
		return "worktree-restore-storage"
	default:
		return "worktree-restore-unknown"
	}
}

// Identity and BridgeMarker must come from the caller's authorized persistent
// binding, not from a message, a pathname guess or an owner file alone.
type WorktreeRestoreInput struct {
	RestoreID    string                       `json:"restoreId"`
	Identity     store.ThreadWorktreeIdentity `json:"identity"`
	CodexHome    string                       `json:"codexHome"`
	BridgeMarker json.RawMessage              `json:"bridgeMarker,omitempty"`
}

type WorktreeRestorePlan struct {
	Version int `json:"version"`
	WorktreeRestoreInput
	SnapshotRef        string            `json:"snapshotRef"`
	SnapshotCommit     string            `json:"snapshotCommit"`
	StageRoot          string            `json:"stageRoot"`
	SourceIdentity     ownerFileIdentity `json:"sourceIdentity"`
	CommonIdentity     ownerFileIdentity `json:"commonIdentity"`
	ManagedIdentity    ownerFileIdentity `json:"managedIdentity"`
	ParentIdentity     ownerFileIdentity `json:"parentIdentity"`
	PublicationBytes   []byte            `json:"publicationBytes,omitempty"`
	PublicationExists  bool              `json:"publicationExists"`
	DelayedPublication bool              `json:"delayedPublication"`
}

type WorktreeRestorePhysical struct {
	GitDir   string            `json:"gitdir"`
	Checkout ownerFileIdentity `json:"checkout"`
	GitFile  ownerFileIdentity `json:"gitfile"`
	Metadata ownerFileIdentity `json:"metadata"`
	Common   ownerFileIdentity `json:"common"`
}

type WorktreeRestoreCheckpoint struct {
	Version            int                     `json:"version"`
	Phase              string                  `json:"phase"`
	PlanDigest         string                  `json:"planDigest"`
	StageIdentity      string                  `json:"stageIdentity,omitempty"`
	Physical           WorktreeRestorePhysical `json:"physical"`
	ParentIdentity     ownerFileIdentity       `json:"parentIdentity"`
	Generation         string                  `json:"generation,omitempty"`
	PublicationPending bool                    `json:"publicationPending"`
	Limitations        []string                `json:"limitations"`
}

type WorktreeRestoreResult = WorktreeRestoreCheckpoint

type WorktreeRestorePersist func(context.Context, WorktreeRestoreCheckpoint) error

var restoreOldDirectoryName = regexp.MustCompile(`^[0-9]{6}-[0-9]{4}-[0-9a-fA-F]{8}$`)

func restorePlanDigest(p WorktreeRestorePlan) string {
	raw, _ := json.Marshal(p)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func restoreRootRequest(p WorktreeRestorePlan, stage bool) worktreeRequest {
	root, cwd := p.Identity.Root, p.Identity.RawCWD
	if stage {
		root, cwd = p.StageRoot, filepath.Join(p.StageRoot, p.Identity.RelativeCWD)
	}
	return worktreeRequest{DraftID: p.RestoreID, Source: p.Identity.SourceRoot, Repository: p.Identity.SourceRoot,
		Common: p.Identity.CommonDir, Commit: p.SnapshotCommit, Destination: root, CWD: cwd}
}

func restorePlanShape(p WorktreeRestorePlan) error {
	i := p.Identity
	if p.Version != 1 || len(p.RestoreID) != 32 || strings.Trim(p.RestoreID, "0123456789abcdef") != "" ||
		i.HostID == "" || i.AppID == "" || !validOwnerThreadID(i.ThreadID) || !restorePathString(i.Root) ||
		!restorePathString(i.RawCWD) || !restorePathString(i.SourceRoot) || !restorePathString(i.CommonDir) ||
		!restorePathString(p.CodexHome) || i.RelativeCWD == "" || filepath.IsAbs(i.RelativeCWD) ||
		filepath.Clean(i.RelativeCWD) != i.RelativeCWD || !rules.Inside(i.Root, i.RawCWD) || filepath.Join(i.Root, i.RelativeCWD) != i.RawCWD {
		return ErrWorktreeRestoreSource
	}
	if filepath.Dir(filepath.Dir(i.Root)) != filepath.Join(p.CodexHome, "worktrees") {
		return ErrWorktreeRestoreUnsupported
	}
	if p.StageRoot != filepath.Join(p.CodexHome, "worktrees", "codex-feishu-"+p.RestoreID, "restore") || p.StageRoot == i.Root ||
		p.SnapshotRef != WorktreeSnapshotRef(i.Root) || !validRestoreCommit(p.SnapshotCommit) {
		return ErrWorktreeRestoreSource
	}
	if len(p.BridgeMarker) != 0 {
		var bridge worktreeRequest
		if json.Unmarshal(p.BridgeMarker, &bridge) != nil || len(bridge.DraftID) != 32 || strings.Trim(bridge.DraftID, "0123456789abcdef") != "" ||
			bridge.Destination != i.Root || bridge.CWD != i.RawCWD || bridge.Common != i.CommonDir || bridge.Repository != i.SourceRoot {
			return ErrWorktreeRestoreSource
		}
	}
	return nil
}

func restoreSource(ctx context.Context, p WorktreeRestorePlan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, item := range []struct {
		path     string
		identity ownerFileIdentity
	}{{p.Identity.SourceRoot, p.SourceIdentity}, {p.Identity.CommonDir, p.CommonIdentity}} {
		root, info, err := openOwnerDirectory(item.path)
		if err != nil {
			return ErrWorktreeRestoreSource
		}
		root.Close()
		if ownerFileIdentityOf(info) != item.identity {
			return ErrWorktreeRestoreSource
		}
	}
	checkout, err := inspectCheckout(ctx, p.Identity.SourceRoot)
	if err != nil || checkout.Root != p.Identity.SourceRoot || checkout.Common != p.Identity.CommonDir {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrWorktreeRestoreSource
	}
	return nil
}

func restoreMissing(path string) error {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return ErrWorktreeRestoreConflict
}

func restoreNoRegistration(ctx context.Context, p WorktreeRestorePlan) error {
	records, err := registeredWorktrees(ctx, p.Identity.SourceRoot)
	if err != nil {
		return ErrWorktreeRestoreSource
	}
	for _, record := range records {
		if record.Path == p.Identity.Root {
			return ErrWorktreeRestoreStale
		}
	}
	return nil
}

// Planning is read-only, and hashes the exact frozen root before rejecting any
// aliases. The snapshot ref itself is never updated and base branches are not
// fallback recovery sources.
func PlanWorktreeRestore(ctx context.Context, in WorktreeRestoreInput) (WorktreeRestorePlan, error) {
	p := WorktreeRestorePlan{Version: 1, WorktreeRestoreInput: in,
		SnapshotRef: WorktreeSnapshotRef(in.Identity.Root),
		StageRoot:   filepath.Join(in.CodexHome, "worktrees", "codex-feishu-"+in.RestoreID, "restore")}
	for _, path := range []string{in.CodexHome, filepath.Join(in.CodexHome, "worktrees"), in.Identity.SourceRoot, in.Identity.CommonDir} {
		root, info, err := openOwnerDirectory(path)
		if err != nil {
			return p, ErrWorktreeRestoreSource
		}
		root.Close()
		switch path {
		case in.Identity.SourceRoot:
			p.SourceIdentity = ownerFileIdentityOf(info)
		case in.Identity.CommonDir:
			p.CommonIdentity = ownerFileIdentityOf(info)
		case filepath.Join(in.CodexHome, "worktrees"):
			p.ManagedIdentity = ownerFileIdentityOf(info)
		}
	}
	commit, err := worktreeGit(ctx, in.Identity.SourceRoot, "show-ref", "--verify", "--hash", p.SnapshotRef)
	if err != nil || !validRestoreCommit(commit) {
		if ctx.Err() != nil {
			return p, ctx.Err()
		}
		return p, ErrWorktreeSnapshotMissing
	}
	p.SnapshotCommit = commit
	if err := restorePlanShape(p); err != nil {
		return p, err
	}
	if err := restoreSource(ctx, p); err != nil {
		return p, err
	}
	if kind, err := worktreeGit(ctx, in.Identity.SourceRoot, "cat-file", "-t", commit); err != nil || kind != "commit" {
		return p, ErrWorktreeSnapshotMissing
	}
	if err := restoreMissing(in.Identity.Root); err != nil {
		return p, err
	}
	if err := restoreMissing(filepath.Dir(p.StageRoot)); err != nil {
		return p, err
	}
	if err := restoreNoRegistration(ctx, p); err != nil {
		return p, err
	}
	if in.Identity.RelativeCWD != "." {
		kind, err := worktreeGit(ctx, in.Identity.SourceRoot, "cat-file", "-t", commit+":"+filepath.ToSlash(in.Identity.RelativeCWD))
		if err != nil || kind != "tree" {
			return p, ErrWorktreeRestoreIncomplete
		}
	}
	bucket := filepath.Base(filepath.Dir(in.Identity.Root))
	p.DelayedPublication = !(len(bucket) == 4 && strings.Trim(bucket, "0123456789abcdefABCDEF") == "") && !restoreOldDirectoryName.MatchString(filepath.Base(in.Identity.Root))
	parent, info, err := openOwnerDirectory(filepath.Dir(in.Identity.Root))
	if err == nil {
		defer parent.Close()
		p.ParentIdentity = ownerFileIdentityOf(info)
		if err := restoreParentContents(parent, p); err != nil {
			return p, err
		}
		raw, _, markerErr := readOwnerControl(parent, ".codex-worktree-name")
		if markerErr == nil {
			p.PublicationBytes, p.PublicationExists = raw, true
		} else if !errors.Is(markerErr, os.ErrNotExist) {
			return p, ErrWorktreeRestoreConflict
		}
	} else if _, statErr := os.Lstat(filepath.Dir(in.Identity.Root)); !errors.Is(statErr, os.ErrNotExist) {
		return p, ErrWorktreeRestoreConflict
	}
	return p, nil
}

func restoreParentContents(root *os.Root, p WorktreeRestorePlan) error {
	dir, err := root.Open(".")
	if err != nil {
		return ErrWorktreeRestoreStorage
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return ErrWorktreeRestoreStorage
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return ErrWorktreeRestoreConflict
		}
		if entry.Name() == ".codex-worktree-name" {
			continue
		}
		if strings.HasPrefix(entry.Name(), ".codex-feishu-restore-") && strings.HasSuffix(entry.Name(), ".json") {
			raw, _, err := readOwnerControl(root, entry.Name())
			var old WorktreeRestorePlan
			if err != nil || json.Unmarshal(raw, &old) != nil || old.Identity != p.Identity {
				return ErrWorktreeRestoreConflict
			}
		}
	}
	return nil
}

func restoreControl(root *os.Root, name string, raw []byte, create bool) error {
	if create {
		tmp := ".codex-feishu-tmp-" + store.ID()
		f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return ErrWorktreeRestoreStorage
		}
		defer root.Remove(tmp)
		_, err = f.Write(raw)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			return ErrWorktreeRestoreStorage
		}
		if err := root.Link(tmp, name); err != nil && !errors.Is(err, os.ErrExist) {
			return ErrWorktreeRestoreStorage
		}
		if err := root.Remove(tmp); err != nil {
			return ErrWorktreeRestoreStorage
		}
		if err := restoreSyncRoot(root); err != nil {
			return err
		}
	}
	actual, info, err := readOwnerControl(root, name)
	private := name != ".codex-worktree-name"
	if err != nil || private && info.Mode().Perm()&0077 != 0 || !bytes.Equal(actual, raw) {
		return ErrWorktreeRestoreConflict
	}
	return nil
}

func restoreSyncRoot(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return ErrWorktreeRestoreStorage
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ErrWorktreeRestoreStorage
	}
	return nil
}

func restorePlanBytes(p WorktreeRestorePlan) []byte { raw, _ := json.Marshal(p); return raw }
func restoreReservationName(p WorktreeRestorePlan) string {
	return ".codex-feishu-restore-" + p.RestoreID + ".json"
}

func prepareRestoreParent(p WorktreeRestorePlan) (ownerFileIdentity, error) {
	parent := filepath.Dir(p.Identity.Root)
	if p.ParentIdentity == (ownerFileIdentity{}) {
		managed, err := openRestoreManaged(p)
		if err != nil {
			return ownerFileIdentity{}, err
		}
		err = managed.Mkdir(filepath.Base(parent), 0700)
		managed.Close()
		if err != nil {
			return ownerFileIdentity{}, ErrWorktreeRestoreConflict
		}
	}
	root, info, err := openOwnerDirectory(parent)
	if err != nil {
		return ownerFileIdentity{}, ErrWorktreeRestoreConflict
	}
	defer root.Close()
	identity := ownerFileIdentityOf(info)
	if p.ParentIdentity != (ownerFileIdentity{}) && p.ParentIdentity != identity {
		return identity, ErrWorktreeRestoreConflict
	}
	if err := restoreParentContents(root, p); err != nil {
		return identity, err
	}
	if err := restoreControl(root, restoreReservationName(p), restorePlanBytes(p), true); err != nil {
		return identity, err
	}
	if len(p.BridgeMarker) != 0 {
		var bridge worktreeRequest
		json.Unmarshal(p.BridgeMarker, &bridge)
		if err := restoreControl(root, ".codex-feishu-"+bridge.DraftID+".json", p.BridgeMarker, true); err != nil {
			return identity, err
		}
	}
	if p.DelayedPublication {
		raw, _, err := readOwnerControl(root, ".codex-worktree-name")
		if err == nil {
			if !p.PublicationExists || !bytes.Equal(raw, p.PublicationBytes) {
				return identity, ErrWorktreeRestoreConflict
			}
			if err := root.Remove(".codex-worktree-name"); err != nil {
				return identity, ErrWorktreeRestoreStorage
			}
			if err := restoreSyncRoot(root); err != nil {
				return identity, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return identity, ErrWorktreeRestoreConflict
		}
	}
	return identity, nil
}

func openRestoreManaged(p WorktreeRestorePlan) (*os.Root, error) {
	root, info, err := openOwnerDirectory(filepath.Join(p.CodexHome, "worktrees"))
	if err != nil {
		return nil, ErrWorktreeRestoreSource
	}
	if ownerFileIdentityOf(info) != p.ManagedIdentity {
		root.Close()
		return nil, ErrWorktreeRestoreSource
	}
	return root, nil
}

func verifyRestoreParent(p WorktreeRestorePlan, c WorktreeRestoreCheckpoint) (*os.Root, error) {
	root, info, err := openOwnerDirectory(filepath.Dir(p.Identity.Root))
	if err != nil {
		return nil, ErrWorktreeRestoreUnknown
	}
	if c.ParentIdentity != (ownerFileIdentity{}) && ownerFileIdentityOf(info) != c.ParentIdentity ||
		p.ParentIdentity != (ownerFileIdentity{}) && ownerFileIdentityOf(info) != p.ParentIdentity {
		root.Close()
		return nil, ErrWorktreeRestoreConflict
	}
	if err := restoreControl(root, restoreReservationName(p), restorePlanBytes(p), false); err != nil {
		root.Close()
		return nil, err
	}
	return root, nil
}

func restoreExecutionDirectory(p WorktreeRestorePlan, root string) error {
	cwd := filepath.Join(root, p.Identity.RelativeCWD)
	actual, err := physicalDirectory(cwd)
	if err != nil || actual != cwd || unix.Access(cwd, unix.R_OK|unix.W_OK|unix.X_OK) != nil {
		return ErrWorktreeRestoreIncomplete
	}
	return nil
}

func restoreSnapshotCheckout(ctx context.Context, p WorktreeRestorePlan, root string) error {
	if err := restoreExecutionDirectory(p, root); err != nil {
		return err
	}
	head, err := worktreeGit(ctx, root, "rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
	if err != nil || head != p.SnapshotCommit {
		return ErrWorktreeRestoreIncomplete
	}
	name, err := worktreeGit(ctx, root, "rev-parse", "--symbolic-full-name", "HEAD")
	if err != nil || name != "HEAD" {
		return ErrWorktreeRestoreIncomplete
	}
	status, err := worktreeGit(ctx, root, "status", "--porcelain=v1", "--untracked-files=normal")
	if err != nil || status != "" {
		return ErrWorktreeRestoreIncomplete
	}
	return nil
}

func restoreStageProof(ctx context.Context, p WorktreeRestorePlan, createBirth bool) (WorktreeRestoreCheckpoint, error) {
	c := WorktreeRestoreCheckpoint{Version: 1, Phase: "stage-ready", PlanDigest: restorePlanDigest(p), Limitations: []string{"desktop-cleanup-queue-not-coordinated"}}
	if !p.DelayedPublication {
		c.Limitations = append(c.Limitations, "legacy-path-cannot-delay-publication")
	}
	bucket, _, err := openOwnerDirectory(filepath.Dir(p.StageRoot))
	if err != nil {
		return c, ErrWorktreeRestoreUnknown
	}
	defer bucket.Close()
	err = restoreControl(bucket, ".codex-feishu-restore.json", restorePlanBytes(p), false)
	if err != nil {
		return c, err
	}
	w, err := resolveWorktreeOwner(ctx, restoreRootRequest(p, true))
	if err != nil {
		return c, ErrWorktreeRestoreUnknown
	}
	defer w.close()
	if err := restoreSnapshotCheckout(ctx, p, p.StageRoot); err != nil {
		return c, err
	}
	if _, err := w.metadata.Lstat("locked"); err == nil || !errors.Is(err, os.ErrNotExist) {
		return c, ErrWorktreeRestoreUnknown
	}
	owner, err := w.read()
	if err != nil || owner != "" && owner != p.Identity.ThreadID {
		return c, ErrWorktreeRestoreConflict
	}
	c.StageIdentity, err = w.identity()
	if err != nil {
		return c, err
	}
	c.Physical = WorktreeRestorePhysical{w.gitdir, ownerFileIdentityOf(w.checkoutInfo), ownerFileIdentityOf(w.gitfileInfo), ownerFileIdentityOf(w.metadataInfo), ownerFileIdentityOf(w.commonInfo)}
	parent, info, err := openOwnerDirectory(filepath.Dir(p.Identity.Root))
	if err != nil {
		return c, ErrWorktreeRestoreUnknown
	}
	c.ParentIdentity = ownerFileIdentityOf(info)
	err = restoreControl(parent, restoreReservationName(p), restorePlanBytes(p), false)
	parent.Close()
	if err != nil {
		return c, err
	}
	birth, _ := json.Marshal(c)
	if err := restoreControl(bucket, ".codex-feishu-restore.identity", birth, createBirth); err != nil {
		return c, ErrWorktreeRestoreUnknown
	}
	if err := w.verify(ctx, restoreRootRequest(p, true)); err != nil {
		return c, err
	}
	return c, nil
}

// After the no-replace rename the backlink legitimately still names StageRoot.
// Verify the frozen physical tuple before repairing only that held gitdir.
func openRenamedRestore(ctx context.Context, p WorktreeRestorePlan, c WorktreeRestoreCheckpoint) (*worktreeOwnerHandle, string, error) {
	w := &worktreeOwnerHandle{gitdir: c.Physical.GitDir}
	ok := false
	defer func() {
		if !ok {
			w.close()
		}
	}()
	var err error
	w.checkout, w.checkoutInfo, err = openOwnerDirectory(p.Identity.Root)
	if err != nil {
		return nil, "", ErrWorktreeRestoreUnknown
	}
	w.metadata, w.metadataInfo, err = openOwnerDirectory(w.gitdir)
	if err != nil {
		return nil, "", ErrWorktreeRestoreConflict
	}
	common, info, err := openOwnerDirectory(p.Identity.CommonDir)
	if err != nil {
		return nil, "", ErrWorktreeRestoreSource
	}
	w.commonInfo = info
	common.Close()
	gitfile, info, err := readOwnerControl(w.checkout, ".git")
	if err != nil || !bytes.HasPrefix(gitfile, []byte("gitdir: ")) {
		return nil, "", ErrWorktreeRestoreConflict
	}
	w.gitfileInfo = info
	gd, err := ownerControlPath(p.Identity.Root, gitfile[len("gitdir: "):])
	if err != nil || gd != w.gitdir || filepath.Dir(gd) != filepath.Join(p.Identity.CommonDir, "worktrees") {
		return nil, "", ErrWorktreeRestoreConflict
	}
	current := WorktreeRestorePhysical{w.gitdir, ownerFileIdentityOf(w.checkoutInfo), ownerFileIdentityOf(w.gitfileInfo), ownerFileIdentityOf(w.metadataInfo), ownerFileIdentityOf(w.commonInfo)}
	if current != c.Physical {
		return nil, "", ErrWorktreeRestoreConflict
	}
	identity, err := w.identity()
	if err != nil || identity != c.StageIdentity {
		return nil, "", ErrWorktreeRestoreConflict
	}
	commondir, info, err := readOwnerControl(w.metadata, "commondir")
	if err != nil {
		return nil, "", ErrWorktreeRestoreConflict
	}
	w.commondirInfo = info
	commonPath, err := ownerControlPath(w.gitdir, commondir)
	if err != nil || commonPath != p.Identity.CommonDir {
		return nil, "", ErrWorktreeRestoreConflict
	}
	backlink, info, err := readOwnerControl(w.metadata, "gitdir")
	if err != nil {
		return nil, "", ErrWorktreeRestoreConflict
	}
	w.backlinkInfo = info
	backlinkPath, err := ownerControlPath(w.gitdir, backlink)
	if err != nil || backlinkPath != filepath.Join(p.StageRoot, ".git") && backlinkPath != filepath.Join(p.Identity.Root, ".git") {
		return nil, "", ErrWorktreeRestoreConflict
	}
	owner, err := w.read()
	if err != nil || owner != p.Identity.ThreadID {
		return nil, "", ErrWorktreeRestoreConflict
	}
	resolved, err := worktreeGit(ctx, p.Identity.Root, "rev-parse", "--path-format=absolute", "--git-path", worktreeOwnerName)
	if err != nil || resolved != filepath.Join(w.gitdir, worktreeOwnerName) {
		return nil, "", ErrWorktreeRestoreConflict
	}
	// Once exposed at the original cwd, the already-running owner may write or
	// commit immediately. Its exact physical identity was proved while staged;
	// do not discard legitimate new work by demanding a pristine snapshot now.
	if err := restoreExecutionDirectory(p, p.Identity.Root); err != nil {
		return nil, "", err
	}
	ok = true
	return w, backlinkPath, nil
}

func restoredProof(ctx context.Context, p WorktreeRestorePlan, c WorktreeRestoreCheckpoint) (WorktreeRestoreCheckpoint, error) {
	w, err := resolveWorktreeOwner(ctx, restoreRootRequest(p, false))
	if err != nil {
		return c, ErrWorktreeRestoreUnknown
	}
	defer w.close()
	id, err := w.identity()
	owner, ownerErr := w.read()
	if err != nil || id != c.StageIdentity || ownerErr != nil || owner != p.Identity.ThreadID {
		return c, ErrWorktreeRestoreConflict
	}
	if err := w.verify(ctx, restoreRootRequest(p, false)); err != nil {
		return c, err
	}
	c.Phase, c.Generation, c.PublicationPending = "restored", id, p.DelayedPublication
	return c, nil
}

// Advance is called only after the journal's dispatch claim. Persist must CAS
// every checkpoint before it returns; no mutation begins if it fails.
func AdvanceWorktreeRestore(ctx context.Context, p WorktreeRestorePlan, c WorktreeRestoreCheckpoint, persist WorktreeRestorePersist) (WorktreeRestoreResult, error) {
	if persist == nil {
		return c, ErrWorktreeRestoreStorage
	}
	if err := restorePlanShape(p); err != nil {
		return c, err
	}
	if err := restoreSource(ctx, p); err != nil {
		return c, err
	}
	if c.Version == 0 && c.Phase == "" {
		c = WorktreeRestoreCheckpoint{Version: 1, Phase: "planned", PlanDigest: restorePlanDigest(p)}
	}
	if c.Version != 1 || c.PlanDigest != restorePlanDigest(p) {
		return c, ErrWorktreeRestoreConflict
	}
	checkpoint := func(next WorktreeRestoreCheckpoint) error {
		if err := persist(ctx, next); err != nil {
			return err
		}
		c = next
		return nil
	}
	save := func(phase string) error { next := c; next.Phase = phase; return checkpoint(next) }
	for {
		if err := ctx.Err(); err != nil {
			return c, err
		}
		switch c.Phase {
		case "planned":
			if kind, err := worktreeGit(ctx, p.Identity.SourceRoot, "cat-file", "-t", p.SnapshotCommit); err != nil || kind != "commit" {
				return c, ErrWorktreeSnapshotMissing
			}
			if err := restoreMissing(p.Identity.Root); err != nil {
				return c, err
			}
			if err := restoreNoRegistration(ctx, p); err != nil {
				return c, err
			}
			if err := save("stage-dispatching"); err != nil {
				return c, err
			}
			if _, err := prepareRestoreParent(p); err != nil {
				return c, err
			}
			managed, err := openRestoreManaged(p)
			if err != nil {
				return c, err
			}
			err = managed.Mkdir(filepath.Base(filepath.Dir(p.StageRoot)), 0700)
			managed.Close()
			if err != nil {
				return c, ErrWorktreeRestoreConflict
			}
			bucket, _, err := openOwnerDirectory(filepath.Dir(p.StageRoot))
			if err != nil {
				return c, err
			}
			err = restoreControl(bucket, ".codex-feishu-restore.json", restorePlanBytes(p), true)
			if err == nil {
				err = bucket.Mkdir(filepath.Base(p.StageRoot), 0700)
			}
			bucket.Close()
			if err != nil {
				return c, ErrWorktreeRestoreConflict
			}
			reservedStage, reservedInfo, err := openOwnerDirectory(p.StageRoot)
			if err != nil {
				return c, ErrWorktreeRestoreConflict
			}
			defer reservedStage.Close()
			if _, err := worktreeGit(ctx, p.Identity.SourceRoot, "worktree", "add", "--detach", "--", p.StageRoot, p.SnapshotCommit); err != nil {
				return c, ErrWorktreeRestoreUnknown
			}
			actualStage, err := os.Lstat(p.StageRoot)
			if err != nil || !os.SameFile(actualStage, reservedInfo) {
				return c, ErrWorktreeRestoreConflict
			}
			stageCWD := filepath.Join(p.StageRoot, p.Identity.RelativeCWD)
			if _, err := physicalDirectory(stageCWD); err != nil && p.Identity.RelativeCWD != "." {
				if _, err := worktreeGit(ctx, p.StageRoot, "--literal-pathspecs", "restore", "--source="+p.SnapshotCommit, "--worktree", "--ignore-skip-worktree-bits", "--", p.Identity.RelativeCWD); err != nil {
					return c, ErrWorktreeRestoreIncomplete
				}
			}
			proof, err := restoreStageProof(ctx, p, true)
			if err != nil {
				return c, err
			}
			if err := checkpoint(proof); err != nil {
				return c, err
			}
		case "stage-ready":
			if err := save("owner-dispatching"); err != nil {
				return c, err
			}
			if err := ensureWorktreeOwnerWithIdentity(ctx, restoreRootRequest(p, true), p.Identity.ThreadID, c.StageIdentity); err != nil {
				return c, err
			}
			if err := save("owner-ready"); err != nil {
				return c, err
			}
		case "owner-ready":
			proof, err := restoreStageProof(ctx, p, false)
			if err != nil || proof.StageIdentity != c.StageIdentity {
				return c, ErrWorktreeRestoreConflict
			}
			owner, err := readWorktreeOwner(ctx, restoreRootRequest(p, true))
			if err != nil || owner != p.Identity.ThreadID {
				return c, ErrWorktreeRestoreConflict
			}
			if err := restoreMissing(p.Identity.Root); err != nil {
				return c, err
			}
			if err := restoreNoRegistration(ctx, p); err != nil {
				return c, err
			}
			heldStage, err := resolveWorktreeOwner(ctx, restoreRootRequest(p, true))
			if err != nil {
				return c, ErrWorktreeRestoreConflict
			}
			defer heldStage.close()
			heldIdentity, err := heldStage.identity()
			if err != nil || heldIdentity != c.StageIdentity {
				return c, ErrWorktreeRestoreConflict
			}
			parent, err := verifyRestoreParent(p, c)
			if err != nil {
				return c, err
			}
			stageParent, _, err := openOwnerDirectory(filepath.Dir(p.StageRoot))
			if err != nil {
				parent.Close()
				return c, err
			}
			if err := save("move-dispatching"); err != nil {
				stageParent.Close()
				parent.Close()
				return c, err
			}
			if err := heldStage.verify(ctx, restoreRootRequest(p, true)); err != nil {
				stageParent.Close()
				parent.Close()
				return c, ErrWorktreeRestoreConflict
			}
			stageChild, err := stageParent.Lstat(filepath.Base(p.StageRoot))
			if err != nil || !os.SameFile(stageChild, heldStage.checkoutInfo) {
				stageParent.Close()
				parent.Close()
				return c, ErrWorktreeRestoreConflict
			}
			err = renameRestoreNoReplace(stageParent, filepath.Base(p.StageRoot), parent, filepath.Base(p.Identity.Root))
			if err != nil {
				stageParent.Close()
				parent.Close()
				if errors.Is(err, os.ErrExist) || errors.Is(err, unix.ENOTEMPTY) {
					return c, ErrWorktreeRestoreConflict
				}
				if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EXDEV) {
					return c, ErrWorktreeRestoreUnsupported
				}
				return c, ErrWorktreeRestoreUnknown
			}
			if err == nil {
				err = restoreSyncRoot(stageParent)
			}
			if err == nil {
				err = restoreSyncRoot(parent)
			}
			stageParent.Close()
			parent.Close()
			if err != nil {
				return c, ErrWorktreeRestoreUnknown
			}
			w, _, err := openRenamedRestore(ctx, p, c)
			if err != nil {
				return c, err
			}
			w.close()
			if err := save("moved"); err != nil {
				return c, err
			}
		case "moved":
			w, backlink, err := openRenamedRestore(ctx, p, c)
			if err != nil {
				return c, err
			}
			if err := save("repair-dispatching"); err != nil {
				w.close()
				return c, err
			}
			fresh, freshBacklink, err := openRenamedRestore(ctx, p, c)
			w.close()
			if err != nil {
				return c, err
			}
			w, backlink = fresh, freshBacklink
			if backlink == filepath.Join(p.StageRoot, ".git") {
				tmp := ".codex-feishu-backlink-" + p.RestoreID
				data := []byte(filepath.Join(p.Identity.Root, ".git") + "\n")
				if err := restoreControl(w.metadata, tmp, data, true); err != nil {
					w.close()
					return c, err
				}
				actual, info, err := readOwnerControl(w.metadata, "gitdir")
				if err != nil || !os.SameFile(info, w.backlinkInfo) || strings.TrimSuffix(string(actual), "\n") != backlink {
					w.close()
					return c, ErrWorktreeRestoreConflict
				}
				if err := w.metadata.Rename(tmp, "gitdir"); err != nil {
					w.close()
					return c, ErrWorktreeRestoreUnknown
				}
				if err := restoreSyncRoot(w.metadata); err != nil {
					w.close()
					return c, err
				}
			}
			w.close()
			proof, err := restoredProof(ctx, p, c)
			if err != nil {
				return c, err
			}
			if err := checkpoint(proof); err != nil {
				return c, err
			}
		case "restored":
			return c, nil
		default:
			return c, ErrWorktreeRestoreUnknown
		}
	}
}

// Unknown phases are inspected only. The caller must CAS a verified resume
// before invoking Advance again. Missing evidence never authorizes another add.
func ReconcileWorktreeRestore(ctx context.Context, p WorktreeRestorePlan, c WorktreeRestoreCheckpoint) (WorktreeRestoreCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		return c, err
	}
	if err := restorePlanShape(p); err != nil {
		return c, err
	}
	if c.Version == 0 && (c.Phase == "" || c.Phase == "planned") {
		c = WorktreeRestoreCheckpoint{Version: 1, Phase: "planned", PlanDigest: restorePlanDigest(p)}
	}
	if c.Version != 1 || c.PlanDigest != restorePlanDigest(p) {
		return c, ErrWorktreeRestoreConflict
	}
	switch c.Phase {
	case "planned":
		if err := restoreSource(ctx, p); err != nil {
			return c, err
		}
		managed, err := openRestoreManaged(p)
		if err != nil {
			return c, err
		}
		managed.Close()
		if err := restoreMissing(p.Identity.Root); err != nil {
			return c, err
		}
		if err := restoreMissing(filepath.Dir(p.StageRoot)); err != nil {
			return c, err
		}
		if err := restoreNoRegistration(ctx, p); err != nil {
			return c, err
		}
		if p.ParentIdentity == (ownerFileIdentity{}) {
			if err := restoreMissing(filepath.Dir(p.Identity.Root)); err != nil {
				return c, err
			}
		} else {
			parent, info, err := openOwnerDirectory(filepath.Dir(p.Identity.Root))
			if err != nil {
				return c, ErrWorktreeRestoreConflict
			}
			if ownerFileIdentityOf(info) != p.ParentIdentity {
				parent.Close()
				return c, ErrWorktreeRestoreConflict
			}
			err = restoreParentContents(parent, p)
			parent.Close()
			if err != nil {
				return c, err
			}
		}
		if kind, err := worktreeGit(ctx, p.Identity.SourceRoot, "cat-file", "-t", p.SnapshotCommit); err != nil || kind != "commit" {
			return c, ErrWorktreeSnapshotMissing
		}
		return c, nil
	case "stage-dispatching":
		return restoreStageProof(ctx, p, false)
	case "stage-ready", "owner-dispatching", "owner-ready":
		proof, err := restoreStageProof(ctx, p, false)
		if err != nil || proof.StageIdentity != c.StageIdentity {
			return c, ErrWorktreeRestoreUnknown
		}
		owner, err := readWorktreeOwner(ctx, restoreRootRequest(p, true))
		if err != nil || owner != "" && owner != p.Identity.ThreadID || c.Phase == "owner-ready" && owner == "" {
			return c, ErrWorktreeRestoreUnknown
		}
		c.Phase = "stage-ready"
		if owner == p.Identity.ThreadID {
			c.Phase = "owner-ready"
		}
		return c, nil
	case "move-dispatching":
		if restoreMissing(p.Identity.Root) == nil {
			proof, err := restoreStageProof(ctx, p, false)
			if err != nil || proof.StageIdentity != c.StageIdentity {
				return c, ErrWorktreeRestoreUnknown
			}
			owner, err := readWorktreeOwner(ctx, restoreRootRequest(p, true))
			if err != nil || owner != p.Identity.ThreadID {
				return c, ErrWorktreeRestoreUnknown
			}
			c.Phase = "owner-ready"
			return c, nil
		}
		fallthrough
	case "moved", "repair-dispatching":
		w, backlink, err := openRenamedRestore(ctx, p, c)
		if err != nil {
			return c, err
		}
		w.close()
		if backlink == filepath.Join(p.Identity.Root, ".git") {
			return restoredProof(ctx, p, c)
		}
		c.Phase = "moved"
		return c, nil
	case "restored":
		return restoredProof(ctx, p, c)
	default:
		return c, ErrWorktreeRestoreUnknown
	}
}

// Validate a confirmed restore generation after ordinary task edits/commits.
// Its authority comes from the caller's confirmed journal, not snapshot HEAD.
func ValidateRestoredWorktree(ctx context.Context, p WorktreeRestorePlan, c WorktreeRestoreResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := restorePlanShape(p); err != nil {
		return err
	}
	if c.Version != 1 || c.Phase != "restored" || c.Generation == "" || c.PlanDigest != restorePlanDigest(p) {
		return ErrWorktreeRestoreConflict
	}
	request := restoreRootRequest(p, false)
	id, err := worktreeOwnerIdentity(ctx, request)
	owner, ownerErr := readWorktreeOwner(ctx, request)
	if err != nil || id != c.Generation || ownerErr != nil || owner != p.Identity.ThreadID {
		return ErrWorktreeRestoreConflict
	}
	parent, err := verifyRestoreParent(p, c)
	if err != nil {
		return err
	}
	parent.Close()
	return nil
}

// The caller first persists the confirmed result with PublicationPending set.
// After native queue/history proves the original owner active, it publishes the
// marker and clears that flag with a CAS after successful readback. Old desktop
// cleanup jobs cannot be canceled by removing or publishing this marker.
func PublishRestoredWorktree(ctx context.Context, p WorktreeRestorePlan, c WorktreeRestoreResult) error {
	if err := ValidateRestoredWorktree(ctx, p, c); err != nil {
		return err
	}
	root := restoreRootRequest(p, false)
	if !p.DelayedPublication {
		return nil
	}
	w, err := resolveWorktreeOwner(ctx, root)
	if err != nil {
		return ErrWorktreeRestoreConflict
	}
	defer w.close()
	id, err := w.identity()
	if err != nil || id != c.Generation {
		return ErrWorktreeRestoreConflict
	}
	parent, err := verifyRestoreParent(p, c)
	if err != nil {
		return err
	}
	defer parent.Close()
	child, err := parent.Lstat(filepath.Base(p.Identity.Root))
	if err != nil || !os.SameFile(child, w.checkoutInfo) {
		return ErrWorktreeRestoreConflict
	}
	if err := w.verify(ctx, root); err != nil {
		return ErrWorktreeRestoreConflict
	}
	if err := restoreControl(parent, ".codex-worktree-name", p.PublicationBytes, true); err != nil {
		return err
	}
	if err := w.verify(ctx, root); err != nil {
		return ErrWorktreeRestoreConflict
	}
	return ValidateRestoredWorktree(ctx, p, c)
}
