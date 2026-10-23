package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/rules"
)

func notificationStoreFixture(t *testing.T) (*Store, rules.Policy) {
	t.Helper()
	s, err := Open(freshStorePath(t, "notifications.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	p, err := rules.DefaultPolicy("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}

func TestNotificationPolicyInitPreservesExistingSettings(t *testing.T) {
	s, defaults := notificationStoreFixture(t)
	ctx := context.Background()
	initial, err := s.InitNotificationPolicy(ctx, "app", "user", defaults)
	if err != nil || !reflect.DeepEqual(initial, defaults) {
		t.Fatalf("initial policy: %+v %v", initial, err)
	}
	next := initial
	next.Enabled = false
	next.SnoozeUntil = time.Date(2026, 10, 12, 23, 0, 0, 123456789, time.UTC)
	next.QuietStart, next.QuietEnd, next.Revision = "23:00", "08:00", 1
	if err := s.SetNotificationPolicy(ctx, "app", "user", 0, next); err != nil {
		t.Fatal(err)
	}
	defaults.Timezone = "UTC"
	got, err := s.InitNotificationPolicy(ctx, "app", "user", defaults)
	if err != nil || !reflect.DeepEqual(got, next) {
		t.Fatalf("initialization reset existing settings: %+v %v", got, err)
	}
}

func TestNotificationPolicyScopesAndCompareAndSwap(t *testing.T) {
	s, defaults := notificationStoreFixture(t)
	ctx := context.Background()
	for _, key := range [][2]string{{"app1", "user1"}, {"app1", "user2"}, {"app2", "user1"}} {
		if _, err := s.InitNotificationPolicy(ctx, key[0], key[1], defaults); err != nil {
			t.Fatal(err)
		}
	}
	next := defaults
	next.Enabled, next.Revision = false, 1
	if err := s.SetNotificationPolicy(ctx, "app1", "user1", 0, next); err != nil {
		t.Fatal(err)
	}
	stale := defaults
	stale.QuietStart, stale.QuietEnd, stale.Revision = "23:00", "08:00", 1
	if err := s.SetNotificationPolicy(ctx, "app1", "user1", 0, stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale writer accepted: %v", err)
	}
	for _, key := range [][2]string{{"app1", "user1"}, {"app1", "user2"}, {"app2", "user1"}} {
		got, err := s.NotificationPolicy(ctx, key[0], key[1])
		want := defaults
		if key == [2]string{"app1", "user1"} {
			want = next
		}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("policy scope %v: %+v %v", key, got, err)
		}
	}
	if _, err := s.NotificationPolicy(ctx, "app1", "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing lookup: %v", err)
	}
	if err := s.SetNotificationPolicy(ctx, "app1", "missing", 0, next); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing update: %v", err)
	}
}

func TestNotificationPolicyConcurrentWritersCannotLoseUpdates(t *testing.T) {
	s, defaults := notificationStoreFixture(t)
	ctx := context.Background()
	if _, err := s.InitNotificationPolicy(ctx, "app", "user", defaults); err != nil {
		t.Fatal(err)
	}
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			next := defaults
			next.Enabled, next.Revision = false, 1
			err := s.SetNotificationPolicy(ctx, "app", "user", 0, next)
			if err == nil {
				winners.Add(1)
			} else if !errors.Is(err, ErrConflict) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("CAS winners = %d, want 1", winners.Load())
	}
}

func TestNotificationPolicyReopenPreservesAbsoluteDeadlines(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "notifications.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	defaults, err := rules.DefaultPolicy("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	deadlines := []time.Time{
		time.Date(2026, 10, 13, 9, 0, 0, 123456789, time.FixedZone("local", 8*60*60)),
		time.Date(9999, 12, 31, 23, 59, 59, 987654321, time.UTC),
	}
	for i, user := range []string{"subsecond", "year9999"} {
		p := defaults
		p.SnoozeUntil = deadlines[i]
		if _, err := s.InitNotificationPolicy(ctx, "app", user, p); err != nil {
			t.Fatal(err)
		}
		var raw string
		if err := s.DB.QueryRow(`SELECT snooze_until FROM notification_rules WHERE app_id=? AND open_id=?`, "app", user).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if raw != deadlines[i].UTC().Format(time.RFC3339Nano) {
			t.Fatalf("deadline not stored as UTC RFC3339Nano: %q", raw)
		}
	}
	if _, err := s.InitNotificationPolicy(ctx, "app", "unset", defaults); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i, user := range []string{"subsecond", "year9999"} {
		got, err := s.NotificationPolicy(ctx, "app", user)
		if err != nil || !got.SnoozeUntil.Equal(deadlines[i]) || got.SnoozeUntil.Location() != time.UTC {
			t.Fatalf("reopen changed absolute deadline for %s: %+v %v", user, got, err)
		}
		if reason, err := got.Suppression(deadlines[i]); err != nil || reason != rules.NotificationAllowed {
			t.Fatalf("deadline did not expire exactly: %q %v", reason, err)
		}
	}
	unset, err := s.NotificationPolicy(ctx, "app", "unset")
	if err != nil || !unset.SnoozeUntil.IsZero() {
		t.Fatalf("unset snooze changed: %+v %v", unset, err)
	}
}

func TestNotificationPolicyRejectsInvalidWritesWithoutChanges(t *testing.T) {
	s, defaults := notificationStoreFixture(t)
	ctx := context.Background()
	badTimezone, badQuiet, badRevision, badDeadline := defaults, defaults, defaults, defaults
	badTimezone.Timezone = "invalid"
	badQuiet.QuietStart, badQuiet.QuietEnd = "23:00", "23:00"
	badRevision.Revision = 1
	badDeadline.SnoozeUntil = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, invalid := range []rules.Policy{badTimezone, badQuiet, badRevision, badDeadline} {
		if _, err := s.InitNotificationPolicy(ctx, "app", "missing", invalid); err == nil {
			t.Errorf("invalid defaults accepted: %+v", invalid)
		}
		if _, err := s.NotificationPolicy(ctx, "app", "missing"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("invalid defaults wrote a row: %v", err)
		}
	}
	if _, err := s.InitNotificationPolicy(ctx, "app", "user", defaults); err != nil {
		t.Fatal(err)
	}
	badTimezone.Revision, badQuiet.Revision, badDeadline.Revision = 1, 1, 1
	for _, test := range []struct {
		expected int64
		next     rules.Policy
	}{
		{0, badTimezone}, {0, badQuiet}, {0, badDeadline}, {0, defaults},
		{-1, defaults}, {math.MaxInt64, defaults}, {1, badRevision},
	} {
		if err := s.SetNotificationPolicy(ctx, "app", "user", test.expected, test.next); err == nil {
			t.Errorf("invalid update accepted: %+v", test)
		}
		got, err := s.NotificationPolicy(ctx, "app", "user")
		if err != nil || !reflect.DeepEqual(got, defaults) {
			t.Fatalf("invalid update modified policy: %+v %v", got, err)
		}
	}
	for _, key := range [][2]string{{"", "user"}, {"app", ""}, {" ", "user"}, {"app", "\t"}} {
		if _, err := s.InitNotificationPolicy(ctx, key[0], key[1], defaults); err == nil {
			t.Errorf("empty init key accepted: %q", key)
		}
		if _, err := s.NotificationPolicy(ctx, key[0], key[1]); err == nil {
			t.Errorf("empty read key accepted: %q", key)
		}
		next := defaults
		next.Revision = 1
		if err := s.SetNotificationPolicy(ctx, key[0], key[1], 0, next); err == nil {
			t.Errorf("empty update key accepted: %q", key)
		}
	}
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM notification_rules`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("invalid writes created extra rows: %d %v", count, err)
	}
}

func TestNotificationPolicyRejectsCorruptStoredData(t *testing.T) {
	s, defaults := notificationStoreFixture(t)
	ctx := context.Background()
	if _, err := s.InitNotificationPolicy(ctx, "app", "user", defaults); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`UPDATE notification_rules SET enabled=2`,
		`UPDATE notification_rules SET enabled=1,snooze_until='invalid'`,
		`UPDATE notification_rules SET snooze_until='',timezone='invalid'`,
		`UPDATE notification_rules SET timezone='UTC',quiet_start='23:00',quiet_end=''`,
		`UPDATE notification_rules SET quiet_start='',quiet_end='',revision=-1`,
	} {
		if _, err := s.DB.Exec(query); err != nil {
			t.Fatal(err)
		}
		if _, err := s.NotificationPolicy(ctx, "app", "user"); err == nil {
			t.Fatalf("corrupt stored policy accepted after %s", query)
		}
		if _, err := s.InitNotificationPolicy(ctx, "app", "user", defaults); err == nil {
			t.Fatalf("initialization hid corrupt stored policy after %s", query)
		}
	}
}
