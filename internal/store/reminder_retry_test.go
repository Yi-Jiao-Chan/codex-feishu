package store

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/rules"
)

func retryRequestFixture(t *testing.T) (*Store, string, MentionOptions, PendingRequest, time.Time) {
	t.Helper()
	p, _ := rules.DefaultPolicy("UTC")
	at := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	s, path, o, r := requestFixture(t, p, at)
	observeRequest(t, s, r, o, at)
	bindMirrorThread(t, s)
	return s, path, o, r, at
}

func reminderDeadline(t *testing.T, s *Store) (int64, int64) {
	t.Helper()
	var due, count int64
	if err := s.DB.QueryRow(`SELECT next_retry_at_ms,rejection_count FROM request_reminder_throttles`).Scan(&due, &count); err != nil {
		t.Fatal(err)
	}
	return due, count
}

func TestReminderRateRetryPersistsDeadlineAndRequiresLiveRequestAfterRestart(t *testing.T) {
	ctx := context.Background()
	s, path, o, r, at := retryRequestFixture(t)
	a := claimRequest(t, s, o, at)
	responseAt := at.Add(3 * time.Second).UnixMilli()
	if err := s.DeferRequestReminder(ctx, a.ID, responseAt, 5000); err != nil {
		t.Fatal(err)
	}
	due, count := reminderDeadline(t, s)
	if due != responseAt+5000 || count != 1 {
		t.Fatal(due, count)
	}
	if err := s.DeferRequestReminder(ctx, a.ID, responseAt+2000, 9000); err != nil {
		t.Fatal(err)
	}
	if next, n := reminderDeadline(t, s); next != due || n != 1 {
		t.Fatal("duplicate rejection advanced deadline", next, n)
	}
	h, err := s.Health(ctx, SchemaVersion, "host", "app")
	if err != nil || h.Requests["reminder_deferred"] != 1 || h.ReminderAttempts["rejected"] != 1 {
		t.Fatal(h, err)
	}
	if _, ok, err := s.ClaimRequestReminder(ctx, "host", "app", "thread", due-1, o); err != nil || ok {
		t.Fatalf("early retry %t %v", ok, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.RecoverRequestReminders(ctx, "host"); err != nil {
		t.Fatal(err)
	}
	if ready, err := s.PrepareRequestRemindersAt(ctx, "host", "app", "thread", due, o); err != nil || ready {
		t.Fatalf("restart invented pending proof %t %v", ready, err)
	}
	// A real replay reaffirms pending state without replacing first eligibility.
	r.Replay = true
	observeRequest(t, s, r, o, time.UnixMilli(due))
	b := claimRequest(t, s, o, time.UnixMilli(due))
	if a.ID == b.ID || a.Card != b.Card || b.ParentMessageID != "original-input" {
		t.Fatal("retry lost fresh UUID or original relationship", a, b)
	}
	if err := s.FinishRequestReminder(ctx, a.ID, "confirmed", "stale-A"); !errors.Is(err, ErrConflict) {
		t.Fatal("old rejection confirmed new attempt", err)
	}
	if err := s.FinishRequestReminder(ctx, a.ID, "rejected", ""); err != nil {
		t.Fatal(err)
	}
	if d := requestDecision(t, s); d.AttemptID != b.ID || d.State != "dispatching" {
		t.Fatal("old result changed B", d)
	}
	if err := s.FinishRequestReminder(ctx, b.ID, "confirmed", "remote-B"); err != nil {
		t.Fatal(err)
	}
	var frozen, state string
	if err := s.DB.QueryRow(`SELECT card_json,state FROM request_reminder_attempts WHERE attempt_id=?`, a.ID).Scan(&frozen, &state); err != nil || frozen != a.Card || state != "rejected" {
		t.Fatal("rejected payload changed", err)
	}
	h, err = s.Health(ctx, SchemaVersion, "host", "app")
	if err != nil || h.Requests["reminder_deferred"] != 0 || h.ReminderAttempts["confirmed"] != 1 {
		t.Fatal(h, err)
	}
}

func TestReminderRateRetryRechecksResolutionPolicyAndRecipientsDuringLongWait(t *testing.T) {
	for _, variant := range []string{"resolved_during_rpc", "terminal_after_rejection", "off_on", "same_ms_off_on", "whole_quiet", "expired_snooze", "target_removed", "event_disabled", "member_removed"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			s, _, o, r, at := retryRequestFixture(t)
			a := claimRequest(t, s, o, at)
			if variant == "resolved_during_rpc" {
				if err := s.ResolveAppRequest(ctx, "host", "app", "thread", r.Key); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.DeferRequestReminder(ctx, a.ID, at.UnixMilli(), int64((25*time.Hour)/time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			p, _ := rules.DefaultPolicy("UTC")
			changeAt := at.Add(time.Hour)
			switch variant {
			case "terminal_after_rejection":
				if err := s.ReconcileAppRequests(ctx, "host", "app", "thread", []string{"turn"}); err != nil {
					t.Fatal(err)
				}
			case "off_on", "same_ms_off_on":
				p.Enabled, p.Revision = false, 1
				if err := s.SetNotificationPolicyAt(ctx, "app", "ou_user", 0, p, changeAt); err != nil {
					t.Fatal(err)
				}
				p.Enabled, p.Revision = true, 2
				if variant != "same_ms_off_on" {
					changeAt = changeAt.Add(time.Hour)
				}
				if err := s.SetNotificationPolicyAt(ctx, "app", "ou_user", 1, p, changeAt); err != nil {
					t.Fatal(err)
				}
			case "whole_quiet":
				p.QuietStart, p.QuietEnd, p.Revision = "23:00", "08:00", 1
				if err := s.SetNotificationPolicyAt(ctx, "app", "ou_user", 0, p, changeAt); err != nil {
					t.Fatal(err)
				}
			case "expired_snooze":
				p.SnoozeUntil, p.Revision = at.Add(2*time.Hour), 1
				if err := s.SetNotificationPolicyAt(ctx, "app", "ou_user", 0, p, changeAt); err != nil {
					t.Fatal(err)
				}
			case "target_removed":
				o.Targets = nil
			case "event_disabled":
				o.MentionOn = []string{"completed"}
			case "member_removed":
				o.MemberIDs = nil
			}
			due, _ := reminderDeadline(t, s)
			if _, ok, err := s.ClaimRequestReminder(ctx, "host", "app", "thread", due, o); err != nil || ok {
				t.Fatalf("suppressed retry %t %v", ok, err)
			}
			d := requestDecision(t, s)
			if d.State != "suppressed" {
				t.Fatal("waiting notification revived", d)
			}
			// Later requests/replays and morning policy no longer resurrect it.
			observeRequest(t, s, r, o, time.UnixMilli(due+1000))
			if _, ok, err := s.ClaimRequestReminder(ctx, "host", "app", "thread", due+1000, o); err != nil || ok {
				t.Fatalf("replayed suppressed reminder %t %v", ok, err)
			}
			h, err := s.Health(ctx, SchemaVersion, "host", "app")
			if err != nil || h.Requests["reminder_deferred"] != 0 {
				t.Fatal(h, err)
			}
		})
	}
}

func TestReminderRetryFreshCardDropsRecipientWithoutChangingRejectedPayload(t *testing.T) {
	ctx := context.Background()
	s, _, o, _, at := retryRequestFixture(t)
	p, _ := rules.DefaultPolicy("UTC")
	if _, err := s.InitNotificationPolicyAt(ctx, "app", "ou_other", p, at.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`INSERT INTO request_mentions(host_id,app_id,thread_id,request_key,open_id,state,reason,policy_revision) VALUES('host','app','thread','request','ou_other','eligible','',0)`); err != nil {
		t.Fatal(err)
	}
	o.Targets, o.MemberIDs = []string{"ou_user", "ou_other"}, []string{"ou_user", "ou_other"}
	a := claimRequest(t, s, o, at)
	if err := s.DeferRequestReminder(ctx, a.ID, at.UnixMilli(), 1000); err != nil {
		t.Fatal(err)
	}
	o.MemberIDs = []string{"ou_user"}
	b := claimRequest(t, s, o, at.Add(time.Second))
	if !strings.Contains(a.Card, "ou_other") || strings.Contains(b.Card, "ou_other") || !strings.Contains(b.Card, "ou_user") || a.ID == b.ID {
		t.Fatal("stale recipient or UUID", a, b)
	}
	if err := s.FinishRequestReminder(ctx, b.ID, "unknown", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ClaimRequestReminder(ctx, "host", "app", "thread", at.Add(time.Hour).UnixMilli(), o); err != nil || ok {
		t.Fatalf("unknown retry %t %v", ok, err)
	}
}

func TestReminderRetryBackoffOverflowAndExactProofRollback(t *testing.T) {
	ctx := context.Background()
	s, _, o, _, at := retryRequestFixture(t)
	for n, delay := range []int64{1000, 2000, 4000, 8000, 16000, 32000, 60000, 60000} {
		a := claimRequest(t, s, o, at)
		if err := s.DeferRequestReminder(ctx, a.ID, at.UnixMilli(), 0); err != nil {
			t.Fatal(err)
		}
		due, count := reminderDeadline(t, s)
		if count != int64(n+1) || due-at.UnixMilli() != delay {
			t.Fatal(due, count, n)
		}
		at = time.UnixMilli(due)
	}
	a := claimRequest(t, s, o, at)
	if err := s.DeferRequestReminder(ctx, a.ID, at.UnixMilli(), math.MaxInt64); !errors.Is(err, ErrConflict) {
		t.Fatal("overflow accepted", err)
	}
	if d := requestDecision(t, s); d.State != "dispatching" || d.AttemptID != a.ID {
		t.Fatal(d)
	}
	if _, err := s.DB.Exec(`CREATE TRIGGER ignore_reminder_defer BEFORE UPDATE ON app_requests BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeferRequestReminder(ctx, a.ID, at.UnixMilli(), 1000); !errors.Is(err, ErrConflict) {
		t.Fatal("lost proof accepted", err)
	}
	if _, count := reminderDeadline(t, s); count != 8 {
		t.Fatal("partial backoff committed", count)
	}
	var state string
	if err := s.DB.QueryRow(`SELECT state FROM request_reminder_attempts WHERE attempt_id=?`, a.ID).Scan(&state); err != nil || state != "dispatching" {
		t.Fatal(state, err)
	}
	if _, err := s.DB.Exec(`DROP TRIGGER ignore_reminder_defer`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeferRequestReminder(ctx, a.ID, at.UnixMilli(), 1000); err != nil {
		t.Fatal(err)
	}
}

func TestReminderRetryMigratesSchema14WithoutInventingRejection(t *testing.T) {
	ctx := context.Background()
	s, path, o, _, at := retryRequestFixture(t)
	a := claimRequest(t, s, o, at)
	if _, err := s.DB.Exec(`DROP TABLE request_reminder_throttles; PRAGMA user_version=14`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.RecoverRequestReminders(ctx, "host"); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := s.DB.QueryRow(`SELECT state FROM request_reminder_attempts WHERE attempt_id=?`, a.ID).Scan(&state); err != nil || state != "unknown" {
		t.Fatal(state, err)
	}
	if err := s.DeferRequestReminder(ctx, a.ID, at.Add(time.Second).UnixMilli(), 1000); !errors.Is(err, ErrConflict) {
		t.Fatal("migration retried unknown", err)
	}
}
