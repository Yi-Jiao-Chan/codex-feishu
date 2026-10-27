// Package submissions uses the original App thread's native execution queue.
// Its durable journals serialize bridge writes; they do not lock the App.
package submissions

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

type Native interface {
	Add(context.Context, string, string, string) (codexclient.Queued, error)
	Delete(context.Context, string, string) (bool, error)
	Steer(context.Context, string, string, string, string) error
	Interrupt(context.Context, string, string) error
	StartQueue(context.Context, string) error
	Queue(context.Context, string) ([]codexclient.Queued, error)
	Turns(context.Context, string) ([]codexclient.Turn, error)
}

// InputNative is optional so existing Native fixtures and integrations can
// keep the legacy text methods while deployments add native multipart input.
type InputNative interface {
	AddInput(context.Context, string, json.RawMessage, string) (codexclient.Queued, error)
	SteerInput(context.Context, string, string, json.RawMessage, string) error
}

type RetryNative interface {
	Retry(context.Context, string, string) (codexclient.Turn, error)
}

var _ Native = (*codexclient.Client)(nil)
var _ InputNative = (*codexclient.Client)(nil)

var errInputNativeUnsupported = errors.New("native multipart input unsupported")

// Error deliberately omits native error messages and input content. Cause is
// retained for programmatic classification, never concatenated into log text.
type Error struct{ Cause error }

func (*Error) Error() string   { return "native input processing awaits verification" }
func (e *Error) Unwrap() error { return e.Cause }

type Controller struct {
	Native                  Native
	Store                   *store.Store
	HostID, AppID, ThreadID string
	BeginWrite              func(context.Context) (context.Context, func(), error)
	RetryEvidence           func(context.Context, string, []string) (map[string]string, error)
	mu                      sync.Mutex
	recovered               bool
	// Points to a call-local result only while TickWithHistory holds mu.
	historyHandoff *[]codexclient.Turn
}

func (c *Controller) scope() store.SubmissionScope {
	return store.SubmissionScope{HostID: c.HostID, AppID: c.AppID, ThreadID: c.ThreadID}
}

func (c *Controller) valid() error {
	if c.Store == nil || c.HostID == "" || c.AppID == "" || c.ThreadID == "" {
		return store.ErrConflict
	}
	return nil
}

// claimAndDispatch owns one durable native write stage. A stage gets its own
// write lease, so a later stage cannot reuse a grant that covered an earlier
// native call. The lease stays held until dispatch has persisted its outcome.
func (c *Controller) claimAndDispatch(ctx context.Context, n store.Submission, actionID, stage string) (bool, error) {
	if err := c.validateInput(n, stage); err != nil {
		return false, err
	}
	leaseCtx := ctx
	release := func() {}
	if c.BeginWrite != nil {
		var err error
		leaseCtx, release, err = c.BeginWrite(ctx)
		if err != nil {
			if release != nil {
				release()
			}
			return false, err
		}
		if leaseCtx == nil {
			if release != nil {
				release()
			}
			return false, errors.New("write lease unavailable")
		}
		if release == nil {
			return false, errors.New("write lease unavailable")
		}
	}
	defer release()
	if err := leaseCtx.Err(); err != nil {
		return false, err
	}
	if stage == "retry" {
		op, err := c.Store.SubmissionAction(leaseCtx, c.scope(), actionID)
		if err != nil {
			return false, err
		}
		snap, err := c.snapshot(leaseCtx)
		if err != nil {
			return false, err
		}
		if !snap.canRetry(op.ExpectedTurnID) {
			return false, c.Store.ResolveSubmissionAction(leaseCtx, c.scope(), op.ID, "rejected", "changed")
		}
	}
	a, claimed, err := c.Store.ClaimSubmissionAttempt(leaseCtx, c.scope(), n.ID, actionID, stage)
	if err != nil || !claimed {
		return claimed, err
	}
	if err = c.dispatch(leaseCtx, a, n); err != nil {
		return true, err
	}
	return true, nil
}

// ClaimAction does local durable work only. The composition root authenticates
// the operator and card token/chat/message before passing the receipt identity.
// A callback can therefore complete while the actor's Tick is awaiting an RPC.
func (c *Controller) ClaimAction(ctx context.Context, receiptID, kind, expectedQueueID string, revision int64) (store.SubmissionAction, error) {
	if err := c.valid(); err != nil {
		return store.SubmissionAction{}, err
	}
	if err := c.Store.StageSubmissions(ctx, c.scope()); err != nil {
		return store.SubmissionAction{}, &Error{err}
	}
	a, err := c.Store.ClaimSubmissionAction(ctx, c.scope(), receiptID, kind, expectedQueueID, revision)
	return a, safeClaimError(err)
}

func (c *Controller) Stop(ctx context.Context, actionID string) (store.SubmissionAction, error) {
	return c.claimControl(ctx, "stop", actionID, "")
}

// StopExpected binds a card action to the turn the card displayed. It performs
// no native reads or writes; an ended/replaced turn is rejected by Tick.
func (c *Controller) StopExpected(ctx context.Context, actionID, expectedTurnID string) (store.SubmissionAction, error) {
	if expectedTurnID == "" {
		return store.SubmissionAction{}, store.ErrSubmissionStale
	}
	return c.claimControl(ctx, "stop", actionID, expectedTurnID)
}

func (c *Controller) Continue(ctx context.Context, actionID string) (store.SubmissionAction, error) {
	return c.claimControl(ctx, "continue", actionID, "")
}

// ContinueExpected resumes only the queue associated with the displayed latest
// interrupted turn. Slash Continue intentionally keeps a thread-level intent.
func (c *Controller) ContinueExpected(ctx context.Context, actionID, expectedTurnID string) (store.SubmissionAction, error) {
	if expectedTurnID == "" {
		return store.SubmissionAction{}, store.ErrSubmissionStale
	}
	return c.claimControl(ctx, "continue", actionID, expectedTurnID)
}

func (c *Controller) RetryExpected(ctx context.Context, actionID, expectedTurnID string) (store.SubmissionAction, error) {
	if expectedTurnID == "" {
		return store.SubmissionAction{}, store.ErrSubmissionStale
	}
	return c.claimControl(ctx, "retry", actionID, expectedTurnID)
}

func (c *Controller) claimControl(ctx context.Context, kind, actionID, expectedTurnID string) (store.SubmissionAction, error) {
	if err := c.valid(); err != nil {
		return store.SubmissionAction{}, err
	}
	if err := c.Store.StageSubmissions(ctx, c.scope()); err != nil {
		return store.SubmissionAction{}, &Error{err}
	}
	a, err := c.Store.ClaimThreadSubmissionActionExpected(ctx, c.scope(), kind, actionID, expectedTurnID)
	return a, safeClaimError(err)
}

func safeClaimError(err error) error {
	if err == nil || errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrSubmissionStale) || errors.Is(err, store.ErrSubmissionBusy) {
		return err
	}
	return &Error{err}
}

// Tick runs once per actor wake/reconnect. Recovery precedes all native writes;
// a lost success/local-save failure is reconciled, never blindly dispatched.
func (c *Controller) Tick(ctx context.Context) error {
	_, err := c.TickWithHistory(ctx)
	return err
}

// TickWithHistory returns the last complete history read during this tick so
// the actor can project it without a second native read. Every action still
// takes fresh evidence as before; a native mutation invalidates the handoff
// until a subsequent snapshot succeeds. No history survives the call.
func (c *Controller) TickWithHistory(ctx context.Context) ([]codexclient.Turn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var history []codexclient.Turn
	c.historyHandoff = &history
	defer func() { c.historyHandoff = nil }()
	if err := c.tick(ctx); err != nil {
		return nil, err
	}
	return history, nil
}

func (c *Controller) tick(ctx context.Context) error {
	if err := c.valid(); err != nil {
		return err
	}
	if c.Native == nil {
		return &Error{errors.New("native connection unavailable")}
	}
	if err := c.Store.StageSubmissions(ctx, c.scope()); err != nil {
		return &Error{err}
	}
	if !c.recovered {
		if err := c.Store.RecoverSubmissionDispatches(ctx, c.scope()); err != nil {
			return &Error{err}
		}
		c.recovered = true
	}
	snap, err := c.snapshot(ctx)
	if err != nil {
		return &Error{err}
	}
	if err = c.reconcile(ctx, snap); err != nil {
		return &Error{err}
	}
	if blocked, err := c.Store.SubmissionBlocked(ctx, c.scope()); err != nil {
		return &Error{err}
	} else if blocked {
		return nil // Cards keep the durable unknown state; polling is read-only.
	}
	// Controls run before ordinary adds. Receipt-bound continue intentions are
	// delayed until their fresh input has actually been accepted by queue/add.
	actions, err := c.Store.SubmissionActions(ctx, c.scope())
	if err != nil {
		return &Error{err}
	}
	initial := map[string]string{}
	for _, a := range actions {
		if a.State != "ready" {
			continue
		}
		if _, inputContinue := store.InputContinueSource(a); inputContinue {
			continue
		}
		if initialSteer(a) {
			initial[a.ReceiptID] = a.ID
			continue
		}
		if err = c.processAction(ctx, a.ID); err != nil {
			return &Error{err}
		}
	}
	// An explicit async answer may steer past ordinary pending inputs. Its
	// idle/rejected queue fallback still belongs at the receipt's FIFO position.
	for _, a := range actions {
		if a.State == "ready" && initialSteer(a) {
			if err = c.processActionWithInitialQueue(ctx, a.ID, false); err != nil {
				return &Error{err}
			}
		}
	}
	inputs, err := c.Store.Submissions(ctx, c.scope())
	if err != nil {
		return &Error{err}
	}
	for _, n := range inputs {
		if n.State == "collecting" {
			break
		}
		if actionID := initial[n.ID]; actionID != "" {
			if err = c.processAction(ctx, actionID); err != nil {
				return &Error{err}
			}
			continue
		}
		if n.State != "received" {
			continue
		}
		claimed, err := c.claimAndDispatch(ctx, n, "", "add")
		if errors.Is(err, store.ErrSubmissionStale) || errors.Is(err, store.ErrSubmissionBusy) {
			continue
		}
		if err != nil {
			return &Error{err}
		}
		if !claimed {
			continue
		}
		// Refresh after each add: an idle add may already have been consumed,
		// and queue ordinals include every App submission as well as ours.
		snap, err = c.snapshot(ctx)
		if err != nil {
			return &Error{err}
		}
		if err = c.reconcile(ctx, snap); err != nil {
			return &Error{err}
		}
	}
	actions, err = c.Store.SubmissionActions(ctx, c.scope())
	if err != nil {
		return &Error{err}
	}
	for _, a := range actions {
		if _, inputContinue := store.InputContinueSource(a); a.State == "ready" && inputContinue {
			if err = c.processAction(ctx, a.ID); err != nil {
				return &Error{err}
			}
		}
	}
	return nil
}

// SubmitInitial is used only by the durable creation workflow, before another
// client can resume/read a not-yet-persisted rollout. It sends queue/add on the
// creating connection, with the ordinary receipt/attempt machinery. Once an
// attempt exists, only native queue/history evidence can resolve an unknown.
func (c *Controller) SubmitInitial(ctx context.Context, receipt string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.valid(); err != nil {
		return err
	}
	if c.Native == nil {
		return &Error{errors.New("native connection unavailable")}
	}
	i, err := c.Store.BridgeCreationThread(ctx, c.HostID, c.AppID, c.ThreadID)
	if err != nil || i.ReceiptID != receipt {
		return store.ErrConflict
	}
	if err = c.Store.StageSubmissions(ctx, c.scope()); err != nil {
		return err
	}
	if !c.recovered {
		if err = c.Store.RecoverSubmissionDispatches(ctx, c.scope()); err != nil {
			return err
		}
		c.recovered = true
	}
	n, err := c.Store.Submission(ctx, c.scope(), receipt)
	if err != nil {
		return err
	}
	if n.State == "collecting" || n.State == "failed" {
		return nil
	}
	if n.State == "received" {
		stage, err := c.Store.InitialCreationAddStage(ctx, c.HostID, c.AppID, c.ThreadID, receipt)
		if err != nil {
			return err
		}
		_, err = c.claimAndDispatch(ctx, n, "", stage)
		return err
	}
	snap, err := c.snapshot(ctx)
	if err != nil {
		return &Error{err}
	}
	return c.reconcile(ctx, snap)
}

type historyInput struct{ TurnID, ItemID string }

type snapshot struct {
	queue   []codexclient.Queued
	byQueue map[string]codexclient.Queued
	ordinal map[string]int
	history map[string]historyInput
	turns   map[string]codexclient.Turn
	order   map[string]int
	active  string
	latest  string
}

func (c *Controller) snapshot(ctx context.Context) (snapshot, error) {
	if _, err := c.Store.SubmissionBinding(ctx, c.scope()); err != nil {
		return snapshot{}, err
	}
	readQueue := c.Native.Queue
	if metadata, ok := c.Native.(codexclient.QueueStateReader); ok {
		readQueue = metadata.QueueState
	}
	queue, err := readQueue(ctx, c.ThreadID)
	if err != nil {
		return snapshot{}, err
	}
	turns, err := c.Native.Turns(ctx, c.ThreadID)
	if err != nil && !codexclient.BeforeFirstInput(err) {
		return snapshot{}, err
	}
	s := snapshot{queue: queue, byQueue: map[string]codexclient.Queued{}, ordinal: map[string]int{}, history: map[string]historyInput{}, turns: map[string]codexclient.Turn{}, order: map[string]int{}}
	queueIDs := map[string]bool{}
	for i, q := range queue {
		if q.ID == "" || queueIDs[q.ID] {
			return snapshot{}, store.ErrConflict
		}
		queueIDs[q.ID] = true
		if q.ClientUserMessageID == nil || *q.ClientUserMessageID == "" {
			continue
		}
		client := *q.ClientUserMessageID
		if _, duplicate := s.byQueue[client]; duplicate {
			return snapshot{}, store.ErrConflict
		}
		s.byQueue[client], s.ordinal[client] = q, i+1
	}
	for index, t := range turns {
		if t.ID == "" || s.turns[t.ID].ID != "" || t.ItemsView != "" && t.ItemsView != "full" {
			return snapshot{}, store.ErrConflict
		}
		switch t.Status {
		case "inProgress":
			if s.active != "" {
				return snapshot{}, store.ErrConflict
			}
			s.active = t.ID
		case "completed", "failed", "interrupted":
		default:
			return snapshot{}, store.ErrConflict
		}
		s.turns[t.ID] = t
		s.order[t.ID] = index
		s.latest = t.ID
		itemIDs := map[string]bool{}
		for _, item := range t.Items {
			if item.ID == "" || itemIDs[item.ID] {
				return snapshot{}, store.ErrConflict
			}
			itemIDs[item.ID] = true
			if item.Type != "userMessage" || item.ClientID == nil || *item.ClientID == "" {
				continue
			}
			if _, duplicate := s.history[*item.ClientID]; duplicate {
				return snapshot{}, store.ErrConflict
			}
			s.history[*item.ClientID] = historyInput{t.ID, item.ID}
		}
	}
	if c.historyHandoff != nil {
		*c.historyHandoff = turns
	}
	return s, nil
}

func (c *Controller) observe(ctx context.Context, n store.Submission, snap snapshot, state, actionID string) (bool, error) {
	var action []string
	if actionID != "" {
		action = []string{actionID}
	}
	if h, ok := snap.history[n.ClientID]; ok {
		if state == "steered" && n.TurnID != "" && h.TurnID != n.TurnID {
			return false, nil
		}
		if state != "steered" {
			state = "consumed"
		}
		err := c.Store.ObserveSubmission(ctx, c.scope(), n.ID, state, n.QueueID, 0, h.TurnID, h.ItemID, action...)
		return true, err
	}
	if q, ok := snap.byQueue[n.ClientID]; ok {
		if state == "steered" {
			return false, nil
		}
		err := c.Store.ObserveSubmission(ctx, c.scope(), n.ID, "queued", q.ID, snap.ordinal[n.ClientID], "", "", action...)
		return true, err
	}
	return false, nil
}

func (c *Controller) reconcile(ctx context.Context, snap snapshot) error {
	inputs, err := c.Store.Submissions(ctx, c.scope())
	if err != nil {
		return err
	}
	actions, err := c.Store.SubmissionActions(ctx, c.scope())
	if err != nil {
		return err
	}
	attempts, err := c.Store.SubmissionAttempts(ctx, c.scope())
	if err != nil {
		return err
	}
	active := map[string]store.SubmissionAction{}
	unknownAdds := map[string]store.SubmissionAttempt{}
	unknownActions := map[string]store.SubmissionAttempt{}
	for _, a := range actions {
		if _, inputContinue := store.InputContinueSource(a); inputContinue {
			continue // This control does not hide evidence for its ordinary add.
		}
		if a.ReceiptID != "" && (a.State == "ready" || a.State == "running" || a.State == "unknown") {
			active[a.ReceiptID] = a
		}
	}
	for _, a := range attempts {
		if a.State != "unknown" {
			continue
		}
		if a.ActionID == "" && store.IsQueueAddStage(a.Stage) {
			unknownAdds[a.ReceiptID] = a
		} else if a.ActionID != "" {
			unknownActions[a.ActionID] = a
		}
	}
	for _, n := range inputs {
		if n.State == "removed" || n.State == "failed" {
			continue
		}
		if op, ok := active[n.ID]; ok {
			if op.State != "unknown" {
				continue
			}
			a, ok := unknownActions[op.ID]
			if !ok {
				continue
			}
			switch a.Stage {
			case "steer":
				h, ok := snap.history[n.ClientID]
				if !ok || h.TurnID != a.ExpectedTurnID {
					continue // Exact correlation in another turn proves no steer.
				}
				if err = c.Store.FinishSubmissionAttempt(ctx, c.scope(), a.ID, "confirmed", "", -1); err != nil {
					return err
				}
				n.TurnID = h.TurnID
				_, err = c.observe(ctx, n, snap, "steered", op.ID)
			case "restore":
				err = c.confirmAddEvidence(ctx, n, snap, a, op.ID)
			case "delete":
				if _, found := snap.history[n.ClientID]; !found {
					continue // Neither disappearance nor continued presence permits steer.
				}
				if _, err = c.observe(ctx, n, snap, "consumed", op.ID); err == nil {
					err = c.Store.ResolveSubmissionAction(ctx, c.scope(), op.ID, "rejected", "consumed")
				}
			}
			if err != nil && !errors.Is(err, store.ErrSubmissionStale) {
				return err
			}
			continue
		}
		if a, ok := unknownAdds[n.ID]; ok {
			if err = c.confirmAddEvidence(ctx, n, snap, a, ""); err != nil && !errors.Is(err, store.ErrSubmissionStale) {
				return err
			}
			continue
		}
		state := "consumed"
		if n.State == "steered" {
			state = "steered"
		}
		found, err := c.observe(ctx, n, snap, state, "")
		if err != nil && !errors.Is(err, store.ErrSubmissionStale) {
			return err
		}
		if !found && (n.State == "queued" || n.State == "accepted") {
			// A known accepted input may be between queue consumption and history
			// publication. It cannot be declared removed from an absent queue.
			err = c.Store.ObserveSubmission(ctx, c.scope(), n.ID, "accepted", n.QueueID, 0, "", "")
			if err != nil && !errors.Is(err, store.ErrSubmissionStale) {
				return err
			}
		}
	}
	for _, op := range actions {
		if op.Kind != "stop" || op.State != "unknown" {
			continue
		}
		t, found := snap.turns[op.ExpectedTurnID]
		if !found || t.Status == "inProgress" {
			continue
		}
		if t.Status == "interrupted" {
			a, found := unknownActions[op.ID]
			if found {
				err = c.Store.FinishSubmissionAttempt(ctx, c.scope(), a.ID, "confirmed", "", -1)
			}
		} else {
			err = c.Store.ResolveSubmissionAction(ctx, c.scope(), op.ID, "rejected", "ended")
		}
		if err != nil {
			return err
		}
	}
	var retries []string
	for _, op := range actions {
		if a, ok := unknownActions[op.ID]; op.Kind == "retry" && op.State == "unknown" && ok {
			retries = append(retries, a.ID)
		}
	}
	if len(retries) > 0 && c.RetryEvidence != nil {
		proof, err := c.RetryEvidence(ctx, c.ThreadID, retries)
		if err != nil {
			return err
		}
		for _, op := range actions {
			a, ok := unknownActions[op.ID]
			if !ok || op.Kind != "retry" || op.State != "unknown" {
				continue
			}
			turn := proof[a.ID]
			if turn == "" || turn == op.ExpectedTurnID || snap.turns[turn].ID == "" || snap.turns[op.ExpectedTurnID].ID == "" || snap.order[turn] <= snap.order[op.ExpectedTurnID] {
				continue
			}
			if err = c.Store.FinishSubmissionAttempt(ctx, c.scope(), a.ID, "confirmed", turn, -1); err != nil {
				return err
			}
		}
	}
	// An active App turn alone cannot prove an unknown queue start or retry.
	// Only the retry's exact persisted nonce can settle its outcome above.
	return nil
}

func (c *Controller) confirmAddEvidence(ctx context.Context, n store.Submission, snap snapshot, a store.SubmissionAttempt, actionID string) error {
	if _, found := snap.history[n.ClientID]; found {
		if _, err := c.observe(ctx, n, snap, "consumed", actionID); err != nil {
			return err
		}
		return c.Store.FinishSubmissionAttempt(ctx, c.scope(), a.ID, "confirmed", "", -1)
	}
	if q, found := snap.byQueue[n.ClientID]; found {
		if err := c.Store.FinishSubmissionAttempt(ctx, c.scope(), a.ID, "confirmed", q.ID, -1); err != nil {
			return err
		}
		_, err := c.observe(ctx, n, snap, "queued", actionID)
		return err
	}
	return nil
}

func initialSteer(a store.SubmissionAction) bool {
	return a.Kind == "steer" && a.QueueID == ""
}

func (c *Controller) processAction(ctx context.Context, actionID string) error {
	return c.processActionWithInitialQueue(ctx, actionID, true)
}

func (c *Controller) processActionWithInitialQueue(ctx context.Context, actionID string, allowInitialQueue bool) error {
	for step := 0; step < 5; step++ {
		op, err := c.Store.SubmissionAction(ctx, c.scope(), actionID)
		if err != nil || op.State != "ready" {
			return err
		}
		if _, inputContinue := store.InputContinueSource(op); inputContinue {
			ready, err := c.Store.PrepareInputContinue(ctx, c.scope(), op.ID)
			if err != nil || !ready {
				return err
			}
			// The confirmed source stop may have frozen its target after ingress.
			op, err = c.Store.SubmissionAction(ctx, c.scope(), op.ID)
			if err != nil {
				return err
			}
		}
		if initialSteer(op) && op.Stage == "restore" && !allowInitialQueue {
			return nil
		}
		snap, err := c.snapshot(ctx)
		if err != nil {
			return err
		}
		var n store.Submission
		if op.ReceiptID != "" {
			inputs, err := c.Store.Submissions(ctx, c.scope())
			if err != nil {
				return err
			}
			for _, v := range inputs {
				if v.ID == op.ReceiptID {
					n = v
					break
				}
			}
			if n.ID == "" {
				return store.ErrConflict
			}
		}
		if op.Stage == "prepare" {
			switch op.Kind {
			case "steer", "remove":
				if found, err := c.observeHistory(ctx, n, snap, op.ID); err != nil {
					return err
				} else if found {
					return c.Store.ResolveSubmissionAction(ctx, c.scope(), op.ID, "rejected", "consumed")
				}
				if initialSteer(op) {
					if _, found := snap.byQueue[n.ClientID]; found {
						if _, err = c.observe(ctx, n, snap, "queued", op.ID); err != nil {
							return err
						}
						return c.Store.ResolveSubmissionAction(ctx, c.scope(), op.ID, "rejected", "retained")
					}
					stage := "steer"
					if snap.active == "" {
						stage = "restore"
					}
					err = c.Store.PrepareSubmissionAction(ctx, c.scope(), op.ID, stage, snap.active, "", n.ClientID)
					break
				}
				q, found := snap.byQueue[n.ClientID]
				if !found || q.ID != op.QueueID {
					return c.Store.ResolveSubmissionAction(ctx, c.scope(), op.ID, "rejected", "changed")
				}
				if op.Kind == "steer" && snap.active == "" {
					if _, err = c.observe(ctx, n, snap, "queued", op.ID); err != nil {
						return err
					}
					return c.Store.ResolveSubmissionAction(ctx, c.scope(), op.ID, "rejected", "idle")
				}
				err = c.Store.PrepareSubmissionAction(ctx, c.scope(), op.ID, "delete", snap.active, op.QueueID, n.ClientID)
			case "stop":
				if op.ExpectedTurnID != "" && snap.active != op.ExpectedTurnID {
					result := "changed"
					if target, found := snap.turns[op.ExpectedTurnID]; found && target.Status != "inProgress" {
						result = "ended"
					}
					return c.Store.ResolveSubmissionAction(ctx, c.scope(), op.ID, "rejected", result)
				}
				if snap.active == "" {
					return c.Store.ResolveSubmissionAction(ctx, c.scope(), op.ID, "rejected", "idle")
				}
				err = c.Store.PrepareSubmissionAction(ctx, c.scope(), op.ID, "interrupt", snap.active, "", "")
			case "continue":
				if op.ExpectedTurnID != "" && !snap.canContinue(op.ExpectedTurnID) {
					return c.Store.ResolveSubmissionAction(ctx, c.scope(), op.ID, "rejected", "changed")
				}
				if snap.active != "" {
					return c.Store.ResolveSubmissionAction(ctx, c.scope(), op.ID, "rejected", "busy")
				}
				if len(snap.queue) == 0 {
					return c.Store.ResolveSubmissionAction(ctx, c.scope(), op.ID, "rejected", "empty")
				}
				client := ""
				if snap.queue[0].ClientUserMessageID != nil {
					client = *snap.queue[0].ClientUserMessageID
				}
				err = c.Store.PrepareSubmissionAction(ctx, c.scope(), op.ID, "start", op.ExpectedTurnID, snap.queue[0].ID, client)
			case "retry":
				if _, ok := c.Native.(RetryNative); !ok || !snap.canRetry(op.ExpectedTurnID) {
					return c.Store.ResolveSubmissionAction(ctx, c.scope(), op.ID, "rejected", "changed")
				}
				err = c.Store.PrepareSubmissionAction(ctx, c.scope(), op.ID, "retry", op.ExpectedTurnID, "", "")
			}
			if errors.Is(err, store.ErrSubmissionStale) {
				return nil
			}
			if err != nil {
				return err
			}
			continue
		}
		if op.Kind == "continue" && op.ExpectedTurnID != "" && !snap.canContinue(op.ExpectedTurnID) {
			return c.Store.ResolveSubmissionAction(ctx, c.scope(), op.ID, "rejected", "changed")
		}
		if op.Kind == "retry" && !snap.canRetry(op.ExpectedTurnID) {
			return c.Store.ResolveSubmissionAction(ctx, c.scope(), op.ID, "rejected", "changed")
		}
		if op.Stage == "steer" || op.Stage == "restore" {
			// Even after a known delete, App may have consumed/reintroduced this
			// input. Never inject or restore when exact evidence already exists.
			if found, err := c.observe(ctx, n, snap, "consumed", op.ID); err != nil {
				return err
			} else if found {
				result := "retained"
				if _, ok := snap.history[n.ClientID]; ok {
					result = "consumed"
				}
				return c.Store.ResolveSubmissionAction(ctx, c.scope(), op.ID, "rejected", result)
			}
		}
		claimed, err := c.claimAndDispatch(ctx, n, op.ID, op.Stage)
		if errors.Is(err, store.ErrSubmissionStale) || errors.Is(err, store.ErrSubmissionBusy) {
			return nil
		}
		if err != nil || !claimed {
			return err
		}
		fresh, err := c.snapshot(ctx)
		if err != nil {
			return err
		}
		if err = c.reconcile(ctx, fresh); err != nil {
			return err
		}
	}
	return store.ErrConflict
}

func (s snapshot) canContinue(turn string) bool {
	return s.active == "" && s.latest == turn && s.turns[turn].Status == "interrupted"
}

func (s snapshot) canRetry(turn string) bool {
	return turn != "" && s.active == "" && s.latest == turn && len(s.queue) == 0 && s.turns[turn].RetryableFailure()
}

func (c *Controller) observeHistory(ctx context.Context, n store.Submission, snap snapshot, actionID string) (bool, error) {
	if _, found := snap.history[n.ClientID]; !found {
		return false, nil
	}
	return c.observe(ctx, n, snap, "consumed", actionID)
}

func (c *Controller) dispatch(ctx context.Context, a store.SubmissionAttempt, n store.Submission) error {
	// Recheck the confirmed binding after journaling and immediately before the
	// RPC. A failed check leaves dispatching for conservative startup recovery.
	if _, err := c.Store.SubmissionBinding(ctx, c.scope()); err != nil {
		c.recovered = false
		return err
	}
	// The lease can be revoked after the binding check. Keep the durable
	// dispatching attempt for conservative recovery, but never issue the RPC.
	if err := ctx.Err(); err != nil {
		c.recovered = false
		return err
	}
	var err error
	remoteID, deleted := "", -1
	stage := a.Stage
	if store.IsQueueAddStage(stage) {
		stage = "add"
	}
	if c.historyHandoff != nil {
		*c.historyHandoff = nil
	}
	switch stage {
	case "add", "restore":
		var q codexclient.Queued
		if n.InputJSON != "" {
			inputNative, ok := c.Native.(InputNative)
			if !ok {
				return errInputNativeUnsupported
			}
			q, err = inputNative.AddInput(ctx, c.ThreadID, json.RawMessage(n.InputJSON), a.ClientID)
		} else {
			q, err = c.Native.Add(ctx, c.ThreadID, n.Body, a.ClientID)
		}
		remoteID = q.ID
		if err == nil && remoteID == "" {
			err = errors.New("native add response lacks queue identity")
		}
	case "delete":
		var removed bool
		removed, err = c.Native.Delete(ctx, c.ThreadID, a.QueueID)
		if err == nil {
			deleted = 0
			if removed {
				deleted = 1
			}
		}
	case "steer":
		if n.InputJSON != "" {
			inputNative, ok := c.Native.(InputNative)
			if !ok {
				return errInputNativeUnsupported
			}
			err = inputNative.SteerInput(ctx, c.ThreadID, a.ExpectedTurnID, json.RawMessage(n.InputJSON), a.ClientID)
		} else {
			err = c.Native.Steer(ctx, c.ThreadID, a.ExpectedTurnID, n.Body, a.ClientID)
		}
	case "interrupt":
		err = c.Native.Interrupt(ctx, c.ThreadID, a.ExpectedTurnID)
	case "start":
		err = c.Native.StartQueue(ctx, c.ThreadID)
	case "retry":
		retry, ok := c.Native.(RetryNative)
		if !ok {
			return store.ErrConflict
		}
		var turn codexclient.Turn
		turn, err = retry.Retry(ctx, c.ThreadID, a.ID)
		remoteID = turn.ID
		if err == nil && (remoteID == "" || remoteID == a.ExpectedTurnID) {
			err = &codexclient.UnknownError{Method: "turn/start"}
		}
		if err == nil && c.RetryEvidence != nil {
			proof, proofErr := c.RetryEvidence(ctx, c.ThreadID, []string{a.ID})
			if proofErr != nil || proof[a.ID] != remoteID {
				err = &codexclient.UnknownError{Method: "turn/start", Cause: proofErr}
			}
		}
	default:
		return store.ErrConflict
	}
	outcome := "confirmed"
	if err != nil {
		outcome = "unknown"
		var rejected *codexclient.RPCError
		if errors.As(err, &rejected) {
			outcome = "rejected"
		}
	}
	if saveErr := c.Store.FinishSubmissionAttempt(ctx, c.scope(), a.ID, outcome, remoteID, deleted); saveErr != nil {
		c.recovered = false
		return saveErr
	}
	if outcome == "unknown" {
		return err
	}
	return nil
}

// validateInput runs before ClaimSubmissionAttempt so malformed or unsupported
// stored multipart inputs do not create a durable dispatch journal or reach the
// native connection. Legacy receipts with no InputJSON retain the old path.
func (c *Controller) validateInput(n store.Submission, stage string) error {
	if n.InputJSON == "" || !store.IsQueueAddStage(stage) && stage != "restore" && stage != "steer" {
		return nil
	}
	if _, ok := c.Native.(InputNative); !ok {
		return errInputNativeUnsupported
	}
	return codexclient.ValidateInput(json.RawMessage(n.InputJSON))
}
