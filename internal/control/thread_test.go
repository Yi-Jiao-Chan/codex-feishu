//go:build linux

package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/pairing"
	"codex-feishu/internal/store"
	"codex-feishu/internal/verification"
)

type threadProbe struct {
	begins   atomic.Int32
	states   atomic.Int32
	state    verification.NativeSessionResult
	beginErr error
	stateErr error
}

func (p *threadProbe) Begin(context.Context, string) (verification.NativeSessionResult, error) {
	p.begins.Add(1)
	return p.state, p.beginErr
}

func (p *threadProbe) State(context.Context, string) (verification.NativeSessionResult, error) {
	p.states.Add(1)
	return p.state, p.stateErr
}

func validThreadResult(id string) verification.NativeSessionResult {
	return verification.NativeSessionResult{
		NativeResult: verification.NativeResult{
			RequestID:         id,
			BridgeCommit:      "fixture-commit",
			BinarySHA256:      strings.Repeat("b", 64),
			State:             "confirmed",
			CreationStatus:    "pass",
			ThreadID:          "thr_fixture",
			CreatorGeneration: 1,
			CreatorConnected:  true,
			ExpiresAtMS:       time.Now().Add(time.Minute).UnixMilli(),
		},
		ResourcesReady:           true,
		ResourceRecoveryRequired: false,
		ModelRequests:            1,
	}
}

func TestManagedThreadCreationOverRealUnixSocket(t *testing.T) {
	path, identity := controlFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := &threadProbe{}
	probe.state = validThreadResult(store.ID())
	s, err := Listen(ctx, path, identity, func(context.Context) (Snapshot, error) {
		snapshot := snapshot()
		snapshot.VerificationThreadCreationSupported = true
		return snapshot, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.VerificationThreads = probe
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	defer func() { cancel(); s.Close(); <-done }()

	id := store.ID()
	probe.state = validThreadResult(id)
	begin, err := VerifyThreadCreation(ctx, path, identity, id, true)
	if err != nil || begin.RequestID != id || begin.CreationStatus != "pass" || begin.ThreadID == "" {
		t.Fatalf("thread begin failed: %#v %v", begin, err)
	}
	state, err := VerifyThreadCreation(ctx, path, identity, id, false)
	if err != nil || state.RequestID != id || state.CreationStatus != "pass" {
		t.Fatalf("thread state failed: %#v %v", state, err)
	}
	if probe.begins.Load() != 1 || probe.states.Load() != 1 {
		t.Fatalf("provider calls begin=%d state=%d", probe.begins.Load(), probe.states.Load())
	}
	if _, err := VerifyThreadCreation(ctx, path, strings.Repeat("c", 64), id, true); !errors.Is(err, ErrIdentity) || probe.begins.Load() != 1 {
		t.Fatalf("wrong identity reached thread creator: %v", err)
	}

	missing := &threadProbe{stateErr: verification.ErrNotFound}
	s.VerificationThreads = missing
	if _, err := VerifyThreadCreation(ctx, path, identity, store.ID(), false); !errors.Is(err, verification.ErrNotFound) || missing.states.Load() != 1 || missing.begins.Load() != 0 {
		t.Fatalf("missing state invoked wrong provider path: %v begin=%d state=%d", err, missing.begins.Load(), missing.states.Load())
	}
}

func TestThreadPayloadsAreMutuallyExclusiveAndStrict(t *testing.T) {
	path, identity := controlFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := &threadProbe{state: validThreadResult(store.ID())}
	s, err := Listen(ctx, path, identity, func(context.Context) (Snapshot, error) { return snapshot(), nil })
	if err != nil {
		t.Fatal(err)
	}
	s.VerificationThreads = probe
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	defer func() { cancel(); s.Close(); <-done }()
	id := store.ID()
	base := map[string]any{"schema_version": 1, "identity": identity, "method": "verify.thread.begin", "verification_thread": map[string]any{"request_id": id}}
	for _, extra := range []map[string]any{
		{"pair": map[string]any{"request_id": id}},
		{"verification": map[string]any{"request_id": id}},
		{"verification_group": map[string]any{"request_id": id}},
		{"verification_thread": map[string]any{"request_id": id, "cwd": "/tmp/forbidden"}},
		{"cwd": "/tmp/forbidden"},
	} {
		body := make(map[string]any, len(base)+len(extra))
		for key, value := range base {
			body[key] = value
		}
		for key, value := range extra {
			body[key] = value
		}
		encoded, _ := json.Marshal(body)
		result := map[string]any{}
		json.Unmarshal(wire(t, path, string(encoded)), &result)
		if result["code"] == "ok" || probe.begins.Load() != 0 {
			t.Fatalf("mixed or arbitrary thread payload accepted: %s -> %v", encoded, result)
		}
	}
	for _, method := range []string{"health", "verify.card.begin", "verify.group.begin", "pair.begin", "turn/start"} {
		body := map[string]any{"schema_version": 1, "identity": identity, "method": method, "verification_thread": map[string]any{"request_id": id}}
		encoded, _ := json.Marshal(body)
		result := map[string]any{}
		json.Unmarshal(wire(t, path, string(encoded)), &result)
		if result["code"] == "ok" || probe.begins.Load() != 0 {
			t.Fatalf("thread payload accepted by %s: %v", method, result)
		}
	}
}

func TestLegacyClientsRejectMixedThreadResponses(t *testing.T) {
	id := store.ID()
	thread := &verification.NativeSessionResult{}
	tests := []struct {
		name string
		call func(context.Context, string, string, string) error
	}{
		{name: "health", call: func(ctx context.Context, path, identity, id string) error {
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
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path, identity, stop := staticControlResponse(t, func(identity string) response {
				result := response{Schema: protocol, Identity: identity, Code: "ok", VerificationThread: thread}
				switch test.name {
				case "health":
					result.Snapshot = &Snapshot{}
				case "pair":
					result.Pair = &pairing.Challenge{}
				case "card":
					result.Verification = &verification.Result{}
				case "group":
					result.VerificationGroup = &verification.GroupResult{}
				}
				return result
			})
			if err := test.call(context.Background(), path, identity, id); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("mixed response accepted: %v", err)
			}
			stop()
		})
	}
}

func TestVerifyThreadCreationRejectsMalformedResults(t *testing.T) {
	id := store.ID()
	valid := validThreadResult(id)
	cases := []struct {
		name string
		edit func(*verification.NativeSessionResult)
	}{
		{name: "state", edit: func(v *verification.NativeSessionResult) { v.State = "dispatching" }},
		{name: "status", edit: func(v *verification.NativeSessionResult) { v.CreationStatus = "pending" }},
		{name: "missing-thread", edit: func(v *verification.NativeSessionResult) { v.ThreadID = "" }},
		{name: "missing-generation", edit: func(v *verification.NativeSessionResult) { v.CreatorGeneration = 0 }},
		{name: "negative-model-requests", edit: func(v *verification.NativeSessionResult) { v.ModelRequests = -1 }},
		{name: "both-resource-flags", edit: func(v *verification.NativeSessionResult) { v.ResourceRecoveryRequired = true }},
		{name: "neither-resource-flag", edit: func(v *verification.NativeSessionResult) { v.ResourcesReady = false }},
		{name: "connected-without-generation", edit: func(v *verification.NativeSessionResult) { v.CreatorGeneration = 0 }},
		{name: "missing-commit", edit: func(v *verification.NativeSessionResult) { v.BridgeCommit = "" }},
		{name: "invalid-hash", edit: func(v *verification.NativeSessionResult) { v.BinarySHA256 = "bad" }},
		{name: "missing-expiry", edit: func(v *verification.NativeSessionResult) { v.ExpiresAtMS = 0 }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			result := valid
			test.edit(&result)
			path, identity, stop := staticControlResponse(t, func(identity string) response {
				return response{Schema: protocol, Identity: identity, Code: "ok", VerificationThread: &result}
			})
			defer stop()
			if _, err := VerifyThreadCreation(context.Background(), path, identity, id, false); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("malformed result accepted: %v", err)
			}
		})
	}
}

func staticControlResponse(t *testing.T, makeResponse func(string) response) (string, string, func()) {
	t.Helper()
	path, identity := controlFixture(t)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		defer conn.Close()
		io.Copy(io.Discard, conn)
		_ = json.NewEncoder(conn).Encode(makeResponse(identity))
	}()
	stop := func() {
		listener.Close()
		<-done
		os.Remove(path)
	}
	t.Cleanup(stop)
	return path, identity, stop
}
