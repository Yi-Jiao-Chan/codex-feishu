package store

import (
	"context"
	"database/sql"
	"errors"
)

// ReleaseUnsentReactionTransition only releases a transition whose next remote
// operation was never journaled. A known completed deletion can be retained;
// a sending/unknown operation cannot be reclassified or retried here.
func (s *Store) ReleaseUnsentReactionTransition(ctx context.Context, t ReactionTransition) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM m0_operations WHERE key=?`, ReactionOperationKey(t, "add")).Scan(&count); err != nil || count != 0 {
		if err != nil {
			return err
		}
		return ErrConflict
	}
	var kind, state, remote string
	err = tx.QueryRowContext(ctx, `SELECT kind,state,remote_id FROM m0_operations WHERE key=?`, ReactionOperationKey(t, "delete")).Scan(&kind, &state, &remote)
	deleted := err == nil && kind == "feishu.reaction.delete" && t.PreviousID != "" && state == "confirmed" && remote == t.PreviousID
	if err != nil && !errors.Is(err, sql.ErrNoRows) || err == nil && !deleted {
		return ErrConflict
	}
	expectedID := t.PreviousID
	if deleted {
		expectedID = ""
	}
	result, err := tx.ExecContext(ctx, `UPDATE m0_reactions SET state='confirmed' WHERE receipt_id=? AND revision=? AND state='changing' AND reaction_id=?`, t.ReceiptID, t.Revision, expectedID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrConflict
	}
	return tx.Commit()
}
