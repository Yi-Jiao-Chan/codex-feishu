package store

import "context"

// SubmissionInputSequence is an admission watermark, including terminal inputs.
// A newer durable input can wake a suspended workspace even if its in-process
// notification was lost. State updates to old receipts do not count as input.
func (s *Store) SubmissionInputSequence(ctx context.Context, scope SubmissionScope) (int64, error) {
	var sequence int64
	err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(rowid),0) FROM submission_inputs WHERE host_id=? AND app_id=? AND thread_id=?`, scope.HostID, scope.AppID, scope.ThreadID).Scan(&sequence)
	return sequence, err
}
