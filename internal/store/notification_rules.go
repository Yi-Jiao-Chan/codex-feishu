package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"codex-feishu/internal/rules"
)

// InitNotificationPolicy creates a recipient's initial policy only if absent.
// Existing settings are returned unchanged, even if the defaults have changed.
func (s *Store) InitNotificationPolicy(ctx context.Context, app, user string, defaults rules.Policy) (rules.Policy, error) {
	return s.InitNotificationPolicyAt(ctx, app, user, defaults, time.Now())
}

// InitNotificationPolicyAt records the first trustworthy policy observation.
// Existing pre-upgrade settings get a baseline at now, never invented history.
func (s *Store) InitNotificationPolicyAt(ctx context.Context, app, user string, defaults rules.Policy, now time.Time) (rules.Policy, error) {
	if err := notificationKeys(app, user); err != nil {
		return rules.Policy{}, err
	}
	if defaults.Revision != 0 {
		return rules.Policy{}, errors.New("initial notification revision must be zero")
	}
	if now.UnixMilli() <= 0 {
		return rules.Policy{}, errors.New("invalid notification policy time")
	}
	snooze, err := notificationPolicyValues(defaults)
	if err != nil {
		return rules.Policy{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return rules.Policy{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO notification_rules(app_id,open_id,enabled,snooze_until,quiet_start,quiet_end,timezone,revision) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(app_id,open_id) DO NOTHING`, app, user, defaults.Enabled, snooze, defaults.QuietStart, defaults.QuietEnd, defaults.Timezone, defaults.Revision)
	if err != nil {
		return rules.Policy{}, err
	}
	p, err := scanNotificationPolicy(tx.QueryRowContext(ctx, `SELECT enabled,snooze_until,quiet_start,quiet_end,timezone,revision FROM notification_rules WHERE app_id=? AND open_id=?`, app, user))
	if err != nil {
		return rules.Policy{}, err
	}
	if err := auditNotificationPolicy(ctx, tx, app, user, p, now.UnixMilli(), true); err != nil {
		return rules.Policy{}, err
	}
	if err := tx.Commit(); err != nil {
		return rules.Policy{}, err
	}
	return p, nil
}

// NotificationPolicy returns sql.ErrNoRows for an uninitialized recipient.
// Corrupt stored policy is an error, never an implicit permissive default.
func (s *Store) NotificationPolicy(ctx context.Context, app, user string) (rules.Policy, error) {
	if err := notificationKeys(app, user); err != nil {
		return rules.Policy{}, err
	}
	return scanNotificationPolicy(s.DB.QueryRowContext(ctx, `SELECT enabled,snooze_until,quiet_start,quiet_end,timezone,revision FROM notification_rules WHERE app_id=? AND open_id=?`, app, user))
}

// SetNotificationPolicy compares the persisted revision before replacing policy.
// The caller must authorize the app/user identity; this storage layer does not.
func (s *Store) SetNotificationPolicy(ctx context.Context, app, user string, expectedRevision int64, next rules.Policy) error {
	return s.SetNotificationPolicyAt(ctx, app, user, expectedRevision, next, time.Now())
}

func (s *Store) SetNotificationPolicyAt(ctx context.Context, app, user string, expectedRevision int64, next rules.Policy, now time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := setNotificationPolicyTx(ctx, tx, app, user, expectedRevision, next, now); err != nil {
		return err
	}
	return tx.Commit()
}

// setNotificationPolicyTx lets command effects and the inbound marker commit
// together. It must be called inside the caller's short local transaction.
func setNotificationPolicyTx(ctx context.Context, tx *sql.Tx, app, user string, expectedRevision int64, next rules.Policy, now time.Time) error {
	if err := notificationKeys(app, user); err != nil {
		return err
	}
	if expectedRevision < 0 || expectedRevision == math.MaxInt64 || next.Revision != expectedRevision+1 {
		return errors.New("notification revision must advance by one without overflow")
	}
	if now.UnixMilli() <= 0 {
		return errors.New("invalid notification policy time")
	}
	snooze, err := notificationPolicyValues(next)
	if err != nil {
		return err
	}
	var currentRevision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM notification_rules WHERE app_id=? AND open_id=?`, app, user).Scan(&currentRevision); errors.Is(err, sql.ErrNoRows) {
		return ErrConflict
	} else if err != nil {
		return err
	} else if currentRevision != expectedRevision {
		return ErrConflict
	}
	var latest sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(effective_at_ms) FROM notification_policy_history WHERE app_id=? AND open_id=?`, app, user).Scan(&latest); err != nil {
		return err
	}
	if latest.Valid && now.UnixMilli() < latest.Int64 {
		return errors.New("notification policy time must not regress")
	}
	result, err := tx.ExecContext(ctx, `UPDATE notification_rules SET enabled=?,snooze_until=?,quiet_start=?,quiet_end=?,timezone=?,revision=? WHERE app_id=? AND open_id=? AND revision=?`, next.Enabled, snooze, next.QuietStart, next.QuietEnd, next.Timezone, next.Revision, app, user, expectedRevision)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrConflict
	}
	return auditNotificationPolicy(ctx, tx, app, user, next, now.UnixMilli(), false)
}

func auditNotificationPolicy(ctx context.Context, tx *sql.Tx, app, user string, p rules.Policy, nowMS int64, initialize bool) error {
	snooze, err := notificationPolicyValues(p)
	if err != nil {
		return err
	}
	query := `INSERT INTO notification_policy_history(app_id,open_id,revision,effective_at_ms,enabled,snooze_until,quiet_start,quiet_end,timezone) VALUES(?,?,?,?,?,?,?,?,?)`
	if initialize {
		query += ` ON CONFLICT(app_id,open_id,revision) DO NOTHING`
	}
	_, err = tx.ExecContext(ctx, query, app, user, p.Revision, nowMS, p.Enabled, snooze, p.QuietStart, p.QuietEnd, p.Timezone)
	return err
}

func notificationKeys(app, user string) error {
	if strings.TrimSpace(app) == "" || strings.TrimSpace(user) == "" {
		return errors.New("notification app and user must be nonempty")
	}
	return nil
}

func notificationPolicyValues(p rules.Policy) (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	if p.SnoozeUntil.IsZero() {
		return "", nil
	}
	// MarshalText uses RFC3339Nano and rejects unrepresentable years. UnixNano
	// would overflow for otherwise valid deadlines such as year 9999.
	text, err := p.SnoozeUntil.UTC().MarshalText()
	if err != nil {
		return "", fmt.Errorf("invalid notification deadline: %w", err)
	}
	return string(text), nil
}

func scanNotificationPolicy(row interface{ Scan(...any) error }) (rules.Policy, error) {
	var p rules.Policy
	var enabled int
	var snooze string
	if err := row.Scan(&enabled, &snooze, &p.QuietStart, &p.QuietEnd, &p.Timezone, &p.Revision); err != nil {
		return rules.Policy{}, err
	}
	if enabled != 0 && enabled != 1 {
		return rules.Policy{}, errors.New("invalid stored notification enabled value")
	}
	p.Enabled = enabled == 1
	if snooze != "" {
		at, err := time.Parse(time.RFC3339Nano, snooze)
		if err != nil {
			return rules.Policy{}, fmt.Errorf("invalid stored notification deadline: %w", err)
		}
		p.SnoozeUntil = at.UTC()
	}
	if err := p.Validate(); err != nil {
		return rules.Policy{}, err
	}
	return p, nil
}
