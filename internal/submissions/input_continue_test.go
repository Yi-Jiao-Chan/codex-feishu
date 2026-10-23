package submissions

import (
	"context"
	"errors"
	"testing"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

func ordinaryInput(t *testing.T, c *Controller, message string) store.Receipt {
	t.Helper()
	r, fresh, err := c.Store.ReceiveSubmissionInput(context.Background(), c.scope(), store.Receipt{AppID: c.AppID, ThreadID: c.ThreadID, ChatID: "chat", MessageID: message, Body: "synthetic ordinary input"})
	if err != nil || !fresh {
		t.Fatalf("ordinary input admission: fresh=%v err=%v", fresh, err)
	}
	return r
}

func inputContinue(t *testing.T, c *Controller, receipt string) store.SubmissionAction {
	t.Helper()
	actions, err := c.Store.SubmissionActions(context.Background(), c.scope())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range actions {
		if _, ok := store.InputContinueSource(a); ok && a.ReceiptID == receipt {
			return a
		}
	}
	return store.SubmissionAction{}
}

func TestOrdinaryInputAfterStopResumesPreservedQueueInOrder(t *testing.T) {
	c, f, _ := fixture(t, true)
	old := claim(t, c, "old-input")
	tick(t, c)
	if _, err := c.Stop(context.Background(), "stop"); err != nil {
		t.Fatal(err)
	}
	tick(t, c)
	if f.starts != 0 || len(f.queue) != 1 {
		t.Fatal("stop resumed or lost the earlier queue")
	}
	fresh := ordinaryInput(t, c, "fresh-input")
	tick(t, c)
	if f.starts != 1 || f.addCalls != 2 || f.steerCalls != 0 || f.deleteCalls != 0 || view(t, c, old.ID).State != "consumed" || len(f.queue) != 1 || *f.queue[0].ClientUserMessageID != fresh.ClientID || inputContinue(t, c, fresh.ID).Result != "continued" {
		t.Fatal("fresh input did not resume the original queue once in FIFO order")
	}
}

func TestOrdinaryInputAdmissionOrderAndPendingStopTarget(t *testing.T) {
	for _, afterStop := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-stop", true: "after-pending-stop"}[afterStop], func(t *testing.T) {
			c, f, _ := fixture(t, true)
			var r store.Receipt
			if !afterStop {
				r = ordinaryInput(t, c, "input")
			}
			stop, err := c.Stop(context.Background(), "stop-not-yet-dispatched")
			if err != nil || stop.ExpectedTurnID != "" {
				t.Fatal("pending slash stop must not have actor-selected target yet", err)
			}
			if afterStop {
				r = ordinaryInput(t, c, "input")
				if source, ok := store.InputContinueSource(inputContinue(t, c, r.ID)); !ok || source != stop.ID {
					t.Fatal("ordinary input did not freeze its pending source stop")
				}
			}
			tick(t, c)
			if f.interruptCalls != 1 || f.steerCalls != 0 || f.addCalls != 1 || afterStop && f.starts != 1 || !afterStop && f.starts != 0 {
				t.Fatal("actor processing time overrode receipt admission order")
			}
		})
	}
}

func TestLaterStopSupersedesEarlierOrdinaryContinue(t *testing.T) {
	c, f, _ := fixture(t, true)
	if _, err := c.Stop(context.Background(), "first-stop"); err != nil {
		t.Fatal(err)
	}
	tick(t, c)
	r := ordinaryInput(t, c, "new-input")
	if _, err := c.Stop(context.Background(), "later-stop"); err != nil {
		t.Fatal(err)
	}
	tick(t, c)
	if f.starts != 0 || f.steerCalls != 0 || len(f.queue) != 1 || inputContinue(t, c, r.ID).Result != "superseded" {
		t.Fatal("a later accepted stop did not override earlier input's continue intent")
	}
}

func TestOrdinaryContinueReconcilesUnknownWithoutRetry(t *testing.T) {
	for _, stage := range []string{"interrupt", "add", "start"} {
		t.Run(stage, func(t *testing.T) {
			c, f, path := fixture(t, true)
			claim(t, c, "old-queued-input")
			tick(t, c)
			if _, err := c.Stop(context.Background(), "stop"); err != nil {
				t.Fatal(err)
			}
			if stage == "interrupt" {
				f.stopReplyErr = errors.New("synthetic reply loss")
			} else {
				tick(t, c)
			}
			r := ordinaryInput(t, c, "new-input")
			if stage == "add" {
				f.addReplyErr = errors.New("synthetic reply loss")
			}
			if stage == "start" {
				f.startReplyErr = errors.New("synthetic reply loss")
			}
			if err := c.Tick(context.Background()); err == nil {
				t.Fatal("ambiguous operation was not retained")
			}
			c = reopen(t, c, path)
			tick(t, c)
			tick(t, c)
			if f.interruptCalls != 1 || f.addCalls != 2 || f.starts != 1 || f.steerCalls != 0 {
				t.Fatal("unknown was retried or confirmed add was lost")
			}
			if stage == "start" {
				a := inputContinue(t, c, r.ID)
				if a.State != "unknown" || a.Result != "" || view(t, c, r.ID).State != "queued" {
					t.Fatalf("unknown start inferred success or erased the confirmed queue/add: action=%s result=%q input=%s", a.State, a.Result, view(t, c, r.ID).State)
				}
			}
		})
	}
}

func TestOrdinaryContinueUsesObservedAppInterruptionOnly(t *testing.T) {
	for _, observed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unobserved", true: "observed"}[observed], func(t *testing.T) {
			c, f, _ := fixture(t, true)
			f.turns[0].Status, f.paused = "interrupted", true
			if observed {
				ctx := context.Background()
				if err := c.Store.StageTaskControls(ctx, c.scope(), []store.TaskControl{{TurnID: "t1", NativeStatus: "interrupted", State: "unavailable", QueueCount: 0}}); err != nil {
					t.Fatal(err)
				}
				if err := c.Store.StageMirrors(ctx, c.HostID, c.ThreadID, nil, store.Watermark{TurnID: "t1", ObservedAtMS: 1000}); err != nil {
					t.Fatal(err)
				}
			}
			ordinaryInput(t, c, "input-after-app-stop")
			tick(t, c)
			if f.steerCalls != 0 || observed && f.starts != 1 || !observed && f.starts != 0 {
				t.Fatal("local evidence of the App interruption was ignored")
			}
		})
	}
}

func TestOrdinaryInputReplayAfterAnotherStopDoesNotResume(t *testing.T) {
	c, f, path := fixture(t, true)
	if _, err := c.Stop(context.Background(), "first-stop"); err != nil {
		t.Fatal(err)
	}
	tick(t, c)
	r := ordinaryInput(t, c, "one-message")
	tick(t, c)
	if _, err := c.Stop(context.Background(), "second-stop"); err != nil {
		t.Fatal(err)
	}
	tick(t, c)
	c = reopen(t, c, path)
	for i := 0; i < 10; i++ {
		_, fresh, err := c.Store.ReceiveSubmissionInput(context.Background(), c.scope(), r)
		if err != nil || fresh {
			t.Fatal("replayed message was admitted anew", err)
		}
		tick(t, c)
	}
	if f.starts != 1 || f.addCalls != 1 || f.interruptCalls != 2 || inputContinue(t, c, r.ID).State != "confirmed" {
		t.Fatal("replay or restart recreated a continue intention")
	}
}

func TestStopArrivingAtStartClaimSupersedesInputContinue(t *testing.T) {
	c, f, _ := fixture(t, true)
	if _, err := c.Stop(context.Background(), "first-stop"); err != nil {
		t.Fatal(err)
	}
	tick(t, c)
	r := ordinaryInput(t, c, "new-input")
	c.BeginWrite = func(ctx context.Context) (context.Context, func(), error) {
		if inputContinue(t, c, r.ID).Stage == "start" {
			if _, err := c.Stop(ctx, "stop-at-start-claim"); err != nil {
				t.Fatal(err)
			}
		}
		return ctx, func() {}, nil
	}
	tick(t, c)
	if f.starts != 0 || len(f.queue) != 1 || inputContinue(t, c, r.ID).Result != "superseded" {
		t.Fatal("final start claim did not recheck the later stop fence")
	}
}

func TestOrdinaryContinueDoesNotSteerWhenAppStartsFirst(t *testing.T) {
	c, f, _ := fixture(t, true)
	claim(t, c, "earlier-input")
	tick(t, c)
	if _, err := c.Stop(context.Background(), "stop"); err != nil {
		t.Fatal(err)
	}
	tick(t, c)
	r := ordinaryInput(t, c, "new-input")
	f.beforeStart = func(f *fakeNative, _ string) { f.consume(f.queue[0].ID) }
	tick(t, c)
	if f.starts != 1 || f.steerCalls != 0 || f.addCalls != 2 || len(f.queue) != 1 || *f.queue[0].ClientUserMessageID != r.ClientID || inputContinue(t, c, r.ID).State != "rejected" {
		t.Fatal("busy native queue/start was retargeted or changed FIFO inputs")
	}
}

func TestUnknownOrdinaryAddConsumedByAppReconcilesWithoutStart(t *testing.T) {
	c, f, path := fixture(t, true)
	if _, err := c.Stop(context.Background(), "stop"); err != nil {
		t.Fatal(err)
	}
	tick(t, c)
	r := ordinaryInput(t, c, "new-input")
	f.addReplyErr = errors.New("synthetic reply loss")
	if err := c.Tick(context.Background()); err == nil {
		t.Fatal("accepted add response loss was not retained")
	}
	f.consume(f.queue[0].ID) // Independent App action, before bridge readback.
	c = reopen(t, c, path)
	tick(t, c)
	if f.addCalls != 1 || f.starts != 0 || f.steerCalls != 0 || view(t, c, r.ID).State != "consumed" || inputContinue(t, c, r.ID).State != "rejected" {
		t.Fatal("exact native history was hidden by pending ordinary continue")
	}
	attempts, err := c.Store.SubmissionAttempts(context.Background(), c.scope())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range attempts {
		if a.ReceiptID == r.ID && a.Stage == "add" && a.State != "confirmed" {
			t.Fatal("history did not resolve the original add attempt")
		}
	}
}

func TestEmptyManualContinueDoesNotHideEarlierPendingStop(t *testing.T) {
	c, f, _ := fixture(t, true)
	ctx := context.Background()
	stop, err := c.Stop(ctx, "pending-stop")
	if err != nil {
		t.Fatal(err)
	}
	manual, err := c.Continue(ctx, "pending-empty-continue")
	if err != nil {
		t.Fatal(err)
	}
	r := ordinaryInput(t, c, "new-input-after-controls")
	if source, ok := store.InputContinueSource(inputContinue(t, c, r.ID)); !ok || source != stop.ID {
		t.Fatal("manual continue hid the preceding stop at input admission")
	}
	tick(t, c)
	tick(t, c)
	tick(t, c)
	manual, err = c.Store.SubmissionAction(ctx, c.scope(), manual.ID)
	if err != nil || manual.State != "rejected" || manual.Result != "empty" || f.interruptCalls != 1 || f.starts != 1 || f.addCalls != 1 || f.steerCalls != 0 || view(t, c, r.ID).State != "consumed" || inputContinue(t, c, r.ID).Result != "continued" {
		t.Fatal("empty manual continue left the later fresh input halted", err)
	}
}

func TestFreshOrdinaryInputPreservesEarlierUnstagedReceiptFIFO(t *testing.T) {
	for _, attachmentsEnabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "attachment-text"}[attachmentsEnabled], func(t *testing.T) {
			c, f, _ := fixture(t, true)
			older := claim(t, c, "older-unstaged-input")
			var freshClient string
			if attachmentsEnabled {
				if _, fresh, err := c.Store.ReceiveAttachmentInput(context.Background(), store.AttachmentInput{Scope: c.scope(), ChatID: "chat", SenderID: "synthetic-operator", MessageID: "fresh-input", Body: "synthetic content"}, 10, 5); err != nil || !fresh {
					t.Fatal(err)
				}
				if err := c.Store.DB.QueryRow(`SELECT client_id FROM m0_receipts WHERE message_id='fresh-input'`).Scan(&freshClient); err != nil {
					t.Fatal(err)
				}
			} else {
				freshClient = ordinaryInput(t, c, "fresh-input").ClientID
			}
			tick(t, c)
			if len(f.queue) != 2 || *f.queue[0].ClientUserMessageID != older.ClientID || *f.queue[1].ClientUserMessageID != freshClient || f.steerCalls != 0 {
				t.Fatal("fresh admission reordered an older unstaged ordinary receipt")
			}
		})
	}
}

func TestRepeatedFrozenStopsAllowLaterOrdinaryInputForSameInterruptedTurn(t *testing.T) {
	c, f, _ := fixture(t, true)
	ctx := context.Background()
	if _, err := c.StopExpected(ctx, "first-frozen-stop", "t1"); err != nil {
		t.Fatal(err)
	}
	second, err := c.StopExpected(ctx, "second-frozen-stop", "t1")
	if err != nil {
		t.Fatal(err)
	}
	r := ordinaryInput(t, c, "input-after-two-stops")
	tick(t, c)
	second, err = c.Store.SubmissionAction(ctx, c.scope(), second.ID)
	if err != nil || second.State != "rejected" || second.Result != "ended" || second.ExpectedTurnID != "t1" {
		t.Fatal("second frozen stop did not see the already interrupted target", err)
	}
	if f.interruptCalls != 1 || f.starts != 1 || f.addCalls != 1 || f.steerCalls != 0 || view(t, c, r.ID).State != "consumed" || inputContinue(t, c, r.ID).ExpectedTurnID != "t1" {
		t.Fatal("redundant same-turn stop erased the new input's continue intention")
	}
	if source, ok := store.InputContinueSource(inputContinue(t, c, r.ID)); !ok || source != second.ID {
		t.Fatal("continuation changed its durable stop source")
	}
	attempts, err := c.Store.SubmissionAttempts(ctx, c.scope())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range attempts {
		if a.ActionID == second.ID {
			t.Fatal("redundant stop unexpectedly dispatched a native operation")
		}
	}
}

func TestOrdinaryContinueDoesNotBypassNativeStopRejection(t *testing.T) {
	c, f, _ := fixture(t, true)
	if _, err := c.StopExpected(context.Background(), "native-rejected-stop", "t1"); err != nil {
		t.Fatal(err)
	}
	r := ordinaryInput(t, c, "new-input")
	// The fixture exposes an interrupted target despite an explicit native error.
	// An actual dispatched/rejected stop never qualifies as the redundant case.
	f.stopReplyErr = &codexclient.RPCError{Code: -32600, Message: "synthetic rejection"}
	tick(t, c)
	if f.starts != 0 || f.addCalls != 1 || f.steerCalls != 0 || inputContinue(t, c, r.ID).State != "rejected" {
		t.Fatal("native stop rejection was treated as a redundant unsent stop")
	}
}

func TestOrdinaryInputAfterRedundantStopBeforeHistoryObservationResumesSameTurn(t *testing.T) {
	c, f, _ := fixture(t, true)
	ctx := context.Background()
	if _, err := c.StopExpected(ctx, "first-stop", "t1"); err != nil {
		t.Fatal(err)
	}
	second, err := c.StopExpected(ctx, "second-stop", "t1")
	if err != nil {
		t.Fatal(err)
	}
	tick(t, c) // No TaskControls/history snapshot has observed the stop yet.
	r := ordinaryInput(t, c, "input-after-already-redundant-stop")
	if source, ok := store.InputContinueSource(inputContinue(t, c, r.ID)); !ok || source != second.ID {
		t.Fatal("an already redundant same-target stop hid its frozen target")
	}
	tick(t, c)
	if f.starts != 1 || f.addCalls != 1 || f.steerCalls != 0 || view(t, c, r.ID).State != "consumed" {
		t.Fatal("new input did not resume the same already-interrupted target")
	}
}
