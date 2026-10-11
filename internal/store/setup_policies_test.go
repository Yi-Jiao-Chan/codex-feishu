package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"
)

func setupPolicyFixture(t *testing.T, recipients int) (*Store, []NotificationPolicyChange, time.Time) {
	t.Helper()
	s, defaults := notificationStoreFixture(t)
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	ctx := context.Background()
	if _, err := s.BeginBaseline(ctx, "setup-host", "setup-app", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	defaults.Enabled = false
	defaults.SnoozeUntil = time.Date(2026, 10, 14, 9, 0, 0, 123456789, time.FixedZone("local", 8*60*60))
	defaults.QuietStart, defaults.QuietEnd = "21:00", "06:00"
	var changes []NotificationPolicyChange
	for _, user := range []string{"ou_setup_first_valid", "ou_setup_second_valid"}[:recipients] {
		if _, err := s.InitNotificationPolicyAt(ctx, "setup-app", user, defaults, now); err != nil {
			t.Fatal(err)
		}
		next := defaults
		next.QuietStart, next.QuietEnd, next.Timezone, next.Revision = "23:00", "08:00", "UTC", 1
		changes = append(changes, NotificationPolicyChange{OpenID: user, Before: defaults, After: next})
	}
	return s, changes, now.Add(time.Minute)
}

func assertSetupPolicyState(t *testing.T, s *Store, changes []NotificationPolicyChange, applied bool, historyCount int) {
	t.Helper()
	for _, change := range changes {
		got, err := s.NotificationPolicy(context.Background(), "setup-app", change.OpenID)
		want := change.Before
		if applied {
			want = change.After
		}
		if err != nil || !sameNotificationPolicy(got, want) {
			t.Fatalf("policy %s differs: %+v, error %v", change.OpenID, got, err)
		}
	}
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM notification_policy_history`).Scan(&count); err != nil || count != historyCount {
		t.Fatalf("history count = %d, expected %d; error %v", count, historyCount, err)
	}
}

func TestApplyNotificationPoliciesPreservesOffSnoozeAndReplaysExactlyOnce(t *testing.T) {
	s, changes, now := setupPolicyFixture(t, 2)
	changed, err := s.ApplyNotificationPolicies(context.Background(), "setup-host", "setup-app", changes, now)
	if err != nil || !changed {
		t.Fatal("batch not applied", changed, err)
	}
	assertSetupPolicyState(t, s, changes, true, 4)
	changed, err = s.ApplyNotificationPolicies(context.Background(), "setup-host", "setup-app", changes, now.Add(time.Hour))
	if err != nil || changed {
		t.Fatal("replay wrote policy", changed, err)
	}
	assertSetupPolicyState(t, s, changes, true, 4)
	var effective int64
	if err := s.DB.QueryRow(`SELECT effective_at_ms FROM notification_policy_history WHERE open_id=? AND revision=1`, changes[0].OpenID).Scan(&effective); err != nil || effective != now.UnixMilli() {
		t.Fatal("replay altered audit time", effective, err)
	}
}

func TestApplyNotificationPoliciesRejectsFullFieldConflicts(t *testing.T) {
	for _, field := range []string{"enabled", "snooze", "quiet-start", "quiet-end", "timezone", "revision", "missing"} {
		t.Run(field, func(t *testing.T) {
			s, changes, now := setupPolicyFixture(t, 1)
			query := map[string]string{
				"enabled":     `UPDATE notification_rules SET enabled=1`,
				"snooze":      `UPDATE notification_rules SET snooze_until=''`,
				"quiet-start": `UPDATE notification_rules SET quiet_start='22:00'`,
				"quiet-end":   `UPDATE notification_rules SET quiet_end='09:00'`,
				"timezone":    `UPDATE notification_rules SET timezone='UTC'`,
				"revision":    `UPDATE notification_rules SET revision=3`,
				"missing":     `DELETE FROM notification_rules`,
			}[field]
			if _, err := s.DB.Exec(query); err != nil {
				t.Fatal(err)
			}
			changed, err := s.ApplyNotificationPolicies(context.Background(), "setup-host", "setup-app", changes, now)
			if changed || !errors.Is(err, ErrConflict) {
				t.Fatal("unexpected policy accepted", changed, err)
			}
			var count int
			if err := s.DB.QueryRow(`SELECT COUNT(*) FROM notification_policy_history`).Scan(&count); err != nil || count != 1 {
				t.Fatal("conflict wrote audit", count, err)
			}
		})
	}
}

func TestApplyNotificationPoliciesRejectsMixedBatch(t *testing.T) {
	s, changes, now := setupPolicyFixture(t, 2)
	if err := s.SetNotificationPolicyAt(context.Background(), "setup-app", changes[0].OpenID, 0, changes[0].After, now); err != nil {
		t.Fatal(err)
	}
	changed, err := s.ApplyNotificationPolicies(context.Background(), "setup-host", "setup-app", changes, now.Add(time.Minute))
	if changed || !errors.Is(err, ErrConflict) {
		t.Fatal("mixed batch accepted", changed, err)
	}
	got, err := s.NotificationPolicy(context.Background(), "setup-app", changes[1].OpenID)
	if err != nil || !sameNotificationPolicy(got, changes[1].Before) {
		t.Fatal("mixed batch wrote untouched recipient", got, err)
	}
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM notification_policy_history`).Scan(&count); err != nil || count != 3 {
		t.Fatal("mixed batch wrote audit", count, err)
	}
}

func TestApplyNotificationPoliciesRollsBackFirstUserWhenSecondAuditFails(t *testing.T) {
	s, changes, now := setupPolicyFixture(t, 2)
	if _, err := s.DB.Exec(`CREATE TRIGGER reject_second_setup_audit BEFORE INSERT ON notification_policy_history
 WHEN NEW.open_id='ou_setup_second_valid' AND NEW.revision=1
 BEGIN SELECT RAISE(ABORT,'fixture audit failure'); END;`); err != nil {
		t.Fatal(err)
	}
	changed, err := s.ApplyNotificationPolicies(context.Background(), "setup-host", "setup-app", changes, now)
	if err == nil || changed {
		t.Fatal("failed batch reported committed", changed, err)
	}
	assertSetupPolicyState(t, s, changes, false, 2)
}

func TestApplyNotificationPoliciesRejectsDeploymentAndSchema(t *testing.T) {
	for _, fault := range []string{"host", "app", "empty-host", "old-schema", "future-schema"} {
		t.Run(fault, func(t *testing.T) {
			s, changes, now := setupPolicyFixture(t, 1)
			host, app := "setup-host", "setup-app"
			switch fault {
			case "host":
				host = "other-host"
			case "app":
				app = "other-app"
			case "empty-host":
				host = " "
			case "old-schema":
				if _, err := s.DB.Exec(`PRAGMA user_version=8`); err != nil {
					t.Fatal(err)
				}
			case "future-schema":
				if _, err := s.DB.Exec(fmt.Sprintf("PRAGMA user_version=%d", SchemaVersion+1)); err != nil {
					t.Fatal(err)
				}
			}
			changed, err := s.ApplyNotificationPolicies(context.Background(), host, app, changes, now)
			if err == nil || changed {
				t.Fatal("invalid deployment accepted", changed, err)
			}
			assertSetupPolicyState(t, s, changes, false, 1)
		})
	}
}

func TestApplyNotificationPoliciesValidatesBatchBeforeWrites(t *testing.T) {
	for _, fault := range []string{"duplicate", "empty-user", "blank-user", "before-invalid", "after-invalid", "revision", "overflow", "deadline", "time"} {
		t.Run(fault, func(t *testing.T) {
			s, original, now := setupPolicyFixture(t, 1)
			changes := append([]NotificationPolicyChange(nil), original...)
			switch fault {
			case "duplicate":
				changes = append(changes, changes[0])
			case "empty-user":
				changes[0].OpenID = ""
			case "blank-user":
				changes[0].OpenID = " \t"
			case "before-invalid":
				changes[0].Before.Timezone = "invalid"
			case "after-invalid":
				changes[0].After.QuietEnd = changes[0].After.QuietStart
			case "revision":
				changes[0].After.Revision = 2
			case "overflow":
				changes[0].Before.Revision, changes[0].After.Revision = math.MaxInt64, math.MaxInt64
			case "deadline":
				changes[0].After.SnoozeUntil = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			case "time":
				now = time.Time{}
			}
			changed, err := s.ApplyNotificationPolicies(context.Background(), "setup-host", "setup-app", changes, now)
			if err == nil || changed {
				t.Fatal("invalid batch accepted", changed, err)
			}
			assertSetupPolicyState(t, s, original, false, 1)
		})
	}
}

func TestApplyNotificationPoliciesEmptyBatchChecksIdentityAndIsNoop(t *testing.T) {
	s, _, now := setupPolicyFixture(t, 0)
	changed, err := s.ApplyNotificationPolicies(context.Background(), "setup-host", "setup-app", nil, now)
	if err != nil || changed {
		t.Fatal("empty batch was not noop", changed, err)
	}
	changed, err = s.ApplyNotificationPolicies(context.Background(), "foreign-host", "setup-app", nil, now)
	if changed || !errors.Is(err, ErrConflict) {
		t.Fatal("empty batch bypassed identity", changed, err)
	}
}
