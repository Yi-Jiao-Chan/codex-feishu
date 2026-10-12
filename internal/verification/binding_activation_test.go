package verification

import (
	"context"
	"errors"
	"testing"

	"codex-feishu/internal/store"
)

func TestBindingActivationIsSeparateFromRegistrationAndReadOnlyState(t *testing.T) {
	m, n, g, id := bindingFixture(t)
	activated, present := 0, false
	m.Activate = func(ctx context.Context, b store.VerificationBinding) error {
		if ctx.Err() != nil || b.RequestID != id || b.ThreadID != "owned-thread" || b.ChatID != "oc_test" {
			t.Error("activation received wrong target")
		}
		activated++
		present = true
		return nil
	}
	m.ActorStarted = func(b store.VerificationBinding) bool { return present && b.RequestID == id }
	result, err := m.Begin(context.Background(), id)
	if err != nil || !result.ActivationRequested || !result.ActorStarted || result.BindingStatus != "pass" || activated != 1 {
		t.Fatal(result, err, activated)
	}
	joins, reads := n.joins.Load(), g.reads.Load()
	state, err := m.State(context.Background(), id)
	if err != nil || state.ActivationRequested || !state.ActorStarted || activated != 1 || n.joins.Load() != joins || g.reads.Load() != reads {
		t.Fatal("state activated or did network work", state, err)
	}
	if _, err := m.Begin(context.Background(), id); err != nil || activated != 2 || n.calls.Load() != 1 || g.calls.Load() != 1 {
		t.Fatal("activation replay recreated components", err)
	}
	if err := m.Sessions.Close(); err != nil {
		t.Fatal(err)
	}
	state, err = m.State(context.Background(), id)
	if err != nil || state.ActorStarted || state.ActivationRequested || state.ResourcesReady {
		t.Fatal("historical actor implied current authority", state, err)
	}
}

func TestBindingRejectedActivationRetainsExactLocalRegistration(t *testing.T) {
	m, _, _, id := bindingFixture(t)
	m.Activate = func(context.Context, store.VerificationBinding) error { return errors.New("PRIVATE callback failure") }
	result, err := m.Begin(context.Background(), id)
	if !errors.Is(err, ErrNotReady) || result.ActivationRequested || result.BindingStatus != "pass" {
		t.Fatal("activation failure lost registration or leaked callback", result, err)
	}
	state, err := m.State(context.Background(), id)
	if err != nil || state.BoundAtMS != result.BoundAtMS || state.ExpiresAtMS != result.ExpiresAtMS || state.ActorStarted || state.ActivationRequested {
		t.Fatal(state, err)
	}
}
