package store

import (
	"context"
	"database/sql"
)

// A successful SQL call with an ignored proof write is not durable evidence.
// Optional recipient updates may match zero rows; required journal/target
// writes must match exactly one. Bulk recovery has its own zero-to-many rules.
func mirrorWriteOne(ctx context.Context, tx *sql.Tx, query string, args ...any) error {
	return mirrorWriteCount(ctx, tx, 1, query, args...)
}

func mirrorWriteCount(ctx context.Context, tx *sql.Tx, expected int64, query string, args ...any) error {
	if expected < 0 {
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
