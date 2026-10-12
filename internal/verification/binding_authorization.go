package verification

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

// BeginOperation recognizes a verification marker before ordinary routing.
// Once recognized, an invalid marker never falls back to ordinary authority.
// The caller must use the returned context for claim and dispatch and release
// after result persistence. No marker means no fixture API or resource lease.
func (m *BindingManager) BeginOperation(ctx context.Context, thread string) (context.Context, func(), bool, error) {
	return m.beginOperation(ctx, thread, nil)
}

// Authorize is the runtime's metadata gate. Automatic discovery is never
// granted validation authority; even valid fixtures need their explicit route.
func (m *BindingManager) Authorize(ctx context.Context, t codexclient.Thread, automatic bool) (bool, error) {
	if automatic {
		_, handled, err := m.operationMarker(ctx, t.ID)
		if handled {
			return true, store.ErrIneligible
		}
		return false, err
	}
	_, release, handled, err := m.beginOperation(ctx, t.ID, &t)
	if release != nil {
		release()
	}
	return handled, err
}

func (m *BindingManager) operationMarker(ctx context.Context, thread string) (store.VerificationBinding, bool, error) {
	if m == nil || m.Sessions == nil || m.Sessions.native == nil || m.Sessions.native.Store == nil || thread == "" {
		return store.VerificationBinding{}, true, ErrInvalid
	}
	n := m.Sessions.native
	b, err := n.Store.VerificationBindingForThread(ctx, n.HostID, thread)
	if errors.Is(err, sql.ErrNoRows) {
		return b, false, nil
	}
	if err != nil {
		return b, true, ErrNotReady
	}
	return b, true, nil
}

func (m *BindingManager) beginOperation(ctx context.Context, thread string, expected *codexclient.Thread) (context.Context, func(), bool, error) {
	b, handled, err := m.operationMarker(ctx, thread)
	if !handled {
		return ctx, func() {}, false, err
	}
	if err != nil {
		return nil, nil, true, err
	}
	if !m.configured() || m.Groups.Ready == nil || !m.Groups.Ready() {
		return nil, nil, true, ErrNotReady
	}
	manager := m.Sessions.native
	windowAnchor := time.Now()
	now := manager.now().UnixMilli()
	if b.HostID != manager.HostID || b.AppID != manager.AppID || b.OwnerID != manager.OwnerID || b.BotID != m.Groups.BotID || b.BridgeCommit != manager.Commit || b.BinarySHA256 != manager.BinarySHA256 || b.ThreadID != thread || now < b.BoundAtMS || now >= b.ExpiresAtMS || b.MembershipVerifiedAtMS != b.BoundAtMS {
		return nil, nil, true, ErrNotReady
	}
	n, err := manager.Store.VerificationNative(ctx, b.RequestID, manager.HostID, manager.AppID, manager.OwnerID)
	if err != nil {
		return nil, nil, true, ErrNotReady
	}
	g, err := manager.Store.VerificationGroup(ctx, b.RequestID, manager.HostID, manager.AppID, manager.OwnerID)
	if err != nil {
		return nil, nil, true, ErrNotReady
	}
	expires := min(n.ExpiresAtMS, g.ExpiresAtMS)
	if n.State != "confirmed" || g.State != "confirmed" || n.ThreadID != b.ThreadID || g.ChatID != b.ChatID || n.CWD != b.CWD || b.ExpiresAtMS != expires || n.BridgeCommit != b.BridgeCommit || g.BridgeCommit != b.BridgeCommit || n.BinarySHA256 != b.BinarySHA256 || g.BinarySHA256 != b.BinarySHA256 || g.GroupMessageID == "" || g.MembershipVerifiedAtMS <= 0 || g.MembershipVerifiedAtMS > now || b.BoundAtMS < n.CreatedAtMS || b.BoundAtMS < g.CreatedAtMS {
		return nil, nil, true, ErrNotReady
	}
	route, err := manager.Store.Binding(ctx, b.HostID, b.ThreadID)
	if err != nil || route.IntentID != b.IntentID || route.ChatID != b.ChatID {
		return nil, nil, true, ErrNotReady
	}
	intent, err := manager.Store.GroupIntent(ctx, b.IntentID)
	var members []string
	if err != nil || intent.State != "confirmed" || intent.HostID != b.HostID || intent.ThreadID != b.ThreadID || intent.OwnerID != b.OwnerID || intent.ChatID != b.ChatID || intent.Name != g.Name || json.Unmarshal([]byte(g.MembersJSON), &members) != nil || len(members) != 1 || members[0] != b.OwnerID || len(intent.Members) != 1 || intent.Members[0] != b.OwnerID {
		return nil, nil, true, ErrNotReady
	}
	call, releaseResources, spec, err := m.Sessions.ownedOperation(ctx, b.RequestID)
	if err != nil {
		return nil, nil, true, ErrNotReady
	}
	// Use the remaining frozen window rather than deriving a new five minutes.
	// Duration also keeps the test clock separate from the machine wall clock.
	call, cancel := context.WithDeadline(call, windowAnchor.Add(time.Duration(b.ExpiresAtMS-now)*time.Millisecond))
	release := func() { cancel(); releaseResources() }
	fail := func() (context.Context, func(), bool, error) {
		release()
		return nil, nil, true, ErrNotReady
	}
	request, requestErr := codexclient.LocalVerificationRequestJSON(spec)
	if requestErr != nil || request != n.RequestJSON || spec.CWD != b.CWD || spec.ModelURL != n.ModelURL || spec.RunID != b.RequestID || expected != nil && !verificationRunningIdentity(*expected, n, spec) {
		return fail()
	}
	reader, ok := m.currentReader(n)
	if !ok {
		return fail()
	}
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-reader.Done():
			cancel()
		case <-call.Done():
		}
	}()
	previousRelease := release
	release = func() { previousRelease(); <-watchDone }
	bot, err := m.Groups.Gateway.BotInChat(call, b.ChatID)
	if err != nil || !bot {
		return fail()
	}
	ids, err := m.Groups.Gateway.MemberIDs(call, b.ChatID)
	if err != nil || !ownerOnlyRoster(ids, b.OwnerID, b.BotID) {
		return fail()
	}
	joined, err := reader.AttachLocalVerification(call, b.ThreadID, spec)
	if err != nil || !verificationRunningIdentity(joined, n, spec) {
		return fail()
	}
	m.Sessions.mu.Lock()
	resourcesReady := m.Sessions.resourcesLive(m.Sessions.active[b.RequestID])
	m.Sessions.mu.Unlock()
	if _, ok := m.currentReader(n); !ok || !resourcesReady || call.Err() != nil || manager.now().UnixMilli() >= b.ExpiresAtMS || !m.Groups.Ready() {
		return fail()
	}
	// Fresh binding reads detect removal/replacement during network checks.
	current, err := manager.Store.VerificationBindingForThread(call, b.HostID, b.ThreadID)
	if err != nil || current != b {
		return fail()
	}
	route, err = manager.Store.Binding(call, b.HostID, b.ThreadID)
	if err != nil || route.IntentID != b.IntentID || route.ChatID != b.ChatID {
		return fail()
	}
	currentIntent, err := manager.Store.GroupIntent(call, b.IntentID)
	if err != nil || currentIntent.State != intent.State || currentIntent.HostID != intent.HostID || currentIntent.ThreadID != intent.ThreadID || currentIntent.ChatID != intent.ChatID || currentIntent.OwnerID != intent.OwnerID || currentIntent.Name != intent.Name || len(currentIntent.Members) != 1 || currentIntent.Members[0] != b.OwnerID {
		return fail()
	}
	return call, release, true, nil
}
