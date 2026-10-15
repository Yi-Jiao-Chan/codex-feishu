package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
)

const ReminderRetrySchema = `
CREATE TABLE IF NOT EXISTS request_reminder_throttles (
 host_id TEXT NOT NULL, app_id TEXT NOT NULL, thread_id TEXT NOT NULL, request_key TEXT NOT NULL,
 last_attempt_id TEXT NOT NULL REFERENCES request_reminder_attempts(attempt_id),
 next_retry_at_ms INTEGER NOT NULL CHECK(next_retry_at_ms>0),
 rejection_count INTEGER NOT NULL CHECK(rejection_count>0),
 PRIMARY KEY(host_id,app_id,thread_id,request_key),
 FOREIGN KEY(host_id,app_id,thread_id,request_key) REFERENCES app_requests(host_id,app_id,thread_id,request_key)
);
`

// DeferRequestReminder requires an explicit rate-limit rejection or local
// preflight evidence proving the write API was never called. Resolution
// arriving during its RPC suppresses the
// reminder; rejected payloads remain frozen, and only originally selected
// recipients reopen eligibility for a fresh policy/membership check.
func (s *Store) DeferRequestReminder(ctx context.Context, id string, nowMS, minimumDelayMS int64) error {
	if nowMS <= 0 || minimumDelayMS < 0 {
		return ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var host, app, thread, key, state, remote string
	var created int64
	if err := tx.QueryRowContext(ctx, `SELECT host_id,app_id,thread_id,request_key,state,remote_id,created_at_ms FROM request_reminder_attempts WHERE attempt_id=?`, id).Scan(&host, &app, &thread, &key, &state, &remote, &created); err != nil {
		return err
	}
	if remote != "" || nowMS < created {
		return ErrConflict
	}
	var previous string
	var count int64
	err = tx.QueryRowContext(ctx, `SELECT last_attempt_id,rejection_count FROM request_reminder_throttles WHERE host_id=? AND app_id=? AND thread_id=? AND request_key=?`, host, app, thread, key).Scan(&previous, &count)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if state == "rejected" && previous == id {
		return nil
	}
	if state != "dispatching" || count == math.MaxInt64 {
		return ErrConflict
	}
	var pending, reminder string
	if err := tx.QueryRowContext(ctx, `SELECT state,reminder_state FROM app_requests WHERE host_id=? AND app_id=? AND thread_id=? AND request_key=?`, host, app, thread, key).Scan(&pending, &reminder); err != nil {
		return err
	}
	if reminder != "dispatching" {
		return ErrConflict
	}
	count++
	delay := int64(1000)
	for n := int64(1); n < count && delay < 60000; n++ {
		delay *= 2
	}
	if delay > 60000 {
		delay = 60000
	}
	if minimumDelayMS > delay {
		delay = minimumDelayMS
	}
	if delay > math.MaxInt64-nowMS {
		return ErrConflict
	}
	if err := reminderWriteOne(ctx, tx, `INSERT INTO request_reminder_throttles(host_id,app_id,thread_id,request_key,last_attempt_id,next_retry_at_ms,rejection_count) VALUES(?,?,?,?,?,?,?) ON CONFLICT(host_id,app_id,thread_id,request_key) DO UPDATE SET last_attempt_id=excluded.last_attempt_id,next_retry_at_ms=excluded.next_retry_at_ms,rejection_count=excluded.rejection_count`, host, app, thread, key, id, nowMS+delay, count); err != nil {
		return err
	}
	if err := reminderWriteOne(ctx, tx, `UPDATE request_reminder_attempts SET state='rejected' WHERE attempt_id=?`, id); err != nil {
		return err
	}
	next, mention, reason := "ready", "eligible", ""
	if pending != "pending" {
		next, mention, reason = "suppressed", "suppressed", "resolved"
	}
	if err := reminderWriteOne(ctx, tx, `UPDATE app_requests SET reminder_state=? WHERE host_id=? AND app_id=? AND thread_id=? AND request_key=?`, next, host, app, thread, key); err != nil {
		return err
	}
	var selected int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_mentions WHERE host_id=? AND app_id=? AND thread_id=? AND request_key=? AND attempt_id=? AND state='dispatching'`, host, app, thread, key, id).Scan(&selected); err != nil {
		return err
	}
	if err := reminderWriteCount(ctx, tx, selected, `UPDATE request_mentions SET state=?,reason=?,attempt_id='' WHERE host_id=? AND app_id=? AND thread_id=? AND request_key=? AND attempt_id=? AND state='dispatching'`, mention, reason, host, app, thread, key, id); err != nil {
		return err
	}
	return tx.Commit()
}

func reminderWriteOne(ctx context.Context, tx *sql.Tx, query string, args ...any) error {
	return reminderWriteCount(ctx, tx, 1, query, args...)
}

func reminderWriteCount(ctx context.Context, tx *sql.Tx, expected int64, query string, args ...any) error {
	if expected <= 0 {
		return ErrConflict
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != expected {
		return ErrConflict
	}
	return nil
}

// PrepareRequestRemindersAt prunes local reminder eligibility, including while
// a limit pauses network work. It requires neither group reads nor native RPCs.
// Claim repeats these checks after membership I/O to close intervening changes.
func (s *Store) PrepareRequestRemindersAt(ctx context.Context, host, app, thread string, nowMS int64, options MentionOptions) (bool, error) {
	if nowMS <= 0 {
		return false, ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := requestScope(ctx, tx, host, app); err != nil {
		return false, err
	}
	if err := pruneRequestReminderEligibility(ctx, tx, host, app, thread, nowMS, options); err != nil {
		return false, err
	}
	var ready int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM app_requests r JOIN bindings b ON b.host_id=r.host_id AND b.thread_id=r.thread_id WHERE r.host_id=? AND r.app_id=? AND r.thread_id=? AND r.state='pending' AND r.confirmed_pending=1 AND r.reminder_state='ready'
 AND (r.parent_key='' AND r.parent_message_id<>'' OR EXISTS(SELECT 1 FROM mirrors m WHERE m.host_id=r.host_id AND m.thread_id=r.thread_id AND m.mirror_key=r.parent_key AND m.kind='human' AND m.remote_id<>''))
 AND NOT EXISTS(SELECT 1 FROM request_reminder_throttles t WHERE t.host_id=r.host_id AND t.app_id=r.app_id AND t.thread_id=r.thread_id AND t.request_key=r.request_key AND t.next_retry_at_ms>?)
 AND NOT EXISTS(SELECT 1 FROM request_reminder_attempts a WHERE a.host_id=r.host_id AND a.app_id=r.app_id AND a.thread_id=r.thread_id AND a.request_key=r.request_key AND a.state IN ('dispatching','unknown'))
 AND NOT EXISTS(SELECT 1 FROM app_requests busy WHERE busy.host_id=r.host_id AND busy.thread_id=r.thread_id AND busy.reminder_state='dispatching')
 AND NOT EXISTS(SELECT 1 FROM outbound o WHERE o.host_id=r.host_id AND o.thread_id=r.thread_id AND o.state='dispatching')`, host, app, thread, nowMS).Scan(&ready)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return ready > 0, nil
}

func pruneRequestReminderEligibility(ctx context.Context, tx *sql.Tx, host, app, thread string, nowMS int64, options MentionOptions) error {
	rows, err := tx.QueryContext(ctx, `SELECT r.request_key,r.created_at_ms,r.event_at,r.replay,m.open_id FROM app_requests r JOIN request_mentions m USING(host_id,app_id,thread_id,request_key) WHERE r.host_id=? AND r.app_id=? AND r.thread_id=? AND r.state='pending' AND r.reminder_state IN ('none','ready') AND m.state='eligible'`, host, app, thread)
	if err != nil {
		return err
	}
	type eligible struct {
		key, user      string
		created, event int64
		replay         bool
	}
	var all []eligible
	for rows.Next() {
		var item eligible
		if err := rows.Scan(&item.key, &item.created, &item.event, &item.replay, &item.user); err != nil {
			rows.Close()
			return err
		}
		all = append(all, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range all {
		reason := ""
		var revision int64
		switch {
		case item.replay:
			reason = "replay"
		case item.event <= 0 || item.event > nowMS/1000:
			reason = "event_time_unknown"
		case !options.Enabled:
			reason = "disabled"
		case options.MentionOn != nil && !containsMentionID(options.MentionOn, "needs_user"):
			reason = "event_disabled"
		case !containsMentionID(options.Targets, item.user):
			reason = "target_removed"
		}
		if reason == "" {
			reason, revision, err = mentionPolicyWindowReason(ctx, tx, app, item.user, item.created, nowMS)
			if err != nil {
				return err
			}
		}
		if reason != "" {
			if err := reminderWriteOne(ctx, tx, `UPDATE request_mentions SET state='suppressed',reason=?,policy_revision=? WHERE host_id=? AND app_id=? AND thread_id=? AND request_key=? AND open_id=? AND state='eligible'`, reason, revision, host, app, thread, item.key, item.user); err != nil {
				return err
			}
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE app_requests SET reminder_state='suppressed' WHERE host_id=? AND app_id=? AND thread_id=? AND state='pending' AND reminder_state IN ('none','ready') AND NOT EXISTS(SELECT 1 FROM request_mentions m WHERE m.host_id=app_requests.host_id AND m.app_id=app_requests.app_id AND m.thread_id=app_requests.thread_id AND m.request_key=app_requests.request_key AND m.state='eligible')`, host, app, thread)
	return err
}
