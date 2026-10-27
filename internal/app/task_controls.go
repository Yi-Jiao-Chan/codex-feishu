package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
	"codex-feishu/internal/submissions"
	messagesync "codex-feishu/internal/sync"
)

// TaskControls prepares actor-owned render state. Callbacks use a fresh instance
// and persisted authorization, so they never share the actor's mutable cache.
type TaskControls struct {
	Store         *store.Store
	HostID, AppID string
	Waiting       func(thread, turn string) bool
	InFeishu      bool
	RetryEnabled  bool
	Queue         func(context.Context, string) ([]codexclient.Queued, error)
	Icons         cards.TaskIcons
	cache         map[taskRenderKey]store.TaskControl
}

type taskRenderKey struct{ thread, turn string }

type TaskControlError struct{ Cause error }

func (*TaskControlError) Error() string   { return "task controls await state verification" }
func (e *TaskControlError) Unwrap() error { return e.Cause }

func (t *TaskControls) scope(thread string) store.SubmissionScope {
	return store.SubmissionScope{HostID: t.HostID, AppID: t.AppID, ThreadID: thread}
}

// Prepare accepts complete hydrated history only. Queue reads are optional; an
// unavailable/failed read disables Continue and never invents an App queue count.
func (t *TaskControls) Prepare(ctx context.Context, thread string, turns []codexclient.Turn) error {
	if t.Store == nil {
		return store.ErrConflict
	}
	scope := t.scope(thread)
	old, err := t.Store.TaskControls(ctx, scope)
	if err != nil {
		return &TaskControlError{err}
	}
	turns, active, latest, err := taskFullHistory(turns)
	if err != nil {
		return &TaskControlError{err}
	}
	claims, err := t.Store.TaskControlClaims(ctx, scope)
	if err != nil {
		return &TaskControlError{err}
	}
	for _, claim := range claims {
		if claim.State == "confirmed" || claim.State == "rejected" {
			continue
		}
		if err = t.ensureClaim(ctx, scope, claim); err != nil && !errors.Is(err, store.ErrSubmissionBusy) {
			return &TaskControlError{err}
		}
	}
	claims, err = t.Store.TaskControlClaims(ctx, scope)
	if err != nil {
		return &TaskControlError{err}
	}
	lastClaim := map[string]store.TaskControlClaim{}
	for _, c := range claims {
		lastClaim[c.TurnID] = c
	}
	selected := map[string]bool{}
	for _, v := range old {
		selected[v.TurnID] = true
	}
	mirrors, err := t.Store.Mirrors(ctx, t.HostID, thread)
	if err != nil {
		return &TaskControlError{err}
	}
	for _, m := range mirrors {
		if m.Kind == "progress" && !m.Retired {
			selected[m.TurnID] = true
		}
	}
	queueCount := -1
	if active == "" && latest != "" && (turns[len(turns)-1].Status == "interrupted" || t.RetryEnabled && turns[len(turns)-1].RetryableFailure()) && t.Queue != nil {
		queue, readErr := t.Queue(ctx, thread)
		if readErr == nil && taskQueueComplete(queue) {
			queueCount = len(queue)
		}
	}
	blocked := false
	if _, err = t.Store.SubmissionBinding(ctx, scope); err == nil {
		blocked, err = t.Store.SubmissionBlocked(ctx, scope)
		if err != nil {
			return &TaskControlError{err}
		}
	}
	seen := map[string]bool{}
	var views []store.TaskControl
	for _, turn := range turns {
		seen[turn.ID] = true
		if turn.Status != "inProgress" && !((turn.Status == "interrupted" || t.RetryEnabled && turn.RetryableFailure()) && turn.ID == latest && active == "") && !selected[turn.ID] {
			continue
		}
		v := store.TaskControl{ThreadID: thread, TurnID: turn.ID, NativeStatus: turn.Status, State: "unavailable", QueueCount: -1, Fingerprint: taskFingerprint(turn)}
		if turn.Status == "inProgress" {
			v.Kind, v.State = "stop", "available"
			v.Waiting = t.Waiting != nil && t.Waiting(thread, turn.ID)
		} else if turn.Status == "interrupted" && turn.ID == latest && active == "" {
			v.QueueCount = queueCount
			if queueCount > 0 {
				v.Kind, v.State = "continue", "available"
			}
		} else if t.RetryEnabled && turn.RetryableFailure() && turn.ID == latest && active == "" {
			v.Kind, v.QueueCount, v.Reason = "retry", queueCount, turn.RetryReason()
			if queueCount == 0 {
				v.State = "available"
			}
		}
		if c, ok := lastClaim[turn.ID]; ok {
			if c.State == "ready" || c.State == "pending" {
				v.Kind, v.State, v.Reason = c.Kind, "pending", "processing"
			} else if c.State == "unknown" {
				v.Kind, v.State, v.Reason = c.Kind, "unknown", "verifying"
			} else if c.State == "confirmed" && (c.Kind == "stop" && turn.Status == "inProgress" || c.Kind == "continue" && v.Kind == "continue") {
				v.Kind, v.State, v.Reason = c.Kind, "pending", "acknowledged"
			} else if c.State == "confirmed" && c.Kind == "retry" {
				v.Kind, v.State, v.Reason = "retry", "unavailable", "retried"
			}
		}
		if blocked && v.State == "available" {
			v.State, v.Reason = "unknown", "verifying"
		}
		views = append(views, v)
	}
	for _, v := range old {
		if !seen[v.TurnID] {
			v.Kind, v.State, v.Reason, v.QueueCount, v.Waiting = "", "unknown", "verifying", -1, false
			views = append(views, v)
		}
	}
	if err = t.Store.StageTaskControls(ctx, scope, views); err != nil {
		return &TaskControlError{err}
	}
	prepared, err := t.Store.TaskControls(ctx, scope)
	if err != nil {
		return &TaskControlError{err}
	}
	t.cache = make(map[taskRenderKey]store.TaskControl, len(prepared))
	for _, v := range prepared {
		t.cache[taskRenderKey{thread, v.TurnID}] = v
	}
	return nil
}

func (t *TaskControls) RenderFinal(thread, turn, status, card string) string {
	v, ok := t.cache[taskRenderKey{thread, turn}]
	if !ok || status != "failed" || v.NativeStatus != "failed" || v.Kind != "retry" {
		return card
	}
	state := v.State
	if v.Reason == "retried" {
		state = "confirmed"
	}
	return cards.WithRetryControl(card, v.Token, state, v.Revision)
}

// Render is pure: it reads only the cache populated by this actor's Prepare.
// An empty interrupted render suppresses creation of unrelated historical cards.
func (t *TaskControls) Render(thread, turn, status string, progress cards.ProgressView) string {
	v, ok := t.cache[taskRenderKey{thread, turn}]
	if !ok {
		if status == "interrupted" {
			return ""
		}
		return cards.Task(cards.TaskView{Status: status, Progress: progress, State: "unavailable", QueueCount: -1, Icons: t.Icons})
	}
	if v.NativeStatus != status {
		v.Kind, v.State, v.Reason = "", "unknown", "verifying"
	}
	return cards.Task(cards.TaskView{Status: status, Progress: progress, Token: v.Token, Kind: v.Kind, State: v.State, Reason: v.Reason, Revision: v.Revision, QueueCount: v.QueueCount, Waiting: v.Waiting, WaitingInFeishu: t.InFeishu, Icons: t.Icons})
}

// Claim is local-only. The Receiver authenticates the operator first; the store
// checks binding, real progress-card message, token, action and card revision.
func (t *TaskControls) Claim(ctx context.Context, chat, message, kind, token string, revision int64) (string, error) {
	if t.Store == nil {
		return "", store.ErrConflict
	}
	c, err := t.Store.ClaimTaskControl(ctx, t.HostID, t.AppID, chat, message, kind, token, revision)
	if err != nil {
		return "", taskClaimError(err)
	}
	return c.ThreadID, taskClaimError(t.ensureClaim(ctx, t.scope(c.ThreadID), c))
}

func taskClaimError(err error) error {
	if err == nil || errors.Is(err, store.ErrSubmissionStale) || errors.Is(err, store.ErrSubmissionBusy) || errors.Is(err, store.ErrConflict) {
		return err
	}
	return &TaskControlError{err}
}

func (t *TaskControls) ensureClaim(ctx context.Context, scope store.SubmissionScope, claim store.TaskControlClaim) error {
	var a store.SubmissionAction
	var err error
	if claim.NativeActionID != "" {
		a, err = t.Store.SubmissionAction(ctx, scope, claim.NativeActionID)
	} else {
		c := &submissions.Controller{Store: t.Store, HostID: scope.HostID, AppID: scope.AppID, ThreadID: scope.ThreadID}
		if claim.Kind == "stop" {
			a, err = c.StopExpected(ctx, claim.ExternalID, claim.TurnID)
		} else if claim.Kind == "continue" {
			a, err = c.ContinueExpected(ctx, claim.ExternalID, claim.TurnID)
		} else if claim.Kind == "retry" {
			a, err = c.RetryExpected(ctx, claim.ExternalID, claim.TurnID)
		} else {
			return store.ErrConflict
		}
	}
	if err != nil {
		return err
	}
	return t.Store.FinishTaskControlClaim(ctx, scope, claim, a)
}

func taskFullHistory(turns []codexclient.Turn) ([]codexclient.Turn, string, string, error) {
	result := append([]codexclient.Turn(nil), turns...)
	seen := map[string]bool{}
	active, latest := "", ""
	for i, turn := range result {
		if turn.ID == "" || seen[turn.ID] || turn.ItemsView != "" && turn.ItemsView != "full" {
			return nil, "", "", store.ErrConflict
		}
		seen[turn.ID], latest = true, turn.ID
		switch turn.Status {
		case "inProgress":
			if active != "" {
				return nil, "", "", store.ErrConflict
			}
			active = turn.ID
		case "completed", "failed", "interrupted":
		default:
			return nil, "", "", store.ErrConflict
		}
		items := map[string]codexclient.Item{}
		result[i].Items = nil
		for _, item := range turn.Items {
			if item.ID == "" {
				return nil, "", "", store.ErrConflict
			}
			if old, duplicate := items[item.ID]; duplicate {
				if !reflect.DeepEqual(old, item) {
					return nil, "", "", store.ErrConflict
				}
				continue
			}
			items[item.ID] = item
			result[i].Items = append(result[i].Items, item)
		}
	}
	return result, active, latest, nil
}

func taskFingerprint(turn codexclient.Turn) string {
	// Persist a digest, never tool/request/input bodies. Any public projection
	// change invalidates old callbacks even when queue/control state is unchanged.
	var public []any
	for _, item := range turn.Items {
		if item.Type == "agentMessage" && (item.Phase == nil || *item.Phase == "commentary" || *item.Phase == "partial_answer") {
			public = append(public, item)
		} else if item.Type == "agentMessage" || item.Type == "userMessage" {
			// These messages split adjacent tool groups, even though their
			// private/final text is not part of the progress presentation.
			public = append(public, codexclient.Item{Type: item.Type, ID: item.ID, Phase: item.Phase})
		} else if tool, ok := messagesync.ProgressToolSummary(item); ok {
			// Fingerprint exactly the visible, safe tool projection. Raw command
			// arguments and private file diffs never enter this digest input.
			tool.Details = nil
			public = append(public, tool)
		}
	}
	if reason := turn.RetryReason(); turn.Status == "failed" && reason != "" {
		public = append(public, reason)
	}
	encoded, _ := json.Marshal(public)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func taskQueueComplete(queue []codexclient.Queued) bool {
	seen := map[string]bool{}
	for _, q := range queue {
		if q.ID == "" || seen[q.ID] {
			return false
		}
		seen[q.ID] = true
	}
	return true
}
