package app

import (
	"context"
	"database/sql"
	"errors"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
)

// Bound routes retain their identity while a missing cwd releases the actor.
// A native event, changed list metadata, or new durable Feishu input may wake
// them. A reconnect rechecks once; no filesystem polling is retained per route.
type workspaceSuspension struct {
	cwd, status   string
	updatedAt     int64
	inputSequence int64
}

func (r *Runtime) suspendMissingWorkspace(ctx context.Context, a *threadActor, meta codexclient.Thread, inputSequence int64) bool {
	if ctx.Err() != nil {
		return false
	}
	if meta.ID != a.thread {
		var err error
		meta, err = r.Native.Metadata(ctx, a.thread)
		if err != nil || meta.ID != a.thread {
			return false
		}
	}
	if _, err := rules.CanonicalDirectory(meta.CWD); !errors.Is(err, rules.ErrDirectoryMissing) {
		return false
	}
	if _, err := r.Store.Binding(ctx, r.HostID, a.thread); errors.Is(err, sql.ErrNoRows) {
		if err := r.Store.SuspendCandidate(ctx, r.HostID, a.thread); err != nil {
			return false
		}
	} else if err != nil {
		return false
	} else {
		scope := store.SubmissionScope{HostID: r.HostID, AppID: r.AppID, ThreadID: a.thread}
		blocked, err := r.Store.SubmissionBlocked(ctx, scope)
		if err != nil || blocked {
			return false // Native writes still need their original readback.
		}
		attempts, err := r.Store.WorktreeRestoreAttempts(ctx, scope)
		if err != nil {
			return false
		}
		for _, attempt := range attempts {
			if attempt.State == "prepared" || attempt.State == "dispatching" || attempt.State == "unknown" {
				return false // Continue an already journaled restore, never replace it.
			}
		}
		r.mu.Lock()
		if r.suspended == nil {
			r.suspended = map[string]workspaceSuspension{}
		}
		r.suspended[a.thread] = workspaceSuspension{cwd: meta.CWD, status: meta.Status.Type, updatedAt: meta.UpdatedAt, inputSequence: inputSequence}
		r.mu.Unlock()
	}
	r.notice("thread.cwd_suspended")
	return true
}
