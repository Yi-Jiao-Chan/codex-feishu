//go:build linux

package control

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/store"
	"codex-feishu/internal/verification"
)

type releaseBindingProbe struct {
	bindingProbe
	releaseCalls  atomic.Int32
	releaseResult verification.BindingResult
	releaseErr    error
}

func (p *releaseBindingProbe) Release(_ context.Context, id string) (verification.BindingResult, error) {
	p.releaseCalls.Add(1)
	result := p.releaseResult
	result.RequestID = id
	return result, p.releaseErr
}

func TestManagedBindingReleaseOverUnixSocket(t *testing.T) {
	path, identity := controlFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id := store.ID()
	released := validBindingResult(id)
	released.ModelReleased = true
	probe := &releaseBindingProbe{
		bindingProbe:  bindingProbe{result: validBindingResult(id)},
		releaseResult: released,
	}
	s, err := Listen(ctx, path, identity, func(context.Context) (Snapshot, error) {
		got := snapshot()
		got.VerificationBindingSupported = true
		got.VerificationTurnReleaseSupported = true
		return got, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.VerificationBindings = probe
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	defer func() { cancel(); s.Close(); <-done }()

	result, err := VerifyTurnRelease(ctx, path, identity, id)
	if err != nil || !result.ModelReleased || result.BindingStatus != "pass" || probe.releaseCalls.Load() != 1 || probe.beginCalls.Load() != 0 || probe.stateCalls.Load() != 0 {
		t.Fatalf("binding release failed or used wrong provider method: %#v %v release=%d begin=%d state=%d", result, err, probe.releaseCalls.Load(), probe.beginCalls.Load(), probe.stateCalls.Load())
	}
}

func TestBindingReleaseUnsupportedAndInvalidIDDoNotFallback(t *testing.T) {
	path, identity := controlFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := &bindingProbe{result: validBindingResult(store.ID())}
	s, err := Listen(ctx, path, identity, func(context.Context) (Snapshot, error) { return snapshot(), nil })
	if err != nil {
		t.Fatal(err)
	}
	s.VerificationBindings = probe
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	defer func() { cancel(); s.Close(); <-done }()
	if _, err := VerifyTurnRelease(ctx, path, identity, store.ID()); !errors.Is(err, ErrUnsupported) || probe.beginCalls.Load() != 0 || probe.stateCalls.Load() != 0 {
		t.Fatalf("unsupported release fell back to binding method: %v begin=%d state=%d", err, probe.beginCalls.Load(), probe.stateCalls.Load())
	}
	if _, err := VerifyTurnRelease(ctx, path, identity, "invalid"); !errors.Is(err, verification.ErrInvalid) || probe.beginCalls.Load() != 0 || probe.stateCalls.Load() != 0 {
		t.Fatalf("invalid release ID reached endpoint/provider: %v", err)
	}
}

func TestBindingReleaseRejectsCrossPayloads(t *testing.T) {
	path, identity := controlFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id := store.ID()
	probe := &releaseBindingProbe{bindingProbe: bindingProbe{result: validBindingResult(id)}}
	s, err := Listen(ctx, path, identity, func(context.Context) (Snapshot, error) { return snapshot(), nil })
	if err != nil {
		t.Fatal(err)
	}
	s.VerificationBindings = probe
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	defer func() { cancel(); s.Close(); <-done }()
	base := `{"schema_version":1,"identity":"` + identity + `","method":"verify.binding.release","verification_binding":{"request_id":"` + id + `"}}`
	for _, body := range []string{
		strings.Replace(base, `}}`, `},"pair":{"request_id":"`+id+`"}}`, 1),
		strings.Replace(base, `}}`, `},"verification":{"request_id":"`+id+`"}}`, 1),
		strings.Replace(base, `}}`, `},"verification_group":{"request_id":"`+id+`"}}`, 1),
		strings.Replace(base, `}}`, `},"verification_thread":{"request_id":"`+id+`"}}`, 1),
		strings.Replace(base, `"request_id":"`+id+`"}}`, `"request_id":"`+id+`","thread":"forbidden"}}`, 1),
	} {
		var result map[string]any
		json.Unmarshal(wire(t, path, body), &result)
		if result["code"] == "ok" || probe.releaseCalls.Load() != 0 {
			t.Fatalf("cross-payload release reached provider: %s -> %v", body, result)
		}
	}
}

func TestBindingReleaseRejectsMixedResponseAndInvalidFlags(t *testing.T) {
	id := store.ID()
	valid := validBindingResult(id)
	valid.ModelReleased = true

	path, identity, stop := staticControlResponse(t, func(identity string) response {
		return response{Schema: protocol, Identity: identity, Code: "ok", Snapshot: &Snapshot{}, VerificationBinding: &valid}
	})
	if _, err := VerifyTurnRelease(context.Background(), path, identity, id); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("mixed release response accepted: %v", err)
	}
	stop()

	cases := []struct {
		name string
		edit func(*verification.BindingResult)
	}{
		{name: "not-released", edit: func(v *verification.BindingResult) { v.ModelReleased = false }},
		{name: "activation-requested", edit: func(v *verification.BindingResult) { v.ActivationRequested = true }},
		{name: "build-changed", edit: func(v *verification.BindingResult) { v.BuildChanged = true }},
		{name: "expired", edit: func(v *verification.BindingResult) {
			v.ExpiresAtMS = time.Now().Add(-time.Minute).UnixMilli()
			v.BoundAtMS = v.ExpiresAtMS - 1
			v.Expired = true
		}},
		{name: "resources-not-ready", edit: func(v *verification.BindingResult) { v.ResourcesReady = false }},
		{name: "creator-disconnected", edit: func(v *verification.BindingResult) { v.CreatorConnected = false }},
		{name: "pending", edit: func(v *verification.BindingResult) { v.BindingStatus = "pending" }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			result := valid
			test.edit(&result)
			path, identity, stop := staticControlResponse(t, func(identity string) response {
				return response{Schema: protocol, Identity: identity, Code: "ok", VerificationBinding: &result}
			})
			defer stop()
			if _, err := VerifyTurnRelease(context.Background(), path, identity, id); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("invalid release flags accepted: %v", err)
			}
		})
	}
}

func TestBindingStateMayReportReleasedWithoutClaimingRelease(t *testing.T) {
	id := store.ID()
	result := validBindingResult(id)
	result.ModelReleased = true
	path, identity, stop := staticControlResponse(t, func(identity string) response {
		return response{Schema: protocol, Identity: identity, Code: "ok", VerificationBinding: &result}
	})
	defer stop()
	state, err := VerifyBinding(context.Background(), path, identity, id, false)
	if err != nil || !state.ModelReleased || state.BindingStatus != "pass" {
		t.Fatalf("released state was not preserved as a read-only fact: %#v %v", state, err)
	}
}

func TestBindingReleaseMapsProviderErrors(t *testing.T) {
	id := store.ID()
	probe := &releaseBindingProbe{
		bindingProbe:  bindingProbe{result: validBindingResult(id)},
		releaseResult: validBindingResult(id),
		releaseErr:    verification.ErrNotFound,
	}
	path, identity := controlFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := Listen(ctx, path, identity, func(context.Context) (Snapshot, error) { return snapshot(), nil })
	if err != nil {
		t.Fatal(err)
	}
	s.VerificationBindings = probe
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	defer func() { cancel(); s.Close(); <-done }()
	if _, err := VerifyTurnRelease(ctx, path, identity, id); !errors.Is(err, verification.ErrNotFound) || probe.releaseCalls.Load() != 1 {
		t.Fatalf("release provider error was not mapped: %v calls=%d", err, probe.releaseCalls.Load())
	}
}
