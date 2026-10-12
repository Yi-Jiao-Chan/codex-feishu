package verification

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

type bindingNative interface {
	NativeCreator
	AttachLocalVerification(context.Context, string, codexclient.LocalVerificationSpec) (codexclient.Thread, error)
}

// BindingManager registers only two already-confirmed components of the same
// explicitly authorized probe. It performs fresh membership GET and a typed
// no-override join, never group/thread creation, input or message delivery.
// Activation uses the existing runtime and its dedicated authorization/write
// guards. Registration and actor presence never stand in for UI acceptance.
type BindingManager struct {
	Sessions     *NativeSessions
	Groups       *GroupManager
	Activate     func(context.Context, store.VerificationBinding) error
	ActorStarted func(store.VerificationBinding) bool
}

type BindingResult struct {
	RequestID           string                           `json:"request_id"`
	BridgeCommit        string                           `json:"bridge_commit"`
	BinarySHA256        string                           `json:"binary_sha256"`
	BuildChanged        bool                             `json:"build_changed"`
	BindingStatus       string                           `json:"binding_status"`
	ThreadID            string                           `json:"thread_id"`
	ChatID              string                           `json:"chat_id"`
	BoundAtMS           int64                            `json:"bound_at_ms"`
	ExpiresAtMS         int64                            `json:"expires_at_ms"`
	Expired             bool                             `json:"expired"`
	ResourcesReady      bool                             `json:"resources_ready"`
	CreatorConnected    bool                             `json:"creator_connected"`
	ActivationRequested bool                             `json:"activation_requested"`
	ActorStarted        bool                             `json:"actor_started"`
	ModelReleased       bool                             `json:"model_released"`
	InputEvidenceStatus string                           `json:"input_evidence_status,omitempty"`
	InputEvidence       *store.VerificationInputEvidence `json:"input_evidence,omitempty"`
}

func (m *BindingManager) configured() bool {
	if m.Sessions == nil || m.Groups == nil {
		return false
	}
	n, g := m.Sessions.native, m.Groups
	return n != nil && g.Store == n.Store && g.HostID == n.HostID && g.AppID == n.AppID && g.OwnerID == n.OwnerID && g.Commit == n.Commit && g.BinarySHA256 == n.BinarySHA256 && g.BotID != "" && g.BotID != g.OwnerID && g.Gateway != nil
}

func (m *BindingManager) Begin(ctx context.Context, id string) (BindingResult, error) {
	if !ValidRequestID(id) || !m.configured() {
		return BindingResult{}, ErrInvalid
	}
	if m.Groups.Ready == nil || !m.Groups.Ready() {
		return BindingResult{}, ErrNotReady
	}
	call, release, spec, err := m.Sessions.ownedOperation(ctx, id)
	if err != nil {
		return BindingResult{}, err
	}
	defer release()
	native := m.Sessions.native
	n, err := native.Store.VerificationNative(call, id, native.HostID, native.AppID, native.OwnerID)
	if errors.Is(err, sql.ErrNoRows) {
		return BindingResult{}, ErrNotFound
	}
	if err != nil {
		return BindingResult{}, err
	}
	g, err := native.Store.VerificationGroup(call, id, native.HostID, native.AppID, native.OwnerID)
	if errors.Is(err, sql.ErrNoRows) {
		return BindingResult{}, ErrNotFound
	}
	if err != nil {
		return BindingResult{}, err
	}
	p := native.result(n)
	var members []string
	if n.State != "confirmed" || n.ThreadID == "" || p.BuildChanged || p.Expired || !p.CreatorConnected || n.CWD != spec.CWD || n.ModelURL != spec.ModelURL || g.State != "confirmed" || g.ChatID == "" || g.GroupMessageID == "" || g.MembershipVerifiedAtMS <= 0 || g.BridgeCommit != native.Commit || g.BinarySHA256 != native.BinarySHA256 || native.now().UnixMilli() < g.CreatedAtMS || native.now().UnixMilli() >= g.ExpiresAtMS || json.Unmarshal([]byte(g.MembersJSON), &members) != nil || len(members) != 1 || members[0] != native.OwnerID {
		return BindingResult{}, ErrNotReady
	}
	reader, ok := m.currentReader(n)
	if !ok {
		return BindingResult{}, ErrNotReady
	}
	// Recheck the full current roster; the group's initial positive GET does
	// not authorize a later binding after owner/bot/other members have changed.
	bot, err := m.Groups.Gateway.BotInChat(call, g.ChatID)
	if err != nil || !bot {
		return BindingResult{}, ErrNotReady
	}
	ids, err := m.Groups.Gateway.MemberIDs(call, g.ChatID)
	if err != nil || !ownerOnlyRoster(ids, native.OwnerID, m.Groups.BotID) {
		return BindingResult{}, ErrNotReady
	}
	if _, ok := m.currentReader(n); !ok {
		return BindingResult{}, ErrNotReady
	}
	thread, err := reader.AttachLocalVerification(call, n.ThreadID, spec)
	if err != nil {
		return BindingResult{}, ErrNotReady
	}
	if !verificationRunningIdentity(thread, n, spec) {
		return BindingResult{}, ErrNotReady
	}
	m.Sessions.mu.Lock()
	resourcesReady := m.Sessions.resourcesLive(m.Sessions.active[id])
	m.Sessions.mu.Unlock()
	if _, ok := m.currentReader(n); !ok || call.Err() != nil || !resourcesReady {
		return BindingResult{}, ErrNotReady
	}
	b, err := native.Store.RegisterVerificationBinding(call, n, g, m.Groups.BotID, native.now().UnixMilli())
	if err != nil {
		return BindingResult{}, err
	}
	result := m.result(call, b)
	if m.Activate != nil {
		if err := m.Activate(call, b); err != nil {
			return result, ErrNotReady
		}
		result = m.result(call, b)
		if result.BindingStatus != "pass" || result.BuildChanged || result.Expired || !result.ResourcesReady || !result.CreatorConnected {
			return result, ErrNotReady
		}
		result.ActivationRequested = true
	}
	return result, nil
}

func (m *BindingManager) currentReader(n store.VerificationNative) (bindingNative, bool) {
	reader, generation, connection := m.Sessions.native.current()
	r, ok := reader.(bindingNative)
	return r, ok && generation == n.Generation && connection == n.ConnectionID
}

func ownerOnlyRoster(ids []string, owner, bot string) bool {
	seen := false
	for _, id := range ids {
		if id == owner {
			seen = true
		} else if id != bot {
			return false
		}
	}
	return seen
}

func verificationRunningIdentity(t codexclient.Thread, n store.VerificationNative, s codexclient.LocalVerificationSpec) bool {
	var label string
	return t.ID == n.ThreadID && t.CWD == s.CWD && !t.Ephemeral && t.ParentThreadID == nil && t.ForkedFromID == nil && json.Unmarshal(t.ThreadSource, &label) == nil && label == "feishu.bridge.validation"
}

// State reads durable registration and local resource facts only. It performs
// no fresh membership GET or join, so it does not claim current write authority.
func (m *BindingManager) State(ctx context.Context, id string) (BindingResult, error) {
	if !ValidRequestID(id) || !m.configured() {
		return BindingResult{}, ErrInvalid
	}
	n := m.Sessions.native
	b, err := n.Store.VerificationBinding(ctx, id, n.HostID, n.AppID, n.OwnerID)
	if errors.Is(err, sql.ErrNoRows) {
		return BindingResult{}, ErrNotFound
	}
	if err != nil {
		return BindingResult{}, err
	}
	return m.result(ctx, b), nil
}

func (m *BindingManager) result(ctx context.Context, b store.VerificationBinding) BindingResult {
	n := m.Sessions.native
	r := BindingResult{RequestID: b.RequestID, BridgeCommit: b.BridgeCommit, BinarySHA256: b.BinarySHA256, BuildChanged: b.BridgeCommit != n.Commit || b.BinarySHA256 != n.BinarySHA256, BindingStatus: "pass", ThreadID: b.ThreadID, ChatID: b.ChatID, BoundAtMS: b.BoundAtMS, ExpiresAtMS: b.ExpiresAtMS, Expired: n.now().UnixMilli() >= b.ExpiresAtMS}
	route, routeErr := n.Store.Binding(ctx, b.HostID, b.ThreadID)
	intent, intentErr := n.Store.GroupIntent(ctx, b.IntentID)
	if routeErr != nil || intentErr != nil || route.IntentID != b.IntentID || route.ChatID != b.ChatID || intent.HostID != b.HostID || intent.ThreadID != b.ThreadID || intent.OwnerID != b.OwnerID || intent.ChatID != b.ChatID || intent.State != "confirmed" {
		r.BindingStatus = "pending"
	}
	// This is one read-only local snapshot of the exact registered fixture.
	// It reports persisted native evidence, not fresh queue reads or UI proof.
	r.InputEvidenceStatus = "pending"
	if r.BindingStatus == "pass" {
		if evidence, err := n.Store.VerificationInputEvidence(ctx, b); err == nil {
			r.InputEvidenceStatus, r.InputEvidence = "pass", &evidence
		}
	}
	// These are current local facts, not roster or policy revalidation.
	p, err := m.Sessions.State(ctx, b.RequestID)
	if err == nil {
		r.ResourcesReady = p.ResourcesReady
		r.CreatorConnected = p.CreatorConnected
		r.ModelReleased = p.ModelReleased
	}
	if m.ActorStarted != nil && r.BindingStatus == "pass" && !r.BuildChanged && !r.Expired && r.ResourcesReady && r.CreatorConnected {
		r.ActorStarted = m.ActorStarted(b)
	}
	return r
}

// ownedOperation joins resource shutdown before any network work but does not
// hold the resource mutex across that work. Close cancels and waits for it.
func (s *NativeSessions) ownedOperation(parent context.Context, id string) (context.Context, func(), codexclient.LocalVerificationSpec, error) {
	s.mu.Lock()
	entry := s.active[id]
	if !s.resourcesLive(entry) {
		s.mu.Unlock()
		return nil, nil, codexclient.LocalVerificationSpec{}, ErrResourcesUnavailable
	}
	s.calls.Add(1)
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	stopService := context.AfterFunc(s.life, cancel)
	stopModel := context.AfterFunc(entry.life, cancel)
	if s.life.Err() != nil || entry.life.Err() != nil {
		cancel()
	}
	modelWatchDone := make(chan struct{})
	go func() {
		defer close(modelWatchDone)
		select {
		case <-entry.model.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	release := func() {
		stopService()
		stopModel()
		cancel()
		<-modelWatchDone
		s.calls.Done()
	}
	return ctx, release, entry.spec, nil
}
