package sync

import (
	"context"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

func TestRateLimitedFinalWaitsAcrossRestartThenDeliversLatestBody(t *testing.T) {
	ctx := context.Background()
	s, path, m := outboundFixture(t, "final")
	limited := &feishu.RateLimited{Rejection: &feishu.Rejected{Code: 99991400, HTTPStatus: 429}, RetryAfter: 3 * time.Second}
	g := &outboundGateway{s: s, t: t, err: limited}
	o := Outbound{Store: s, Gateway: g, Now: func() time.Time { return time.UnixMilli(1500) }}
	r, err := o.DispatchOne(ctx, "host", "thread", 1000)
	if err != nil || r.State != "deferred" || r.MessageID != "" || len(g.calls) != 1 {
		t.Fatal("limit lost or misclassified", r, err)
	}
	firstID := r.AttemptID
	old, err := s.MirrorAttempt(ctx, firstID)
	if err != nil || old.State != "rejected" {
		t.Fatal("rate rejection not journalled", old, err)
	}
	if r, err = o.DispatchOne(ctx, "host", "thread", 4499); err != nil || r.AttemptID != "" || len(g.calls) != 1 {
		t.Fatal("did not wait from response receipt", r, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.RecoverMirrorDispatches(ctx, "host"); err != nil {
		t.Fatal(err)
	}
	m.Card = `{"body":"corrected final result"}`
	if err := reopened.StageMirrors(ctx, "host", "thread", []store.Mirror{m}, store.Watermark{ObservedAtMS: 103}); err != nil {
		t.Fatal(err)
	}
	g.s, g.err = reopened, nil
	o.Store = reopened
	if r, err = o.DispatchOne(ctx, "host", "thread", 4500); err != nil || r.State != "confirmed" || r.AttemptID == firstID || len(g.calls) != 2 {
		t.Fatal("final did not resume", r, err)
	}
	latest, err := reopened.MirrorAttempt(ctx, r.AttemptID)
	if err != nil || latest.Card != m.Card || latest.ParentMessageID != "original-input" || latest.Operation != "reply" {
		t.Fatal("retry body or relation incorrect", latest, err)
	}
	if r, err = o.DispatchOne(ctx, "host", "thread", 4501); err != nil || r.AttemptID != "" || len(g.calls) != 2 {
		t.Fatal("final resent after success", r, err)
	}
}

func TestRateLimitedFinalPermanentRetryFailureSendsNotice(t *testing.T) {
	ctx := context.Background()
	s, _, _ := outboundFixture(t, "final")
	limited := &feishu.RateLimited{Rejection: &feishu.Rejected{Code: 99991400, HTTPStatus: 429}, RetryAfter: time.Second}
	rejected := &feishu.Rejected{Code: 230099, HTTPStatus: 400, RequestID: "req-after-rate-limit"}
	g := &outboundGateway{s: s, t: t, errs: []error{limited, rejected, nil}}
	clock := time.UnixMilli(1000)
	o := Outbound{Store: s, Gateway: g, Now: func() time.Time { return clock }}
	first, err := o.DispatchOne(ctx, "host", "thread", clock.UnixMilli())
	if err != nil || first.State != "deferred" {
		t.Fatalf("initial rate limit: %+v %v", first, err)
	}
	clock = time.UnixMilli(2000)
	retry, err := o.DispatchOne(ctx, "host", "thread", clock.UnixMilli())
	if err != nil || retry.State != "rejected" {
		t.Fatalf("permanent retry rejection: %+v %v", retry, err)
	}
	attempt, err := s.MirrorAttempt(ctx, retry.AttemptID)
	if err != nil || attempt.RetryOfAttemptID != first.AttemptID {
		t.Fatalf("rate retry ancestry was not frozen: %+v %v", attempt, err)
	}
	notice, err := o.DispatchOne(ctx, "host", "thread", clock.UnixMilli()+1)
	if err != nil || notice.State != "confirmed" || !strings.HasPrefix(notice.Key, "ui:delivery-failure:") ||
		!strings.Contains(g.cards[2], "req-after-rate-limit") {
		t.Fatalf("rate retry failure notice: %+v %v card=%q", notice, err, g.cards[2])
	}
}

func TestRateLimitedUpdateEventuallyPatchesSameMessage(t *testing.T) {
	ctx := context.Background()
	s, _, m := outboundFixture(t, "progress")
	g := &outboundGateway{s: s, t: t}
	clock := time.UnixMilli(1000)
	o := Outbound{Store: s, Gateway: g, Now: func() time.Time { return clock }}
	if r, err := o.DispatchOne(ctx, "host", "thread", 1000); err != nil || r.State != "confirmed" {
		t.Fatal(r, err)
	}
	m.Card = `{"body":"new progress"}`
	if err := s.StageMirrors(ctx, "host", "thread", []store.Mirror{m}, store.Watermark{ObservedAtMS: 103}); err != nil {
		t.Fatal(err)
	}
	g.err = &feishu.RateLimited{Rejection: &feishu.Rejected{Code: 230020, HTTPStatus: 400}, RetryAfter: time.Second}
	clock = time.UnixMilli(3000)
	if r, err := o.DispatchOne(ctx, "host", "thread", 3000); err != nil || r.State != "deferred" {
		t.Fatal(r, err)
	}
	g.err = nil
	if r, err := o.DispatchOne(ctx, "host", "thread", 5000); err != nil || r.State != "confirmed" || r.MessageID != "remote-message" {
		t.Fatal(r, err)
	}
	if len(g.calls) != 3 || g.calls[0] != "reply" || g.calls[1] != "update" || g.calls[2] != "update" {
		t.Fatal("update created extra card", g.calls)
	}
}

func TestRateDeferredMentionDoesNotPollMembershipAndHonorsNewOffRule(t *testing.T) {
	ctx := context.Background()
	s, _, g, membership, o, event := mentionOutboundFixture(t)
	o.Now = func() time.Time { return event.Add(2 * time.Second) }
	g.err = &feishu.RateLimited{Rejection: &feishu.Rejected{Code: 99991400, HTTPStatus: 429}, RetryAfter: 5 * time.Second}
	if r, err := o.DispatchOne(ctx, "host", "thread", event.Add(time.Second).UnixMilli()); err != nil || r.State != "deferred" || membership.calls != 1 {
		t.Fatal(r, err)
	}
	if r, err := o.DispatchOne(ctx, "host", "thread", event.Add(6*time.Second).UnixMilli()); err != nil || r.AttemptID != "" || membership.calls != 1 || len(g.calls) != 1 {
		t.Fatal("waiting request polled membership or sent early", r, err)
	}
	policy, err := s.NotificationPolicy(ctx, "app", "ou_user")
	if err != nil {
		t.Fatal(err)
	}
	policy.Enabled = false
	policy.Revision++
	if err := s.SetNotificationPolicyAt(ctx, "app", "ou_user", policy.Revision-1, policy, event.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	g.err = nil
	r, err := o.DispatchOne(ctx, "host", "thread", event.Add(7*time.Second).UnixMilli())
	if err != nil || r.State != "confirmed" || membership.calls != 2 || len(g.calls) != 2 {
		t.Fatal("retry did not revalidate/send", r, err)
	}
	a, err := s.MirrorAttempt(ctx, r.AttemptID)
	if err != nil || strings.Contains(a.Card, `\u003cat id=`) {
		t.Fatal("off rule ignored by delayed reply", err)
	}
	decisions, err := s.MentionDecisions(ctx, "host", "thread", "document")
	if err != nil || len(decisions) != 1 || decisions[0].State != "suppressed" {
		t.Fatal(decisions, err)
	}
}

type retryMembershipFunc func(context.Context, string) ([]string, error)

func (f retryMembershipFunc) MemberIDs(ctx context.Context, chat string) ([]string, error) {
	return f(ctx, chat)
}

func TestSlowMembershipLookupCrossingQuietStartSuppressesMention(t *testing.T) {
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
	o.Membership = retryMembershipFunc(func(context.Context, string) ([]string, error) {
		clock = before.Add(2 * time.Second)
		return []string{"ou_user"}, nil
	})
	r, err := o.DispatchOne(ctx, "host", "thread", before.UnixMilli())
	if err != nil || r.State != "confirmed" || len(g.calls) != 1 {
		t.Fatal(r, err)
	}
	a, err := s.MirrorAttempt(ctx, r.AttemptID)
	if err != nil || a.CreatedAtMS != clock.UnixMilli() || strings.Contains(a.Card, `\u003cat id=`) {
		t.Fatal("stale membership-start clock bypassed quiet", a.CreatedAtMS, err)
	}
}
