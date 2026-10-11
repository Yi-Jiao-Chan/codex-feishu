package sync

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
)

type mentionMembership struct {
	s     *store.Store
	t     *testing.T
	at    time.Time
	ids   []string
	err   error
	off   bool
	calls int
}

func (g *mentionMembership) MemberIDs(ctx context.Context, chat string) ([]string, error) {
	g.t.Helper()
	g.calls++
	if chat != "chat" {
		g.t.Fatal("membership requested for wrong binding")
	}
	// This read/write would block with the store's single connection if membership
	// RPC ran inside the dispatch transaction. It also models off during lookup.
	p, err := g.s.NotificationPolicy(ctx, "app", "ou_user")
	if err != nil {
		g.t.Fatal(err)
	}
	if g.off {
		next, err := rules.Apply(p, rules.NotificationCommand{Action: rules.NotifyOff}, g.at)
		if err != nil {
			g.t.Fatal(err)
		}
		if err := g.s.SetNotificationPolicyAt(ctx, "app", "ou_user", p.Revision, next, g.at); err != nil {
			g.t.Fatal(err)
		}
	}
	return g.ids, g.err
}

func mentionOutboundFixture(t *testing.T) (*store.Store, string, *outboundGateway, *mentionMembership, Outbound, time.Time) {
	t.Helper()
	ctx := context.Background()
	s, path, _ := outboundFixture(t, "final")
	event := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	base := cards.Message("AI", "Codex", `public result <at id="all"></at>`)
	if _, err := s.DB.Exec(`UPDATE mirrors SET event_type='completed',event_at=?,terminal=1,card_json=?`, event.Unix(), base); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE history_watermarks SET observed_at_ms=?`, event.UnixMilli()+1000); err != nil {
		t.Fatal(err)
	}
	p, _ := rules.DefaultPolicy("UTC")
	if _, err := s.InitNotificationPolicyAt(ctx, "app", "ou_user", p, event.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	options := &store.MentionOptions{Enabled: true, Targets: []string{"ou_user"}}
	if err := s.ScheduleMentions(ctx, "host", "thread", *options); err != nil {
		t.Fatal(err)
	}
	g := &outboundGateway{s: s, t: t}
	member := &mentionMembership{s: s, t: t, at: event.Add(time.Minute), ids: []string{"ou_user"}}
	return s, path, g, member, Outbound{Store: s, Gateway: g, Mentions: options, Membership: member}, event
}

func TestOutboundMentionMembershipAndSendPolicyAreRechecked(t *testing.T) {
	ctx := context.Background()
	for _, variant := range []string{"reachable", "missing_membership", "lookup_error", "not_member", "off_during_lookup"} {
		t.Run(variant, func(t *testing.T) {
			s, _, gateway, member, outbound, event := mentionOutboundFixture(t)
			switch variant {
			case "missing_membership":
				outbound.Membership = nil
				// Configuration cannot smuggle in unverified membership proof.
				outbound.Mentions.ChatID, outbound.Mentions.MemberIDs = "chat", []string{"ou_user"}
			case "lookup_error":
				member.err = errors.New("membership unavailable")
			case "not_member":
				member.ids = []string{"ou_elsewhere"}
			case "off_during_lookup":
				member.off = true
			}
			result, err := outbound.DispatchOne(ctx, "host", "thread", event.UnixMilli()+120000)
			if err != nil || result.State != "confirmed" || len(gateway.calls) != 1 || gateway.calls[0] != "reply" {
				t.Fatalf("body delivery failed: %+v %v", result, err)
			}
			a, err := s.MirrorAttempt(ctx, result.AttemptID)
			if err != nil {
				t.Fatal(err)
			}
			hasMention := strings.Contains(a.Card, `\u003cat id=`)
			if hasMention != (variant == "reachable") {
				t.Fatalf("unsafe/missing mention for %s: %s", variant, a.Card)
			}
			base, err := s.Mirror(ctx, "host", "thread", "document")
			if err != nil || strings.Contains(base.Card, `\u003cat id=`) {
				t.Fatal("mention contaminated public body mirror")
			}
			decisions, err := s.MentionDecisions(ctx, "host", "thread", "document")
			if err != nil || len(decisions) != 1 {
				t.Fatalf("decisions: %+v %v", decisions, err)
			}
			want := "suppressed"
			if variant == "reachable" {
				want = "confirmed"
			}
			if decisions[0].State != want {
				t.Fatalf("mention result not atomic with body: %+v", decisions)
			}
		})
	}
}

func TestOutboundMentionUnknownRejectedAndFailedConfirmationNeverRetry(t *testing.T) {
	ctx := context.Background()
	for _, variant := range []string{"unknown", "rejected", "confirmation_failure"} {
		t.Run(variant, func(t *testing.T) {
			s, path, gateway, member, outbound, event := mentionOutboundFixture(t)
			switch variant {
			case "unknown":
				gateway.err = &feishu.Unknown{Operation: "reply"}
			case "rejected":
				gateway.err = &feishu.Rejected{Code: 230002}
			case "confirmation_failure":
				gateway.closeDB = true
			}
			result, err := outbound.DispatchOne(ctx, "host", "thread", event.UnixMilli()+120000)
			if result.AttemptID == "" || len(gateway.calls) != 1 || (variant == "confirmation_failure") != (err != nil) {
				t.Fatalf("attempt not preserved: %+v %v", result, err)
			}
			if variant != "confirmation_failure" {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			}
			reopened, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err := reopened.RecoverMirrorDispatches(ctx, "host"); err != nil {
				t.Fatal(err)
			}
			a, err := reopened.MirrorAttempt(ctx, result.AttemptID)
			want := "unknown"
			if variant == "rejected" {
				want = "rejected"
			}
			if err != nil || a.State != want || !strings.Contains(a.Card, `\u003cat id=`) {
				t.Fatalf("frozen mention payload lost: %+v %v", a, err)
			}
			decisions, err := reopened.MentionDecisions(ctx, "host", "thread", "document")
			if err != nil || len(decisions) != 1 || decisions[0].State != want || decisions[0].AttemptID != a.ID {
				t.Fatalf("mention link/result lost: %+v %v", decisions, err)
			}
			gateway.s, gateway.closeDB, member.s, outbound.Store = reopened, false, reopened, reopened
			if retry, err := outbound.DispatchOne(ctx, "host", "thread", event.UnixMilli()+180000); err != nil || retry.AttemptID != "" || len(gateway.calls) != 1 {
				t.Fatalf("ambiguous mention resent: %+v %v", retry, err)
			}
		})
	}
}
