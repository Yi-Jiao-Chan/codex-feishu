package app

import (
	"context"
	"errors"

	"codex-feishu/internal/control"
	"codex-feishu/internal/store"
	"codex-feishu/internal/verification"
)

func (s *Service) HealthReadyForVerification() bool { return s.running.Load() }

// VerificationCreator returns the existing initialized generation only.
// It never invokes Connect or replaces native policy. The durable verifier
// rechecks the connection identity after claiming before sending its request.
func (s *Service) VerificationCreator() verification.NativeCreator {
	if !s.running.Load() || !s.nativeConnected.Load() {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.current == nil {
		return nil
	}
	native, ok := s.current.Native.(verification.NativeCreator)
	if !ok {
		return nil
	}
	done := native.Done()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	default:
		return native
	}
}

// Health reads the existing receiver and native generation, not a parallel
// connection. Durable facts are aggregates; thread routing stays in process.
func (s *Service) Health(ctx context.Context) (control.Snapshot, error) {
	result := control.Snapshot{Running: s.running.Load(), StartedAtMS: s.startedAtMS.Load(), ObservedAtMS: s.now().UnixMilli()}
	result.DiscoveryEnabled = s.Config.Discovery.Enabled
	result.PairingSupported = s.Pairing != nil
	result.VerificationCardSupported = s.Verification != nil
	result.VerificationGroupSupported = s.VerificationGroups != nil
	result.VerificationThreadCreationSupported = s.VerificationThreads != nil
	if s.Store == nil {
		return result, errors.New("service health unavailable")
	}
	if result.Running {
		result.NativeConnected = s.NativeConnected()
		result.FeishuConnected = s.Gateway != nil && s.Gateway.Ready()
	}
	health, err := s.Store.Health(ctx, store.SchemaVersion, s.Config.Host.ID, s.Config.Feishu.AppID)
	if err != nil {
		return result, err
	}
	result.Persistent = health
	bindings, err := s.Store.Bindings(ctx, s.Config.Host.ID)
	if err != nil {
		return result, err
	}
	s.mu.RLock()
	if result.NativeConnected {
		for _, b := range bindings {
			if s.actionsReady[b.ThreadID] {
				result.HydratedBindings++
			}
		}
	}
	s.mu.RUnlock()
	return result, nil
}
