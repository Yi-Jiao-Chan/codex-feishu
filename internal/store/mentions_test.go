package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/rules"
)

func mentionFixture(t *testing.T, policy rules.Policy, event time.Time) (*Store, string, MentionOptions, Mirror) {
	t.Helper()
	s, path := discoveryStore(t)
	eligible(t, s, "thread")
	if _, err := s.InitNotificationPolicyAt(context.Background(), "app", "ou_user", policy, event.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	m := Mirror{Key: "final", TurnID: "turn", Kind: "final", Card: cards.Message("AI", "Codex", "public result"), Terminal: true, ParentMessageID: "original-input", EventType: "completed", EventAt: event.Unix()}
	stage(t, s, event.UnixMilli()+1000, m)
	return s, path, MentionOptions{Enabled: true, Targets: []string{"ou_user"}, ChatID: "chat", MemberIDs: []string{"ou_user"}}, m
}

func mentionDecision(t *testing.T, s *Store) MentionDecision {
	t.Helper()
	decisions, err := s.MentionDecisions(context.Background(), "host", "thread", "final")
	if err != nil || len(decisions) != 1 {
		t.Fatalf("mention decisions: %+v %v", decisions, err)
	}
	return decisions[0]
}

func mentionClaim(t *testing.T, s *Store, options MentionOptions, at time.Time) MirrorAttempt {
	t.Helper()
	m, err := s.Mirror(context.Background(), "host", "thread", "final")
	if err != nil {
		t.Fatal(err)
	}
	a, claimed, err := s.ClaimMirrorWithMentions(context.Background(), "host", "thread", "final", m.Revision, at.UnixMilli(), options)
	if err != nil || !claimed {
		t.Fatalf("mention claim: %t %v", claimed, err)
	}
	return a
}

func hasMention(card string) bool { return strings.Contains(card, `\u003cat id=`) }

func TestMentionsQuietEventNeverBecomesMorningBacklog(t *testing.T) {
	ctx := context.Background()
	p := rules.Policy{Enabled: true, Timezone: "Asia/Shanghai", QuietStart: "23:00", QuietEnd: "08:00"}
	event := time.Date(2026, 10, 12, 16, 0, 0, 0, time.UTC) // local midnight
	s, _, options, m := mentionFixture(t, p, event)
	if err := s.ScheduleMentions(ctx, "host", "thread", options); err != nil {
		t.Fatal(err)
	}
	if d := mentionDecision(t, s); d.State != "suppressed" || d.Reason != "quiet" {
		t.Fatalf("quiet event was queued: %+v", d)
	}
	bindMirrorThread(t, s)
	a := mentionClaim(t, s, options, event.Add(9*time.Hour))
	if hasMention(a.Card) || a.Card != expectedMarkedCard(t, s, a.ID, m.Card) {
		t.Fatal("quiet event became a morning mention")
	}
	if err := s.FinishMirrorAttempt(ctx, a.ID, "confirmed", "result-message"); err != nil {
		t.Fatal(err)
	}
	if err := s.ScheduleMentions(ctx, "host", "thread", options); err != nil {
		t.Fatal(err)
	}
	if d := mentionDecision(t, s); d.State != "suppressed" || d.Reason != "quiet" {
		t.Fatalf("reconcile resurrected suppressed event: %+v", d)
	}
}

func TestMentionsEligibleThenOffBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	p, _ := rules.DefaultPolicy("UTC")
	event := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	s, _, options, m := mentionFixture(t, p, event)
	if err := s.ScheduleMentions(ctx, "host", "thread", options); err != nil {
		t.Fatal(err)
	}
	if d := mentionDecision(t, s); d.State != "eligible" {
		t.Fatalf("event did not qualify: %+v", d)
	}
	p.Enabled, p.Revision = false, 1
	if err := s.SetNotificationPolicyAt(ctx, "app", "ou_user", 0, p, event.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	bindMirrorThread(t, s)
	a := mentionClaim(t, s, options, event.Add(2*time.Minute))
	if a.Card != expectedMarkedCard(t, s, a.ID, m.Card) || hasMention(a.Card) {
		t.Fatal("queued eligible event ignored later off")
	}
	if d := mentionDecision(t, s); d.State != "suppressed" || d.Reason != "off" {
		t.Fatalf("off did not permanently suppress event: %+v", d)
	}
}

func TestMentionsAuditRetainsOffOnBeforeHistoryObservation(t *testing.T) {
	ctx := context.Background()
	for _, sameSecond := range []bool{false, true} {
		t.Run(map[bool]string{false: "earlier_off", true: "same_second_off_on"}[sameSecond], func(t *testing.T) {
			p, _ := rules.DefaultPolicy("UTC")
			event := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
			s, _, options, _ := mentionFixture(t, p, event)
			offAt, onAt := event.Add(-time.Minute), event.Add(time.Minute)
			if sameSecond {
				offAt, onAt = event, event.Add(500*time.Millisecond)
			}
			p.Enabled, p.Revision = false, 1
			if err := s.SetNotificationPolicyAt(ctx, "app", "ou_user", 0, p, offAt); err != nil {
				t.Fatal(err)
			}
			p.Enabled, p.Revision = true, 2
			if err := s.SetNotificationPolicyAt(ctx, "app", "ou_user", 1, p, onAt); err != nil {
				t.Fatal(err)
			}
			if err := s.ScheduleMentions(ctx, "host", "thread", options); err != nil {
				t.Fatal(err)
			}
			if d := mentionDecision(t, s); d.State != "suppressed" || d.Reason != "off" {
				t.Fatalf("off->on erased original suppression: %+v", d)
			}
		})
	}
}

func TestMentionsFailClosedForReplayUnknownTimeMissingHistoryAndMembers(t *testing.T) {
	ctx := context.Background()
	for _, variant := range []string{"replay", "unknown_time", "milliseconds", "missing_history", "unreachable", "wrong_chat", "disabled", "unsupported_configuration"} {
		t.Run(variant, func(t *testing.T) {
			p, _ := rules.DefaultPolicy("UTC")
			event := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
			s, _, options, m := mentionFixture(t, p, event)
			// First-observation provenance is immutable; replace the initial fixture
			// row before scheduling to model each kind of first observation.
			switch variant {
			case "replay":
				if _, err := s.DB.Exec(`UPDATE mirrors SET replay=1`); err != nil {
					t.Fatal(err)
				}
			case "unknown_time":
				if _, err := s.DB.Exec(`UPDATE mirrors SET event_at=0`); err != nil {
					t.Fatal(err)
				}
			case "milliseconds":
				if _, err := s.DB.Exec(`UPDATE mirrors SET event_at=?`, event.UnixMilli()); err != nil {
					t.Fatal(err)
				}
			case "missing_history":
				if _, err := s.DB.Exec(`DELETE FROM notification_policy_history`); err != nil {
					t.Fatal(err)
				}
			case "unreachable":
				options.MemberIDs = nil
			case "wrong_chat":
				options.ChatID = "unverified-chat"
			case "disabled":
				options.Enabled = false
			case "unsupported_configuration":
				options.MentionOn = []string{"needs_user"}
			}
			if err := s.ScheduleMentions(ctx, "host", "thread", options); err != nil {
				t.Fatal(err)
			}
			bindMirrorThread(t, s)
			a := mentionClaim(t, s, options, event.Add(time.Minute))
			if hasMention(a.Card) || a.Card != expectedMarkedCard(t, s, a.ID, m.Card) || mentionDecision(t, s).State != "suppressed" {
				t.Fatalf("unsafe mention or body lost: %+v %+v", a, mentionDecision(t, s))
			}
		})
	}
}

func TestMentionsPersistFrozenAttemptAcrossReopenAndCorrection(t *testing.T) {
	ctx := context.Background()
	for _, outcome := range []string{"confirmed", "unknown", "rejected", "crash"} {
		t.Run(outcome, func(t *testing.T) {
			p, _ := rules.DefaultPolicy("UTC")
			event := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
			s, path, options, m := mentionFixture(t, p, event)
			if err := s.ScheduleMentions(ctx, "host", "thread", options); err != nil {
				t.Fatal(err)
			}
			bindMirrorThread(t, s)
			a := mentionClaim(t, s, options, event.Add(time.Minute))
			if !hasMention(a.Card) || a.Card == expectedMarkedCard(t, s, a.ID, m.Card) || mentionDecision(t, s).AttemptID != a.ID {
				t.Fatal("rendered mention not frozen with attempt")
			}
			base, err := s.Mirror(ctx, "host", "thread", m.Key)
			if err != nil || base.Card != m.Card {
				t.Fatal("mention contaminated base public mirror")
			}
			if outcome != "crash" {
				remote := ""
				if outcome == "confirmed" {
					remote = "message"
				}
				if err := s.FinishMirrorAttempt(ctx, a.ID, outcome, remote); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err := reopened.RecoverMirrorDispatches(ctx, "host"); err != nil {
				t.Fatal(err)
			}
			wantState := outcome
			if outcome == "crash" {
				wantState = "unknown"
			}
			frozen, err := reopened.MirrorAttempt(ctx, a.ID)
			if err != nil || frozen.State != wantState || frozen.Card != a.Card || mentionDecision(t, reopened).State != wantState {
				t.Fatalf("reopen lost attempt state/payload: %+v %v", frozen, err)
			}
			m.Card, m.Replay = cards.Message("AI", "Codex", "corrected result"), true
			stage(t, reopened, event.UnixMilli()+120000, m)
			options.Targets = append(options.Targets, "ou_new")
			if err := reopened.ScheduleMentions(ctx, "host", "thread", options); err != nil {
				t.Fatal(err)
			}
			if lenMustMentionDecisions(t, reopened) != 1 {
				t.Fatal("reconnect added new target to an old event")
			}
			updated, claimed, err := reopened.ClaimMirrorWithMentions(ctx, "host", "thread", m.Key, 2, event.UnixMilli()+180000, options)
			if outcome == "confirmed" {
				if err != nil || !claimed || updated.Operation != "update" || hasMention(updated.Card) {
					t.Fatalf("correction reminded again: %+v %t %v", updated, claimed, err)
				}
			} else if err != nil || claimed {
				t.Fatalf("unknown/rejected attempt was retried: %t %v", claimed, err)
			}
		})
	}
}

func lenMustMentionDecisions(t *testing.T, s *Store) int {
	t.Helper()
	decisions, err := s.MentionDecisions(context.Background(), "host", "thread", "final")
	if err != nil {
		t.Fatal(err)
	}
	return len(decisions)
}

func TestMentionsClaimRollbackLeavesNoAttemptOrMention(t *testing.T) {
	ctx := context.Background()
	p, _ := rules.DefaultPolicy("UTC")
	event := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	s, _, options, _ := mentionFixture(t, p, event)
	if err := s.ScheduleMentions(ctx, "host", "thread", options); err != nil {
		t.Fatal(err)
	}
	bindMirrorThread(t, s)
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_mention_link BEFORE UPDATE OF attempt_id ON mentions BEGIN SELECT RAISE(ABORT,'fixture journal failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := s.ClaimMirrorWithMentions(ctx, "host", "thread", "final", 1, event.UnixMilli()+60000, options); err == nil || claimed {
		t.Fatalf("failed journal committed: %t %v", claimed, err)
	}
	var attempts int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM outbound_attempts`).Scan(&attempts); err != nil || attempts != 0 || mentionDecision(t, s).State != "eligible" {
		t.Fatalf("partial mention journal survived rollback: %d %v", attempts, err)
	}
}

func TestNotificationPolicyAuditAndCurrentStateRollbackTogether(t *testing.T) {
	ctx := context.Background()
	s, _ := discoveryStore(t)
	p, _ := rules.DefaultPolicy("UTC")
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	if _, err := s.InitNotificationPolicyAt(ctx, "app", "ou_user", p, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_policy_audit BEFORE INSERT ON notification_policy_history BEGIN SELECT RAISE(ABORT,'fixture audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	p.Enabled, p.Revision = false, 1
	if err := s.SetNotificationPolicyAt(ctx, "app", "ou_user", 0, p, now.Add(time.Second)); err == nil {
		t.Fatal("failed policy audit accepted")
	}
	got, err := s.NotificationPolicy(ctx, "app", "ou_user")
	if err != nil || !got.Enabled || got.Revision != 0 {
		t.Fatalf("audit failure changed policy: %+v %v", got, err)
	}
	if _, err := s.DB.Exec(`DROP TRIGGER fail_policy_audit`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNotificationPolicyAt(ctx, "app", "ou_user", 0, p, now.Add(-time.Second)); err == nil {
		t.Fatal("regressing audit clock accepted")
	}
}

func TestMentionsSecondPrecisionRetainsSubsecondSnooze(t *testing.T) {
	ctx := context.Background()
	event := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	p, _ := rules.DefaultPolicy("UTC")
	p.SnoozeUntil = event.Add(500 * time.Millisecond)
	s, _, options, _ := mentionFixture(t, p, event)
	if err := s.ScheduleMentions(ctx, "host", "thread", options); err != nil {
		t.Fatal(err)
	}
	if d := mentionDecision(t, s); d.State != "suppressed" || d.Reason != "snooze" {
		t.Fatalf("subsecond uncertainty lost suppression: %+v", d)
	}
}

func TestMentionsFirstDisabledConfigurationCannotBeReenabledForOldEvent(t *testing.T) {
	ctx := context.Background()
	event := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	p, _ := rules.DefaultPolicy("UTC")
	s, _, options, _ := mentionFixture(t, p, event)
	disabled := MentionOptions{}
	if err := s.ScheduleMentions(ctx, "host", "thread", disabled); err != nil {
		t.Fatal(err)
	}
	if err := s.ScheduleMentions(ctx, "host", "thread", options); err != nil {
		t.Fatal(err)
	}
	if lenMustMentionDecisions(t, s) != 0 {
		t.Fatal("later config created recipients for an old event")
	}
	bindMirrorThread(t, s)
	if a := mentionClaim(t, s, options, event.Add(time.Minute)); hasMention(a.Card) {
		t.Fatal("later config reminded an old event")
	}
}

func TestNotificationPolicyUpgradeBaselineDoesNotInventHistory(t *testing.T) {
	ctx := context.Background()
	event := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	p, _ := rules.DefaultPolicy("UTC")
	s, _, options, _ := mentionFixture(t, p, event)
	if _, err := s.DB.Exec(`DELETE FROM notification_policy_history`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InitNotificationPolicyAt(ctx, "app", "ou_user", p, event.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.ScheduleMentions(ctx, "host", "thread", options); err != nil {
		t.Fatal(err)
	}
	if d := mentionDecision(t, s); d.State != "suppressed" || d.Reason != "policy_history_unknown" {
		t.Fatalf("upgrade invented event-time settings: %+v", d)
	}
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM notification_policy_history`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("upgrade failed to establish current baseline: %d %v", count, err)
	}
}

func TestMentionsFailedEventQualifiesWithoutProgressMentions(t *testing.T) {
	ctx := context.Background()
	event := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	p, _ := rules.DefaultPolicy("UTC")
	s, _, options, _ := mentionFixture(t, p, event)
	if _, err := s.DB.Exec(`UPDATE mirrors SET event_type='failed'`); err != nil {
		t.Fatal(err)
	}
	if err := s.ScheduleMentions(ctx, "host", "thread", options); err != nil {
		t.Fatal(err)
	}
	bindMirrorThread(t, s)
	if a := mentionClaim(t, s, options, event.Add(time.Minute)); !hasMention(a.Card) {
		t.Fatal("failed event did not qualify under defaults")
	}
}
