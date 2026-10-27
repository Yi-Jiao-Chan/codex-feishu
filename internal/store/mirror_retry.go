package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
)

const MirrorRetrySchema = `
CREATE TABLE IF NOT EXISTS outbound_throttles (
 host_id TEXT NOT NULL, thread_id TEXT NOT NULL, mirror_key TEXT NOT NULL,
 last_attempt_id TEXT NOT NULL REFERENCES outbound_attempts(attempt_id),
 next_retry_at_ms INTEGER NOT NULL CHECK(next_retry_at_ms>0),
 rejection_count INTEGER NOT NULL CHECK(rejection_count>0),
 PRIMARY KEY(host_id,thread_id,mirror_key),
 FOREIGN KEY(host_id,thread_id,mirror_key) REFERENCES outbound(host_id,thread_id,mirror_key)
);
`

// DeferMirrorAttempt may ONLY be called after an explicit Feishu rate-limit
// rejection that proves this write was not accepted. A timeout, transport error,
// ambiguous response, or permanent rejection must never use this retry path.
// The rejected attempt retains an immutable payload hash; the next claim
// freezes a fresh UUID and the latest mirror, and reevaluates the rejected
// attempt's mentions.
// A positive minimumDelayMS is an authoritative server floor, without a cap.
func (s *Store) DeferMirrorAttempt(ctx context.Context, id string, nowMS, minimumDelayMS int64) error {
	if nowMS <= 0 || minimumDelayMS < 0 {
		return errors.New("invalid mirror retry time")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	a, err := scanAttempt(tx.QueryRowContext(ctx, `SELECT `+attemptColumns+` FROM outbound_attempts WHERE attempt_id=?`, id))
	if err != nil {
		return err
	}
	var previousID string
	var count int64
	err = tx.QueryRowContext(ctx, `SELECT last_attempt_id,rejection_count FROM outbound_throttles WHERE host_id=? AND thread_id=? AND mirror_key=?`, a.HostID, a.ThreadID, a.Key).Scan(&previousID, &count)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if a.State == "rejected" && a.RemoteID == "" && previousID == id {
		return nil // Durable proof of this exact deferral; never advance on replay.
	}
	if a.State != "dispatching" || a.RemoteID != "" {
		return ErrConflict
	}
	if nowMS < a.CreatedAtMS {
		return errors.New("mirror retry time precedes dispatch")
	}
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound o JOIN outbound_attempts a USING(host_id,thread_id,mirror_key) WHERE o.host_id=? AND o.thread_id=? AND o.mirror_key=? AND o.state='dispatching' AND a.state='dispatching'`, a.HostID, a.ThreadID, a.Key).Scan(&active); err != nil {
		return err
	}
	if active != 1 {
		return ErrConflict
	}
	if count == math.MaxInt64 {
		return errors.New("mirror rejection count overflow")
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
		return errors.New("mirror retry deadline overflow")
	}
	if err = mirrorWriteOne(ctx, tx, `INSERT INTO outbound_throttles(host_id,thread_id,mirror_key,last_attempt_id,next_retry_at_ms,rejection_count) VALUES(?,?,?,?,?,?) ON CONFLICT(host_id,thread_id,mirror_key) DO UPDATE SET last_attempt_id=excluded.last_attempt_id,next_retry_at_ms=excluded.next_retry_at_ms,rejection_count=excluded.rejection_count`, a.HostID, a.ThreadID, a.Key, id, nowMS+delay, count); err != nil {
		return err
	}
	hash := a.CardHash
	if hash == "" && a.Card != "" {
		hash = cardHash(a.Card)
	}
	cotHash := a.COTPayloadHash
	if cotHash == "" && a.COTStep != "" {
		cotHash = cotPayloadHash(a.COTEvents, a.COTState)
	}
	if err = mirrorWriteOne(ctx, tx, `UPDATE outbound_attempts SET state='rejected',card_hash=?,cot_payload_hash=?,card_json='',cot_events_json='[]',cot_state_json='{}' WHERE attempt_id=?`, hash, cotHash, id); err != nil {
		return err
	}
	retryOf := a.RetryOfAttemptID
	if retryOf == "" {
		retryOf = a.ID
	}
	if err = mirrorWriteOne(ctx, tx, `UPDATE outbound SET state='ready',retry_of_attempt_id=? WHERE host_id=? AND thread_id=? AND mirror_key=?`, retryOf, a.HostID, a.ThreadID, a.Key); err != nil {
		return err
	}
	var selected int64
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mentions WHERE host_id=? AND thread_id=? AND mirror_key=? AND attempt_id=? AND state='dispatching'`, a.HostID, a.ThreadID, a.Key, id).Scan(&selected); err != nil {
		return err
	}
	if err = mirrorWriteCount(ctx, tx, selected, `UPDATE mentions SET state='eligible',reason='',attempt_id=NULL WHERE host_id=? AND thread_id=? AND mirror_key=? AND attempt_id=? AND state='dispatching'`, a.HostID, a.ThreadID, a.Key, id); err != nil {
		return err
	}
	// Retirement may have arrived while this attempt was dispatching. Once its
	// definite rejection is recorded, an unaccepted obsolete tail needs no retry.
	if err = retireUnacceptedMirror(ctx, tx, a.HostID, a.ThreadID, a.Key); err != nil {
		return err
	}
	if err = compactMirrorPayloadTx(ctx, tx, a.HostID, a.ThreadID, a.Key); err != nil {
		return err
	}
	return tx.Commit()
}

// retireUnacceptedMirror completes obsolete segments locally only when every
// attempt is definitely rejected (or absent). Unknown/dispatching journals stay
// frozen, and known remote cards still need their replacement notice update.
func retireUnacceptedMirror(ctx context.Context, tx *sql.Tx, host, thread, key string) error {
	result, err := tx.ExecContext(ctx, `UPDATE outbound SET state='confirmed',retry_of_attempt_id=''
 WHERE host_id=? AND thread_id=? AND mirror_key=? AND state='ready'
 AND EXISTS(SELECT 1 FROM mirrors m WHERE m.host_id=outbound.host_id AND m.thread_id=outbound.thread_id AND m.mirror_key=outbound.mirror_key AND m.retired=1 AND m.remote_id='')
 AND NOT EXISTS(SELECT 1 FROM outbound_attempts a WHERE a.host_id=outbound.host_id AND a.thread_id=outbound.thread_id AND a.mirror_key=outbound.mirror_key AND a.state!='rejected')`, host, thread, key)
	if err != nil {
		return err
	}
	retired, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if retired != 0 {
		_, err = tx.ExecContext(ctx, `DELETE FROM outbound_throttles WHERE host_id=? AND thread_id=? AND mirror_key=?`, host, thread, key)
	}
	return err
}
