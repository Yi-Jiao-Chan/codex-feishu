package sync

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/store"
)

func mirrorAttemptCount(t *testing.T, s *store.Store, key string) int {
	t.Helper()
	var count int
	if err := s.DB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM outbound_attempts WHERE host_id=? AND thread_id=? AND mirror_key=?`, "host", "thread", key).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func latestMirrorAttemptState(s *store.Store, key string) (string, error) {
	var state string
	err := s.DB.QueryRowContext(context.Background(), `SELECT state FROM outbound_attempts WHERE host_id=? AND thread_id=? AND mirror_key=? ORDER BY rowid DESC LIMIT 1`, "host", "thread", key).Scan(&state)
	return state, err
}

func TestOutboundWriteGuardNilKeepsExistingDispatch(t *testing.T) {
	s, _, m := outboundFixture(t, "human")
	g := &outboundGateway{s: s, t: t}
	r, err := (Outbound{Store: s, Gateway: g}).DispatchOne(context.Background(), "host", "thread", 1000)
	if err != nil || r.State != "confirmed" || len(g.calls) != 1 || g.calls[0] != "send" {
		t.Fatalf("nil write guard changed dispatch: %+v %v calls=%v", r, err, g.calls)
	}
	if got, err := s.Mirror(context.Background(), "host", "thread", m.Key); err != nil || got.State != "confirmed" {
		t.Fatalf("nil write guard changed durable state: %+v %v", got, err)
	}
}

func TestOutboundWriteGuardUsesCurrentBindingForSendReplyUpdate(t *testing.T) {
	for _, kind := range []string{"human", "progress", "final"} {
		t.Run(kind, func(t *testing.T) {
			s, _, m := outboundFixture(t, kind)
			g := &outboundGateway{s: s, t: t}
			var grants, releases atomic.Int32
			var badScope, earlyRelease atomic.Bool
			o := Outbound{Store: s, Gateway: g}
			o.BeginWrite = func(parent context.Context, binding store.Binding) (context.Context, func(), error) {
				grants.Add(1)
				if binding.HostID != "host" || binding.ThreadID != "thread" || binding.ChatID != "chat" {
					badScope.Store(true)
				}
				return parent, func() {
					state, err := latestMirrorAttemptState(s, m.Key)
					if err != nil || state != "confirmed" {
						earlyRelease.Store(true)
					}
					releases.Add(1)
				}, nil
			}
			first, err := o.DispatchOne(context.Background(), "host", "thread", 1000)
			if err != nil || first.State != "confirmed" {
				t.Fatalf("initial delivery: %+v %v", first, err)
			}
			m.Card = `{"body":"updated public text"}`
			if err := s.StageMirrors(context.Background(), "host", "thread", []store.Mirror{m}, store.Watermark{ObservedAtMS: 103}); err != nil {
				t.Fatal(err)
			}
			second, err := o.DispatchOne(context.Background(), "host", "thread", 3000)
			if err != nil || second.State != "confirmed" || second.MessageID == "" {
				t.Fatalf("update delivery: %+v %v", second, err)
			}
			wantFirst := "reply"
			if kind == "human" {
				wantFirst = "send"
			}
			if len(g.calls) != 2 || g.calls[0] != wantFirst || g.calls[1] != "update" || grants.Load() != 2 || releases.Load() != 2 || badScope.Load() || earlyRelease.Load() {
				t.Fatalf("write guard scope/release: calls=%v grants=%d releases=%d badScope=%v earlyRelease=%v", g.calls, grants.Load(), releases.Load(), badScope.Load(), earlyRelease.Load())
			}
		})
	}
}

func TestOutboundWriteGuardRefusalKeepsReadyWithoutClaim(t *testing.T) {
	for _, kind := range []string{"human", "progress", "final"} {
		t.Run(kind, func(t *testing.T) {
			s, _, m := outboundFixture(t, kind)
			g := &outboundGateway{s: s, t: t}
			var releases atomic.Int32
			leaseErr := errors.New("lease refused")
			o := Outbound{Store: s, Gateway: g, BeginWrite: func(parent context.Context, _ store.Binding) (context.Context, func(), error) {
				return parent, func() { releases.Add(1) }, leaseErr
			}}
			r, err := o.DispatchOne(context.Background(), "host", "thread", 1000)
			if !errors.Is(err, leaseErr) {
				t.Fatalf("write refusal was not safely reported: %+v %v", r, err)
			}
			got, readErr := s.Mirror(context.Background(), "host", "thread", m.Key)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if got.State != "ready" || mirrorAttemptCount(t, s, m.Key) != 0 || len(g.calls) != 0 || releases.Load() != 1 {
				t.Fatalf("refused delivery crossed claim/native/release boundary: state=%s attempts=%d calls=%d releases=%d", got.State, mirrorAttemptCount(t, s, m.Key), len(g.calls), releases.Load())
			}
		})
	}
}

func TestOutboundWriteGuardCanceledBeforeClaim(t *testing.T) {
	s, _, m := outboundFixture(t, "final")
	g := &outboundGateway{s: s, t: t}
	var releases atomic.Int32
	o := Outbound{Store: s, Gateway: g, BeginWrite: func(context.Context, store.Binding) (context.Context, func(), error) {
		lease, cancel := context.WithCancel(context.Background())
		cancel()
		return lease, func() { releases.Add(1) }, nil
	}}
	r, err := o.DispatchOne(context.Background(), "host", "thread", 1000)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lease was not returned: %+v %v", r, err)
	}
	got, readErr := s.Mirror(context.Background(), "host", "thread", m.Key)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got.State != "ready" || mirrorAttemptCount(t, s, m.Key) != 0 || len(g.calls) != 0 || releases.Load() != 1 {
		t.Fatalf("canceled lease crossed claim/native/release boundary: state=%s attempts=%d calls=%d releases=%d", got.State, mirrorAttemptCount(t, s, m.Key), len(g.calls), releases.Load())
	}
}

// cancelOnSecondErrContext lets the claim complete while making the explicit
// pre-gateway check fail. Its nil Done channel keeps database/sql from
// canceling the claim transaction itself.
type cancelOnSecondErrContext struct {
	context.Context
	checks atomic.Int32
}

func (c *cancelOnSecondErrContext) Done() <-chan struct{} { return nil }

func (c *cancelOnSecondErrContext) Err() error {
	if c.checks.Add(1) > 1 {
		return context.Canceled
	}
	return c.Context.Err()
}

func TestOutboundWriteGuardCanceledAfterClaimDoesNotCallGateway(t *testing.T) {
	s, _, m := outboundFixture(t, "final")
	g := &outboundGateway{s: s, t: t}
	var releases atomic.Int32
	o := Outbound{Store: s, Gateway: g, BeginWrite: func(parent context.Context, _ store.Binding) (context.Context, func(), error) {
		return &cancelOnSecondErrContext{Context: parent}, func() { releases.Add(1) }, nil
	}}
	r, err := o.DispatchOne(context.Background(), "host", "thread", 1000)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("post-claim cancellation was not returned: %+v %v", r, err)
	}
	got, readErr := s.Mirror(context.Background(), "host", "thread", m.Key)
	if readErr != nil {
		t.Fatal(readErr)
	}
	state, stateErr := latestMirrorAttemptState(s, m.Key)
	if stateErr != nil {
		t.Fatal(stateErr)
	}
	if got.State != "dispatching" || state != "dispatching" || r.State != "unknown" || mirrorAttemptCount(t, s, m.Key) != 1 || len(g.calls) != 0 || releases.Load() != 1 {
		t.Fatalf("post-claim cancellation was not conservative: mirror=%s attempt=%s result=%+v attempts=%d calls=%d releases=%d", got.State, state, r, mirrorAttemptCount(t, s, m.Key), len(g.calls), releases.Load())
	}
}

type cancelingOutboundGateway struct {
	entered chan struct{}
	calls   atomic.Int32
}

func (g *cancelingOutboundGateway) wait(ctx context.Context) (string, error) {
	g.calls.Add(1)
	select {
	case <-g.entered:
	default:
		close(g.entered)
	}
	<-ctx.Done()
	return "", ctx.Err()
}

func (g *cancelingOutboundGateway) SendCard(ctx context.Context, _, _, _, _ string) (string, error) {
	return g.wait(ctx)
}

func (g *cancelingOutboundGateway) ReplyCard(ctx context.Context, _, _, _ string) (string, error) {
	return g.wait(ctx)
}

func (g *cancelingOutboundGateway) UpdateCard(ctx context.Context, _, _ string) error {
	_, err := g.wait(ctx)
	return err
}

func TestOutboundWriteGuardCancellationDuringGatewayKeepsDispatching(t *testing.T) {
	s, _, m := outboundFixture(t, "final")
	initial := &outboundGateway{s: s, t: t}
	o := Outbound{Store: s, Gateway: initial}
	if result, err := o.DispatchOne(context.Background(), "host", "thread", 1000); err != nil || result.State != "confirmed" {
		t.Fatalf("initial delivery: %+v %v", result, err)
	}
	m.Card = `{"body":"updated public text"}`
	if err := s.StageMirrors(context.Background(), "host", "thread", []store.Mirror{m}, store.Watermark{ObservedAtMS: 103}); err != nil {
		t.Fatal(err)
	}

	lease, cancel := context.WithCancel(context.Background())
	gw := &cancelingOutboundGateway{entered: make(chan struct{})}
	var releases atomic.Int32
	o.Gateway = gw
	o.BeginWrite = func(context.Context, store.Binding) (context.Context, func(), error) {
		return lease, func() { releases.Add(1) }, nil
	}
	type delivery struct {
		result DeliveryResult
		err    error
	}
	done := make(chan delivery, 1)
	go func() {
		r, err := o.DispatchOne(context.Background(), "host", "thread", 3000)
		done <- delivery{result: r, err: err}
	}()
	<-gw.entered
	cancel()
	got := <-done
	if got.err == nil || got.result.State != "unknown" || got.result.AttemptID == "" || gw.calls.Load() != 1 {
		t.Fatalf("gateway cancellation result: %+v %v calls=%d", got.result, got.err, gw.calls.Load())
	}
	state, err := latestMirrorAttemptState(s, m.Key)
	if err != nil {
		t.Fatal(err)
	}
	if state != "dispatching" || releases.Load() != 1 {
		t.Fatalf("gateway cancellation was not conservatively journalled/released: state=%s releases=%d", state, releases.Load())
	}
}

func TestOutboundWriteGuardBeginWriteCrossingQuietStartSuppressesMention(t *testing.T) {
	ctx := context.Background()
	s, _, g, _, o, event := mentionOutboundFixture(t)
	policy, err := s.NotificationPolicy(ctx, "app", "ou_user")
	if err != nil {
		t.Fatal(err)
	}
	policy.QuietStart, policy.QuietEnd = "23:00", "08:00"
	policy.Revision++
	if err := s.SetNotificationPolicyAt(ctx, "app", "ou_user", policy.Revision-1, policy, event.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	before := time.Date(2026, 10, 12, 22, 59, 59, 0, time.UTC)
	clock := before
	o.Now = func() time.Time { return clock }
	var releases atomic.Int32
	var earlyRelease atomic.Bool
	o.BeginWrite = func(parent context.Context, binding store.Binding) (context.Context, func(), error) {
		if binding.HostID != "host" || binding.ThreadID != "thread" || binding.ChatID != "chat" {
			t.Fatalf("unexpected write binding: %+v", binding)
		}
		clock = before.Add(2 * time.Second)
		return parent, func() {
			state, err := latestMirrorAttemptState(s, "document")
			if err != nil || state != "confirmed" {
				earlyRelease.Store(true)
			}
			releases.Add(1)
		}, nil
	}

	r, err := o.DispatchOne(ctx, "host", "thread", before.UnixMilli())
	if err != nil || r.State != "confirmed" || len(g.calls) != 1 || g.calls[0] != "reply" {
		t.Fatalf("body delivery crossed quiet boundary incorrectly: %+v %v calls=%v", r, err, g.calls)
	}
	a, err := s.MirrorAttempt(ctx, r.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if a.CreatedAtMS != clock.UnixMilli() || strings.Contains(a.Card, `\u003cat id=`) {
		t.Fatalf("lease-time quiet policy was bypassed: created_at=%d clock=%d card=%s", a.CreatedAtMS, clock.UnixMilli(), a.Card)
	}
	decisions, err := s.MentionDecisions(ctx, "host", "thread", "document")
	if err != nil || len(decisions) != 1 || decisions[0].State != "suppressed" {
		t.Fatalf("mention decision was not suppressed with body delivery: %+v %v", decisions, err)
	}
	if releases.Load() != 1 || earlyRelease.Load() {
		t.Fatalf("write lease release count/order: releases=%d early=%v", releases.Load(), earlyRelease.Load())
	}
}
