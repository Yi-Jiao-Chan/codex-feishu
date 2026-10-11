package store

import (
	"context"
	"testing"
	"time"

	"codex-feishu/internal/rules"
)

func TestMirrorMentionWaitingAcrossWholeSuppressionWindowNeverCreatesMorningBacklog(t *testing.T) {
	for _, variant := range []string{"daily_quiet", "off_on", "same_ms_off_on", "expired_snooze", "quiet_removed", "missing_history", "allowed"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			at := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
			p, _ := rules.DefaultPolicy("UTC")
			if variant == "daily_quiet" {
				p.QuietStart, p.QuietEnd = "23:00", "08:00"
			}
			s, _, options, m := mentionFixture(t, p, at)
			if err := s.ScheduleMentions(ctx, "host", "thread", options); err != nil {
				t.Fatal(err)
			}
			if d := mentionDecision(t, s); d.State != "eligible" {
				t.Fatal(d)
			}
			change := at.Add(time.Hour)
			switch variant {
			case "off_on", "same_ms_off_on":
				p.Enabled, p.Revision = false, 1
				if err := s.SetNotificationPolicyAt(ctx, "app", "ou_user", 0, p, change); err != nil {
					t.Fatal(err)
				}
				p.Enabled, p.Revision = true, 2
				if variant != "same_ms_off_on" {
					change = change.Add(time.Hour)
				}
				if err := s.SetNotificationPolicyAt(ctx, "app", "ou_user", 1, p, change); err != nil {
					t.Fatal(err)
				}
			case "expired_snooze":
				p.SnoozeUntil, p.Revision = at.Add(2*time.Hour), 1
				if err := s.SetNotificationPolicyAt(ctx, "app", "ou_user", 0, p, change); err != nil {
					t.Fatal(err)
				}
			case "quiet_removed":
				p.QuietStart, p.QuietEnd, p.Revision = "11:00", "12:00", 1
				if err := s.SetNotificationPolicyAt(ctx, "app", "ou_user", 0, p, change); err != nil {
					t.Fatal(err)
				}
				p.QuietStart, p.QuietEnd, p.Revision = "", "", 2
				if err := s.SetNotificationPolicyAt(ctx, "app", "ou_user", 1, p, change.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
			case "missing_history":
				if _, err := s.DB.Exec(`DELETE FROM notification_policy_history`); err != nil {
					t.Fatal(err)
				}
			}
			bindMirrorThread(t, s)
			a := mentionClaim(t, s, options, at.Add(25*time.Hour))
			if hasMention(a.Card) != (variant == "allowed") {
				t.Fatalf("delayed mention %s: %s", variant, a.Card)
			}
			if variant != "allowed" && a.Card != m.Card {
				t.Fatal("public body changed")
			}
			if err := s.FinishMirrorAttempt(ctx, a.ID, "confirmed", "result"); err != nil {
				t.Fatal(err)
			}
			if err := s.ScheduleMentions(ctx, "host", "thread", options); err != nil {
				t.Fatal(err)
			}
			if d := mentionDecision(t, s); variant != "allowed" && d.State != "suppressed" {
				t.Fatal("suppression revived", d)
			}
		})
	}
}
