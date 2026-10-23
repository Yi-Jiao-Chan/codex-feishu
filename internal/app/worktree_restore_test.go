package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/creation"
	"codex-feishu/internal/store"
	"codex-feishu/internal/submissions"
)

const workspaceTestThread = "018f0000-0000-7000-8000-000000000001"

// Attach deliberately cannot hide a missing checkout. Add consumes the input
// into correlated native history, so reconnect must reconcile instead of add.
type workspaceRuntimeNative struct {
	*serviceNative
	home, thread              string
	attaches, missingAttaches atomic.Int32
	clientsMu                 sync.Mutex
	clients, submittedThreads []string
	queueEntered              chan struct{}
	queueRelease              <-chan struct{}
	queueOnce                 sync.Once
}

func (n *workspaceRuntimeNative) CodexHome() string        { return n.home }
func (n *workspaceRuntimeNative) AppServerVersion() string { return "0.162.1" }

func (n *workspaceRuntimeNative) Queue(ctx context.Context, thread string) ([]codexclient.Queued, error) {
	if n.queueEntered != nil {
		n.queueOnce.Do(func() {
			close(n.queueEntered)
			select {
			case <-n.queueRelease:
			case <-ctx.Done():
			}
		})
	}
	return n.serviceNative.Queue(ctx, thread)
}

func (n *workspaceRuntimeNative) Attach(ctx context.Context, id string) (codexclient.Thread, error) {
	n.attaches.Add(1)
	t, err := n.Metadata(ctx, id)
	if err != nil {
		return t, err
	}
	if info, err := os.Stat(t.CWD); err != nil || !info.IsDir() {
		n.missingAttaches.Add(1)
		return codexclient.Thread{}, errors.New("fixture refuses attach to missing cwd")
	}
	return t, nil
}

func (n *workspaceRuntimeNative) Add(ctx context.Context, thread, body, client string) (codexclient.Queued, error) {
	if thread != n.thread {
		return codexclient.Queued{}, errors.New("fixture rejects replacement thread")
	}
	meta, err := n.Metadata(ctx, thread)
	if err != nil {
		return codexclient.Queued{}, err
	}
	if info, err := os.Stat(meta.CWD); err != nil || !info.IsDir() {
		return codexclient.Queued{}, errors.New("fixture refuses input before restoration")
	}
	ordinal := n.adds.Add(1)
	n.clientsMu.Lock()
	n.clients = append(n.clients, client)
	n.submittedThreads = append(n.submittedThreads, thread)
	n.clientsMu.Unlock()
	content, _ := json.Marshal(map[string]string{"type": "text", "text": body})
	started := time.Now().Unix()
	n.mu.Lock()
	n.turns[thread] = append(n.turns[thread], codexclient.Turn{ID: fmt.Sprintf("synthetic-input-turn-%d", ordinal), Status: "inProgress", ItemsView: "full", StartedAt: &started,
		Items: []codexclient.Item{{ID: fmt.Sprintf("synthetic-input-item-%d", ordinal), Type: "userMessage", ClientID: &client, Content: []json.RawMessage{content}}}})
	meta.Status.Type = "active"
	n.threads[thread] = meta
	n.mu.Unlock()
	return codexclient.Queued{ID: fmt.Sprintf("synthetic-queue-%d", ordinal), ClientUserMessageID: &client}, nil
}

type workspaceRuntimeFixture struct {
	db                *store.Store
	r                 *Runtime
	n                 *workspaceRuntimeNative
	s                 *Service
	source, root, cwd string
	snapshot, chat    string
	bridge            *store.CreationIntent
	prepares          atomic.Int32
}

func newWorkspaceRuntimeFixture(t *testing.T, subdirectory, bridge bool, named ...bool) *workspaceRuntimeFixture {
	t.Helper()
	db, base, _, r := runtimeFixture(t)
	source, err := filepath.EvalSymlinks(r.AllowedRoots[0])
	if err != nil {
		t.Fatal(err)
	}
	r.AllowedRoots = []string{source}
	creationRuntimeGit(t, source, "init", "--initial-branch=main")
	if err := os.Mkdir(filepath.Join(source, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "sub", "task.txt"), []byte("synthetic original content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	creationRuntimeGit(t, source, "add", ".")
	creationRuntimeGit(t, source, "-c", "commit.gpgsign=false", "commit", "-m", "synthetic initial commit")
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	bucket := "abcd"
	if len(named) > 0 && named[0] {
		bucket = "codex-feishu-" + strings.Repeat("c", 32)
	}
	root := filepath.Join(home, "worktrees", bucket, "project")
	if err := os.MkdirAll(filepath.Dir(root), 0700); err != nil {
		t.Fatal(err)
	}
	creationRuntimeGit(t, source, "worktree", "add", "--detach", "--", root, "HEAD")
	if err := os.WriteFile(filepath.Join(filepath.Dir(root), ".codex-worktree-name"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	cwd := root
	if subdirectory {
		cwd = filepath.Join(root, "sub")
	}
	n := &workspaceRuntimeNative{serviceNative: &serviceNative{runtimeNative: base}, home: home, thread: workspaceTestThread}
	r.Native = n
	s := creationRuntimeService(r)
	s.Config.Discovery.AllowedRoots = []string{source}
	f := &workspaceRuntimeFixture{db: db, r: r, n: n, s: s, source: source, root: root, cwd: cwd, chat: "workspace-chat"}
	if bridge {
		creating := &creationRuntimeNative{runtimeNative: base}
		directory := source
		if subdirectory {
			directory = filepath.Join(source, "sub")
		}
		plan := store.CreationIntent{Directory: directory, WorkspaceMode: "worktree", BaseBranch: "refs/heads/main", BaseCommit: creationRuntimeGit(t, source, "rev-parse", "HEAD"), GitRoot: source, GitCommonDir: filepath.Join(source, ".git"), WorktreePath: root}
		d := prepareRuntimeCreationWorkspace(t, r, creating, workspaceTestThread, plan)
		_, initial, err := db.MaterializeCreation(context.Background(), d.ID)
		if err != nil {
			t.Fatal(err)
		}
		acceptRuntimeCreation(t, r, creating, d, initial)
		i, err := db.CreationIntent(context.Background(), d.ID)
		if err != nil {
			t.Fatal(err)
		}
		f.bridge, f.chat = &i, i.ChatID
	} else {
		n.set(workspaceTestThread, cwd, "completed")
		runtimeBind(t, db, r, workspaceTestThread, f.chat)
		if err := db.MarkEligible(context.Background(), r.HostID, workspaceTestThread, "user-"+workspaceTestThread, cwd, source); err != nil {
			t.Fatal(err)
		}
	}
	ownerPath := creationRuntimeGit(t, root, "rev-parse", "--path-format=absolute", "--git-path", "codex-thread.json")
	owner, _ := json.Marshal(map[string]any{"version": 1, "ownerThreadId": workspaceTestThread})
	if err := os.WriteFile(ownerPath, owner, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "task.txt"), []byte("synthetic dirty snapshot content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "captured.txt"), []byte("synthetic captured untracked content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	creationRuntimeGit(t, root, "add", ".")
	tree := creationRuntimeGit(t, root, "write-tree")
	f.snapshot = creationRuntimeGit(t, root, "commit-tree", tree, "-p", "HEAD", "-m", "synthetic desktop snapshot")
	creationRuntimeGit(t, source, "update-ref", creation.WorktreeSnapshotRef(root), f.snapshot)
	f.wire()
	return f
}

func (f *workspaceRuntimeFixture) scope() store.SubmissionScope {
	return store.SubmissionScope{HostID: f.r.HostID, AppID: f.r.AppID, ThreadID: f.n.thread}
}

func (f *workspaceRuntimeFixture) wire() {
	f.r.Store, f.s.Store = f.db, f.db
	f.r.CanAttach, f.r.CreationAuthorize = f.s.creationAttachGate, f.s.authorizeCreation
	f.r.PrepareWorkspace = func(ctx context.Context, thread string) error {
		err := f.s.prepareWorkspace(ctx, f.n, thread)
		f.prepares.Add(1)
		return err
	}
	f.r.RememberWorkspace = func(ctx context.Context, meta codexclient.Thread) error { return f.s.rememberWorkspace(ctx, f.n, meta) }
	f.r.WorkspaceAuthorize = f.s.authorizeRestoredWorkspace
	controller := &submissions.Controller{Store: f.db, Native: f.n, HostID: f.r.HostID, AppID: f.r.AppID, ThreadID: f.n.thread}
	controller.BeginWrite = func(ctx context.Context) (context.Context, func(), error) {
		return f.s.beginSubmissionWrite(ctx, f.n, f.n.thread)
	}
	f.r.ActorTick = func(ctx context.Context, _ string) error { return controller.Tick(ctx) }
	f.r.AfterDelivery = func(ctx context.Context, thread string, turns []codexclient.Turn) error {
		return f.s.publishRestoredWorkspace(ctx, f.n, thread, turns)
	}
}

func (f *workspaceRuntimeFixture) remove(t *testing.T) {
	t.Helper()
	creationRuntimeGit(t, f.source, "worktree", "remove", "--force", "--", f.root)
}

func (f *workspaceRuntimeFixture) receive(t *testing.T, message string, legacy bool) store.Receipt {
	t.Helper()
	in := store.Receipt{AppID: f.r.AppID, ThreadID: f.n.thread, ChatID: f.chat, MessageID: message, Body: "synthetic ordinary Feishu instruction"}
	var r store.Receipt
	var fresh bool
	var err error
	if legacy {
		r, fresh, err = f.db.Claim(context.Background(), in)
	} else {
		r, fresh, err = f.db.ReceiveSubmissionInput(context.Background(), f.scope(), in)
	}
	if err != nil || !fresh {
		t.Fatal("ordinary input was not durably admitted", err)
	}
	return r
}

func (f *workspaceRuntimeFixture) cancelInput(t *testing.T, receipt store.Receipt) {
	t.Helper()
	ctx := context.Background()
	m, err := f.db.Mirror(ctx, f.r.HostID, f.n.thread, store.QueueCardKey(receipt.ID))
	if err != nil {
		t.Fatal(err)
	}
	delivery, claimed, err := f.db.ClaimMirror(ctx, f.r.HostID, f.n.thread, m.Key, m.Revision, time.Now().UnixMilli())
	if err != nil || !claimed {
		t.Fatal("fixture card was not deliverable", err)
	}
	if err := f.db.FinishMirrorAttempt(ctx, delivery.ID, "confirmed", "synthetic-cancel-card"); err != nil {
		t.Fatal(err)
	}
	input, err := f.db.Submission(ctx, f.scope(), receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	receiver := &Receiver{Store: f.db, Config: f.s.Config, BotID: "synthetic-bot", BeginBound: f.s.beginBound, ActionAllowed: func(string) bool { return false }}
	action := receiverAction(t, "creator", "synthetic-cancel-card", receipt.ActionToken, "", input.Revision)
	action.Event.Context.OpenChatID = f.chat
	action.Event.Action.Value["action"] = "cancel_pending"
	if _, err := receiver.Action(ctx, action); err != nil {
		t.Fatal(err)
	}
	input, err = f.db.Submission(ctx, f.scope(), receipt.ID)
	if err != nil || input.State != "removed" {
		t.Fatal("callback did not remove original pending input", input.State, err)
	}
}

func (f *workspaceRuntimeFixture) run(t *testing.T) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.r.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("workspace runtime stopped unexpectedly: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("workspace runtime did not join")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func (f *workspaceRuntimeFixture) assertRestored(t *testing.T, receipt store.Receipt) store.WorktreeRestoreAttempt {
	t.Helper()
	ready := false
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		n, err := f.db.Submission(context.Background(), f.scope(), receipt.ID)
		if err == nil && n.State == "consumed" && f.n.adds.Load() == 1 {
			ready = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !ready {
		input, _ := f.db.Submission(context.Background(), f.scope(), receipt.ID)
		a, _ := f.db.WorktreeRestoreAttempt(context.Background(), f.scope(), receipt.ID)
		meta, _ := f.n.Metadata(context.Background(), f.n.thread)
		t.Fatalf("restore did not reach original input: input=%s add=%d attach=%d missingAttach=%d restore=%s/%s/%s workspaceAuth=%v normalAuth=%v", input.State, f.n.adds.Load(), f.n.attaches.Load(), f.n.missingAttaches.Load(), a.State, a.Phase, a.ErrorCode, f.s.authorizeRestoredWorkspace(context.Background(), meta), f.r.Authorize(context.Background(), meta, false))
	}
	if f.n.missingAttaches.Load() != 0 {
		t.Fatal("Attach observed missing cwd before the restore hook ran")
	}
	f.n.clientsMu.Lock()
	if len(f.n.clients) != 1 || f.n.clients[0] != receipt.ClientID || f.n.submittedThreads[0] != receipt.ThreadID {
		t.Error("original receipt/client/thread identity changed")
	}
	f.n.clientsMu.Unlock()
	if actual := creationRuntimeGit(t, f.root, "rev-parse", "HEAD"); actual != f.snapshot {
		t.Fatal("restoration used source HEAD instead of the exact desktop snapshot")
	}
	for path, expected := range map[string]string{"sub/task.txt": "synthetic dirty snapshot content\n", "captured.txt": "synthetic captured untracked content\n"} {
		raw, err := os.ReadFile(filepath.Join(f.root, path))
		if err != nil || string(raw) != expected {
			t.Fatal("snapshot data was not restored", path, err)
		}
	}
	ownerPath := creationRuntimeGit(t, f.root, "rev-parse", "--path-format=absolute", "--git-path", "codex-thread.json")
	raw, err := os.ReadFile(ownerPath)
	var owner struct {
		Version int    `json:"version"`
		Thread  string `json:"ownerThreadId"`
	}
	if err != nil || json.Unmarshal(raw, &owner) != nil || owner.Version != 1 || owner.Thread != receipt.ThreadID {
		t.Fatal("restoration did not register the original owner", err)
	}
	a, err := f.db.LatestConfirmedWorktreeRestoreAttempt(context.Background(), f.scope())
	if err != nil || a.ReceiptID != receipt.ID {
		t.Fatal("restore proof was not attached to the original receipt", err)
	}
	var result creation.WorktreeRestoreResult
	if json.Unmarshal([]byte(a.ResultJSON), &result) != nil || result.Generation == "" || result.Phase != "restored" {
		t.Fatal("restored physical generation was not recorded")
	}
	meta, _ := f.n.Metadata(context.Background(), f.n.thread)
	if err := f.s.authorizeRestoredWorkspace(context.Background(), meta); err != nil {
		t.Fatal("restored generation could not authorize the original thread", err)
	}
	return a
}

func TestWorkspaceRestoreBeforeAttachKeepsOriginalInputAndAppBinding(t *testing.T) {
	for _, name := range []string{"root", "subdirectory", "legacy-pending"} {
		t.Run(name, func(t *testing.T) {
			f := newWorkspaceRuntimeFixture(t, name != "root", false)
			ctx := context.Background()
			before, _ := f.db.Binding(ctx, f.r.HostID, f.n.thread)
			f.remove(t)
			if _, err := f.db.ThreadWorktreeIdentity(ctx, f.scope()); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("fixture unexpectedly registered a modern worktree identity", err)
			}
			receipt := f.receive(t, "original-feishu-input", name == "legacy-pending")
			f.run(t)
			a := f.assertRestored(t, receipt)
			identity, err := f.db.ThreadWorktreeIdentity(ctx, f.scope())
			if err != nil || identity.Root != f.root || identity.RawCWD != f.cwd || identity.SourceRoot != f.source {
				t.Fatal("observation/snapshot backfill guessed the wrong root or subdirectory", err)
			}
			grant, err := f.db.WorktreeRestoreGrant(ctx, f.scope(), receipt.ID)
			if err != nil || name == "legacy-pending" && grant.Origin != "legacy-pending" {
				t.Fatal("legacy receipt did not retain its own restore permission", err)
			}
			rootInfo, _ := os.Stat(f.root)
			queueReads := f.n.queueReads.Load()
			if _, fresh, err := f.db.ReceiveSubmissionInput(ctx, f.scope(), receipt); err != nil || fresh {
				t.Fatal("duplicate Feishu event created another receipt", err)
			}
			f.r.Wake(f.n.thread)
			runtimeWait(t, func() bool { return f.n.queueReads.Load() > queueReads })
			after, _ := f.db.Binding(ctx, f.r.HostID, f.n.thread)
			current, _ := os.Stat(f.root)
			attempts, err := f.db.WorktreeRestoreAttempts(ctx, f.scope())
			if err != nil || len(attempts) != 1 || attempts[0].ID != a.ID || !os.SameFile(rootInfo, current) || f.n.adds.Load() != 1 || after != before {
				t.Fatal("replay replaced the checkout, route, restore attempt or native input", err)
			}
		})
	}
}

func TestWorkspaceRestoreBridgeIntentRetainsMarkerAndBirthJournal(t *testing.T) {
	f := newWorkspaceRuntimeFixture(t, true, true)
	ctx := context.Background()
	var sequence int
	var databaseName, databasePath string
	if err := f.db.DB.QueryRow(`PRAGMA database_list`).Scan(&sequence, &databaseName, &databasePath); err != nil {
		t.Fatal(err)
	}
	cfg := f.s.Config
	cfg.Storage.DatabaseFile = databasePath
	auditor := creation.WorktreeAuditor{Store: f.db, Native: f.n, Config: cfg}
	report, err := auditor.Audit(ctx)
	if err != nil || len(report.Entries) != 1 || report.Entries[0].Status != "owner-correct" || report.Entries[0].Identity == "" {
		t.Fatal("fixture original bridge generation could not be verified", err)
	}
	// Seed the real physical birth proof instead of the historical {} used by
	// the shared creation fixture. Restoration must keep this old generation
	// immutable and authorize the successor through its separate journal.
	birth, _ := json.Marshal(map[string]any{"version": 1, "identity": report.Entries[0].Identity})
	if _, err := f.db.DB.Exec(`UPDATE creation_attempts SET result_json=? WHERE draft_id=? AND step='worktree'`, string(birth), f.bridge.DraftID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(f.root), ".codex-feishu-"+f.bridge.DraftID+".identity"), birth, 0600); err != nil {
		t.Fatal(err)
	}
	original, err := f.db.CreationAttempt(ctx, f.bridge.DraftID, "worktree")
	if err != nil {
		t.Fatal(err)
	}
	f.remove(t)
	markerPath := filepath.Join(filepath.Dir(f.root), ".codex-feishu-"+f.bridge.DraftID+".json")
	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}
	receipt := f.receive(t, "bridge-followup-input", false)
	f.run(t)
	restored := f.assertRestored(t, receipt)
	var generation creation.WorktreeRestoreResult
	if err := json.Unmarshal([]byte(restored.ResultJSON), &generation); err != nil || generation.Generation == report.Entries[0].Identity {
		t.Fatal("restore reused the old physical generation", err)
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil || string(marker) != string(creation.CreationWorktreeRestoreMarker(*f.bridge)) {
		t.Fatal("restoration did not reproduce the frozen bridge marker", err)
	}
	after, err := f.db.CreationAttempt(ctx, f.bridge.DraftID, "worktree")
	if err != nil || after != original {
		t.Fatal("restoration rewrote the original creation birth journal", err)
	}
	i, err := f.db.BridgeCreationThread(ctx, f.r.HostID, f.r.AppID, f.n.thread)
	if err != nil || i.State != "confirmed" || i.WorktreePath != f.root || i.ExecutionDirectory() != f.cwd {
		t.Fatal("restoration changed the frozen creation workflow", err)
	}
}

func TestWorkspaceRestoreAfterAttachUsesSameActorHook(t *testing.T) {
	f := newWorkspaceRuntimeFixture(t, true, false)
	f.run(t)
	runtimeWait(t, func() bool {
		_, err := f.db.ThreadWorktreeIdentity(context.Background(), f.scope())
		return err == nil && f.n.attaches.Load() == 1 && f.n.queueReads.Load() > 0
	})
	f.remove(t)
	receipt := f.receive(t, "input-after-cleanup", false)
	f.r.Wake(f.n.thread)
	f.assertRestored(t, receipt)
	if f.n.attaches.Load() != 1 {
		t.Fatal("already attached actor resumed a replacement connection")
	}
}

func TestWorkspaceRestoreCleanupAfterActorCheckBeforeSubmissionDispatch(t *testing.T) {
	f := newWorkspaceRuntimeFixture(t, true, false)
	f.n.queueEntered = make(chan struct{})
	release := make(chan struct{})
	f.n.queueRelease = release
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	f.run(t)
	select {
	case <-f.n.queueEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("actor did not reach its queue snapshot after workspace authorization")
	}
	// The actor already ran PrepareWorkspace, attached, and authorized this
	// checkout. Cleanup and new admission now happen before its native claim.
	f.remove(t)
	receipt := f.receive(t, "input-during-cleanup-window", false)
	unblock()
	f.r.Wake(f.n.thread)
	f.assertRestored(t, receipt)
	if f.n.attaches.Load() != 1 {
		t.Fatal("dispatch preflight attached a replacement thread")
	}
}

func TestWorkspaceRestoreHistoryWakeWithoutInputDoesNotRestore(t *testing.T) {
	f := newWorkspaceRuntimeFixture(t, false, false)
	f.run(t)
	runtimeWait(t, func() bool {
		_, err := f.db.ThreadWorktreeIdentity(context.Background(), f.scope())
		return err == nil && f.n.attaches.Load() == 1
	})
	f.remove(t)
	for count := 0; count < 2; count++ {
		before := f.prepares.Load()
		f.r.Wake(f.n.thread)
		runtimeWait(t, func() bool { return f.prepares.Load() > before })
	}
	if _, err := os.Lstat(f.root); !os.IsNotExist(err) {
		t.Fatal("history wake recreated a checkout without ordinary input", err)
	}
	attempts, err := f.db.WorktreeRestoreAttempts(context.Background(), f.scope())
	if err != nil || len(attempts) != 0 || f.n.adds.Load() != 0 {
		t.Fatal("history polling acquired restore or input permission", err)
	}
}

func TestWorkspaceRestoreMissingSnapshotKeepsOriginalReceiptAndFailureCard(t *testing.T) {
	f := newWorkspaceRuntimeFixture(t, true, false)
	f.remove(t)
	creationRuntimeGit(t, f.source, "update-ref", "-d", creation.WorktreeSnapshotRef(f.root))
	receipt := f.receive(t, "snapshot-missing-input", false)
	f.run(t)
	runtimeWait(t, func() bool {
		m, err := f.db.Mirror(context.Background(), f.r.HostID, f.n.thread, store.QueueCardKey(receipt.ID))
		return err == nil && m.ParentMessageID == receipt.MessageID && strings.Contains(m.Card, "未找到工作树快照")
	})
	n, err := f.db.Submission(context.Background(), f.scope(), receipt.ID)
	if err != nil || n.State != "received" || n.ClientID != receipt.ClientID || n.MessageID != receipt.MessageID || f.n.adds.Load() != 0 || f.n.attaches.Load() != 0 {
		t.Fatal("failed restore discarded, replaced or submitted original input", err)
	}
	if _, err := os.Lstat(f.root); !os.IsNotExist(err) {
		t.Fatal("missing snapshot fell back to source HEAD", err)
	}
}

func TestWorkspaceRestoreRestartReconcilesDispatchWithoutAnotherCheckoutOrAdd(t *testing.T) {
	f := newWorkspaceRuntimeFixture(t, true, false)
	f.remove(t)
	receipt := f.receive(t, "restart-original-input", false)
	if _, err := f.db.DB.Exec(`CREATE TRIGGER fail_restore_confirm BEFORE UPDATE OF state ON worktree_restore_attempts WHEN NEW.state='confirmed' BEGIN SELECT RAISE(ABORT,'synthetic final journal failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.s.prepareWorkspace(context.Background(), f.n, f.n.thread); err == nil {
		t.Fatal("fixture did not interrupt after physical restoration")
	}
	a, err := f.db.WorktreeRestoreAttempt(context.Background(), f.scope(), receipt.ID)
	if err != nil || a.State != "dispatching" || a.Phase != "restored" || f.n.adds.Load() != 0 {
		t.Fatal("lost final journal did not retain exact dispatch checkpoint", a.State, a.Phase, err)
	}
	rootInfo, err := os.Stat(f.root)
	if err != nil {
		t.Fatal("physical restoration did not complete before simulated restart", err)
	}
	ownerPath := creationRuntimeGit(t, f.root, "rev-parse", "--path-format=absolute", "--git-path", "codex-thread.json")
	ownerInfo, _ := os.Stat(ownerPath)
	if _, err := f.db.DB.Exec(`DROP TRIGGER fail_restore_confirm`); err != nil {
		t.Fatal(err)
	}
	var sequence int
	var name, database string
	if err := f.db.DB.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &database); err != nil {
		t.Fatal(err)
	}
	f.db.Close()
	f.db, err = store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	f.s = &Service{Store: f.db, Config: f.s.Config}
	f.wire()
	f.run(t)
	confirmed := f.assertRestored(t, receipt)
	currentRoot, _ := os.Stat(f.root)
	currentOwner, _ := os.Stat(ownerPath)
	if confirmed.ID != a.ID || !os.SameFile(rootInfo, currentRoot) || !os.SameFile(ownerInfo, currentOwner) || !ownerInfo.ModTime().Equal(currentOwner.ModTime()) {
		t.Fatal("restart reexecuted physical restore instead of readback")
	}
	attempts, err := f.db.WorktreeRestoreAttempts(context.Background(), f.scope())
	if err != nil || len(attempts) != 1 {
		t.Fatal("restart created a replacement restore attempt", err)
	}
}

func TestWorkspaceRestoreCanceledReceiptReadbackDoesNotBlockLaterInput(t *testing.T) {
	testWorkspaceCanceledRestoreReadback(t, false)
}

func TestWorkspaceRestoreNamedPublicationAfterCanceledReceiptAndLaterNativeInput(t *testing.T) {
	testWorkspaceCanceledRestoreReadback(t, true)
}

func testWorkspaceCanceledRestoreReadback(t *testing.T, named bool) {
	t.Helper()
	f := newWorkspaceRuntimeFixture(t, true, false, named)
	ctx := context.Background()
	f.remove(t)
	canceled := f.receive(t, "canceled-restored-input", false)
	if _, err := f.db.DB.Exec(`CREATE TRIGGER fail_canceled_restore_confirm BEFORE UPDATE OF state ON worktree_restore_attempts WHEN NEW.state='confirmed' BEGIN SELECT RAISE(ABORT,'synthetic final journal failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.s.prepareWorkspace(ctx, f.n, f.n.thread); err == nil {
		t.Fatal("fixture did not lose confirmation after physical restoration")
	}
	original, err := f.db.WorktreeRestoreAttempt(ctx, f.scope(), canceled.ID)
	if err != nil || original.State != "dispatching" || original.Phase != "restored" {
		t.Fatal("physical completion checkpoint missing", err)
	}
	rootInfo, err := os.Stat(f.root)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(filepath.Dir(f.root), ".codex-worktree-name")
	if named {
		var checkpoint creation.WorktreeRestoreResult
		if err := json.Unmarshal([]byte(original.ResultJSON), &checkpoint); err != nil || !checkpoint.PublicationPending {
			t.Fatal("named checkout was not awaiting protected publication", err)
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("named checkout published before input became native-visible", err)
		}
	}
	if _, err := f.db.DB.Exec(`DROP TRIGGER fail_canceled_restore_confirm`); err != nil {
		t.Fatal(err)
	}
	// Exercise the actual cancel_pending callback boundary, including its
	// delivered card ID, operator, token and current submission revision.
	f.cancelInput(t, canceled)
	var sequence int
	var name, database string
	if err := f.db.DB.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &database); err != nil {
		t.Fatal(err)
	}
	f.db.Close()
	f.db, err = store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	f.s = &Service{Store: f.db, Config: f.s.Config}
	f.wire()
	later := f.receive(t, "fresh-input-after-cancel", false)
	if named {
		if err := f.s.prepareWorkspace(ctx, f.n, f.n.thread); err != nil {
			t.Fatal("completed canceled restore did not reconcile", err)
		}
		if a, err := f.db.WorktreeRestoreAttempt(ctx, f.scope(), canceled.ID); err != nil || a.State != "confirmed" {
			t.Fatal("publication fixture has no confirmed physical restore", err)
		}
		if err := f.s.publishRestoredWorkspace(ctx, f.n, f.n.thread, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("later receipt alone published without native input evidence", err)
		}
	}
	f.run(t)
	runtimeWait(t, func() bool {
		n, err := f.db.Submission(ctx, f.scope(), later.ID)
		return err == nil && n.State == "consumed" && f.n.adds.Load() == 1
	})
	confirmed, err := f.db.WorktreeRestoreAttempt(ctx, f.scope(), canceled.ID)
	if err != nil || confirmed.ID != original.ID || confirmed.State != "confirmed" {
		t.Fatal("canceled receipt stranded a completed physical restore", err)
	}
	input, err := f.db.Submission(ctx, f.scope(), canceled.ID)
	if err != nil || input.State != "removed" {
		t.Fatal("readback resurrected canceled input", err)
	}
	f.n.clientsMu.Lock()
	if len(f.n.clients) != 1 || f.n.clients[0] != later.ClientID || f.n.clients[0] == canceled.ClientID {
		t.Error("canceled input reached native queue")
	}
	f.n.clientsMu.Unlock()
	current, _ := os.Stat(f.root)
	if !os.SameFile(rootInfo, current) {
		t.Fatal("readback recreated the already restored checkout")
	}
	attempts, err := f.db.WorktreeRestoreAttempts(ctx, f.scope())
	if err != nil || len(attempts) != 1 {
		t.Fatal("later input bypassed readback with a replacement restore", err)
	}
	if named {
		runtimeWait(t, func() bool {
			a, err := f.db.WorktreeRestoreAttempt(ctx, f.scope(), canceled.ID)
			var checkpoint creation.WorktreeRestoreResult
			return err == nil && json.Unmarshal([]byte(a.ResultJSON), &checkpoint) == nil && !checkpoint.PublicationPending
		})
		if _, err := os.Stat(marker); err != nil {
			t.Fatal("native-visible later input did not publish named checkout", err)
		}
	}
}

func TestWorkspaceRestoreCanceledPartialStagePreservesProofAndReleasesLaterInput(t *testing.T) {
	f := newWorkspaceRuntimeFixture(t, true, false)
	ctx := context.Background()
	f.remove(t)
	canceled := f.receive(t, "canceled-partial-input", false)
	if _, err := f.db.DB.Exec(`CREATE TRIGGER stop_restore_after_stage BEFORE UPDATE OF phase ON worktree_restore_attempts WHEN NEW.phase='owner-dispatching' BEGIN SELECT RAISE(ABORT,'synthetic pause before owner effect'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.s.prepareWorkspace(ctx, f.n, f.n.thread); err == nil {
		t.Fatal("fixture did not stop after completed physical stage")
	}
	a, err := f.db.WorktreeRestoreAttempt(ctx, f.scope(), canceled.ID)
	if err != nil || a.State != "unknown" || a.Phase != "stage-ready" {
		t.Fatal("completed stage checkpoint missing", err)
	}
	var plan creation.WorktreeRestorePlan
	if err := json.Unmarshal([]byte(a.ParamsJSON), &plan); err != nil {
		t.Fatal(err)
	}
	stage, err := os.Stat(plan.StageRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.DB.Exec(`DROP TRIGGER stop_restore_after_stage`); err != nil {
		t.Fatal(err)
	}
	f.cancelInput(t, canceled)
	if err := f.s.prepareWorkspace(ctx, f.n, f.n.thread); err != nil {
		t.Fatal("verified canceled stage could not be abandoned", err)
	}
	blocked, err := f.db.WorktreeRestoreAttempt(ctx, f.scope(), canceled.ID)
	var proof creation.WorktreeRestoreCheckpoint
	if err != nil || blocked.ID != a.ID || blocked.State != "blocked" || blocked.ErrorCode != "input-canceled" || blocked.ParamsJSON != a.ParamsJSON || json.Unmarshal([]byte(blocked.ResultJSON), &proof) != nil || proof.Phase != "stage-ready" || proof.StageIdentity == "" {
		t.Fatal("canceled stage lost its frozen physical evidence", err)
	}
	current, err := os.Stat(plan.StageRoot)
	if err != nil || !os.SameFile(stage, current) {
		t.Fatal("abandon changed or removed the partial stage", err)
	}
	if _, err := os.Stat(f.root); !errors.Is(err, os.ErrNotExist) || f.n.adds.Load() != 0 {
		t.Fatal("canceled input caused further restoration or native submission", err)
	}
	later := f.receive(t, "later-after-canceled-stage", false)
	grant, err := f.db.NextWorktreeRestoreGrant(ctx, f.scope())
	if err != nil || grant.ReceiptID != later.ID {
		t.Fatal("safely abandoned stage blocked later ordinary permission", err)
	}
}
