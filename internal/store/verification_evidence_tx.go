package store

import (
	"context"
	"database/sql"
)

// beginVerificationEvidenceTx gates metadata-only evidence in one consistent
// read-only transaction. The caller must finish or roll back the returned tx.
func (s *Store) beginVerificationEvidenceTx(ctx context.Context, b VerificationBinding) (*sql.Tx, error) {
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*sql.Tx, error) { _ = tx.Rollback(); return nil, err }
	current, err := scanVerificationBinding(ctx, tx, b.RequestID, b.HostID, b.AppID, b.OwnerID)
	if err != nil {
		return fail(err)
	}
	if current != b {
		return fail(ErrConflict)
	}
	var valid int
	// create_intents has no app column; deployment fixes its host's app scope.
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*)
 FROM deployment d JOIN bindings r ON r.host_id=d.host_id
 JOIN create_intents i ON i.intent_id=r.intent_id
 WHERE d.host_id=? AND d.app_id=? AND r.thread_id=? AND r.intent_id=? AND r.chat_id=?
 AND i.host_id=d.host_id AND i.thread_id=r.thread_id AND i.owner_id=? AND i.chat_id=r.chat_id AND i.state='confirmed'`,
		b.HostID, b.AppID, b.ThreadID, b.IntentID, b.ChatID, b.OwnerID).Scan(&valid)
	if err != nil {
		return fail(err)
	}
	if valid != 1 {
		return fail(ErrConflict)
	}
	return tx, nil
}
