package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
	"codex-feishu/internal/submissions"
)

type retryTaskNative struct {
	*taskNative
	t       *testing.T
	s       *store.Store
	path    string
	calls   int
	outcome string
}

func (n *retryTaskNative) Retry(ctx context.Context, thread, attempt string) (codexclient.Turn, error) {
	n.t.Helper()
	var state, target string
	if err := n.s.DB.QueryRowContext(ctx, `SELECT state,expected_turn_id FROM submission_attempts WHERE attempt_id=? AND stage='retry'`, attempt).Scan(&state, &target); err != nil || state != "dispatching" || target != "original-turn" || thread != "thread" {
		n.t.Fatal("retry escaped its durable original-turn claim", err)
	}
	n.calls++
	if n.outcome == "rejected" {
		return codexclient.Turn{}, &codexclient.RPCError{Code: -32600}
	}
	if n.outcome == "not-accepted-unknown" {
		return codexclient.Turn{}, &codexclient.UnknownError{Method: "turn/start"}
	}
	phase := "commentary"
	turn := codexclient.Turn{ID: "retry-turn", Status: "inProgress", ItemsView: "full", Items: []codexclient.Item{{ID: "retry-public", Type: "agentMessage", Text: "synthetic retry progress", Phase: &phase}}}
	n.turns = append(n.turns, turn)
	record, _ := json.Marshal(map[string]any{"type": "event_msg", "payload": map[string]any{
		"type": "task_started", "turn_id": turn.ID,
		"turn_attribution": map[string]any{"turn_id": turn.ID, "turn_trigger": codexclient.ManualRetryTriggerPrefix + attempt},
	}})
	f, err := os.OpenFile(n.path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		n.t.Fatal(err)
	}
	if _, err = f.Write(append(record, '\n')); err != nil {
		n.t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		n.t.Fatal(err)
	}
	if n.outcome == "accepted-unknown" {
		return codexclient.Turn{}, &codexclient.UnknownError{Method: "turn/start"}
	}
	return turn, nil
}

func retryControlFixture(t *testing.T) (*taskFixture, *retryTaskNative, *submissions.Controller) {
	t.Helper()
	f := newTaskFixture(t, true, "inProgress")
	f.controls.RetryEnabled = true
	f.engine.FinalRender = f.controls.RenderFinal
	f.prepare(t, 200)
	f.deliver(t, 1000)
	f.native.turns[0].Status = "failed"
	f.native.turns[0].Error = &codexclient.TurnError{Info: json.RawMessage(`"serverOverloaded"`)}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "sessions", "rollout.jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"thread\"}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	native := &retryTaskNative{taskNative: f.native, t: t, s: f.s, path: path}
	origins := codexclient.NewUnanchoredTurnReader(func(context.Context, string) (codexclient.Thread, error) {
		return codexclient.Thread{ID: "thread", Path: &path}, nil
	}, root)
	f.engine.ReadUnanchoredTurns = origins.Read
	c := &submissions.Controller{Store: f.s, Native: native, HostID: "host", AppID: "app", ThreadID: "thread", RetryEvidence: origins.RetryTurns}
	f.prepare(t, 201)
	f.deliver(t, 4000)
	return f, native, c
}

func retryFinal(t *testing.T, f *taskFixture) store.Mirror {
	t.Helper()
	list, err := f.s.Mirrors(context.Background(), "host", "thread")
	if err != nil {
		t.Fatal(err)
	}
	for _, mirror := range list {
		if mirror.Kind == "final" && mirror.TurnID == "original-turn" {
			return mirror
		}
	}
	t.Fatal("failed result card missing")
	return store.Mirror{}
}

func TestRetryCardClaimIsLocalDeduplicatedAndUsesCompactedFinal(t *testing.T) {
	f, native, c := retryControlFixture(t)
	shown, view := retryFinal(t, f), f.view(t)
	if view.Kind != "retry" || view.State != "available" || shown.Card != "" || !shown.HasCallback || shown.CallbackProofs == "" {
		t.Fatal("failed result lost retry authorization after body compaction")
	}
	reads := native.reads
	var wg sync.WaitGroup
	errors := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			callback := TaskControls{Store: f.s, HostID: "host", AppID: "app"}
			_, err := callback.Claim(context.Background(), "chat", shown.RemoteID, "retry", view.Token, view.Revision)
			errors <- err
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if native.reads != reads || native.calls != 0 {
		t.Fatal("card callback performed native I/O")
	}
	if err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.prepare(t, 202)
	f.deliver(t, 7000)
	if native.calls != 1 || native.starts != 0 || len(native.turns) != 2 || native.turns[1].Items[0].Type == "userMessage" {
		t.Fatal("retry repeated input, started the queue, or dispatched more than once")
	}
	if view = f.view(t); view.State != "unavailable" || view.Reason != "retried" {
		t.Fatal("retry button stayed available after success")
	}
	for range 2 {
		if err := c.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if native.calls != 1 {
		t.Fatal("polling repeated the retry")
	}
}

func TestRetryCardRejectsWrongMessageAndStaleNativeState(t *testing.T) {
	for _, scenario := range []string{"wrong-message", "wrong-chat", "wrong-revision", "new-turn", "queue", "nonretryable", "write-lease-race"} {
		t.Run(scenario, func(t *testing.T) {
			f, native, c := retryControlFixture(t)
			shown, view := retryFinal(t, f), f.view(t)
			chat, message, revision := "chat", shown.RemoteID, view.Revision
			switch scenario {
			case "wrong-message":
				message = "fabricated-card"
			case "wrong-chat":
				chat = "foreign-chat"
			case "wrong-revision":
				revision++
			}
			_, err := f.controls.Claim(context.Background(), chat, message, "retry", view.Token, revision)
			if strings.HasPrefix(scenario, "wrong-") {
				if !errors.Is(err, store.ErrSubmissionStale) || native.calls != 0 {
					t.Fatal("forged retry card was accepted", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			replace := func() {
				native.turns = append(native.turns, codexclient.Turn{ID: "app-new-turn", Status: "inProgress", ItemsView: "full"})
			}
			switch scenario {
			case "new-turn":
				replace()
			case "queue":
				native.queue = []codexclient.Queued{{ID: "app-pending"}}
			case "nonretryable":
				native.turns[0].Error.Info = json.RawMessage(`"usageLimitExceeded"`)
			case "write-lease-race":
				c.BeginWrite = func(ctx context.Context) (context.Context, func(), error) { replace(); return ctx, func() {}, nil }
			}
			if err = c.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			attempts, err := f.s.SubmissionAttempts(context.Background(), store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"})
			if err != nil || native.calls != 0 || len(attempts) != 0 {
				t.Fatal("stale retry reached the native write journal", err)
			}
		})
	}
}

func TestRetryUnknownRequiresExactNativeAttributionAndNeverResends(t *testing.T) {
	for _, outcome := range []string{"accepted-unknown", "not-accepted-unknown"} {
		t.Run(outcome, func(t *testing.T) {
			f, native, c := retryControlFixture(t)
			native.outcome = outcome
			shown, view := retryFinal(t, f), f.view(t)
			if _, err := f.controls.Claim(context.Background(), "chat", shown.RemoteID, "retry", view.Token, view.Revision); err != nil {
				t.Fatal(err)
			}
			if err := c.Tick(context.Background()); err == nil {
				t.Fatal("dropped retry response did not remain unknown")
			}
			f.prepare(t, 202)
			if view := f.view(t); view.State != "unknown" {
				t.Fatal("unknown retry offered another click")
			}
			if outcome == "not-accepted-unknown" {
				native.turns = append(native.turns, codexclient.Turn{ID: "unrelated-app-turn", Status: "completed", ItemsView: "full"})
			}
			for range 2 {
				if err := c.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			actions, err := f.s.SubmissionActions(context.Background(), store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"})
			want := "unknown"
			if outcome == "accepted-unknown" {
				want = "confirmed"
			}
			if err != nil || len(actions) != 1 || actions[0].State != want || native.calls != 1 {
				t.Fatal("unknown retry was guessed from activity or dispatched twice", err)
			}
		})
	}
}

func TestRetryAvailabilityNeedsSupportedLatestFailureAndEmptyKnownQueue(t *testing.T) {
	for _, scenario := range []string{"available", "unsupported", "queue-read-failed", "queue-not-empty", "usage-limit", "later-turn"} {
		t.Run(scenario, func(t *testing.T) {
			f, native, _ := retryControlFixture(t)
			switch scenario {
			case "unsupported":
				f.controls.RetryEnabled = false
			case "queue-read-failed":
				native.queueErr = errors.New("synthetic unavailable queue")
			case "queue-not-empty":
				native.queue = []codexclient.Queued{{ID: "app-pending"}}
			case "usage-limit":
				native.turns[0].Error.Info = json.RawMessage(`"usageLimitExceeded"`)
			case "later-turn":
				native.turns = append(native.turns, codexclient.Turn{ID: "later", Status: "completed", ItemsView: "full"})
			}
			f.prepare(t, 202)
			view, final := f.view(t), retryFinal(t, f)
			want := scenario == "available"
			// Unchanged available cards are already compacted; their durable
			// capability bit is the equivalent of an available callback body.
			if (view.Kind == "retry" && view.State == "available") != want || final.HasCallback != want {
				t.Fatal("retry availability ignored current native or queue evidence")
			}
		})
	}
}

func TestRetryDefiniteRejectionAllowsOneNewUserClick(t *testing.T) {
	f, native, c := retryControlFixture(t)
	native.outcome = "rejected"
	shown, original := retryFinal(t, f), f.view(t)
	if _, err := f.controls.Claim(context.Background(), "chat", shown.RemoteID, "retry", original.Token, original.Revision); err != nil {
		t.Fatal(err)
	}
	if err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.prepare(t, 202)
	f.deliver(t, 7000)
	current, updated := f.view(t), retryFinal(t, f)
	if current.State != "available" || current.Revision <= original.Revision || updated.RemoteID != shown.RemoteID {
		t.Fatal("definite rejection did not refresh the same result card")
	}
	native.outcome = ""
	if _, err := f.controls.Claim(context.Background(), "chat", updated.RemoteID, "retry", current.Token, current.Revision); err != nil {
		t.Fatal(err)
	}
	if err := c.Tick(context.Background()); err != nil || native.calls != 2 {
		t.Fatal("new explicit click did not get one fresh attempt after rejection", err)
	}
}

func TestRetryCrashRecoveryUsesThePersistedAttemptNonce(t *testing.T) {
	f, native, c := retryControlFixture(t)
	ctx := context.Background()
	shown, view := retryFinal(t, f), f.view(t)
	if _, err := f.controls.Claim(ctx, "chat", shown.RemoteID, "retry", view.Token, view.Revision); err != nil {
		t.Fatal(err)
	}
	scope := store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}
	actions, err := f.s.SubmissionActions(ctx, scope)
	if err != nil || len(actions) != 1 {
		t.Fatal(err)
	}
	if err = f.s.PrepareSubmissionAction(ctx, scope, actions[0].ID, "retry", "original-turn", "", ""); err != nil {
		t.Fatal(err)
	}
	attempt, claimed, err := f.s.ClaimSubmissionAttempt(ctx, scope, "", actions[0].ID, "retry")
	if err != nil || !claimed {
		t.Fatal(err)
	}
	if _, err = native.Retry(ctx, "thread", attempt.ID); err != nil {
		t.Fatal(err)
	}
	// Crash after the remote start but before recording the response.
	f.s.Close()
	f.s, err = store.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	native.s = f.s
	restarted := &submissions.Controller{Store: f.s, Native: native, HostID: "host", AppID: "app", ThreadID: "thread", RetryEvidence: c.RetryEvidence}
	if err = restarted.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	actions, err = f.s.SubmissionActions(ctx, scope)
	attempts, readErr := f.s.SubmissionAttempts(ctx, scope)
	if err != nil || readErr != nil || actions[0].State != "confirmed" || len(attempts) != 1 || attempts[0].ID != attempt.ID || attempts[0].RemoteID != "retry-turn" || native.calls != 1 {
		t.Fatal("restart duplicated the retry or lost its confirmed native identity", err, readErr)
	}
}
