// Package bindings creates each eligible thread's group from a durable intent.
package bindings

import (
	"context"
	"database/sql"
	"errors"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

var ErrGroupUnknown = errors.New("group creation awaits verification; redispatch disabled")
var ErrGroupBusy = errors.New("group creation is already in progress")
var ErrGroupRejected = errors.New("group creation was rejected; explicit recovery required")

type Native interface {
	Attach(context.Context, string) (codexclient.Thread, error)
}
type Groups interface {
	CreateGroup(context.Context, string, string, []string, string) (string, error)
}

type Manager struct {
	Store           *store.Store
	Native          Native
	Groups          Groups
	HostID, OwnerID string
	Members         []string
	// Re-read policy immediately before dispatch, using actual attached metadata.
	// This prevents a stale candidate or changed cwd from widening authorization.
	Authorize func(context.Context, codexclient.Thread) error
}

func (m *Manager) Ensure(ctx context.Context, thread, name string) (store.Binding, error) {
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
	i, err := m.Store.PrepareGroup(ctx, m.HostID, thread, m.OwnerID, name, m.Members)
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
	if m.Authorize == nil {
		return store.Binding{}, store.ErrIneligible
	}
	attached, err := m.Native.Attach(ctx, thread)
	if err != nil {
		return store.Binding{}, err
	}
	if attached.ID != thread {
		return store.Binding{}, store.ErrConflict
	}
	if err = m.Authorize(ctx, attached); err != nil {
		return store.Binding{}, err
	}
	claimed, err := m.Store.ClaimGroupDispatch(ctx, i.ID)
	if err != nil {
		return store.Binding{}, err
	}
	if !claimed {
		return store.Binding{}, ErrGroupBusy
	}
	chat, remoteErr := m.Groups.CreateGroup(ctx, i.Name, i.OwnerID, i.Members, i.ID)
	if remoteErr != nil || chat == "" {
		state := "unknown"
		var rejected *feishu.Rejected
		if errors.As(remoteErr, &rejected) {
			state = "rejected"
		}
		if err = m.Store.FinishGroupAttempt(ctx, i.ID, state); err != nil {
			return store.Binding{}, err
		}
		if state == "rejected" {
			return store.Binding{}, ErrGroupRejected
		}
		return store.Binding{}, ErrGroupUnknown
	}
	if err = m.Store.ConfirmGroup(ctx, i.ID, chat); err != nil {
		return store.Binding{}, err
	}
	i.State, i.ChatID = "confirmed", chat
	return m.bind(ctx, i)
}

func (m *Manager) bind(ctx context.Context, i store.GroupIntent) (store.Binding, error) {
	if err := m.Store.BindConfirmedGroup(ctx, i.ID); err != nil {
		return store.Binding{}, err
	}
	return m.Store.Binding(ctx, m.HostID, i.ThreadID)
}
