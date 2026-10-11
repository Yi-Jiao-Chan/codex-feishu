package store

import (
	"context"
	"database/sql"
	"time"
)

// A received label alone cannot prove that no native write ever started.
// Legacy M0 journals and all attempt outcomes are deliberately excluded.
func localPendingSubmission(ctx context.Context, q submissionQuerier, scope SubmissionScope, n Submission) (bool, error) {
	if n.State != "received" || n.QueueID != "" || n.QueueOrdinal != 0 || n.TurnID != "" || n.ItemID != "" || n.ClientID == "" || n.ActionToken == "" {
		return false, nil
	}
	var count int
	err := q.QueryRowContext(ctx, `SELECT
 (SELECT COUNT(*) FROM submission_attempts WHERE receipt_id=?)+
 (SELECT COUNT(*) FROM submission_actions WHERE receipt_id=? AND state IN ('ready','running','unknown'))+
 (SELECT COUNT(*) FROM m0_operations WHERE key='m0.add.'||?)+
 (SELECT COUNT(*) FROM m0_actions WHERE receipt_id=?)+
 (SELECT COUNT(*) FROM input_history_links WHERE receipt_id=?)+
 (SELECT COUNT(*) FROM m0_receipts WHERE id=? AND (state<>'received' OR queue_id<>''))`, n.ID, n.ID, n.ID, n.ID, n.ID, n.ID).Scan(&count)
	return count == 0, err
}

// CancelPendingSubmission is a local callback transaction. Callers supply the
// authenticated installed-app actor and configured operators; callback fields
// cannot grant permission. The real delivered card and current receipt revision
// are checked together with every native-write journal before the state CAS.
// Replays return the original local action, without touching native RPCs.
func (s *Store) CancelPendingSubmission(ctx context.Context, scope SubmissionScope, operator string, operators []string, chat, card, token string, revision int64) (SubmissionAction, error) {
	allowed := false
	for _, id := range operators {
		if id == operator && id != "" {
			allowed = true
			break
		}
	}
	if !allowed || chat == "" || card == "" || token == "" || revision <= 0 {
		return SubmissionAction{}, ErrSubmissionStale
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return SubmissionAction{}, err
	}
	defer tx.Rollback()
	actual, err := submissionBinding(ctx, tx, scope)
	if err != nil {
		return SubmissionAction{}, err
	}
	if actual != chat {
		return SubmissionAction{}, ErrSubmissionStale
	}
	n, err := scanSubmission(tx.QueryRowContext(ctx, `SELECT `+submissionColumns+` FROM submission_inputs n JOIN m0_receipts r ON r.id=n.receipt_id WHERE n.host_id=? AND n.app_id=? AND n.thread_id=? AND r.action_token=? AND r.app_id=n.app_id AND r.thread_id=n.thread_id AND r.chat_id=?`, scope.HostID, scope.AppID, scope.ThreadID, token, chat))
	if err != nil {
		return SubmissionAction{}, ErrSubmissionStale
	}
	m, err := readMirror(ctx, tx, scope.HostID, scope.ThreadID, QueueCardKey(n.ID))
	if err != nil || m.RemoteID != card || m.ParentMessageID != n.MessageID || m.ParentKey != "" || m.TurnID != m.Key || m.Kind != "progress" {
		return SubmissionAction{}, ErrSubmissionStale
	}
	a, err := scanSubmissionAction(tx.QueryRowContext(ctx, `SELECT `+actionColumns+` FROM submission_actions WHERE host_id=? AND app_id=? AND thread_id=? AND token=? AND revision=? AND kind='remove'`, scope.HostID, scope.AppID, scope.ThreadID, token, revision))
	if err == nil {
		if a.ReceiptID != n.ID || a.Stage != "local" || a.State != "confirmed" || a.Result != "removed" {
			return SubmissionAction{}, ErrSubmissionStale
		}
		return a, tx.Commit()
	}
	if err != sql.ErrNoRows {
		return SubmissionAction{}, err
	}
	if n.Revision != revision {
		return SubmissionAction{}, ErrSubmissionStale
	}
	if safe, err := localPendingSubmission(ctx, tx, scope, n); err != nil {
		return SubmissionAction{}, err
	} else if !safe {
		return SubmissionAction{}, ErrSubmissionStale
	}
	a = SubmissionAction{ID: ID(), ReceiptID: n.ID, Token: token, Revision: revision, Kind: "remove", Stage: "local", State: "confirmed", Result: "removed", ClientID: n.ClientID}
	if _, err = tx.ExecContext(ctx, `INSERT INTO submission_actions(action_id,host_id,app_id,thread_id,receipt_id,token,revision,kind,stage,state,client_id,result,created_at_ms) VALUES(?,?,?,?,?,?,?,'remove','local','confirmed',?,'removed',?)`, a.ID, scope.HostID, scope.AppID, scope.ThreadID, n.ID, token, revision, n.ClientID, time.Now().UnixMilli()); err != nil {
		return SubmissionAction{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE submission_inputs SET state='removed',revision=revision+1 WHERE receipt_id=? AND host_id=? AND app_id=? AND thread_id=? AND revision=? AND state='received' AND queue_id=''`, n.ID, scope.HostID, scope.AppID, scope.ThreadID, revision)
	if err != nil {
		return SubmissionAction{}, err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return SubmissionAction{}, err
	}
	if count != 1 {
		return SubmissionAction{}, ErrSubmissionStale
	}
	if _, err = tx.ExecContext(ctx, `UPDATE m0_receipts SET state='removed' WHERE id=? AND app_id=? AND thread_id=? AND chat_id=?`, n.ID, scope.AppID, scope.ThreadID, chat); err != nil {
		return SubmissionAction{}, err
	}
	if err = stageQueueCardTx(ctx, tx, scope, n.ID, QueueCardOptions{LocalPending: true}); err != nil {
		return SubmissionAction{}, err
	}
	return a, tx.Commit()
}
