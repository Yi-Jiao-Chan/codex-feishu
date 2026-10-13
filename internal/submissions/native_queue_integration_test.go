package submissions

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

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

// None of these cases uses fakeNative. The wrapper forwards each operation to
// an explicitly version-checked native client and only injects scheduling or discards an
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
	t.Log("daemon=" + h.version + " Q4 real second-client delete; bridge delete=false; add=1 delete=1 steer=0 restore=0; owned fixture only")
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
	t.Log("daemon=" + h.version + " Q5 real old-turn completion and second-client replacement; original-target steer rejected once; restore=1; subsequent history=1")
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
	t.Log("daemon=" + h.version + " Q6 Controller-boundary accepted-reply loss; SQLite reopen; original-turn history=1; steer=1; not a network-outage claim")
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
			t.Log("daemon=" + h.version + " Q7 " + name + " Controller-boundary accepted-reply loss; SQLite reopen; add=1; exact queue/history association; not a network outage")
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
	t.Log("daemon=" + h.version + " Q8 Controller-boundary accepted-delete reply loss; SQLite reopen; unknown remains frozen; delete=1 steer=0 restore=0")
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
	config := nativeQueueCrashConfig{Ownership: nativeQueueOwnership{Spec: h.spec, ThreadID: h.thread, Version: h.version, CLIVersion: h.version}, Database: h.path, Receipt: r.ID}
	configPath := filepath.Join(h.root, "child-crash.json")
	if err := writeNativePrivateJSON(configPath, config); err != nil {
		t.Fatal("cannot write private child scope")
	}
	if err := h.db.Close(); err != nil {
		t.Fatal("parent journal close failed")
	}
	h.db = nil
	command := exec.CommandContext(h.ctx, os.Args[0], "-test.run=^TestNativeQueueChildCrash$", "-test.timeout=30s")
	command.Env = append(os.Environ(), nativeControllerOptIn+"=1", nativeExpectedVersionEnv+"="+h.version, "CODEX_FEISHU_NATIVE_CHILD_SCOPE="+configPath, "CODEX_FEISHU_NATIVE_CHILD_RUN="+h.spec.RunID)
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
	t.Log("daemon=" + h.version + " Q8 real OS child exit=86 after successful native delete and before local confirmation; reopen converts dispatching to unknown; no steer/restore/re-delete")
}

// Re-exec entrypoint: it cannot create a thread/model or choose a thread not
// recorded by its parent's private typed-birth ownership marker.
func TestNativeQueueChildCrash(t *testing.T) {
	path := os.Getenv("CODEX_FEISHU_NATIVE_CHILD_SCOPE")
	if path == "" {
		t.Skip("owned child process only")
	}
	requireNativeQueueOptIn(t)
	expected := requireNativeExpectedVersion(t)
	root := filepath.Dir(path)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatal("child scope is not a private regular file")
	}
	body, err := os.ReadFile(path)
	var config nativeQueueCrashConfig
	if err != nil || json.Unmarshal(body, &config) != nil || config.Ownership.Spec.RunID != os.Getenv("CODEX_FEISHU_NATIVE_CHILD_RUN") || config.Database != filepath.Join(root, "controller.db") || config.Ownership.Spec.CWD != filepath.Join(root, "cwd") || config.Ownership.Version != expected || config.Ownership.CLIVersion != expected {
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
	if err != nil || !nativeDaemonVersionMatches(daemon, expected) {
		t.Fatal("child requires the existing verified daemon")
	}
	client, err := codexclient.Connect(ctx, daemon.SocketPath)
	if err != nil {
		t.Fatal("child native connection failed")
	}
	defer client.Close()
	if err := nativeOwnedBirthVersion(ctx, client, marker.ThreadID, expected); err != nil {
		t.Fatal(err)
	}
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

// These callbacks enter after authorization at the Controller boundary. They
// test durable arbitration and real native dispatch, not Feishu authentication.
func TestNativeControllerQ2Q3MixedQueueOrderAndMiddleSteer(t *testing.T) {
	requireNativeQueueOptIn(t)
	for _, steer := range []bool{false, true} {
		name := "Q2_normal"
		if steer {
			name = "Q3_middle_steer"
		}
		t.Run(name, func(t *testing.T) {
			h := newNativeQueueHarness(t, true)
			appID := "owned-app-A-" + h.spec.RunID
			if _, err := h.other.Add(h.ctx, h.thread, "owned native A", appID); err != nil {
				t.Fatal("owned App-origin A add failed")
			}
			b := h.claim(t)
			h.tick(t)
			c := h.claim(t)
			h.tick(t)
			nativeQueueOrder(t, h, []string{appID, b.ClientID, c.ClientID})
			if h.view(t, b.ID).QueueOrdinal != 2 || h.view(t, c.ID).QueueOrdinal != 3 {
				t.Fatal("bridge ordinals excluded App queue input")
			}
			expectedTurns := 4
			if steer {
				op := h.steerAction(t, b.ID)
				h.tick(t)
				got, err := h.db.SubmissionAction(h.ctx, h.controller.scope(), op.ID)
				if err != nil || got.State != "confirmed" || got.Result != "steered" || got.ExpectedTurnID != h.active || h.boundary.deletes != 1 || h.boundary.steers != 1 {
					t.Fatal("middle B was not guided once into original turn")
				}
				nativeQueueOrder(t, h, []string{appID, c.ClientID})
				expectedTurns = 3
			}
			nativeQueueDrain(t, h, expectedTurns)
			if steer {
				if m := h.history(t, b.ClientID); len(m) != 1 || m[0] != h.active {
					t.Fatal("B history missing from original target")
				}
			}
			h.tick(t)
			aTurn := nativeQueueHistoryPosition(t, h, appID)
			bTurn := nativeQueueHistoryPosition(t, h, b.ClientID)
			cTurn := nativeQueueHistoryPosition(t, h, c.ClientID)
			if aTurn >= cTurn || !steer && !(aTurn < bTurn && bTurn < cTurn) || steer && bTurn != 0 {
				t.Fatal("native history order changed or steered B executed again")
			}
			wantB := "consumed"
			if steer {
				wantB = "steered"
			}
			if h.view(t, b.ID).State != wantB || h.view(t, c.ID).State != "consumed" || h.boundary.adds != 2 {
				t.Fatal("bridge inputs failed exact-once consumption")
			}
			t.Log("daemon=" + h.version + " " + name + " mixed native/Controller A-B-C; exact native ordinals and history; untouched inputs preserve order")
		})
	}
}

func nativeQueueOrder(t *testing.T, h *nativeQueueHarness, clients []string) {
	t.Helper()
	q, err := h.client.Queue(h.ctx, h.thread)
	if err != nil || len(q) != len(clients) {
		t.Fatal("owned native queue length mismatch")
	}
	for i, id := range clients {
		if q[i].ClientUserMessageID == nil || *q[i].ClientUserMessageID != id {
			t.Fatal("owned native queue order mismatch")
		}
	}
}

func nativeQueueDrain(t *testing.T, h *nativeQueueHarness, completed int) {
	t.Helper()
	if err := nativeQueueEventually(h.ctx, func() (bool, error) {
		// Release only this model's fixture requests. Steering can require another
		// inference in the same original turn; request count is not a turn count.
		h.model.mu.Lock()
		for _, g := range h.model.gates {
			g.once.Do(func() { close(g.release) })
		}
		h.model.mu.Unlock()
		turns, err := h.client.Turns(h.ctx, h.thread)
		if err != nil {
			return false, err
		}
		done := 0
		for _, turn := range turns {
			if turn.Status == "completed" {
				done++
			}
		}
		q, err := h.client.Queue(h.ctx, h.thread)
		return done == completed && len(q) == 0, err
	}); err != nil {
		t.Fatal("owned native queue did not drain expected completed turns")
	}
}

func nativeQueueHistoryPosition(t *testing.T, h *nativeQueueHarness, client string) int {
	t.Helper()
	turns, err := h.client.Turns(h.ctx, h.thread)
	if err != nil {
		t.Fatal("owned native history unavailable")
	}
	position, count := -1, 0
	for i, turn := range turns {
		for _, item := range turn.Items {
			if item.Type == "userMessage" && item.ClientID != nil && *item.ClientID == client {
				count++
				position = i
			}
		}
	}
	if count != 1 {
		t.Fatal("owned input history association is not exactly once")
	}
	return position
}

func TestNativeControllerQ9ConcurrentAuthorizedCallbacksAndReplay(t *testing.T) {
	requireNativeQueueOptIn(t)
	for _, mixed := range []bool{false, true} {
		name := "same_button"
		if mixed {
			name = "steer_remove"
		}
		t.Run(name, func(t *testing.T) {
			h := newNativeQueueHarness(t, true)
			r := h.claim(t)
			h.tick(t)
			n := h.view(t, r.ID)
			type result struct {
				action store.SubmissionAction
				err    error
			}
			results := make(chan result, 12)
			gate := make(chan struct{})
			var wg sync.WaitGroup
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-gate
					callback := &Controller{Store: h.db, HostID: "native-fixture", AppID: "native-fixture", ThreadID: h.thread}
					kind := "steer"
					if mixed && i%2 != 0 {
						kind = "remove"
					}
					a, err := callback.ClaimAction(h.ctx, r.ID, kind, n.QueueID, n.Revision)
					results <- result{a, err}
				}(i)
			}
			close(gate)
			wg.Wait()
			close(results)
			var winner store.SubmissionAction
			for result := range results {
				if result.err != nil {
					if mixed && errors.Is(result.err, store.ErrSubmissionStale) {
						continue
					}
					t.Fatal("authorized callback arbitration failed")
				}
				if winner.ID != "" && winner.ID != result.action.ID {
					t.Fatal("callbacks created multiple durable winners")
				}
				winner = result.action
			}
			if winner.ID == "" {
				t.Fatal("callbacks had no winner")
			}
			h.tick(t)
			got, err := h.db.SubmissionAction(h.ctx, h.controller.scope(), winner.ID)
			wantSteer := 0
			wantResult := "removed"
			if winner.Kind == "steer" {
				wantSteer = 1
				wantResult = "steered"
			}
			if err != nil || got.State != "confirmed" || got.Result != wantResult || h.boundary.deletes != 1 || h.boundary.steers != wantSteer || h.boundary.adds != 1 {
				t.Fatal("durable winner did not dispatch exactly one native action pipeline")
			}
			for i := 0; i < 3; i++ {
				replay, err := h.controller.ClaimAction(h.ctx, r.ID, winner.Kind, n.QueueID, n.Revision)
				if err != nil || replay.ID != winner.ID || replay.Result != wantResult {
					t.Fatal("completed callback replay changed result")
				}
				other := "remove"
				if winner.Kind == "remove" {
					other = "steer"
				}
				if _, err := h.controller.ClaimAction(h.ctx, r.ID, other, n.QueueID, n.Revision); !errors.Is(err, store.ErrSubmissionStale) {
					t.Fatal("stale conflicting callback accepted")
				}
				h.tick(t)
			}
			actions, err := h.db.SubmissionActions(h.ctx, h.controller.scope())
			if err != nil || len(actions) != 1 || h.boundary.deletes != 1 || h.boundary.steers != wantSteer || h.boundary.adds != 1 {
				t.Fatal("callback replay repeated a native mutation")
			}
			nativeQueueOrder(t, h, nil)
			nativeQueueDrain(t, h, 1)
			if m := h.history(t, r.ClientID); len(m) != wantSteer || wantSteer == 1 && m[0] != h.active {
				t.Fatal("winner disagrees with native input history")
			}
			t.Logf("daemon=%s Q9 %s 12 simultaneous authorized Controller claims; one durable %s pipeline; delete=1 steer=%d; replays do not mutate", h.version, name, winner.Kind, wantSteer)
		})
	}
}

func TestNativeControllerQ10ContinueRacesSecondNativeStart(t *testing.T) {
	requireNativeQueueOptIn(t)
	for _, simultaneous := range []bool{false, true} {
		name := "native_wins_busy"
		if simultaneous {
			name = "simultaneous_start"
		}
		t.Run(name, func(t *testing.T) {
			h := newNativeQueueHarness(t, true)
			a := h.claim(t)
			h.tick(t)
			if _, err := h.controller.StopExpected(h.ctx, "owned-stop-"+h.spec.RunID, h.active); err != nil {
				t.Fatal("owned stop claim failed")
			}
			h.tick(t)
			c := h.claim(t)
			h.tick(t)
			nativeQueueOrder(t, h, []string{a.ClientID, c.ClientID})
			turns, err := h.client.Turns(h.ctx, h.thread)
			if err != nil || len(turns) != 1 || turns[0].Status != "interrupted" || h.boundary.starts != 0 || h.boundary.interrupts != 1 {
				t.Fatal("stop or append implicitly resumed owned queue")
			}
			var nativeResult chan error
			h.boundary.beforeStart = func(ctx context.Context) error {
				if !simultaneous {
					if err := h.other.StartQueue(ctx, h.thread); err != nil {
						return err
					}
					if err := h.model.wait(ctx, 2); err != nil {
						return err
					}
					return nativeQueueEventually(ctx, func() (bool, error) {
						ts, err := h.other.Turns(ctx, h.thread)
						for _, turn := range ts {
							if turn.Status == "inProgress" {
								return true, err
							}
						}
						return false, err
					})
				}
				nativeResult = make(chan error, 1)
				ready := make(chan struct{})
				start := make(chan struct{})
				go func() { close(ready); <-start; nativeResult <- h.other.StartQueue(ctx, h.thread) }()
				<-ready
				close(start)
				return nil
			}
			op, err := h.controller.Continue(h.ctx, "owned-continue-"+h.spec.RunID)
			if err != nil {
				t.Fatal("owned continue claim failed")
			}
			h.tick(t)
			got, err := h.db.SubmissionAction(h.ctx, h.controller.scope(), op.ID)
			if err != nil {
				t.Fatal("continue result unavailable")
			}
			if simultaneous {
				var otherErr error
				select {
				case otherErr = <-nativeResult:
				case <-h.ctx.Done():
					t.Fatal("second owned start did not finish")
				}
				var rpc *codexclient.RPCError
				if got.State == "confirmed" {
					if !errors.As(otherErr, &rpc) {
						t.Fatal("bridge winner did not exclude other native start")
					}
				} else if got.State == "rejected" {
					if otherErr != nil {
						t.Fatal("both concurrent native starts failed")
					}
				} else {
					t.Fatal("concurrent native start outcome unresolved")
				}
			} else if got.State != "rejected" {
				t.Fatal("real native busy did not reject bridge start")
			}
			if h.boundary.starts != 1 || h.boundary.steers != 0 || h.boundary.deletes != 0 || h.boundary.adds != 2 {
				t.Fatal("continue competition steered or repeated queue inputs")
			}
			if err := h.model.wait(h.ctx, 2); err != nil {
				t.Fatal("continue winner did not start owned A")
			}
			nativeQueueOrder(t, h, []string{c.ClientID})
			if len(h.history(t, a.ClientID)) != 1 || len(h.history(t, c.ClientID)) != 0 {
				t.Fatal("start competition lost or duplicated native consumption")
			}
			nativeQueueDrain(t, h, 2)
			h.tick(t)
			if nativeQueueHistoryPosition(t, h, a.ClientID) >= nativeQueueHistoryPosition(t, h, c.ClientID) || h.view(t, a.ID).State != "consumed" || h.view(t, c.ID).State != "consumed" {
				t.Fatal("stopped/appended inputs did not drain once in order")
			}
			t.Log("daemon=" + h.version + " Q10 " + name + " real stop+append and two-client start competition; one start winner; steer=0; A-C preserved once")
		})
	}
}

// This fact probe never primes a cold thread or retries resume. It distinguishes
// the original creator connection from the previously observed second-client
// cold resume rejection without changing the production typed proof.
func TestNativeControllerColdCreatorAttachWithoutInput(t *testing.T) {
	h := newNativeQueueHarness(t, false)
	if h.other != nil {
		t.Fatal("cold creator probe must not initialize a second client")
	}
	joined, attachErr := h.client.AttachLocalVerification(h.ctx, h.thread, h.spec)
	outcome := "typed_success"
	code := 0
	if attachErr != nil {
		var rpc *codexclient.RPCError
		if !errors.As(attachErr, &rpc) || rpc.Code != -32600 || !strings.Contains(rpc.Message, "no rollout found for thread id") {
			t.Fatal("cold creator attach returned an unexpected error; no resume retry or input follows")
		}
		outcome, code = "no_rollout", rpc.Code
	} else if joined.ID != h.thread || joined.CWD != h.spec.CWD || joined.Preview != "" || len(joined.Turns) != 0 {
		t.Fatal("typed cold creator response does not match owned zero-input birth")
	}
	queue, err := h.client.Queue(h.ctx, h.thread)
	if err != nil || len(queue) != 0 {
		t.Fatal("cold creator probe queue is not empty")
	}
	turns, err := h.client.Turns(h.ctx, h.thread)
	history := "empty"
	if codexclient.BeforeFirstInput(err) {
		history = "before_first_user_message"
	} else if err != nil || len(turns) != 0 {
		t.Fatal("cold creator probe did not retain zero-input history")
	}
	h.model.mu.Lock()
	requests := len(h.model.gates)
	h.model.mu.Unlock()
	if requests != 0 || h.boundary.adds != 0 || h.boundary.deletes != 0 || h.boundary.steers != 0 || h.boundary.starts != 0 || h.boundary.interrupts != 0 {
		t.Fatal("cold creator probe issued input or model requests")
	}
	t.Logf("daemon=%s original_creator cold typed_attach=%s RPC_code=%d; single resume without overrides; queue=0 history=%s model_requests=0; no prime/retry", h.version, outcome, code, history)
	if attachErr == nil {
		t.Log("typed readback checked owned ID/private cwd, fixture-model/run provider, never approval, readOnly/no-network, non-ephemeral, validation label and no lineage against original spec")
	}
}
