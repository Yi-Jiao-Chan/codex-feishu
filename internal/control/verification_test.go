//go:build linux

package control

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/store"
	"codex-feishu/internal/verification"
)

type controlProbe struct{ begins, reads atomic.Int32 }

func (p *controlProbe) Begin(_ context.Context, id string) (verification.Result, error) {
	p.begins.Add(1)
	return probeResult(id), nil
}
func (p *controlProbe) State(_ context.Context, id string) (verification.Result, error) {
	p.reads.Add(1)
	return probeResult(id), nil
}
func probeResult(id string) verification.Result {
	return verification.Result{RequestID: id, BridgeCommit: strings.Repeat("a", 40), BinarySHA256: strings.Repeat("b", 64), State: "confirmed", CardStatus: "pass", CallbackStatus: "pending", PrivateMessageStatus: "pending", MessageID: "om_card", ExpiresAtMS: time.Now().Add(time.Minute).UnixMilli()}
}

func TestManagedCardOverRealUnixSocketAndMethodBoundaries(t *testing.T) {
	path, identity := controlFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := Listen(ctx, path, identity, func(context.Context) (Snapshot, error) { return snapshot(), nil })
	if err != nil {
		t.Fatal(err)
	}
	p := &controlProbe{}
	s.Verification = p
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	defer func() { cancel(); s.Close(); <-done }()
	id := store.ID()
	for _, begin := range []bool{true, false} {
		r, err := VerifyCard(ctx, path, identity, id, begin)
		if err != nil || r.RequestID != id || r.CardStatus != "pass" {
			t.Fatal(r, err)
		}
	}
	if p.begins.Load() != 1 || p.reads.Load() != 1 {
		t.Fatal("wrong verification method")
	}
	if _, err := VerifyCard(ctx, path, strings.Repeat("b", 64), id, true); err != ErrIdentity || p.begins.Load() != 1 {
		t.Fatal("identity mismatch reached manager", err)
	}
	base := map[string]any{"schema_version": 1, "identity": identity, "method": "verify.card.begin", "verification": map[string]any{"request_id": id}}
	invalid := []map[string]any{
		{"pair": map[string]any{"request_id": id}},
		{"verification": map[string]any{"request_id": id, "owner": "ou_other"}},
		{"verification": map[string]any{"request_id": "invalid"}},
		{"method": "turn/start"},
		{"method": "verify.group.begin"},
		{"verification": map[string]any{"request_id": id, "body": "arbitrary input"}},
	}
	for _, patch := range invalid {
		r := map[string]any{}
		for k, v := range base {
			r[k] = v
		}
		for k, v := range patch {
			r[k] = v
		}
		b, _ := json.Marshal(r)
		wire(t, path, string(b))
		if p.begins.Load() != 1 {
			t.Fatal("unapproved payload reached probe", r)
		}
	}
}

type controlGroupProbe struct{ begins, reads atomic.Int32 }

func (p *controlGroupProbe) Begin(_ context.Context, id string) (verification.GroupResult, error) {
	p.begins.Add(1)
	return verification.GroupResult{RequestID: id, BridgeCommit: "fixture", BinarySHA256: strings.Repeat("b", 64), State: "confirmed", GroupStatus: "pass", MembershipStatus: "pass", GroupMessageStatus: "pending", ChatID: "oc_test", ExpiresAtMS: time.Now().Add(time.Minute).UnixMilli()}, nil
}
func (p *controlGroupProbe) State(_ context.Context, id string) (verification.GroupResult, error) {
	p.reads.Add(1)
	return verification.GroupResult{}, verification.ErrNotFound
}

func TestGroupManagementMethodsCannotCarryCardOrPairPayload(t *testing.T) {
	path, identity := controlFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := Listen(ctx, path, identity, func(context.Context) (Snapshot, error) { return snapshot(), nil })
	if err != nil {
		t.Fatal(err)
	}
	groups := &controlGroupProbe{}
	s.VerificationGroups = groups
	s.Verification = &controlProbe{}
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	defer func() { cancel(); s.Close(); <-done }()
	id := store.ID()
	result, err := VerifyGroup(ctx, path, identity, id, true)
	if err != nil || result.MembershipStatus != "pass" {
		t.Fatal(result, err)
	}
	if _, err := VerifyGroup(ctx, path, identity, store.ID(), false); err != verification.ErrNotFound || groups.begins.Load() != 1 || groups.reads.Load() != 1 {
		t.Fatal("not-found query invoked begin", err)
	}
	for _, extra := range []string{`,"pair":{"request_id":"` + id + `"}`, `,"verification":{"request_id":"` + id + `"}`} {
		body := `{"schema_version":1,"identity":"` + identity + `","method":"verify.group.begin","verification_group":{"request_id":"` + id + `"}` + extra + `}`
		wire(t, path, body)
		if groups.begins.Load() != 1 {
			t.Fatal("mixed authorization reached group creator")
		}
	}
	if _, err := VerifyGroup(ctx, path, strings.Repeat("d", 64), id, true); err != ErrIdentity || groups.begins.Load() != 1 {
		t.Fatal(err)
	}
}
