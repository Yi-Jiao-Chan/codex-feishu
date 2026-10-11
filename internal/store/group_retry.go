package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
)

const GroupRetrySchema = `
CREATE TABLE IF NOT EXISTS group_attempts (
 attempt_id TEXT PRIMARY KEY, intent_id TEXT NOT NULL REFERENCES create_intents(intent_id),
 created_at_ms INTEGER NOT NULL CHECK(created_at_ms>0),
 state TEXT NOT NULL CHECK(state IN ('dispatching','unknown','rejected','confirmed')),
 chat_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS group_attempts_intent ON group_attempts(intent_id,created_at_ms);
CREATE TABLE IF NOT EXISTS group_throttles (
 intent_id TEXT PRIMARY KEY REFERENCES create_intents(intent_id),
 last_attempt_id TEXT NOT NULL REFERENCES group_attempts(attempt_id),
 next_retry_at_ms INTEGER NOT NULL CHECK(next_retry_at_ms>0),
 rejection_count INTEGER NOT NULL CHECK(rejection_count>0)
);
`

type GroupAttempt struct {
	ID, IntentID, State, ChatID string
	CreatedAtMS                 int64
}

// GroupRetryReady is only a read optimization. Claim checks the deadline and
// current eligibility again under its transaction before any remote dispatch.
func (s *Store) GroupRetryReady(ctx context.Context, id string, nowMS int64) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM create_intents i JOIN thread_observations o USING(host_id,thread_id) WHERE i.intent_id=? AND i.state='ready' AND i.chat_id='' AND o.state='eligible' AND o.origin='new' AND NOT EXISTS(SELECT 1 FROM group_throttles t WHERE t.intent_id=i.intent_id AND t.next_retry_at_ms>?)`, id, nowMS).Scan(&n)
	return n == 1, err
}

func (s *Store) GroupRetryDeadline(ctx context.Context, id string) (int64, error) {
	var due int64
	err := s.DB.QueryRowContext(ctx, "SELECT next_retry_at_ms FROM group_throttles WHERE intent_id=?", id).Scan(&due)
	return due, err
}

// ClaimGroupAttempt persists a unique attempt for the intent's frozen request.
// The platform UUID remains intent_id; a definite rejection allows a new local
// attempt, never a new logical group. Unknown requests never become ready.
func (s *Store) ClaimGroupAttempt(ctx context.Context, id string, nowMS int64) (GroupAttempt, bool, error) {
	if nowMS <= 0 {
		return GroupAttempt{}, false, ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return GroupAttempt{}, false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE create_intents SET state='dispatching' WHERE intent_id=? AND state='ready' AND chat_id=''
 AND EXISTS(SELECT 1 FROM thread_observations o WHERE o.host_id=create_intents.host_id AND o.thread_id=create_intents.thread_id AND o.origin='new' AND o.state='eligible' AND o.first_user_item_id<>'')
 AND NOT EXISTS(SELECT 1 FROM bindings b WHERE b.host_id=create_intents.host_id AND b.thread_id=create_intents.thread_id)
 AND NOT EXISTS(SELECT 1 FROM group_attempts a WHERE a.intent_id=create_intents.intent_id AND a.state IN ('dispatching','unknown'))
 AND NOT EXISTS(SELECT 1 FROM group_throttles t WHERE t.intent_id=create_intents.intent_id AND t.next_retry_at_ms>?)`, id, nowMS)
	if err != nil {
		return GroupAttempt{}, false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return GroupAttempt{}, false, err
	}
	a := GroupAttempt{ID: ID(), IntentID: id, State: "dispatching", CreatedAtMS: nowMS}
	if _, err := tx.ExecContext(ctx, `INSERT INTO group_attempts(attempt_id,intent_id,created_at_ms,state) VALUES(?,?,?,'dispatching')`, a.ID, id, nowMS); err != nil {
		return GroupAttempt{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return GroupAttempt{}, false, err
	}
	return a, true, nil
}

// FinishGroupDispatch binds an outcome to the exact dispatch, so a stale result
// cannot complete a later retry. Legacy/readback callers omit attemptID below.
func (s *Store) FinishGroupDispatch(ctx context.Context, a GroupAttempt, state, chat string) error {
	if a.ID == "" || a.IntentID == "" {
		return ErrConflict
	}
	return s.finishGroup(ctx, a.IntentID, a.ID, state, chat)
}

func (s *Store) finishGroup(ctx context.Context, id, attemptID, state, chat string) error {
	if state != "confirmed" && state != "unknown" && state != "rejected" || state == "confirmed" && chat == "" || state != "confirmed" && chat != "" {
		return ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current, currentChat string
	if err := tx.QueryRowContext(ctx, `SELECT state,chat_id FROM create_intents WHERE intent_id=?`, id).Scan(&current, &currentChat); err != nil {
		return err
	}
	if current != "dispatching" && !(state == "confirmed" && current == "unknown") && !(state == "confirmed" && current == "confirmed" && currentChat == chat) {
		return ErrConflict
	}
	// The old intent-only API is unambiguous for a first dispatch or an
	// authoritative unknown readback. After retries, live outcomes need the
	// exact attempt ID; a delayed old caller cannot finish the new dispatch.
	if attemptID == "" && current == "dispatching" {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM group_attempts WHERE intent_id=?", id).Scan(&count); err != nil {
			return err
		}
		if count > 1 {
			return ErrConflict
		}
	}
	if attemptID != "" {
		var recordedIntent, recordedState string
		if err := tx.QueryRowContext(ctx, `SELECT intent_id,state FROM group_attempts WHERE attempt_id=?`, attemptID).Scan(&recordedIntent, &recordedState); err != nil {
			return err
		}
		if recordedIntent != id || recordedState != "dispatching" || current != "dispatching" {
			return ErrConflict
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE group_attempts SET state=?,chat_id=? WHERE intent_id=? AND state IN ('dispatching','unknown')`, state, chat, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE create_intents SET state=?,chat_id=? WHERE intent_id=?`, state, chat, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM group_throttles WHERE intent_id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// DeferGroupAttempt may only follow a typed, definite rate-limit rejection.
// Both the immutable rejected attempt and the reopen/deadline are committed
// together; a failed commit leaves dispatching, for conservative recovery.
func (s *Store) DeferGroupAttempt(ctx context.Context, a GroupAttempt, nowMS, minimumDelayMS int64) error {
	if nowMS <= 0 || minimumDelayMS < 0 {
		return ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var intent, state, chat string
	var created int64
	if err := tx.QueryRowContext(ctx, `SELECT intent_id,state,chat_id,created_at_ms FROM group_attempts WHERE attempt_id=?`, a.ID).Scan(&intent, &state, &chat, &created); err != nil {
		return err
	}
	if intent != a.IntentID || chat != "" || nowMS < created {
		return ErrConflict
	}
	var previous string
	var count int64
	err = tx.QueryRowContext(ctx, `SELECT last_attempt_id,rejection_count FROM group_throttles WHERE intent_id=?`, intent).Scan(&previous, &count)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if state == "rejected" && previous == a.ID {
		return nil
	}
	if state != "dispatching" || count == math.MaxInt64 {
		return ErrConflict
	}
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM create_intents WHERE intent_id=? AND chat_id=''`, intent).Scan(&current); err != nil {
		return err
	}
	if current != "dispatching" {
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO group_throttles(intent_id,last_attempt_id,next_retry_at_ms,rejection_count) VALUES(?,?,?,?) ON CONFLICT(intent_id) DO UPDATE SET last_attempt_id=excluded.last_attempt_id,next_retry_at_ms=excluded.next_retry_at_ms,rejection_count=excluded.rejection_count`, intent, a.ID, nowMS+delay, count); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE group_attempts SET state='rejected' WHERE attempt_id=?`, a.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE create_intents SET state='ready' WHERE intent_id=?`, intent); err != nil {
		return err
	}
	return tx.Commit()
}
