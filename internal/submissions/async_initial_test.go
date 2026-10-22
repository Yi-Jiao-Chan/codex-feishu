package submissions

import (
	"context"
	"errors"
	"testing"

	"codex-feishu/internal/store"
)

// Seed a synthetic delivered card, then use the real callback transaction so
// every delivery/recovery test exercises the intent it actually persists.
func initialAsyncAnswer(t *testing.T, c *Controller, message string) (store.Receipt, store.SubmissionAction) {
	t.Helper()
	ctx := context.Background()
	key := "async:" + message
	if err := c.Store.ObserveInteraction(ctx, store.InteractionRequest{
		Key: key, HostID: c.HostID, AppID: c.AppID, ThreadID: c.ThreadID,
		TurnID: "question-turn", ItemID: "question-item", Mode: "async", Method: "async",
		Payload: `{}`, ConnectionID: "fixture-connection", Generation: 1, State: "pending", CreatedAtMS: 101,
	}); err != nil {
		t.Fatal(err)
	}
	row, err := c.Store.Interaction(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Store.DB.Exec(`UPDATE interaction_requests SET operable=1 WHERE request_key=?`, key); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Store.DB.Exec(`INSERT INTO mirrors(host_id,thread_id,mirror_key,turn_id,kind,card_json,remote_id) VALUES(?,?,?,?,'progress','{}',?)`, c.HostID, c.ThreadID, store.InteractionCardKey(key), row.TurnID, message); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Store.DB.Exec(`INSERT INTO outbound(host_id,thread_id,mirror_key,state) VALUES(?,?,?,'confirmed')`, c.HostID, c.ThreadID, store.InteractionCardKey(key)); err != nil {
		t.Fatal(err)
	}
	claimed, err := c.Store.ClaimAsyncInteraction(ctx, c.scope(), "operator", []string{"operator"}, "chat", message, row.Token, row.Revision, row.ConnectionID, row.Generation, `{}`, "fixture answer", "fixture async answer", 102)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Store.StageSubmissions(ctx, c.scope()); err != nil {
		t.Fatal(err)
	}
	n, err := c.Store.Submission(ctx, c.scope(), claimed.SubmissionID)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := c.Store.SubmissionActions(ctx, c.scope())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range actions {
		if a.ReceiptID == n.ID {
			return n.Receipt, a
		}
	}
	t.Fatal("atomic async claim omitted its submission action")
	return store.Receipt{}, store.SubmissionAction{}
}

func setInitialTestState(t *testing.T, c *Controller, receipt, state string) {
	t.Helper()
	for _, query := range []string{`UPDATE m0_receipts SET state=? WHERE id=?`, `UPDATE submission_inputs SET state=? WHERE receipt_id=?`} {
		if _, err := c.Store.DB.Exec(query, state, receipt); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInitialAsyncAnswerSteersBeforeOrdinaryInputWithoutQueueDelete(t *testing.T) {
	c, f, _ := fixture(t, true)
	ordinary := claim(t, c, "ordinary-before-answer")
	r, a := initialAsyncAnswer(t, c, "direct-answer")
	f.beforeSteer = func(_ *fakeNative, client string) {
		attempts, err := c.Store.SubmissionAttempts(context.Background(), c.scope())
		if err != nil || len(attempts) != 1 || attempts[0].State != "dispatching" || attempts[0].Stage != "steer" || attempts[0].ExpectedTurnID != "t1" || attempts[0].ClientID != client {
			t.Fatal("steer reached native before its exact durable attempt", err)
		}
	}
	tick(t, c)
	got, err := c.Store.SubmissionAction(context.Background(), c.scope(), a.ID)
	if err != nil || got.Result != "steered" || got.ExpectedTurnID != "t1" || view(t, c, r.ID).State != "steered" || view(t, c, ordinary.ID).State != "queued" || f.steerCalls != 1 || f.deleteCalls != 0 || f.addCalls != 1 || f.starts != 0 {
		t.Fatal("initial async answer did not steer the original turn once", err)
	}
	if len(f.clients) != 2 || f.clients[0] != r.ClientID || f.clients[1] != ordinary.ClientID {
		t.Fatal("ordinary input was steered or dispatched before the explicit answer")
	}
}

func TestInitialAsyncAnswerActiveSteerBypassesCollecting(t *testing.T) {
	c, f, _ := fixture(t, true)
	attachment := claim(t, c, "collecting-before-active-answer")
	r, _ := initialAsyncAnswer(t, c, "answer-past-collecting")
	setInitialTestState(t, c, attachment.ID, "collecting")
	ordinary := claim(t, c, "ordinary-after-answer")
	tick(t, c)
	if f.steerCalls != 1 || f.addCalls != 0 || f.deleteCalls != 0 || view(t, c, r.ID).State != "steered" || view(t, c, attachment.ID).State != "collecting" || view(t, c, ordinary.ID).State != "received" {
		t.Fatal("explicit answer did not bypass collecting without releasing ordinary FIFO")
	}
}

func TestInitialAsyncAnswerIdleFallbackWaitsForCollectingAndStaysQueued(t *testing.T) {
	c, f, _ := fixture(t, false)
	attachment := claim(t, c, "collecting-before-idle-answer")
	r, a := initialAsyncAnswer(t, c, "idle-answer")
	setInitialTestState(t, c, attachment.ID, "collecting")
	tick(t, c)
	got, err := c.Store.SubmissionAction(context.Background(), c.scope(), a.ID)
	if err != nil || got.State != "ready" || got.Stage != "restore" || got.ExpectedTurnID != "" || f.addCalls != 0 || f.steerCalls != 0 {
		t.Fatal("idle answer bypassed collecting or failed to freeze the queue path", err)
	}
	setInitialTestState(t, c, attachment.ID, "received")
	tick(t, c)
	if f.addCalls != 2 || f.steerCalls != 0 || f.deleteCalls != 0 || len(f.clients) != 2 || f.clients[0] != attachment.ClientID || f.clients[1] != r.ClientID || view(t, c, r.ID).State != "queued" {
		t.Fatal("idle answer changed to steering after an earlier input started a turn")
	}
}

func TestInitialAsyncAnswerRejectedSteerRestoresOnlyAtFIFOPosition(t *testing.T) {
	c, f, path := fixture(t, true)
	attachment := claim(t, c, "collecting-before-rejected-answer")
	r, a := initialAsyncAnswer(t, c, "rejected-answer")
	setInitialTestState(t, c, attachment.ID, "collecting")
	f.beforeSteer = func(f *fakeNative, _ string) {
		f.end()
		f.start("")
	}
	tick(t, c)
	got, err := c.Store.SubmissionAction(context.Background(), c.scope(), a.ID)
	if err != nil || got.State != "ready" || got.Stage != "restore" || got.ExpectedTurnID != "t1" || f.addCalls != 0 || f.steerCalls != 1 || f.deleteCalls != 0 {
		t.Fatal("rejected direct steer skipped FIFO while restoring", err)
	}
	c = reopen(t, c, path)
	setInitialTestState(t, c, attachment.ID, "failed")
	tick(t, c)
	got, err = c.Store.SubmissionAction(context.Background(), c.scope(), a.ID)
	if err != nil || got.Result != "restored" || got.ExpectedTurnID != "t1" || f.addCalls != 1 || f.steerCalls != 1 || f.expected[0] != "t1" || view(t, c, r.ID).State != "queued" {
		t.Fatal("restore after restart retargeted or duplicated the direct answer", err)
	}
}

func TestInitialAsyncAnswerStopRunsBeforeIdleFallback(t *testing.T) {
	c, f, _ := fixture(t, true)
	r, _ := initialAsyncAnswer(t, c, "stopped-answer")
	if _, err := c.Stop(context.Background(), "stop-after-answer-claim"); err != nil {
		t.Fatal(err)
	}
	tick(t, c)
	if f.interruptCalls != 1 || f.steerCalls != 0 || f.addCalls != 1 || f.deleteCalls != 0 || f.starts != 0 || !f.paused || len(f.queue) != 1 || view(t, c, r.ID).State != "queued" {
		t.Fatal("async answer resumed or steered a stopped queue")
	}
}

func TestInitialAsyncAnswerLostSteerReplyReconcilesExactOriginalTurn(t *testing.T) {
	c, f, path := fixture(t, true)
	r, a := initialAsyncAnswer(t, c, "lost-direct-reply")
	f.steerReplyErr = errors.New("fixture lost reply")
	if err := c.Tick(context.Background()); err == nil || view(t, c, r.ID).State != "unknown" {
		t.Fatal("lost direct reply was not held for verification")
	}
	c = reopen(t, c, path)
	tick(t, c)
	tick(t, c)
	got, err := c.Store.SubmissionAction(context.Background(), c.scope(), a.ID)
	if err != nil || got.Result != "steered" || got.ExpectedTurnID != "t1" || f.steerCalls != 1 || f.addCalls != 0 || f.deleteCalls != 0 || view(t, c, r.ID).State != "steered" {
		t.Fatal("direct steer recovery did not use exact original-turn evidence", err)
	}
}

func TestInitialAsyncAnswerUnknownNeverFallsBackWithoutExactTurnEvidence(t *testing.T) {
	for _, wrongTurn := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "other_turn"}[wrongTurn], func(t *testing.T) {
			c, f, path := fixture(t, true)
			r, _ := initialAsyncAnswer(t, c, "unproven-direct-reply")
			f.steerReplyErr = errors.New("fixture lost reply")
			if err := c.Tick(context.Background()); err == nil {
				t.Fatal("expected lost reply")
			}
			f.turns[0].Items = f.turns[0].Items[:1]
			f.end()
			if wrongTurn {
				f.start(r.ClientID)
			}
			c = reopen(t, c, path)
			tick(t, c)
			tick(t, c)
			if f.steerCalls != 1 || f.addCalls != 0 || f.deleteCalls != 0 || view(t, c, r.ID).State != "unknown" {
				t.Fatal("unproven direct steer was resent, queued, or falsely confirmed")
			}
		})
	}
}

func TestInitialAsyncAnswerPreparedTargetSurvivesRestart(t *testing.T) {
	c, f, path := fixture(t, true)
	r, a := initialAsyncAnswer(t, c, "prepared-direct-answer")
	if err := c.Store.PrepareSubmissionAction(context.Background(), c.scope(), a.ID, "steer", "t1", "", r.ClientID); err != nil {
		t.Fatal(err)
	}
	f.end()
	f.start("")
	c = reopen(t, c, path)
	tick(t, c)
	got, err := c.Store.SubmissionAction(context.Background(), c.scope(), a.ID)
	if err != nil || got.ExpectedTurnID != "t1" || got.Result != "restored" || f.steerCalls != 1 || f.expected[0] != "t1" || f.addCalls != 1 || f.deleteCalls != 0 {
		t.Fatal("restart retargeted a prepared initial steer", err)
	}
}

func TestInitialAsyncAnswerDispatchCrashCannotResend(t *testing.T) {
	c, f, path := fixture(t, true)
	r, a := initialAsyncAnswer(t, c, "crashed-direct-answer")
	ctx := context.Background()
	if err := c.Store.PrepareSubmissionAction(ctx, c.scope(), a.ID, "steer", "t1", "", r.ClientID); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := c.Store.ClaimSubmissionAttempt(ctx, c.scope(), r.ID, a.ID, "steer"); err != nil || !claimed {
		t.Fatal("could not journal the pre-crash direct steer", err)
	}
	c = reopen(t, c, path)
	tick(t, c)
	if f.steerCalls != 0 || f.addCalls != 0 || f.deleteCalls != 0 || view(t, c, r.ID).State != "unknown" {
		t.Fatal("a dispatching direct steer became safe to resend after restart")
	}
}

func TestInitialAsyncAnswerLostIdleAddReplyNeverResends(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(map[bool]string{false: "consumed", true: "queued"}[paused], func(t *testing.T) {
			c, f, path := fixture(t, false)
			f.paused = paused
			r, a := initialAsyncAnswer(t, c, "lost-idle-add")
			f.addReplyErr = errors.New("fixture lost reply")
			if err := c.Tick(context.Background()); err == nil {
				t.Fatal("expected unknown queue/add")
			}
			c = reopen(t, c, path)
			tick(t, c)
			got, err := c.Store.SubmissionAction(context.Background(), c.scope(), a.ID)
			wantState := map[bool]string{false: "consumed", true: "queued"}[paused]
			if err != nil || got.Result != "restored" || view(t, c, r.ID).State != wantState || f.addCalls != 1 || f.steerCalls != 0 || f.deleteCalls != 0 || f.starts != 0 {
				t.Fatal("idle queue/add recovery duplicated or resumed the answer", err)
			}
		})
	}
}

func TestAsyncProvenanceAloneDoesNotUpgradeLegacyQueuedAnswer(t *testing.T) {
	c, f, _ := fixture(t, true)
	r, a := initialAsyncAnswer(t, c, "legacy-answer")
	if _, err := c.Store.DB.Exec(`DELETE FROM submission_actions WHERE action_id=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	setInitialTestState(t, c, r.ID, "received")
	tick(t, c)
	if f.addCalls != 1 || f.steerCalls != 0 || f.deleteCalls != 0 || view(t, c, r.ID).State != "queued" {
		t.Fatal("legacy async provenance was treated as a new explicit steer intent")
	}
}

func TestInitialAsyncAnswerRequiresProvenanceAtPrepareAndDispatch(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		t.Run(map[bool]string{false: "prepare", true: "dispatch"}[prepared], func(t *testing.T) {
			c, f, _ := fixture(t, true)
			r, a := initialAsyncAnswer(t, c, "guarded-answer")
			ctx := context.Background()
			if prepared {
				if err := c.Store.PrepareSubmissionAction(ctx, c.scope(), a.ID, "steer", "t1", "", r.ClientID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := c.Store.DB.Exec(`UPDATE interaction_requests SET mode='native' WHERE submission_id=?`, r.ID); err != nil {
				t.Fatal(err)
			}
			var err error
			if prepared {
				_, _, err = c.Store.ClaimSubmissionAttempt(ctx, c.scope(), r.ID, a.ID, "steer")
			} else {
				err = c.Store.PrepareSubmissionAction(ctx, c.scope(), a.ID, "steer", "t1", "", r.ClientID)
			}
			attempts, attemptsErr := c.Store.SubmissionAttempts(ctx, c.scope())
			if !errors.Is(err, store.ErrSubmissionStale) || attemptsErr != nil || len(attempts) != 0 || f.steerCalls != 0 || f.addCalls != 0 {
				t.Fatal("ordinary provenance permitted a direct async attempt", err, attemptsErr)
			}
		})
	}
}
