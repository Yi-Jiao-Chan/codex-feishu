package verification

import (
	"context"
	"database/sql"
	"errors"
)

// Release opens only the already-started original local model response gate.
// The original binding's operation grant is rechecked before release. It does
// not create, bind, activate, submit, steer, archive or claim turn completion.
// Repeating it is harmless because the local gate is a sync.Once operation.
func (m *BindingManager) Release(ctx context.Context, id string) (BindingResult, error) {
	if !ValidRequestID(id) || !m.configured() {
		return BindingResult{}, ErrInvalid
	}
	n := m.Sessions.native
	b, err := n.Store.VerificationBinding(ctx, id, n.HostID, n.AppID, n.OwnerID)
	if errors.Is(err, sql.ErrNoRows) {
		return BindingResult{}, ErrNotFound
	}
	if err != nil {
		return BindingResult{}, ErrNotReady
	}
	call, release, handled, err := m.BeginOperation(ctx, b.ThreadID)
	if release != nil {
		defer release()
	}
	if err != nil || !handled {
		return BindingResult{}, ErrNotReady
	}
	// Preserve the exact request mapping across the network checks.
	current, err := n.Store.VerificationBinding(call, id, n.HostID, n.AppID, n.OwnerID)
	if err != nil || current != b {
		return BindingResult{}, ErrNotReady
	}
	s := m.Sessions
	s.mu.Lock()
	entry := s.active[id]
	if call.Err() != nil || n.now().UnixMilli() >= b.ExpiresAtMS || !s.resourcesLive(entry) {
		s.mu.Unlock()
		return BindingResult{}, ErrNotReady
	}
	// Releasing before response.created would skip the busy window that this
	// fixture is intended to exercise. Never start a request to make it ready.
	select {
	case <-entry.model.Started():
		entry.model.Release()
	default:
		s.mu.Unlock()
		return BindingResult{}, ErrNotReady
	}
	s.mu.Unlock()
	return m.result(call, b), nil
}
