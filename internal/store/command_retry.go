package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
)

const CommandRetrySchema = `
CREATE TABLE IF NOT EXISTS command_reply_attempts (
 attempt_id TEXT PRIMARY KEY, host_id TEXT NOT NULL, app_id TEXT NOT NULL, message_id TEXT NOT NULL,
 chat_id TEXT NOT NULL, chat_type TEXT NOT NULL, user_id TEXT NOT NULL, thread_id TEXT NOT NULL, kind TEXT NOT NULL,
 card_json TEXT NOT NULL, created_at_ms INTEGER NOT NULL, dispatched_at_ms INTEGER NOT NULL CHECK(dispatched_at_ms>0),
 state TEXT NOT NULL CHECK(state IN ('dispatching','unknown','rejected','confirmed')),
 remote_id TEXT NOT NULL DEFAULT '',
 FOREIGN KEY(app_id,message_id) REFERENCES command_replies(app_id,message_id)
);
CREATE INDEX IF NOT EXISTS command_reply_attempts_message ON command_reply_attempts(app_id,message_id,dispatched_at_ms);
CREATE TABLE IF NOT EXISTS command_reply_throttles (
 app_id TEXT NOT NULL, message_id TEXT NOT NULL,
 last_attempt_id TEXT NOT NULL REFERENCES command_reply_attempts(attempt_id),
 next_retry_at_ms INTEGER NOT NULL CHECK(next_retry_at_ms>0),
 rejection_count INTEGER NOT NULL CHECK(rejection_count>0),
 PRIMARY KEY(app_id,message_id),
 FOREIGN KEY(app_id,message_id) REFERENCES command_replies(app_id,message_id)
);
`

// Proof writes must affect one row; a trigger/no-op cannot publish a claim,
// deadline or acknowledgement that was never recorded.
func commandWriteOne(ctx context.Context, tx *sql.Tx, query string, args ...any) error {
	result, err := tx.ExecContext(ctx, query, args...)
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
	return nil
}

func (s *Store) commandReplyAttempt(ctx context.Context, tx *sql.Tx, id string) (CommandReply, error) {
	return scanCommandReply(tx.QueryRowContext(ctx, `SELECT `+commandColumns+` FROM command_reply_attempts WHERE attempt_id=?`, id))
}

// DeferCommandReplyAttempt requires a typed, definite rate-limit rejection or
// local preflight evidence that the write API was never called.
// It reopens only delivery, keeping local command effects and the original card
// intact. Rejected payload/UUID evidence is never reused or overwritten.
func (s *Store) DeferCommandReplyAttempt(ctx context.Context, id string, nowMS, minimumDelayMS int64) error {
	if nowMS <= 0 || minimumDelayMS < 0 {
		return ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	a, err := s.commandReplyAttempt(ctx, tx, id)
	if err != nil {
		return err
	}
	if a.RemoteID != "" || nowMS < a.DispatchedAtMS {
		return ErrConflict
	}
	var previous string
	var count int64
	err = tx.QueryRowContext(ctx, `SELECT last_attempt_id,rejection_count FROM command_reply_throttles WHERE app_id=? AND message_id=?`, a.AppID, a.MessageID).Scan(&previous, &count)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if a.State == "rejected" && previous == id {
		return nil
	}
	if a.State != "dispatching" || count == math.MaxInt64 {
		return ErrConflict
	}
	current, err := scanCommandReply(tx.QueryRowContext(ctx, `SELECT `+commandColumns+` FROM command_replies WHERE app_id=? AND message_id=?`, a.AppID, a.MessageID))
	if err != nil {
		return err
	}
	if current.ID != id || current.State != "dispatching" || current.RemoteID != "" || current.HostID != a.HostID {
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
	if err := commandWriteOne(ctx, tx, `INSERT INTO command_reply_throttles(app_id,message_id,last_attempt_id,next_retry_at_ms,rejection_count) VALUES(?,?,?,?,?) ON CONFLICT(app_id,message_id) DO UPDATE SET last_attempt_id=excluded.last_attempt_id,next_retry_at_ms=excluded.next_retry_at_ms,rejection_count=excluded.rejection_count`, a.AppID, a.MessageID, id, nowMS+delay, count); err != nil {
		return err
	}
	if err := commandWriteOne(ctx, tx, `UPDATE command_reply_attempts SET state='rejected' WHERE attempt_id=?`, id); err != nil {
		return err
	}
	if err := commandWriteOne(ctx, tx, `UPDATE command_replies SET state='ready',attempt_id=?,dispatched_at_ms=0 WHERE app_id=? AND message_id=?`, ID(), a.AppID, a.MessageID); err != nil {
		return err
	}
	return tx.Commit()
}
