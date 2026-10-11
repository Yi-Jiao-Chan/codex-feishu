package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"
	"time"

	"codex-feishu/internal/rules"
)

type NotificationPolicyChange struct {
	OpenID        string
	Before, After rules.Policy
}

func sameNotificationPolicy(a, b rules.Policy) bool {
	return a.Enabled == b.Enabled && a.SnoozeUntil.Equal(b.SnoozeUntil) &&
		a.QuietStart == b.QuietStart && a.QuietEnd == b.QuietEnd &&
		a.Timezone == b.Timezone && a.Revision == b.Revision
}

// ApplyNotificationPolicies atomically applies a planned batch, or recognizes
// its complete replay. The caller must hold the application and listener locks.
// It never adopts a mixed batch or an unrelated update as a successful replay.
func (s *Store) ApplyNotificationPolicies(ctx context.Context, host, app string, changes []NotificationPolicyChange, now time.Time) (bool, error) {
	if strings.TrimSpace(host) == "" || strings.TrimSpace(app) == "" {
		return false, ErrConflict
	}
	if now.UnixMilli() <= 0 {
		return false, errors.New("invalid notification policy time")
	}
	seen := make(map[string]bool, len(changes))
	for _, change := range changes {
		if notificationKeys(app, change.OpenID) != nil || seen[change.OpenID] {
			return false, errors.New("notification recipients must be valid and unique")
		}
		seen[change.OpenID] = true
		if _, err := notificationPolicyValues(change.Before); err != nil {
			return false, err
		}
		if _, err := notificationPolicyValues(change.After); err != nil {
			return false, err
		}
		if change.Before.Revision == math.MaxInt64 || change.After.Revision != change.Before.Revision+1 {
			return false, errors.New("notification revision must advance by one without overflow")
		}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var schema int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&schema); err != nil {
		return false, err
	}
	if schema != SchemaVersion {
		return false, errors.New("notification setup requires the current database schema")
	}
	var recordedApp string
	if err := tx.QueryRowContext(ctx, "SELECT app_id FROM deployment WHERE host_id=?", host).Scan(&recordedApp); errors.Is(err, sql.ErrNoRows) {
		return false, ErrConflict
	} else if err != nil {
		return false, err
	} else if recordedApp != app {
		return false, ErrConflict
	}
	allBefore, allAfter := true, true
	for _, change := range changes {
		current, err := scanNotificationPolicy(tx.QueryRowContext(ctx, `SELECT enabled,snooze_until,quiet_start,quiet_end,timezone,revision FROM notification_rules WHERE app_id=? AND open_id=?`, app, change.OpenID))
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrConflict
		} else if err != nil {
			return false, err
		}
		allBefore = allBefore && sameNotificationPolicy(current, change.Before)
		allAfter = allAfter && sameNotificationPolicy(current, change.After)
	}
	if allAfter {
		return false, nil
	}
	if !allBefore {
		return false, ErrConflict
	}
	for _, change := range changes {
		if err := setNotificationPolicyTx(ctx, tx, app, change.OpenID, change.Before.Revision, change.After, now); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
