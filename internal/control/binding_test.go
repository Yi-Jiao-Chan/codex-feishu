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

	"codex-feishu/internal/pairing"
	"codex-feishu/internal/store"
	"codex-feishu/internal/verification"
)

type bindingProbe struct {
	beginCalls atomic.Int32
	stateCalls atomic.Int32
	result     verification.BindingResult
	beginErr   error
	stateErr   error
}

func (p *bindingProbe) Begin(_ context.Context, id string) (verification.BindingResult, error) {
	p.beginCalls.Add(1)
	result := p.result
	result.RequestID = id
	return result, p.beginErr
}

func (p *bindingProbe) State(_ context.Context, id string) (verification.BindingResult, error) {
	p.stateCalls.Add(1)
	result := p.result
	result.RequestID = id
	return result, p.stateErr
}

func validBindingResult(id string) verification.BindingResult {
	now := time.Now().UnixMilli()
	return verification.BindingResult{
		RequestID: id, BridgeCommit: "fixture-commit", BinarySHA256: strings.Repeat("b", 64),
		BindingStatus: "pass", ThreadID: "thr_fixture", ChatID: "oc_fixture",
		BoundAtMS: now - time.Second.Milliseconds(), ExpiresAtMS: now + time.Minute.Milliseconds(),
		ResourcesReady: true, CreatorConnected: true,
	}
}

func TestManagedBindingOverRealUnixSocketAndMethodBoundaries(t *testing.T) {
	path, identity := controlFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := &bindingProbe{result: validBindingResult(store.ID())}
	s, err := Listen(ctx, path, identity, func(context.Context) (Snapshot, error) {
		got := snapshot()
		got.VerificationBindingSupported = true
		return got, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.VerificationBindings = probe
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	defer func() { cancel(); s.Close(); <-done }()

	id := store.ID()
	begin, err := VerifyBinding(ctx, path, identity, id, true)
	if err != nil || begin.RequestID != id || begin.BindingStatus != "pass" || begin.ThreadID == "" || begin.ChatID == "" {
		t.Fatalf("binding begin failed: %#v %v", begin, err)
	}
	state, err := VerifyBinding(ctx, path, identity, id, false)
	if err != nil || state.RequestID != id || state.BindingStatus != "pass" {
		t.Fatalf("binding state failed: %#v %v", state, err)
	}
	if probe.beginCalls.Load() != 1 || probe.stateCalls.Load() != 1 {
		t.Fatalf("provider calls begin=%d state=%d", probe.beginCalls.Load(), probe.stateCalls.Load())
	}
	if _, err := VerifyBinding(ctx, path, strings.Repeat("c", 64), id, true); !errors.Is(err, ErrIdentity) || probe.beginCalls.Load() != 1 {
		t.Fatalf("wrong identity reached binding provider: %v", err)
	}

	base := `{"schema_version":1,"identity":"` + identity + `","method":"verify.binding.begin","verification_binding":{"request_id":"` + id + `"}}`
	for _, body := range []string{
		strings.Replace(base, `}}`, `,"prompt":"forbidden"}}`, 1),
		strings.Replace(base, `}}`, `,"thread":"forbidden"}}`, 1),
		strings.Replace(base, `}}`, `,"chat":"forbidden"}}`, 1),
		strings.Replace(base, `}}`, `,"cwd":"/tmp/forbidden"}}`, 1),
		strings.Replace(base, `}}`, `},"pair":{"request_id":"`+id+`"}}`, 1),
		strings.Replace(base, `}}`, `},"verification":{"request_id":"`+id+`"}}`, 1),
		strings.Replace(base, `}}`, `},"verification_group":{"request_id":"`+id+`"}}`, 1),
		strings.Replace(base, `}}`, `},"verification_thread":{"request_id":"`+id+`"}}`, 1),
		strings.Replace(base, `"verify.binding.begin"`, `"health"`, 1),
		strings.Replace(base, `"verify.binding.begin"`, `"verify.card.begin"`, 1),
		strings.Replace(base, `"verify.binding.begin"`, `"verify.group.begin"`, 1),
		strings.Replace(base, `"verify.binding.begin"`, `"verify.thread.begin"`, 1),
		strings.Replace(base, `"verify.binding.begin"`, `"pair.begin"`, 1),
	} {
		var result map[string]any
		json.Unmarshal(wire(t, path, body), &result)
		if result["code"] == "ok" || probe.beginCalls.Load() != 1 {
			t.Fatalf("binding payload crossed method boundary: %s -> %v", body, result)
		}
	}
	if _, err := VerifyBinding(ctx, path, identity, "BAD", true); !errors.Is(err, verification.ErrInvalid) || probe.beginCalls.Load() != 1 {
		t.Fatalf("invalid binding ID reached provider: %v", err)
	}
}

func TestBindingStateNotFoundUsesExistingControlError(t *testing.T) {
	path, identity := controlFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := &bindingProbe{result: validBindingResult(store.ID()), stateErr: verification.ErrNotFound}
	s, err := Listen(ctx, path, identity, func(context.Context) (Snapshot, error) { return snapshot(), nil })
	if err != nil {
		t.Fatal(err)
	}
	s.VerificationBindings = probe
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	defer func() { cancel(); s.Close(); <-done }()
	if _, err := VerifyBinding(ctx, path, identity, store.ID(), false); !errors.Is(err, verification.ErrNotFound) || probe.stateCalls.Load() != 1 || probe.beginCalls.Load() != 0 {
		t.Fatalf("missing binding state was not mapped safely: %v begin=%d state=%d", err, probe.beginCalls.Load(), probe.stateCalls.Load())
	}
}

func TestLegacyClientsRejectMixedBindingResponses(t *testing.T) {
	id := store.ID()
	binding := validBindingResult(id)
	tests := []struct {
		name string
		call func(context.Context, string, string, string) error
	}{
		{name: "health", call: func(ctx context.Context, path, identity, _ string) error {
			_, err := Query(ctx, path, identity)
			return err
		}},
		{name: "pair", call: func(ctx context.Context, path, identity, id string) error {
			_, err := PairState(ctx, path, identity, id)
			return err
		}},
		{name: "card", call: func(ctx context.Context, path, identity, id string) error {
			_, err := VerifyCard(ctx, path, identity, id, false)
			return err
		}},
		{name: "group", call: func(ctx context.Context, path, identity, id string) error {
			_, err := VerifyGroup(ctx, path, identity, id, false)
			return err
		}},
		{name: "thread", call: func(ctx context.Context, path, identity, id string) error {
			_, err := VerifyThreadCreation(ctx, path, identity, id, false)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path, identity, stop := staticControlResponse(t, func(identity string) response {
				result := response{Schema: protocol, Identity: identity, Code: "ok", VerificationBinding: &binding}
				switch test.name {
				case "health":
					result.Snapshot = &Snapshot{}
				case "pair":
					result.Pair = &pairing.Challenge{}
				case "card":
					result.Verification = &verification.Result{}
				case "group":
					result.VerificationGroup = &verification.GroupResult{}
				case "thread":
					result.VerificationThread = &verification.NativeSessionResult{}
				}
				return result
			})
			if err := test.call(context.Background(), path, identity, id); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("mixed binding response accepted: %v", err)
			}
			stop()
		})
	}
}

func TestVerifyBindingRejectsMalformedResults(t *testing.T) {
	id := store.ID()
	valid := validBindingResult(id)
	cases := []struct {
		name string
		edit func(*verification.BindingResult)
	}{
		{name: "request-id", edit: func(v *verification.BindingResult) { v.RequestID = store.ID() }},
		{name: "missing-commit", edit: func(v *verification.BindingResult) { v.BridgeCommit = "" }},
		{name: "invalid-hash", edit: func(v *verification.BindingResult) { v.BinarySHA256 = "bad" }},
		{name: "invalid-status", edit: func(v *verification.BindingResult) { v.BindingStatus = "confirmed" }},
		{name: "pass-without-thread", edit: func(v *verification.BindingResult) { v.ThreadID = "" }},
		{name: "pass-without-chat", edit: func(v *verification.BindingResult) { v.ChatID = "" }},
		{name: "missing-bound-time", edit: func(v *verification.BindingResult) { v.BoundAtMS = 0 }},
		{name: "expiry-at-bound", edit: func(v *verification.BindingResult) { v.ExpiresAtMS = v.BoundAtMS }},
		{name: "expiry-flag", edit: func(v *verification.BindingResult) { v.Expired = true }},
		{name: "activation-build-changed", edit: func(v *verification.BindingResult) { v.ActivationRequested, v.BuildChanged = true, true }},
		{name: "activation-resources-not-ready", edit: func(v *verification.BindingResult) { v.ActivationRequested, v.ResourcesReady = true, false }},
		{name: "actor-disconnected", edit: func(v *verification.BindingResult) { v.ActorStarted, v.CreatorConnected = true, false }},
		{name: "pending-activation", edit: func(v *verification.BindingResult) {
			v.BindingStatus, v.ThreadID, v.ChatID, v.ActivationRequested = "pending", "", "", true
		}},
		{name: "pending-actor", edit: func(v *verification.BindingResult) {
			v.BindingStatus, v.ThreadID, v.ChatID, v.ActorStarted = "pending", "", "", true
		}},
		{name: "actor-expired", edit: func(v *verification.BindingResult) {
			v.ActorStarted = true
			v.ExpiresAtMS = time.Now().Add(-time.Minute).UnixMilli()
			v.BoundAtMS = v.ExpiresAtMS - 1
			v.Expired = true
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			result := valid
			test.edit(&result)
			path, identity, stop := staticControlResponse(t, func(identity string) response {
				return response{Schema: protocol, Identity: identity, Code: "ok", VerificationBinding: &result}
			})
			defer stop()
			if _, err := VerifyBinding(context.Background(), path, identity, id, false); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("malformed binding result accepted: %v", err)
			}
		})
	}

	stateActivation := valid
	stateActivation.ActivationRequested = true
	path, identity, stop := staticControlResponse(t, func(identity string) response {
		return response{Schema: protocol, Identity: identity, Code: "ok", VerificationBinding: &stateActivation}
	})
	defer stop()
	if _, err := VerifyBinding(context.Background(), path, identity, id, false); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("state response falsely reported activation: %v", err)
	}

	pending := valid
	pending.BindingStatus, pending.ThreadID, pending.ChatID = "pending", "", ""
	path, identity, stop = staticControlResponse(t, func(identity string) response {
		return response{Schema: protocol, Identity: identity, Code: "ok", VerificationBinding: &pending}
	})
	defer stop()
	if result, err := VerifyBinding(context.Background(), path, identity, id, false); err != nil || result.BindingStatus != "pending" || result.ThreadID != "" || result.ChatID != "" {
		t.Fatalf("valid pending historical result rejected: %#v %v", result, err)
	}

	historical := valid
	historical.BuildChanged, historical.ResourcesReady, historical.CreatorConnected = true, false, false
	path, identity, stop = staticControlResponse(t, func(identity string) response {
		return response{Schema: protocol, Identity: identity, Code: "ok", VerificationBinding: &historical}
	})
	defer stop()
	if result, err := VerifyBinding(context.Background(), path, identity, id, false); err != nil || result.BindingStatus != "pass" || result.ResourcesReady || result.CreatorConnected {
		t.Fatalf("historical binding facts were overclaimed: %#v %v", result, err)
	}
}

func TestVerifyBindingRejectsOldResultPayload(t *testing.T) {
	id := store.ID()
	for _, test := range []struct {
		name  string
		value any
	}{
		{name: "card", value: &verification.Result{}},
		{name: "group", value: &verification.GroupResult{}},
		{name: "thread", value: &verification.NativeSessionResult{}},
		{name: "pair", value: &pairing.Challenge{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, identity, stop := staticControlResponse(t, func(identity string) response {
				result := response{Schema: protocol, Identity: identity, Code: "ok"}
				switch value := test.value.(type) {
				case *verification.Result:
					result.Verification = value
				case *verification.GroupResult:
					result.VerificationGroup = value
				case *verification.NativeSessionResult:
					result.VerificationThread = value
				case *pairing.Challenge:
					result.Pair = value
				}
				return result
			})
			defer stop()
			if _, err := VerifyBinding(context.Background(), path, identity, id, false); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("old response type accepted as binding: %v", err)
			}
		})
	}
}
