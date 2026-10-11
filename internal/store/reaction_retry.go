package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
)

const ReactionRetrySchema = `
CREATE TABLE IF NOT EXISTS reaction_throttles (
 receipt_id TEXT PRIMARY KEY REFERENCES m0_reactions(receipt_id),
 last_operation_key TEXT NOT NULL REFERENCES m0_operations(key),
 revision INTEGER NOT NULL CHECK(revision>0),
 next_retry_at_ms INTEGER NOT NULL CHECK(next_retry_at_ms>0),
 rejection_count INTEGER NOT NULL CHECK(rejection_count>0)
);
`

func ReactionOperationKey(t ReactionTransition, operation string) string {
	return fmt.Sprintf("p1.reaction.%s.%d.%s", t.ReceiptID, t.Revision, operation)
}

// RecordReactionDeletion commits the known deletion and clears its old ID
// together. A later rejected add must not cause this ID to be deleted again.
func (s *Store) RecordReactionDeletion(ctx context.Context, t ReactionTransition) error {
	if t.PreviousID == "" {
		return ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE m0_reactions SET emoji='',reaction_id='' WHERE receipt_id=? AND revision=? AND state='changing' AND reaction_id=?`, t.ReceiptID, t.Revision, t.PreviousID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrConflict
	}
	result, err = tx.ExecContext(ctx, `UPDATE m0_operations SET state='confirmed',remote_id=? WHERE key=? AND kind='feishu.reaction.delete' AND state='sending' AND remote_id=''`, t.PreviousID, ReactionOperationKey(t, "delete"))
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrConflict
	}
	return tx.Commit()
}

// DeferReactionTransition requires an explicit rate-limit rejection. Neither
// transport failures nor other rejections authorize reopening a reaction write.
// The rejected operation is retained; the next revision uses current input
// state, not the rejected emoji, and known deleted IDs remain cleared.
func (s *Store) DeferReactionTransition(ctx context.Context, t ReactionTransition, operation string, nowMS, minimumDelayMS int64) error {
	if t.Revision <= 0 || nowMS <= 0 || minimumDelayMS < 0 || operation != "add" && operation != "delete" || operation == "add" && t.Desired == "" || operation == "delete" && t.PreviousID == "" {
		return ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	key := ReactionOperationKey(t, operation)
	var kind, state, remote string
	if err := tx.QueryRowContext(ctx, `SELECT kind,state,remote_id FROM m0_operations WHERE key=?`, key).Scan(&kind, &state, &remote); err != nil {
		return err
	}
	var previous string
	var count, revision int64
	err = tx.QueryRowContext(ctx, `SELECT last_operation_key,rejection_count,revision FROM reaction_throttles WHERE receipt_id=?`, t.ReceiptID).Scan(&previous, &count, &revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if kind != "feishu.reaction."+operation || remote != "" {
		return ErrConflict
	}
	if state == "rejected" && previous == key && revision == t.Revision {
		return nil
	}
	if state != "sending" {
		return ErrConflict
	}
	var currentID, reactionState string
	var currentRevision int64
	if err := tx.QueryRowContext(ctx, `SELECT reaction_id,state,revision FROM m0_reactions WHERE receipt_id=?`, t.ReceiptID).Scan(&currentID, &reactionState, &currentRevision); err != nil {
		return err
	}
	if reactionState != "changing" || currentRevision != t.Revision || operation == "delete" && currentID != t.PreviousID || operation == "add" && currentID != "" {
		return ErrConflict
	}
	if count == math.MaxInt64 {
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO reaction_throttles(receipt_id,last_operation_key,revision,next_retry_at_ms,rejection_count) VALUES(?,?,?,?,?) ON CONFLICT(receipt_id) DO UPDATE SET last_operation_key=excluded.last_operation_key,revision=excluded.revision,next_retry_at_ms=excluded.next_retry_at_ms,rejection_count=excluded.rejection_count`, t.ReceiptID, key, t.Revision, nowMS+delay, count); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE m0_operations SET state='rejected' WHERE key=?`, key); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE m0_reactions SET state='confirmed' WHERE receipt_id=? AND revision=?`, t.ReceiptID, t.Revision); err != nil {
		return err
	}
	return tx.Commit()
}
