package app

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
)

const (
	defaultActorIdleTimeout        = 5 * time.Minute
	defaultActivationSweepInterval = 15 * time.Second
	defaultActivationRetryInitial  = time.Second
	defaultActivationRetryMax      = 5 * time.Minute
)

func (r *Runtime) actorIdleTimeout() time.Duration {
	if r.ActorIdleTimeout > 0 {
		return r.ActorIdleTimeout
	}
	return defaultActorIdleTimeout
}

func (r *Runtime) activationSweepInterval() time.Duration {
	if r.ActivationSweepInterval > 0 {
		return r.ActivationSweepInterval
	}
	return defaultActivationSweepInterval
}

func (r *Runtime) activationTimerInterval() time.Duration {
	interval := time.Second
	for _, candidate := range []time.Duration{r.activationSweepInterval(), r.ActivationRetryInitial} {
		if candidate > 0 && candidate < interval {
			interval = candidate
		}
	}
	return interval
}

func (r *Runtime) queueActivationLocked(thread string, authorize bool) {
	if r.pending == nil {
		return
	}
	r.pending[thread] = r.pending[thread] || authorize
	select {
	case r.activations <- struct{}{}:
	default:
	}
}

type activationRequest struct {
	thread    string
	authorize bool
	boundOnly bool
}

func (r *Runtime) takeActivations(limit int) []activationRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.pending))
	for id := range r.pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	requests := make([]activationRequest, 0, len(ids))
	for _, id := range ids {
		requests = append(requests, activationRequest{thread: id, authorize: r.pending[id]})
		delete(r.pending, id)
		r.activating[id] = true
	}
	if len(r.pending) > 0 {
		select {
		case r.activations <- struct{}{}:
		default:
		}
	}
	return requests
}

func (r *Runtime) finishActivation(thread string) {
	r.mu.Lock()
	delete(r.activating, thread)
	r.mu.Unlock()
}

// captureCarryover freezes the routes that were active, retrying, or queued
// when a native generation ended. The next generation rehydrates those routes
// once so history written during the disconnect cannot be missed.
func (r *Runtime) captureCarryover() {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make(map[string]bool, len(r.actors)+len(r.retries)+len(r.pending)+len(r.activating))
	for id := range r.actors {
		if r.bindings[id] {
			ids[id] = true
		}
	}
	for id := range r.retries {
		if r.bindings[id] {
			ids[id] = true
		}
	}
	for id := range r.pending {
		if r.bindings[id] {
			ids[id] = true
		}
	}
	for id := range r.activating {
		if r.bindings[id] {
			ids[id] = true
		}
	}
	r.carryover = r.carryover[:0]
	for id := range ids {
		r.carryover = append(r.carryover, id)
	}
	sort.Strings(r.carryover)
	r.running = false
}

func (r *Runtime) carryoverThreads() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.carryover...)
}

func (r *Runtime) scheduleRetryLocked(thread string, authorize, boundOnly bool) {
	initial, maximum := r.ActivationRetryInitial, r.ActivationRetryMax
	if initial <= 0 {
		initial = defaultActivationRetryInitial
	}
	if maximum <= 0 {
		maximum = defaultActivationRetryMax
	}
	if maximum < initial {
		maximum = initial
	}
	next := initial
	if previous := r.retries[thread].delay; previous > 0 {
		next = previous * 2
		if next < previous || next > maximum {
			next = maximum
		}
	}
	r.retries[thread] = actorRetry{due: r.now().Add(next), delay: next, authorize: authorize, boundOnly: boundOnly}
}

func (r *Runtime) dueRetries(now time.Time) []activationRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	var requests []activationRequest
	for id, retry := range r.retries {
		if r.actors[id] == nil && !retry.due.After(now) {
			requests = append(requests, activationRequest{thread: id, authorize: retry.authorize, boundOnly: retry.boundOnly})
		}
	}
	return requests
}

func (r *Runtime) actorHealthy(thread string) {
	r.mu.Lock()
	delete(r.retries, thread)
	r.mu.Unlock()
}

// activationAuthorize defers workspace and filesystem scope checks until the
// actor has run PrepareWorkspace. Durable creation/verification markers still
// fail closed before an attach, and ordinary routes receive the complete
// Authorize check immediately after preparation.
func (r *Runtime) activationAuthorize(ctx context.Context, t codexclient.Thread) error {
	if r.PrepareWorkspace == nil {
		return r.Authorize(ctx, t, false)
	}
	if _, err := rules.ResolveScope(ctx, t.CWD, r.AllowedRoots); err == nil {
		return r.Authorize(ctx, t, false)
	}
	if r.CreationAuthorize != nil {
		if handled, err := r.CreationAuthorize(ctx, t, false); handled || err != nil {
			return err
		}
	}
	if r.VerificationAuthorize != nil {
		if handled, err := r.VerificationAuthorize(ctx, t, false); handled || err != nil {
			return err
		}
	}
	if !t.InteractiveMain() || t.ForkedFromID != nil {
		return store.ErrIneligible
	}
	return nil
}

// observeBoundActivity is called only from a complete native list page. A
// current event or a newer native update activates the durable route; an old
// synchronized route remains dormant.
func (r *Runtime) observeBoundActivity(ctx context.Context, t codexclient.Thread, archived bool) error {
	if t.ID == "" {
		return nil
	}
	r.mu.Lock()
	if r.bindings != nil {
		r.bindings[t.ID] = true
	}
	r.mu.Unlock()
	if archived {
		return nil
	}
	mark, err := r.Store.Watermark(ctx, r.HostID, t.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	missing := errors.Is(err, sql.ErrNoRows)
	if missing && r.ActorStarted(t.ID) {
		return nil
	}
	changed := missing || t.UpdatedAt <= 0 || t.Status.Type != "idle" && t.Status.Type != "notLoaded"
	if !changed {
		// Native timestamps are second-granularity. Keep a one-second overlap so
		// a reconnect cannot lose an update that shares the hydration second.
		changed = t.UpdatedAt >= (mark.ObservedAtMS-1000)/1000
	}
	if changed {
		r.Wake(t.ID)
	}
	return nil
}
