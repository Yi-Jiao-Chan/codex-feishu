package app

import (
	"encoding/json"
	"sync"

	"codex-feishu/internal/codexclient"
)

type pendingAppRequest struct{ thread, turn string }

// RequestTracker contains only routing identity, never request commands,
// questions, decisions or answers. One instance belongs to one native
// generation; resume replays pending requests after a reconnect.
type RequestTracker struct {
	mu       sync.RWMutex
	requests map[string]pendingAppRequest
}

func (t *RequestTracker) Observe(event codexclient.Event) {
	var p struct {
		ThreadID  string          `json:"threadId"`
		TurnID    string          `json:"turnId"`
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(event.Params, &p) != nil || p.ThreadID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if event.Method == "serverRequest/resolved" {
		key := appRequestKey(event.Generation, p.RequestID)
		if old, ok := t.requests[key]; ok && old.thread == p.ThreadID {
			delete(t.requests, key)
		}
		return
	}
	switch event.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval", "item/tool/requestUserInput":
	default:
		return
	}
	key := appRequestKey(event.Generation, event.RequestID)
	if key == "" || p.TurnID == "" {
		return
	}
	if t.requests == nil {
		t.requests = map[string]pendingAppRequest{}
	}
	t.requests[key] = pendingAppRequest{p.ThreadID, p.TurnID}
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

// Explicit terminal history clears stale requests even if a resolved
// notification was lost. Ordinary agent output does not resolve approvals.
func (t *RequestTracker) Reconcile(thread string, turns []codexclient.Turn) {
	terminal := map[string]bool{}
	for _, turn := range turns {
		if turn.Status != "inProgress" {
			terminal[turn.ID] = true
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for key, r := range t.requests {
		if r.thread == thread && terminal[r.turn] {
			delete(t.requests, key)
		}
	}
}
