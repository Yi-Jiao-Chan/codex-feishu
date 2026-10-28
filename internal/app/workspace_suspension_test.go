package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

type suspendedWorkspaceNative struct {
	*runtimeNative
	attaches, metadata atomic.Int32
	resumeEvents       bool
}

func (n *suspendedWorkspaceNative) Attach(ctx context.Context, id string) (codexclient.Thread, error) {
	n.attaches.Add(1)
	meta, err := n.runtimeNative.Attach(ctx, id)
	if err == nil && n.resumeEvents {
		params, _ := json.Marshal(map[string]any{"thread": meta})
		n.events <- codexclient.Event{Method: "thread/started", Params: params}
	}
	return meta, err
}

func (n *suspendedWorkspaceNative) Metadata(ctx context.Context, id string) (codexclient.Thread, error) {
	n.metadata.Add(1)
	return n.runtimeNative.Metadata(ctx, id)
}

func workspaceSuspended(r *Runtime, thread string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, held := r.suspended[thread]
	return held && r.actors[thread] == nil && r.retries[thread].due.IsZero()
}

func waitWorkspaceSuspended(t *testing.T, r *Runtime, thread string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if workspaceSuspended(r, thread) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.mu.Lock()
	hold, held := r.suspended[thread]
	retry := r.retries[thread]
	active := r.actors[thread] != nil
	r.mu.Unlock()
	meta, _ := r.Native.Metadata(context.Background(), thread)
	_, statErr := os.Stat(meta.CWD)
	t.Fatalf("workspace was not suspended: active=%v held=%v retry=%v inputSequence=%d cwdMissing=%v authorization=%v", active, held, !retry.due.IsZero(), hold.inputSequence, os.IsNotExist(statErr), r.Authorize(context.Background(), meta, false))
}

func TestRuntimeMissingBoundWorkspaceSkipsPeriodicChecksAndNativeEventResumes(t *testing.T) {
	db, base, _, r := runtimeFixture(t)
	native := &suspendedWorkspaceNative{runtimeNative: base}
	r.Native, r.DiscoveryEnabled = native, false
	r.ActivationSweepInterval = 10 * time.Millisecond
	r.ScanInterval = 10 * time.Millisecond
	missing := filepath.Join(r.AllowedRoots[0], "restored-by-desktop")
	base.set("thread", missing, "completed")
	runtimeBind(t, db, r, "thread", "chat")
	runAdaptiveRuntime(t, r, base)
	runtimeWait(t, func() bool { return workspaceSuspended(r, "thread") })
	beforeAttach, beforeMetadata := native.attaches.Load(), native.metadata.Load()
	time.Sleep(80 * time.Millisecond)
	if native.attaches.Load() != beforeAttach || native.metadata.Load() != beforeMetadata {
		t.Fatal("missing bound cwd kept periodic attach/metadata retries")
	}
	if err := os.Mkdir(missing, 0700); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if !workspaceSuspended(r, "thread") || native.attaches.Load() != beforeAttach {
		t.Fatal("bound suspension polled for a restored directory")
	}
	runtimeNameEvent(base, "thread")
	runtimeWait(t, func() bool {
		_, err := db.Watermark(context.Background(), r.HostID, "thread")
		return err == nil && native.attaches.Load() > beforeAttach
	})
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	r.Wake("thread")
	runtimeWait(t, func() bool { return workspaceSuspended(r, "thread") })
	// Explicit activation while still missing must also return to suspension.
	metadataBefore := native.metadata.Load()
	if !r.Activate("thread") {
		t.Fatal("explicit route activation was lost")
	}
	runtimeWait(t, func() bool { return native.metadata.Load() > metadataBefore && workspaceSuspended(r, "thread") })
	if err := os.Mkdir(missing, 0700); err != nil {
		t.Fatal(err)
	}
	beforeAttach = native.attaches.Load()
	base.mu.Lock()
	meta := base.threads["thread"]
	meta.UpdatedAt++ // Still older than the history watermark; newer than the hold.
	base.threads["thread"] = meta
	base.mu.Unlock()
	runtimeWait(t, func() bool { return native.attaches.Load() > beforeAttach })
}

func TestRuntimeMissingWorkspaceCannotLoopOnItsOwnResumeEvents(t *testing.T) {
	db, base, _, r := runtimeFixture(t)
	native := &suspendedWorkspaceNative{runtimeNative: base, resumeEvents: true}
	r.Native = native
	r.ActivationSweepInterval = 10 * time.Millisecond
	base.set("thread", filepath.Join(r.AllowedRoots[0], "missing"), "completed")
	runtimeBind(t, db, r, "thread", "chat")
	runAdaptiveRuntime(t, r, base)
	waitWorkspaceSuspended(t, r, "thread")
	if native.attaches.Load() != 0 {
		t.Fatal("missing workspace resumed itself and generated another wakeup")
	}
}

func TestRuntimeSuspendedWorkspaceFeishuInputRestoresOriginalThread(t *testing.T) {
	for _, explicitWake := range []bool{true, false} {
		name := "message-wakeup"
		if !explicitWake {
			name = "durable-receipt-compensation"
		}
		t.Run(name, func(t *testing.T) {
			f := newWorkspaceRuntimeFixture(t, true, false)
			f.r.ActivationSweepInterval = 10 * time.Millisecond
			f.run(t)
			runtimeWait(t, func() bool {
				_, err := f.db.ThreadWorktreeIdentity(context.Background(), f.scope())
				return err == nil && f.n.attaches.Load() == 1
			})
			f.remove(t)
			f.r.Wake(f.n.thread)
			waitWorkspaceSuspended(t, f.r, f.n.thread)
			prepares := f.prepares.Load()
			time.Sleep(80 * time.Millisecond)
			if f.prepares.Load() != prepares {
				t.Fatal("suspended worktree kept preparing old pending work")
			}
			missingAttaches := f.n.missingAttaches.Load()
			receipt := f.receive(t, "wake-original-worktree", false)
			if explicitWake {
				f.r.Wake(f.n.thread)
			}
			runtimeWait(t, func() bool {
				input, err := f.db.Submission(context.Background(), f.scope(), receipt.ID)
				return err == nil && input.State == "consumed" && f.n.adds.Load() == 1
			})
			if f.n.missingAttaches.Load() != missingAttaches || creationRuntimeGit(t, f.root, "rev-parse", "HEAD") != f.snapshot {
				t.Fatal("new input attached before restoring the original snapshot")
			}
			f.n.clientsMu.Lock()
			defer f.n.clientsMu.Unlock()
			if len(f.n.clients) != 1 || f.n.clients[0] != receipt.ClientID || f.n.submittedThreads[0] != receipt.ThreadID {
				t.Fatal("suspended input changed its receipt, client or thread identity")
			}
		})
	}
}

func TestRuntimeTransientWorkspaceErrorRetainsBackoffRetry(t *testing.T) {
	db, native, _, r := runtimeFixture(t)
	r.DiscoveryEnabled = false
	r.ActivationRetryInitial = 20 * time.Millisecond
	r.ActivationRetryMax = 40 * time.Millisecond
	settledRuntimeBinding(t, db, native, r, time.Now().Add(-48*time.Hour).Unix())
	var prepares atomic.Int32
	r.PrepareWorkspace = func(context.Context, string) error {
		if prepares.Add(1) == 1 {
			return errors.New("synthetic transient workspace failure")
		}
		return nil
	}
	runAdaptiveRuntime(t, r, native)
	r.Wake("thread")
	runtimeWait(t, func() bool { return prepares.Load() >= 2 && r.ActorStarted("thread") })
	if workspaceSuspended(r, "thread") {
		t.Fatal("temporary workspace failure was treated as a missing directory")
	}
}

func TestRuntimeMissingCWDDoesNotSuspendJournaledRestoreRecovery(t *testing.T) {
	f := newWorkspaceRuntimeFixture(t, true, false)
	f.remove(t)
	receipt := f.receive(t, "recover-original-stage", false)
	f.r.ActivationRetryInitial = 50 * time.Millisecond
	f.r.ActivationRetryMax = 100 * time.Millisecond
	if _, err := f.db.DB.Exec(`CREATE TRIGGER stop_restore_after_stage BEFORE UPDATE OF phase ON worktree_restore_attempts WHEN NEW.phase='owner-dispatching' BEGIN SELECT RAISE(ABORT,'synthetic pause before owner effect'); END`); err != nil {
		t.Fatal(err)
	}
	f.run(t)
	runtimeWait(t, func() bool {
		a, err := f.db.WorktreeRestoreAttempt(context.Background(), f.scope(), receipt.ID)
		return err == nil && a.State == "unknown" && a.Phase == "stage-ready"
	})
	if _, err := os.Stat(f.cwd); !os.IsNotExist(err) {
		t.Fatal("fixture did not pause before restoring cwd", err)
	}
	if _, err := f.db.DB.Exec(`DROP TRIGGER stop_restore_after_stage`); err != nil {
		t.Fatal(err)
	}
	// No new Feishu/native activity: the original restore must reconcile itself.
	f.assertRestored(t, receipt)
}

func TestMissingCWDDoesNotSuspendUnknownNativeInput(t *testing.T) {
	db, native, _, r := runtimeFixture(t)
	ctx := context.Background()
	native.set("thread", filepath.Join(r.AllowedRoots[0], "missing"), "completed")
	runtimeBind(t, db, r, "thread", "chat")
	scope := store.SubmissionScope{HostID: r.HostID, AppID: r.AppID, ThreadID: "thread"}
	receipt, _, err := db.ReceiveSubmissionInput(ctx, scope, store.Receipt{AppID: r.AppID, ThreadID: "thread", ChatID: "chat", MessageID: "ordinary-input", Body: "synthetic input"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, claimed, err := db.ClaimSubmissionAttempt(ctx, scope, receipt.ID, "", "add")
	if err != nil || !claimed {
		t.Fatal("native input fixture was not claimed", err)
	}
	if err := db.FinishSubmissionAttempt(ctx, scope, attempt.ID, "unknown", "", -1); err != nil {
		t.Fatal(err)
	}
	meta, _ := native.Metadata(ctx, "thread")
	if r.suspendMissingWorkspace(ctx, &threadActor{thread: "thread"}, meta, 0) {
		t.Fatal("missing cwd suspended a native input awaiting readback")
	}
	input, err := db.Submission(ctx, scope, receipt.ID)
	if err != nil || input.State != "unknown" || input.ClientID != receipt.ClientID {
		t.Fatal("missing cwd changed the unresolved native input", err)
	}
}

func TestRuntimeActivityDuringCandidateSuspensionRechecksEligibility(t *testing.T) {
	db, native, gateway, r := runtimeFixture(t)
	r.ScanInterval = time.Hour
	cwd := filepath.Join(r.AllowedRoots[0], "candidate")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	entered, paused := make(chan struct{}), make(chan struct{}, 1)
	prepareGate, pauseGate := make(chan struct{}, 1), make(chan struct{}, 1)
	defer close(prepareGate)
	defer close(pauseGate)
	var prepares atomic.Int32
	r.PrepareWorkspace = func(ctx context.Context, _ string) error {
		if prepares.Add(1) == 1 {
			close(entered)
			select {
			case <-prepareGate:
			case <-ctx.Done():
				return ctx.Err()
			}
			return errors.New("synthetic failure after cwd removal")
		}
		return nil
	}
	r.Notice = func(code string) {
		if code == "thread.cwd_suspended" {
			paused <- struct{}{}
			<-pauseGate
		}
	}
	runAdaptiveRuntime(t, r, native)
	runtimeWait(t, func() bool { _, ok, err := db.BaselineFence(context.Background(), r.HostID); return err == nil && ok })
	native.set("candidate", cwd, "completed")
	meta, _ := native.Metadata(context.Background(), "candidate")
	params, _ := json.Marshal(map[string]any{"thread": meta})
	native.events <- codexclient.Event{Method: "thread/started", Params: params}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("candidate actor did not prepare")
	}
	if err := os.Remove(cwd); err != nil {
		t.Fatal(err)
	}
	runtimeNameEvent(native, "candidate")
	runtimeWait(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.actors["candidate"] != nil && r.actors["candidate"].activity != 0
	})
	prepareGate <- struct{}{}
	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		t.Fatal("missing candidate was not suspended")
	}
	o, err := db.Observation(context.Background(), r.HostID, "candidate")
	if err != nil || !o.Suspended() {
		t.Fatal("candidate lost its suspension evidence", err)
	}
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	pauseGate <- struct{}{}
	// The one activity event arrived during preparation; no later event/scan.
	runtimeWait(t, func() bool {
		_, err := db.Binding(context.Background(), r.HostID, "candidate")
		return err == nil
	})
	o, err = db.Observation(context.Background(), r.HostID, "candidate")
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	if err != nil || o.Suspended() || o.State != "eligible" || gateway.groups != 1 {
		t.Fatal("activity did not revalidate the same candidate exactly once", err)
	}
}
