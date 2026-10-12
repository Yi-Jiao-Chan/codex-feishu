package app

import (
	"context"

	"codex-feishu/internal/store"
	"codex-feishu/internal/verification"
)

// ActivateVerificationBinding queues only an already registered, authorized
// fixture route. No connection, group, thread, or input is created here.
func (s *Service) ActivateVerificationBinding(ctx context.Context, b store.VerificationBinding) error {
	if !s.running.Load() || !s.NativeConnected() || b.HostID != s.Config.Host.ID || b.AppID != s.Config.Feishu.AppID || b.OwnerID != s.Config.Access.OwnerOpenID || b.BotID != s.BotID {
		return verification.ErrNotReady
	}
	opctx, release, handled, err := s.beginVerificationOperation(ctx, b.ThreadID)
	if err != nil || !handled || release == nil {
		if release != nil {
			release()
		}
		return verification.ErrNotReady
	}
	defer release()
	current, err := s.Store.VerificationBindingForThread(opctx, b.HostID, b.ThreadID)
	if err != nil || current != b || opctx.Err() != nil {
		return verification.ErrNotReady
	}
	s.mu.RLock()
	runtime := s.current
	s.mu.RUnlock()
	if runtime == nil || !runtime.Activate(b.ThreadID) {
		return verification.ErrNotReady
	}
	return nil
}

// VerificationActorStarted reads local lifecycle facts only, without fresh
// membership, native RPCs, activation, or receipt changes.
func (s *Service) VerificationActorStarted(b store.VerificationBinding) bool {
	if !s.running.Load() || !s.NativeConnected() || b.HostID != s.Config.Host.ID || b.AppID != s.Config.Feishu.AppID || b.OwnerID != s.Config.Access.OwnerOpenID || b.BotID != s.BotID {
		return false
	}
	s.mu.RLock()
	runtime := s.current
	s.mu.RUnlock()
	return runtime != nil && runtime.ActorStarted(b.ThreadID)
}
