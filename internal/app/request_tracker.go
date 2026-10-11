package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
	messages "codex-feishu/internal/sync"
)

type pendingAppRequest struct {
	thread, turn, key, method string
	observedMS, eventAt       int64
	persisted, replay         bool
}

// RequestTracker owns no native callbacks. Only routing identities are retained;
// the App remains the place to answer approvals and questions. One tracker lives
// for one native generation, while durable IDs exclude connection generation.
type RequestTracker struct {
	Store         *store.Store
	HostID, AppID string
	Options       store.MentionOptions
	Gateway       messages.MessageGateway
	Membership    messages.MembershipGateway
	Now           func() time.Time
	// A thread/started projection for an older thread is not a birth proof.
	GenerationStartedAtMS int64
	mu                    sync.RWMutex
	requests              map[string]pendingAppRequest
	hydrated              map[string]int64
	births                map[string]int64
	birthTurn             map[string]string
	started               map[string]map[string]int64
}

func (t *RequestTracker) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}
func requestRoutingKey(turn string, raw json.RawMessage) string {
	id := appRequestKey(0, raw)
	if id == "" {
		return ""
	}
	b, _ := json.Marshal([]string{turn, id})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func (t *RequestTracker) Observe(event codexclient.Event) {
	_ = t.ObserveContext(context.Background(), event)
}

// Production uses the context-bearing variant to expose local journal failure.
func (t *RequestTracker) ObserveContext(ctx context.Context, event codexclient.Event) error {
	var p struct {
		ThreadID  string          `json:"threadId"`
		TurnID    string          `json:"turnId"`
		RequestID json.RawMessage `json:"requestId"`
		Thread    struct {
			ID        string `json:"id"`
			CreatedAt int64  `json:"createdAt"`
		} `json:"thread"`
		Turn struct {
			ID        string `json:"id"`
			StartedAt *int64 `json:"startedAt"`
		} `json:"turn"`
	}
	if json.Unmarshal(event.Params, &p) != nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if event.Method == "thread/started" {
		if p.Thread.ID != "" && p.Thread.CreatedAt > 0 && p.Thread.CreatedAt <= t.now().Unix() && (t.Store == nil || t.GenerationStartedAtMS > 0 && p.Thread.CreatedAt > t.GenerationStartedAtMS/1000) {
			if t.births == nil {
				t.births = map[string]int64{}
			}
			if _, ok := t.births[p.Thread.ID]; !ok {
				t.births[p.Thread.ID] = p.Thread.CreatedAt
			}
		}
		return nil
	}
	if p.ThreadID == "" {
		return nil
	}
	if event.Method == "turn/started" {
		if p.Turn.ID != "" && p.Turn.StartedAt != nil && *p.Turn.StartedAt > 0 && *p.Turn.StartedAt <= t.now().Unix() {
			t.rememberTurn(p.ThreadID, p.Turn.ID, *p.Turn.StartedAt)
		}
		return nil
	}
	if event.Method == "serverRequest/resolved" {
		key := appRequestKey(event.Generation, p.RequestID)
		if old, ok := t.requests[key]; ok && old.thread == p.ThreadID {
			if t.Store != nil {
				if err := t.Store.ResolveAppRequest(ctx, t.HostID, t.AppID, old.thread, old.key); err != nil {
					return err
				}
			}
			delete(t.requests, key)
		}
		return nil
	}
	switch event.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval", "item/tool/requestUserInput":
	default:
		return nil
	}
	key := appRequestKey(event.Generation, event.RequestID)
	if key == "" || p.TurnID == "" {
		return nil
	}
	if t.requests == nil {
		t.requests = map[string]pendingAppRequest{}
	}
	r, ok := t.requests[key]
	if ok && (r.thread != p.ThreadID || r.turn != p.TurnID || r.method != event.Method) {
		return store.ErrConflict
	}
	if !ok {
		r = pendingAppRequest{thread: p.ThreadID, turn: p.TurnID, key: requestRoutingKey(p.TurnID, event.RequestID), method: event.Method, observedMS: t.now().UnixMilli(), replay: true}
		t.requests[key] = r
	}
	if t.hydrated[p.ThreadID] != 0 {
		return t.persist(ctx, key, r, t.birthTurn[r.thread] == r.turn)
	}
	return nil
}
func (t *RequestTracker) rememberTurn(thread, turn string, at int64) {
	if t.started == nil {
		t.started = map[string]map[string]int64{}
	}
	if t.started[thread] == nil {
		t.started[thread] = map[string]int64{}
	}
	if _, ok := t.started[thread][turn]; !ok {
		t.started[thread][turn] = at
	}
}
func (t *RequestTracker) persist(ctx context.Context, mapKey string, r pendingAppRequest, newBirth bool) error {
	if t.Store == nil || r.persisted {
		return nil
	}
	start := t.started[r.thread][r.turn]
	// Strict seconds avoid granting events in an ambiguous hydration second.
	live := start > t.hydrated[r.thread]/1000 && t.hydrated[r.thread] > 0 && start <= r.observedMS/1000
	if newBirth && start >= t.births[r.thread] && start > 0 && start <= r.observedMS/1000 {
		live = true
	}
	if live {
		r.replay = false
		r.eventAt = r.observedMS / 1000
	}
	err := t.Store.ObserveAppRequest(ctx, t.HostID, t.AppID, store.PendingRequest{Key: r.key, ThreadID: r.thread, TurnID: r.turn, Method: r.method, EventAt: r.eventAt, Replay: r.replay}, t.Options, r.observedMS)
	if errors.Is(err, store.ErrIneligible) || errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	r.persisted = true
	t.requests[mapKey] = r
	return nil
}

// Hydrated runs after authorized complete history commits. A native birth in
// this generation also requires durable new/birth proof to admit its first turn.
func (t *RequestTracker) Hydrated(ctx context.Context, thread string, turns []codexclient.Turn) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.hydrated == nil {
		t.hydrated = map[string]int64{}
	}
	if t.hydrated[thread] == 0 {
		t.hydrated[thread] = t.now().UnixMilli()
	}
	for _, turn := range turns {
		if turn.StartedAt != nil && *turn.StartedAt > 0 && *turn.StartedAt <= t.now().Unix() {
			t.rememberTurn(thread, turn.ID, *turn.StartedAt)
		}
	}
	newBirth := false
	if t.Store != nil && t.births[thread] > 0 {
		o, err := t.Store.Observation(ctx, t.HostID, thread)
		if err != nil {
			return err
		}
		newBirth = o.Origin == "new" && o.Proof == "birth" && o.State == "eligible" && o.CreatedAt == t.births[thread]
	}
	if newBirth && len(turns) > 0 {
		if t.birthTurn == nil {
			t.birthTurn = map[string]string{}
		}
		if t.birthTurn[thread] == "" {
			t.birthTurn[thread] = turns[0].ID
		}
	}
	for key, r := range t.requests {
		if r.thread == thread {
			if err := t.persist(ctx, key, r, newBirth && t.birthTurn[thread] == r.turn); err != nil {
				return err
			}
		}
	}
	if t.Store != nil {
		return t.Store.StageRequestReminders(ctx, t.HostID, t.AppID, thread)
	}
	return nil
}
func (t *RequestTracker) Waiting(thread, turn string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, r := range t.requests {
		if r.thread == thread && r.turn == turn {
			return true
		}
	}
	return false
}
func (t *RequestTracker) Reconcile(thread string, turns []codexclient.Turn) {
	_ = t.ReconcileContext(context.Background(), thread, turns)
}
func (t *RequestTracker) ReconcileContext(ctx context.Context, thread string, turns []codexclient.Turn) error {
	terminal := map[string]bool{}
	var terminalIDs []string
	for _, turn := range turns {
		if turn.Status != "inProgress" {
			terminal[turn.ID] = true
			terminalIDs = append(terminalIDs, turn.ID)
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Store != nil {
		if err := t.Store.ReconcileAppRequests(ctx, t.HostID, t.AppID, thread, terminalIDs); err != nil {
			return err
		}
	}
	for key, r := range t.requests {
		if r.thread == thread && terminal[r.turn] {
			delete(t.requests, key)
		}
	}
	return nil
}

// The per-thread actor dispatches after its public outbox. Member proof is read
// outside SQL; absent proof permanently suppresses the reminder.
func (t *RequestTracker) DispatchOne(ctx context.Context, thread string) (bool, error) {
	if t.Store == nil || t.Gateway == nil {
		return false, nil
	}
	preparedAt := t.now().UnixMilli()
	ready, err := t.Store.PrepareRequestRemindersAt(ctx, t.HostID, t.AppID, thread, preparedAt, t.Options)
	if err != nil || !ready {
		return false, err
	}
	options := t.Options
	options.ChatID, options.MemberIDs = "", nil
	b, err := t.Store.Binding(ctx, t.HostID, thread)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if t.Membership != nil {
		members, e := t.Membership.MemberIDs(ctx, b.ChatID)
		if e == nil {
			options.ChatID, options.MemberIDs = b.ChatID, members
		}
	}
	a, claimed, err := t.Store.ClaimRequestReminder(ctx, t.HostID, t.AppID, thread, max(preparedAt, t.now().UnixMilli()), options)
	if err != nil || !claimed {
		return false, err
	}
	remote, sendErr := t.Gateway.ReplyCard(ctx, a.ParentMessageID, a.Card, a.ID)
	var limited *feishu.RateLimited
	if errors.As(sendErr, &limited) {
		at := max(a.CreatedAtMS, t.now().UnixMilli())
		delay := max(int64(1000), limited.RetryAfter.Milliseconds())
		return true, t.Store.DeferRequestReminder(ctx, a.ID, at, delay)
	}
	state := "confirmed"
	if sendErr != nil || remote == "" {
		state = "unknown"
		remote = ""
		var rejected *feishu.Rejected
		if errors.As(sendErr, &rejected) {
			state = "rejected"
		}
	}
	if err := t.Store.FinishRequestReminder(ctx, a.ID, state, remote); err != nil {
		return true, err
	}
	return true, nil
}
