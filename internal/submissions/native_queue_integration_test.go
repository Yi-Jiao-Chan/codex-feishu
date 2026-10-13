package submissions

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

// None of these cases uses fakeNative. The wrapper forwards each operation to
// a real 0.162.0 client and only injects deterministic scheduling or discards an
// already-successful reply at the Controller boundary. This is not a claim of
// a network transport outage or App/Feishu UI verification.
func TestNativeControllerQ4OtherClientDeletesBeforeBridgeDelete(t *testing.T) {
	h := newNativeQueueHarness(t, true)
	r := h.claim(t)
	h.tick(t)
	op := h.steerAction(t, r.ID)
	h.boundary.beforeDelete = func(ctx context.Context, queue string) error {
		deleted, err := h.other.Delete(ctx, h.thread, queue)
		if err == nil && !deleted {
			return errors.New("other native client did not remove owned B")
		}
		return err
	}
	h.tick(t)
	got, err := h.db.SubmissionAction(h.ctx, h.controller.scope(), op.ID)
	if err != nil || got.State != "rejected" || got.Result != "changed" || h.boundary.adds != 1 || h.boundary.deletes != 1 || h.boundary.steers != 0 || len(h.history(t, r.ClientID)) != 0 {
		t.Fatal("native deleted=false must not inject or restore B")
	}
	q, err := h.client.Queue(h.ctx, h.thread)
	if err != nil || len(q) != 0 {
		t.Fatal("other-client deletion did not leave an empty owned queue")
	}
	attempts, err := h.db.SubmissionAttempts(h.ctx, h.controller.scope())
	if err != nil {
		t.Fatal("attempt read failed")
	}
	for _, a := range attempts {
		if a.Stage == "delete" && (a.State != "confirmed" || a.Deleted != 0) {
			t.Fatal("actual false delete was not journaled")
		}
	}
	t.Log("daemon=0.162.0 Q4 real second-client delete; bridge delete=false; add=1 delete=1 steer=0 restore=0; owned fixture only")
}

func TestNativeControllerQ5OldTurnEndsBeforeSteerAndNeverRetargets(t *testing.T) {
	h := newNativeQueueHarness(t, true)
	r := h.claim(t)
	h.tick(t)
	op := h.steerAction(t, r.ID)
	var replacement string
	h.boundary.afterDelete = func(ctx context.Context, _ string) error {
		h.model.release(1)
		if err := nativeQueueEventually(ctx, func() (bool, error) {
			turns, err := h.other.Turns(ctx, h.thread)
			for _, turn := range turns {
				if turn.ID == h.active {
					return turn.Status == "completed", err
				}
			}
			return false, err
		}); err != nil {
			return err
		}
		if _, err := h.other.Add(ctx, h.thread, "owned second-client replacement turn", "owned-replacement-"+h.spec.RunID); err != nil {
			return err
		}
		if err := h.model.wait(ctx, 2); err != nil {
			return err
		}
		replacement = h.waitActive(t)
		if replacement == h.active {
			return errors.New("replacement did not become a different actual turn")
		}
		return nil
	}
	h.tick(t)
	got, err := h.db.SubmissionAction(h.ctx, h.controller.scope(), op.ID)
	if err != nil || got.State != "confirmed" || got.Result != "restored" || got.ExpectedTurnID != h.active || replacement == "" || h.boundary.adds != 2 || h.boundary.deletes != 1 || h.boundary.steers != 1 || len(h.boundary.steerTargets) != 1 || h.boundary.steerTargets[0] != h.active {
		t.Fatal("ended target must reject original steer then restore, never retarget")
	}
	if h.view(t, r.ID).State != "queued" || len(h.history(t, r.ClientID)) != 0 {
		t.Fatal("restored B must be pending behind replacement turn")
	}
	attempts, err := h.db.SubmissionAttempts(h.ctx, h.controller.scope())
	if err != nil {
		t.Fatal("attempt read failed")
	}
	steerRejected, restoreConfirmed := 0, 0
	for _, a := range attempts {
		if a.Stage == "steer" && a.State == "rejected" && a.ExpectedTurnID == h.active {
			steerRejected++
		}
		if a.Stage == "restore" && a.State == "confirmed" {
			restoreConfirmed++
		}
	}
	if steerRejected != 1 || restoreConfirmed != 1 {
		t.Fatal("real rejection and restore not journaled exactly once")
	}
	h.model.release(2)
	if err := h.model.wait(h.ctx, 3); err != nil {
		t.Fatal("restored B did not execute after replacement")
	}
	h.model.release(3)
	if err := nativeQueueEventually(h.ctx, func() (bool, error) {
		turns, err := h.client.Turns(h.ctx, h.thread)
		count := 0
		for _, turn := range turns {
			if turn.Status == "completed" {
				count++
			}
		}
		return count == 3, err
	}); err != nil {
		t.Fatal("owned replacement/restore sequence did not finish")
	}
	h.tick(t)
	matches := h.history(t, r.ClientID)
	if len(matches) != 1 || matches[0] == h.active || matches[0] == replacement || h.view(t, r.ID).State != "consumed" {
		t.Fatal("restored input must execute once in its subsequent turn")
	}
	t.Log("daemon=0.162.0 Q5 real old-turn completion and second-client replacement; original-target steer rejected once; restore=1; subsequent history=1")
}

func TestNativeControllerQ6AcceptedSteerReplyDiscardedAtBoundary(t *testing.T) {
	h := newNativeQueueHarness(t, true)
	r := h.claim(t)
	h.tick(t)
	op := h.steerAction(t, r.ID)
	h.boundary.discardSteer = true
	if err := h.controller.Tick(h.ctx); err == nil || h.boundary.steers != 1 {
		t.Fatal("accepted steering reply was not discarded")
	}
	h.model.release(1)
	if err := nativeQueueEventually(h.ctx, func() (bool, error) { return len(h.history(t, r.ClientID)) == 1, nil }); err != nil {
		t.Fatal("accepted steer history did not become observable")
	}
	h.reopen(t)
	h.tick(t)
	h.tick(t)
	got, err := h.db.SubmissionAction(h.ctx, h.controller.scope(), op.ID)
	matches := h.history(t, r.ClientID)
	if err != nil || got.State != "confirmed" || got.Result != "steered" || len(matches) != 1 || matches[0] != h.active || h.boundary.steers != 1 || h.boundary.adds != 1 || h.boundary.deletes != 1 {
		t.Fatal("steer recovery must prove the original turn without retransmission")
	}
	t.Log("daemon=0.162.0 Q6 Controller-boundary accepted-reply loss; SQLite reopen; original-turn history=1; steer=1; not a network-outage claim")
}

func TestNativeControllerQ7AcceptedAddReplyDiscardedAtBoundary(t *testing.T) {
	requireNativeQueueOptIn(t)
	for _, busy := range []bool{true, false} {
		name := "idle"
		if busy {
			name = "busy"
		}
		t.Run(name, func(t *testing.T) {
			h := newNativeQueueHarness(t, busy)
			r := h.claim(t)
			h.boundary.discardAdd = true
			if err := h.controller.Tick(h.ctx); err == nil || h.boundary.adds != 1 {
				t.Fatal("accepted add reply was not discarded")
			}
			if !busy {
				if err := h.model.wait(h.ctx, 1); err != nil {
					t.Fatal("idle accepted add did not reach the owned model")
				}
				if err := nativeQueueEventually(h.ctx, func() (bool, error) { return len(h.history(t, r.ClientID)) == 1, nil }); err != nil {
					t.Fatal("idle add history unavailable")
				}
			}
			h.reopen(t)
			h.tick(t)
			h.tick(t)
			n := h.view(t, r.ID)
			want := "consumed"
			if busy {
				want = "queued"
			}
			if n.State != want || h.boundary.adds != 1 {
				t.Fatal("accepted add was duplicated or not recovered")
			}
			queue, err := h.client.Queue(h.ctx, h.thread)
			if err != nil {
				t.Fatal("owned queue unavailable")
			}
			queueMatches := 0
			for _, q := range queue {
				if q.ClientUserMessageID != nil && *q.ClientUserMessageID == r.ClientID {
					queueMatches++
				}
			}
			if busy && (queueMatches != 1 || len(h.history(t, r.ClientID)) != 0) || !busy && (queueMatches != 0 || len(h.history(t, r.ClientID)) != 1) {
				t.Fatal("queue/history did not contain exactly one accepted input")
			}
			attempts, err := h.db.SubmissionAttempts(h.ctx, h.controller.scope())
			if err != nil || len(attempts) != 1 || attempts[0].Stage != "add" || attempts[0].State != "confirmed" {
				t.Fatal("add journal did not reconcile a single native attempt")
			}
			t.Log("daemon=0.162.0 Q7 " + name + " Controller-boundary accepted-reply loss; SQLite reopen; add=1; exact queue/history association; not a network outage")
		})
	}
}

func TestNativeControllerQ8AcceptedDeleteReplyDiscardedAtBoundary(t *testing.T) {
	h := newNativeQueueHarness(t, true)
	r := h.claim(t)
	h.tick(t)
	h.steerAction(t, r.ID)
	h.boundary.discardDelete = true
	if err := h.controller.Tick(h.ctx); err == nil || h.boundary.deletes != 1 {
		t.Fatal("accepted delete reply was not discarded")
	}
	h.reopen(t)
	assertNativeQueueDeleteFrozen(t, h, r)
	if h.boundary.adds != 1 || h.boundary.deletes != 1 || h.boundary.steers != 0 {
		t.Fatal("unknown delete triggered a repeated native mutation")
	}
	t.Log("daemon=0.162.0 Q8 Controller-boundary accepted-delete reply loss; SQLite reopen; unknown remains frozen; delete=1 steer=0 restore=0")
}

func assertNativeQueueDeleteFrozen(t *testing.T, h *nativeQueueHarness, r store.Receipt) {
	t.Helper()
	for range 3 {
		h.tick(t)
	}
	if h.view(t, r.ID).State != "unknown" || len(h.history(t, r.ClientID)) != 0 {
		t.Fatal("missing queue alone must not prove deletion/steering")
	}
	queue, err := h.client.Queue(h.ctx, h.thread)
	if err != nil || len(queue) != 0 {
		t.Fatal("owned delete did not leave an empty native queue")
	}
	attempts, err := h.db.SubmissionAttempts(h.ctx, h.controller.scope())
	if err != nil || len(attempts) != 2 {
		t.Fatal("recovery created an extra native attempt")
	}
	deletes := 0
	for _, a := range attempts {
		if a.Stage == "delete" && a.State == "unknown" {
			deletes++
		}
		if a.Stage == "steer" || a.Stage == "restore" {
			t.Fatal("unknown delete advanced a native write stage")
		}
	}
	if deletes != 1 {
		t.Fatal("crash/response loss did not freeze exactly one delete attempt")
	}
}

type nativeQueueCrashConfig struct {
	Ownership nativeQueueOwnership `json:"ownership"`
	Database  string               `json:"database"`
	Receipt   string               `json:"receipt"`
}

func TestNativeControllerQ8ProcessExitAfterDeleteBeforePersistence(t *testing.T) {
	h := newNativeQueueHarness(t, true)
	r := h.claim(t)
	h.tick(t)
	h.steerAction(t, r.ID)
	config := nativeQueueCrashConfig{Ownership: nativeQueueOwnership{Spec: h.spec, ThreadID: h.thread, Version: "0.162.0"}, Database: h.path, Receipt: r.ID}
	configPath := filepath.Join(h.root, "child-crash.json")
	if err := writeNativePrivateJSON(configPath, config); err != nil {
		t.Fatal("cannot write private child scope")
	}
	if err := h.db.Close(); err != nil {
		t.Fatal("parent journal close failed")
	}
	h.db = nil
	command := exec.CommandContext(h.ctx, os.Args[0], "-test.run=^TestNativeQueueChildCrash$", "-test.timeout=30s")
	command.Env = append(os.Environ(), nativeControllerOptIn+"=1", "CODEX_FEISHU_NATIVE_CHILD_SCOPE="+configPath, "CODEX_FEISHU_NATIVE_CHILD_RUN="+h.spec.RunID)
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != nativeCrashExit {
		safe := strings.NewReplacer(h.thread, "[owned-thread]", h.spec.RunID, "[owned-run]", h.root, "[private-scratch]", h.model.server.URL, "[owned-loopback]").Replace(string(output))
		t.Fatalf("child did not exit at the accepted-delete persistence boundary: %s", safe)
	}
	body, err := os.ReadFile(filepath.Join(h.root, "delete-observed.json"))
	var observed nativeQueueOwnership
	if err != nil || json.Unmarshal(body, &observed) != nil || observed != config.Ownership {
		t.Fatal("real child delete ownership proof missing")
	}
	db, err := store.Open(h.path)
	if err != nil {
		t.Fatal("crash journal reopen failed")
	}
	h.db = db
	h.controller = &Controller{Native: h.boundary, Store: db, HostID: "native-fixture", AppID: "native-fixture", ThreadID: h.thread}
	attempts, err := db.SubmissionAttempts(h.ctx, h.controller.scope())
	if err != nil || len(attempts) != 2 || attempts[1].State != "dispatching" {
		t.Fatal("child exit did not preserve an unfinished durable delete")
	}
	assertNativeQueueDeleteFrozen(t, h, r)
	if h.boundary.adds != 1 || h.boundary.deletes != 0 || h.boundary.steers != 0 {
		t.Fatal("post-crash parent retransmitted a native mutation")
	}
	t.Log("daemon=0.162.0 Q8 real OS child exit=86 after successful native delete and before local confirmation; reopen converts dispatching to unknown; no steer/restore/re-delete")
}

// Re-exec entrypoint: it cannot create a thread/model or choose a thread not
// recorded by its parent's private typed-birth ownership marker.
func TestNativeQueueChildCrash(t *testing.T) {
	path := os.Getenv("CODEX_FEISHU_NATIVE_CHILD_SCOPE")
	if path == "" {
		t.Skip("owned child process only")
	}
	requireNativeQueueOptIn(t)
	root := filepath.Dir(path)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatal("child scope is not a private regular file")
	}
	body, err := os.ReadFile(path)
	var config nativeQueueCrashConfig
	if err != nil || json.Unmarshal(body, &config) != nil || config.Ownership.Spec.RunID != os.Getenv("CODEX_FEISHU_NATIVE_CHILD_RUN") || config.Database != filepath.Join(root, "controller.db") || config.Ownership.Spec.CWD != filepath.Join(root, "cwd") || config.Ownership.Version != "0.162.0" {
		t.Fatal("child scope does not match its parent-owned scratch fixture")
	}
	body, err = os.ReadFile(filepath.Join(root, "owned-thread.json"))
	var marker nativeQueueOwnership
	if err != nil || json.Unmarshal(body, &marker) != nil || marker != config.Ownership {
		t.Fatal("child target lacks typed-birth ownership marker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	daemon, err := codexclient.Discover(ctx, "auto")
	if err != nil || daemon.AppServerVersion != "0.162.0" {
		t.Fatal("child requires the existing verified daemon")
	}
	client, err := codexclient.Connect(ctx, daemon.SocketPath)
	if err != nil {
		t.Fatal("child native connection failed")
	}
	defer client.Close()
	if _, err := client.AttachLocalVerification(ctx, marker.ThreadID, marker.Spec); err != nil {
		t.Fatal("child could not verify original fixture policy")
	}
	db, err := store.Open(config.Database)
	if err != nil {
		t.Fatal("child journal open failed")
	}
	defer db.Close()
	native := &nativeQueueBoundary{client: client, owned: marker.ThreadID}
	native.afterDelete = func(context.Context, string) error {
		if err := writeNativePrivateJSON(filepath.Join(root, "delete-observed.json"), marker); err != nil {
			return errors.New("child delete marker persistence failed")
		}
		os.Exit(nativeCrashExit)
		return nil
	}
	controller := &Controller{Native: native, Store: db, HostID: "native-fixture", AppID: "native-fixture", ThreadID: marker.ThreadID}
	rows, err := db.Submissions(ctx, controller.scope())
	if err != nil || len(rows) != 1 || rows[0].ID != config.Receipt || rows[0].State != "steering" {
		t.Fatal("child journal is not the exact parent receipt")
	}
	actions, err := db.SubmissionActions(ctx, controller.scope())
	if err != nil || len(actions) != 1 || actions[0].Kind != "steer" || actions[0].ReceiptID != config.Receipt || actions[0].State != "ready" || actions[0].QueueID != rows[0].QueueID {
		t.Fatal("child journal is not the exact parent steering action")
	}
	if err := controller.Tick(ctx); err != nil {
		t.Fatal("child failed before accepted native deletion")
	}
	t.Fatal("child did not reach the owned successful-delete exit boundary")
}
