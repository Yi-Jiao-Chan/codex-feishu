// Package bindings creates each eligible thread's group from a durable intent.
package bindings

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/latency"
	"codex-feishu/internal/store"
)

var ErrGroupUnknown = errors.New("group creation awaits verification; redispatch disabled")
var ErrGroupDeferred = errors.New("group creation is waiting for its persisted rate-limit deadline")
var ErrGroupBusy = errors.New("group creation is already in progress")
var ErrGroupRejected = errors.New("group creation was rejected; explicit recovery required")

type GroupDeferredError struct{ RetryAtMS int64 }

func (e *GroupDeferredError) Error() string { return ErrGroupDeferred.Error() }
func (e *GroupDeferredError) Unwrap() error { return ErrGroupDeferred }

func (m *Manager) deferred(ctx context.Context, id string) error {
	due, err := m.Store.GroupRetryDeadline(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrGroupBusy
	}
	if err != nil {
		return err
	}
	return &GroupDeferredError{RetryAtMS: due}
}

type Native interface {
	Attach(context.Context, string) (codexclient.Thread, error)
}
type Groups interface {
	CreateGroupWithDescription(context.Context, string, string, []string, string, string) (string, error)
}

type Manager struct {
	Store           *store.Store
	Native          Native
	Groups          Groups
	HostID, OwnerID string
	Members         []string
	Now             func() time.Time
	Latency         latency.Observer
	// Re-read policy immediately before dispatch, using actual attached metadata.
	// This prevents a stale candidate or changed cwd from widening authorization.
	Authorize func(context.Context, codexclient.Thread) error
}

func (m *Manager) Ensure(ctx context.Context, thread, name string) (binding store.Binding, resultErr error) {
	started := time.Now()
	m.Latency.Record("group.ensure.started", m.HostID, thread, "", "", 0, "observed")
	defer func() {
		outcome := latency.Result(resultErr)
		switch {
		case errors.Is(resultErr, ErrGroupUnknown):
			outcome = "unknown"
		case errors.Is(resultErr, ErrGroupRejected):
			outcome = "rejected"
		case errors.Is(resultErr, ErrGroupDeferred):
			outcome = "deferred"
		case errors.Is(resultErr, ErrGroupBusy):
			outcome = "busy"
		}
		m.Latency.Record("group.ensure", m.HostID, thread, "", "", time.Since(started), outcome)
	}()
	if b, err := m.Store.Binding(ctx, m.HostID, thread); err == nil {
		return b, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return b, err
	}
	_, complete, err := m.Store.BaselineFence(ctx, m.HostID)
	if err != nil {
		return store.Binding{}, err
	}
	if !complete {
		return store.Binding{}, store.ErrIneligible
	}
	stepStarted := time.Now()
	i, err := m.Store.PrepareGroup(ctx, m.HostID, thread, m.OwnerID, name, m.Members)
	m.Latency.Record("group.intent", m.HostID, thread, "", "", time.Since(stepStarted), latency.Result(err))
	if err != nil {
		return store.Binding{}, err
	}
	switch i.State {
	case "confirmed":
		return m.bind(ctx, i)
	case "unknown":
		return store.Binding{}, ErrGroupUnknown
	case "dispatching":
		return store.Binding{}, ErrGroupBusy
	case "rejected":
		return store.Binding{}, ErrGroupRejected
	case "ready":
	default:
		return store.Binding{}, store.ErrConflict
	}
	// A frozen request cannot bypass an operator's configuration change.
	// Already-created groups keep their binding; unsent intents pause until
	// their owner/member scope is consistent again, without changing UUID data.
	if i.OwnerID != m.OwnerID || !sameGroupMembers(i.Members, m.Members, m.OwnerID) {
		return store.Binding{}, store.ErrConflict
	}
	clock := m.Now
	if clock == nil {
		clock = time.Now
	}
	ready, err := m.Store.GroupRetryReady(ctx, i.ID, clock().UnixMilli())
	if err != nil {
		return store.Binding{}, err
	}
	if !ready {
		current, err := m.Store.GroupIntent(ctx, i.ID)
		if err != nil {
			return store.Binding{}, err
		}
		switch current.State {
		case "confirmed":
			return m.bind(ctx, current)
		case "dispatching":
			return store.Binding{}, ErrGroupBusy
		case "unknown":
			return store.Binding{}, ErrGroupUnknown
		case "rejected":
			return store.Binding{}, ErrGroupRejected
		case "ready":
			o, err := m.Store.Observation(ctx, m.HostID, thread)
			if err != nil {
				return store.Binding{}, err
			}
			if o.State != "eligible" {
				return store.Binding{}, store.ErrIneligible
			}
			return store.Binding{}, m.deferred(ctx, i.ID)
		default:
			return store.Binding{}, store.ErrConflict
		}
	}
	if m.Authorize == nil {
		return store.Binding{}, store.ErrIneligible
	}
	stepStarted = time.Now()
	attached, err := m.Native.Attach(ctx, thread)
	m.Latency.Record("group.attach", m.HostID, thread, "", "", time.Since(stepStarted), latency.Result(err))
	if err != nil {
		return store.Binding{}, err
	}
	if attached.ID != thread {
		return store.Binding{}, store.ErrConflict
	}
	stepStarted = time.Now()
	err = m.Authorize(ctx, attached)
	m.Latency.Record("group.authorize", m.HostID, thread, "", "", time.Since(stepStarted), latency.Result(err))
	if err != nil {
		return store.Binding{}, err
	}
	if !filepath.IsAbs(attached.CWD) {
		return store.Binding{}, store.ErrIneligible
	}
	description, err := m.Store.FreezeGroupDescription(ctx, i.ID, "工作目录："+attached.CWD)
	if err != nil {
		return store.Binding{}, err
	}
	attempt, claimed, err := m.Store.ClaimGroupAttempt(ctx, i.ID, clock().UnixMilli())
	if err != nil {
		return store.Binding{}, err
	}
	if !claimed {
		return store.Binding{}, ErrGroupBusy
	}
	m.Latency.Record("group.create.started", m.HostID, thread, "", "", 0, "observed")
	stepStarted = time.Now()
	chat, remoteErr := m.Groups.CreateGroupWithDescription(ctx, i.Name, i.OwnerID, i.Members, i.ID, description)
	outcome := "confirmed"
	if remoteErr != nil || chat == "" {
		outcome = "unknown"
		var rejected *feishu.Rejected
		var limited *feishu.RateLimited
		if errors.As(remoteErr, &rejected) {
			outcome = "rejected"
		} else if errors.As(remoteErr, &limited) {
			outcome = "deferred"
		}
	}
	m.Latency.Record("group.create", m.HostID, thread, "", "", time.Since(stepStarted), outcome)
	var limited *feishu.RateLimited
	if errors.As(remoteErr, &limited) {
		at := clock().UnixMilli()
		if at < attempt.CreatedAtMS {
			at = attempt.CreatedAtMS
		}
		delay := limited.RetryAfter.Milliseconds()
		if delay < 1000 {
			delay = 1000
		}
		if err := m.Store.DeferGroupAttempt(ctx, attempt, at, delay); err != nil {
			return store.Binding{}, err
		}
		return store.Binding{}, m.deferred(ctx, i.ID)
	}
	if remoteErr != nil || chat == "" {
		state := "unknown"
		var rejected *feishu.Rejected
		if errors.As(remoteErr, &rejected) {
			state = "rejected"
		}
		if err = m.Store.FinishGroupDispatch(ctx, attempt, state, ""); err != nil {
			return store.Binding{}, err
		}
		if state == "rejected" {
			return store.Binding{}, ErrGroupRejected
		}
		return store.Binding{}, ErrGroupUnknown
	}
	if err = m.Store.FinishGroupDispatch(ctx, attempt, "confirmed", chat); err != nil {
		return store.Binding{}, err
	}
	i.State, i.ChatID = "confirmed", chat
	return m.bind(ctx, i)
}

func (m *Manager) bind(ctx context.Context, i store.GroupIntent) (binding store.Binding, resultErr error) {
	started := time.Now()
	defer func() {
		m.Latency.Record("group.bound", m.HostID, i.ThreadID, "", "", time.Since(started), latency.Result(resultErr))
	}()
	if err := m.Store.BindConfirmedGroup(ctx, i.ID); err != nil {
		return store.Binding{}, err
	}
	return m.Store.Binding(ctx, m.HostID, i.ThreadID)
}

func sameGroupMembers(a, b []string, owner string) bool {
	left, right := map[string]bool{owner: true}, map[string]bool{owner: true}
	for _, id := range a {
		if id != "" {
			left[id] = true
		}
	}
	for _, id := range b {
		if id != "" {
			right[id] = true
		}
	}
	if len(left) != len(right) {
		return false
	}
	for id := range left {
		if !right[id] {
			return false
		}
	}
	return true
}
