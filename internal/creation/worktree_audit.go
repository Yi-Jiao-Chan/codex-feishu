package creation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codex-feishu/compat"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/store"
	"golang.org/x/sys/unix"
)

const WorktreeAuditWarning = "Desktop compatibility is limited to " + compat.ManagedWorktreeDesktopVersion + "; the desktop version is not detected automatically. Owner repair cannot cancel cleanup already queued by the desktop. Resolve conflicting owners through the desktop or a separately reviewed manual repair; this tool never overwrites them."

const maxWorktreeAuditEntries = 4096
const maxWorktreeAuditBytes = 8 << 20

var ErrWorktreeAudit = errors.New("worktree audit identity or private report unavailable")

// This capability deliberately excludes every native mutation and history read.
type WorktreeAuditNative interface {
	Metadata(context.Context, string) (codexclient.Thread, error)
	CodexHome() string
	AppServerVersion() string
}

// Reports contain private routing and filesystem metadata, never task bodies.
type WorktreeAuditReport struct {
	Version          int                  `json:"version"`
	HostID           string               `json:"host_id"`
	AppID            string               `json:"app_id"`
	DatabaseFile     string               `json:"database_file"`
	CodexHome        string               `json:"codex_home"`
	AppServerVersion string               `json:"app_server_version"`
	DesktopVersion   string               `json:"desktop_compatibility_version"`
	CreatedAtMS      int64                `json:"created_at_ms"`
	Warning          string               `json:"warning"`
	Entries          []WorktreeAuditEntry `json:"entries"`
}

type WorktreeAuditEntry struct {
	DraftID       string `json:"draft_id"`
	IntentID      string `json:"intent_id"`
	ThreadID      string `json:"thread_id"`
	Path          string `json:"path"`
	Layout        string `json:"layout"`
	ExpectedOwner string `json:"expected_owner"`
	ObservedOwner string `json:"observed_owner"`
	Identity      string `json:"physical_identity"`
	IntentDigest  string `json:"intent_digest"`
	Status        string `json:"status"`
}

type WorktreeAuditor struct {
	Store  *store.Store
	Native WorktreeAuditNative
	Config config.Config
	Now    func() time.Time
}

func (a WorktreeAuditor) scope() (WorktreeAuditReport, error) {
	if a.Store == nil || a.Native == nil || a.Config.Host.ID == "" || a.Config.Feishu.AppID == "" || !compat.VerifiedWorktreeCodexVersion(a.Native.AppServerVersion()) {
		return WorktreeAuditReport{}, ErrWorktreeAudit
	}
	home, err := physicalDirectory(a.Native.CodexHome())
	if err != nil || home != a.Native.CodexHome() {
		return WorktreeAuditReport{}, ErrWorktreeAudit
	}
	database, err := filepath.EvalSymlinks(a.Config.Storage.DatabaseFile)
	if err != nil || !filepath.IsAbs(database) {
		return WorktreeAuditReport{}, ErrWorktreeAudit
	}
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	return WorktreeAuditReport{Version: 1, HostID: a.Config.Host.ID, AppID: a.Config.Feishu.AppID, DatabaseFile: database, CodexHome: home,
		AppServerVersion: a.Native.AppServerVersion(), DesktopVersion: compat.ManagedWorktreeDesktopVersion,
		CreatedAtMS: now.UnixMilli(), Warning: WorktreeAuditWarning, Entries: []WorktreeAuditEntry{}}, nil
}

func (a WorktreeAuditor) Audit(ctx context.Context) (WorktreeAuditReport, error) {
	report, err := a.scope()
	if err != nil {
		return report, err
	}
	ids, err := a.Store.ConfirmedCreationThreads(ctx, report.HostID, report.AppID)
	if err != nil || len(ids) > maxWorktreeAuditEntries {
		return report, ErrWorktreeAudit
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		i, err := a.Store.BridgeCreationThread(ctx, report.HostID, report.AppID, id)
		if err != nil {
			return report, ErrWorktreeAudit
		}
		if i.State != "confirmed" || i.WorkspaceMode != "worktree" {
			continue
		}
		entry, _ := a.inspect(ctx, report, i)
		report.Entries = append(report.Entries, entry)
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	return report, nil
}

func (a WorktreeAuditor) inspect(ctx context.Context, scope WorktreeAuditReport, i store.CreationIntent) (WorktreeAuditEntry, error) {
	e := WorktreeAuditEntry{DraftID: i.DraftID, IntentID: i.ID, ThreadID: i.NativeThreadID, Path: i.WorktreePath, ExpectedOwner: i.NativeThreadID, Status: "identity-unverified"}
	if i.State != "confirmed" || i.WorkspaceMode != "worktree" || i.HostID != scope.HostID || i.AppID != scope.AppID || i.ExecutionDirectory() == "" {
		return e, ErrWorktreeAudit
	}
	legacy, err := legacyWorktreePath(scope.DatabaseFile, i.DraftID)
	if err != nil {
		return e, ErrWorktreeAudit
	}
	switch {
	case i.WorktreePath == legacy:
		e.Layout = "legacy"
	case filepath.Clean(i.WorktreePath) == i.WorktreePath && filepath.Dir(filepath.Dir(i.WorktreePath)) == filepath.Join(scope.CodexHome, "worktrees"):
		e.Layout = "managed"
	default:
		return e, ErrWorktreeAudit
	}
	chat := i.ChatID
	if i.FollowMode == "topic" {
		chat = i.TargetChatID
	}
	b, err := a.Store.Binding(ctx, scope.HostID, i.NativeThreadID)
	if err != nil || b.IntentID != i.ID || b.ChatID != chat || b.RootMessageID != i.RootMessageID || b.PlatformThreadID != i.PlatformThreadID || (b.FollowMode == "topic") != (i.FollowMode == "topic") || b.RepairRequired {
		return e, ErrWorktreeAudit
	}
	g, err := a.Store.GroupIntent(ctx, i.ID)
	if err != nil || g.State != "confirmed" || g.HostID != scope.HostID || g.ThreadID != i.NativeThreadID || g.ChatID != b.ChatID || g.OwnerID != i.OwnerID {
		return e, ErrWorktreeAudit
	}
	w, err := a.Store.CreationAttempt(ctx, i.DraftID, "worktree")
	if err != nil || w.State != "confirmed" || w.RemoteID != i.WorktreePath {
		return e, ErrWorktreeAudit
	}
	wantWorktree, _ := json.Marshal(frozenWorktree(i))
	if w.Params != string(wantWorktree) {
		return e, ErrWorktreeAudit
	}
	n, err := a.Store.CreationAttempt(ctx, i.DraftID, "native")
	wantNative, paramsErr := codexclient.CreationRequestJSON(codexclient.CreationSpec{IntentID: i.ID, CWD: i.ExecutionDirectory(), ProjectID: i.ProjectID})
	if err != nil || paramsErr != nil || n.State != "confirmed" || n.RemoteID != i.NativeThreadID || n.Params != wantNative {
		return e, ErrWorktreeAudit
	}
	// Freeze only authorization/routing metadata. No task text enters this plan.
	frozen, _ := json.Marshal(struct {
		Request                                                     worktreeRequest
		Host, App, Intent, Thread, Project, Creator, Owner, Receipt string
		Binding                                                     store.Binding
		WorktreeAttempt, NativeAttempt                              store.CreationAttempt
	}{frozenWorktree(i), i.HostID, i.AppID, i.ID, i.NativeThreadID, i.ProjectID, i.CreatorID, i.OwnerID, i.ReceiptID, b,
		store.CreationAttempt{ID: w.ID, State: w.State, Params: w.Params, RemoteID: w.RemoteID, Result: w.Result},
		store.CreationAttempt{ID: n.ID, State: n.State, Params: n.Params, RemoteID: n.RemoteID}})
	digest := sha256.Sum256(frozen)
	e.IntentDigest = hex.EncodeToString(digest[:])
	call, cancel := context.WithTimeout(ctx, 5*time.Second)
	t, err := a.Native.Metadata(call, i.NativeThreadID)
	cancel()
	var source string
	expected, sourceErr := codexclient.CreationThreadSource(i.ID)
	if err != nil || sourceErr != nil || t.ID != i.NativeThreadID || t.CWD != i.ExecutionDirectory() || t.Ephemeral || t.ParentThreadID != nil || t.ForkedFromID != nil ||
		json.Unmarshal(t.ThreadSource, &source) != nil || source != expected || i.ProjectID != "" && (t.ProjectID == nil || *t.ProjectID != i.ProjectID) {
		e.Status = "native-unverified"
		return e, ErrWorktreeAudit
	}
	if _, err := os.Lstat(i.WorktreePath); errors.Is(err, os.ErrNotExist) {
		e.Status = "missing-directory"
		return e, ErrWorktreeAudit
	}
	p := frozenWorktree(i)
	if err := verifyWorktree(ctx, p, false); err != nil {
		return e, ErrWorktreeAudit
	}
	e.Identity, err = worktreeOwnerIdentity(ctx, p)
	if err != nil || verifyCreationWorktreeGeneration(ctx, i, w) != nil {
		return e, ErrWorktreeAudit
	}
	e.ObservedOwner, err = readWorktreeOwner(ctx, p)
	if err != nil {
		return e, ErrWorktreeAudit
	}
	switch e.ObservedOwner {
	case "":
		e.Status = "owner-missing"
	case i.NativeThreadID:
		e.Status = "owner-correct"
	default:
		e.Status = "owner-conflict"
	}
	return e, nil
}

// Repair requires both application/deployment locks held by its caller. It
// never recovers unrelated creation lanes or rewrites a frozen workflow.
func (a WorktreeAuditor) Repair(ctx context.Context, plan WorktreeAuditReport) (map[string]int, error) {
	counts := map[string]int{}
	current, err := a.scope()
	if err != nil || validateWorktreeAuditReport(plan) != nil || plan.HostID != current.HostID || plan.AppID != current.AppID || plan.DatabaseFile != current.DatabaseFile ||
		plan.CodexHome != current.CodexHome || plan.AppServerVersion != current.AppServerVersion || plan.DesktopVersion != current.DesktopVersion {
		return counts, ErrWorktreeAudit
	}
	for _, old := range plan.Entries {
		if old.Status != "owner-missing" {
			if old.Status == "owner-correct" {
				counts["skipped"]++
			} else {
				counts["refused"]++
			}
			continue
		}
		if err := ctx.Err(); err != nil {
			return counts, err
		}
		i, err := a.Store.BridgeCreationThread(ctx, plan.HostID, plan.AppID, old.ThreadID)
		if err != nil {
			counts["refused"]++
			continue
		}
		fresh, inspectErr := a.inspect(ctx, current, i)
		if inspectErr != nil || (fresh.Status != "owner-missing" && fresh.Status != "owner-correct") || fresh.DraftID != old.DraftID || fresh.IntentID != old.IntentID || fresh.Path != old.Path ||
			fresh.ExpectedOwner != old.ExpectedOwner || fresh.Layout != old.Layout || fresh.IntentDigest != old.IntentDigest || fresh.Identity != old.Identity || old.Identity == "" {
			counts["refused"]++
			continue
		}
		params, _ := json.Marshal(struct {
			Version                                                        int `json:"version"`
			Host, App, Draft, Intent, Thread, Path, Identity, IntentDigest string
		}{1, plan.HostID, plan.AppID, old.DraftID, old.IntentID, old.ThreadID, old.Path, old.Identity, old.IntentDigest})
		attempt, err := a.Store.PrepareCreationAttempt(ctx, old.DraftID, "worktree-owner-repair:"+old.Identity, string(params), current.CreatedAtMS)
		if err != nil {
			return counts, ErrWorktreeAudit
		}
		if attempt.State == "confirmed" {
			if fresh.Status == "owner-correct" && attempt.RemoteID == old.ThreadID {
				counts["confirmed"]++
			} else {
				counts["refused"]++
			}
			continue
		}
		if attempt.State == "dispatching" || attempt.State == "unknown" {
			// Only this exact scope/physical-identity step is recovered. Missing
			// ownership never authorizes another write after an ambiguous dispatch.
			outcome := "unknown"
			if fresh.Status == "owner-correct" {
				outcome = "confirmed"
			}
			if err := a.Store.FinishCreationAttempt(ctx, attempt, outcome, old.ThreadID, `{}`); err != nil {
				return counts, ErrWorktreeAudit
			}
			counts[outcome]++
			continue
		}
		if attempt.State != "prepared" {
			counts["refused"]++
			continue
		}
		claimed, err := a.Store.DispatchCreationAttempt(ctx, attempt)
		if err != nil || !claimed {
			return counts, ErrWorktreeAudit
		}
		err = ensureWorktreeOwnerWithIdentity(ctx, frozenWorktree(i), old.ThreadID, old.Identity)
		outcome := "confirmed"
		if err != nil {
			outcome = "unknown"
		}
		if finishErr := a.Store.FinishCreationAttempt(ctx, attempt, outcome, old.ThreadID, `{}`); finishErr != nil {
			return counts, ErrWorktreeAudit
		}
		counts[outcome]++
	}
	return counts, nil
}

func validateWorktreeAuditReport(p WorktreeAuditReport) error {
	if p.Version != 1 || p.HostID == "" || p.AppID == "" || !filepath.IsAbs(p.DatabaseFile) || !filepath.IsAbs(p.CodexHome) ||
		!compat.VerifiedWorktreeCodexVersion(p.AppServerVersion) || p.DesktopVersion != compat.ManagedWorktreeDesktopVersion || len(p.Entries) > maxWorktreeAuditEntries {
		return ErrWorktreeAudit
	}
	seen := map[string]bool{}
	for _, e := range p.Entries {
		if e.ThreadID == "" || e.ExpectedOwner != e.ThreadID || e.DraftID == "" || e.IntentID == "" || !filepath.IsAbs(e.Path) || seen[e.ThreadID] {
			return ErrWorktreeAudit
		}
		seen[e.ThreadID] = true
		switch e.Status {
		case "missing-directory", "owner-missing", "owner-correct", "owner-conflict", "identity-unverified", "native-unverified":
		default:
			return ErrWorktreeAudit
		}
		if e.Status == "owner-missing" && (e.ObservedOwner != "" || !strings.HasPrefix(e.Identity, "v1:") || e.IntentDigest == "") {
			return ErrWorktreeAudit
		}
	}
	return nil
}

func privateAuditRoot(path string) (*os.Root, string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, "", ErrWorktreeAudit
	}
	root, _, err := openOwnerDirectory(filepath.Dir(path))
	if err != nil {
		if root != nil {
			root.Close()
		}
		return nil, "", ErrWorktreeAudit
	}
	return root, filepath.Base(path), nil
}

func WriteWorktreeAuditReport(path string, report WorktreeAuditReport) error {
	if err := validateWorktreeAuditReport(report); err != nil {
		return err
	}
	root, name, err := privateAuditRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil || len(raw) > maxWorktreeAuditBytes {
		return ErrWorktreeAudit
	}
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return ErrWorktreeAudit
	}
	_, err = f.Write(append(raw, '\n'))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ErrWorktreeAudit
	}
	dir, err := root.Open(".")
	if err != nil {
		return ErrWorktreeAudit
	}
	defer dir.Close()
	if dir.Sync() != nil {
		return ErrWorktreeAudit
	}
	return nil
}

func ReadWorktreeAuditReport(path string) (WorktreeAuditReport, error) {
	var report WorktreeAuditReport
	root, name, err := privateAuditRoot(path)
	if err != nil {
		return report, err
	}
	defer root.Close()
	before, err := root.Lstat(name)
	if err != nil || !ownerControlInfo(before, false) || before.Mode().Perm() != 0600 || before.Size() > maxWorktreeAuditBytes {
		return report, ErrWorktreeAudit
	}
	f, err := root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return report, ErrWorktreeAudit
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || !ownerControlInfo(after, false) || after.Mode().Perm() != 0600 || after.Size() > maxWorktreeAuditBytes {
		return report, ErrWorktreeAudit
	}
	decoder := json.NewDecoder(io.LimitReader(f, maxWorktreeAuditBytes+1))
	decoder.DisallowUnknownFields()
	var extra any
	if decoder.Decode(&report) != nil || !errors.Is(decoder.Decode(&extra), io.EOF) || validateWorktreeAuditReport(report) != nil {
		return WorktreeAuditReport{}, ErrWorktreeAudit
	}
	return report, nil
}
