package store

import (
	"context"
	"database/sql"
)

// VerificationInputEvidence describes persisted input facts only. A queued
// snapshot is neither a fresh native read nor proof that the thread was busy.
type VerificationInputEvidence struct {
	StagedInputs       int `json:"staged_inputs"`
	AddConfirmed       int `json:"add_confirmed"`
	QueuedSnapshot     int `json:"queued_snapshot"`
	HistoryLinked      int `json:"history_linked"`
	SteerConfirmed     int `json:"steer_confirmed"`
	SteerHistoryLinked int `json:"steer_history_linked"`
	UnresolvedAttempts int `json:"unresolved_attempts"`
}

// VerificationInputEvidence reads one consistent snapshot. The original
// private binding and its derived route must still match before any counts are
// reported. No receipt body, action token, or frozen request is selected.
func (s *Store) VerificationInputEvidence(ctx context.Context, b VerificationBinding) (VerificationInputEvidence, error) {
	var result VerificationInputEvidence
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	current, err := scanVerificationBinding(ctx, tx, b.RequestID, b.HostID, b.AppID, b.OwnerID)
	if err != nil {
		return result, err
	}
	if current != b {
		return result, ErrConflict
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
		return result, err
	}
	if valid != 1 {
		return result, ErrConflict
	}
	err = tx.QueryRowContext(ctx, `WITH inputs AS (
 SELECT n.receipt_id,n.host_id,n.app_id,n.thread_id,n.state,n.queue_id,n.queue_ordinal,n.turn_id,n.item_id,r.client_id
 FROM submission_inputs n JOIN m0_receipts r ON r.id=n.receipt_id
 WHERE n.host_id=? AND n.app_id=? AND n.thread_id=?
 AND r.app_id=n.app_id AND r.thread_id=n.thread_id AND r.chat_id=? AND r.client_id<>''
), history AS (
 SELECT n.receipt_id,n.turn_id FROM inputs n JOIN input_history_links h
 ON h.receipt_id=n.receipt_id AND h.host_id=n.host_id AND h.thread_id=n.thread_id
 AND h.turn_id=n.turn_id AND h.item_id=n.item_id
 WHERE n.turn_id<>'' AND n.item_id<>''
), steer AS (
 SELECT DISTINCT n.receipt_id,a.expected_turn_id FROM inputs n JOIN submission_actions a
 ON a.host_id=n.host_id AND a.app_id=n.app_id AND a.thread_id=n.thread_id
 AND a.receipt_id=n.receipt_id AND a.client_id=n.client_id
 WHERE a.kind='steer' AND a.stage='steer' AND a.state='confirmed' AND a.result='steered'
 AND a.queue_id<>'' AND a.expected_turn_id<>''
 AND EXISTS(SELECT 1 FROM submission_attempts d
 WHERE d.host_id=a.host_id AND d.app_id=a.app_id AND d.thread_id=a.thread_id
 AND d.receipt_id=a.receipt_id AND d.action_id=a.action_id AND d.client_id=a.client_id
 AND d.queue_id=a.queue_id AND d.expected_turn_id=a.expected_turn_id
 AND d.stage='delete' AND d.state='confirmed' AND d.deleted=1)
 AND EXISTS(SELECT 1 FROM submission_attempts v
 WHERE v.host_id=a.host_id AND v.app_id=a.app_id AND v.thread_id=a.thread_id
 AND v.receipt_id=a.receipt_id AND v.action_id=a.action_id AND v.client_id=a.client_id
 AND v.queue_id=a.queue_id AND v.expected_turn_id=a.expected_turn_id
 AND v.stage='steer' AND v.state='confirmed')
)
 SELECT
 (SELECT COUNT(*) FROM inputs),
 (SELECT COUNT(*) FROM inputs n WHERE EXISTS(SELECT 1 FROM submission_attempts a
 WHERE a.host_id=n.host_id AND a.app_id=n.app_id AND a.thread_id=n.thread_id
 AND a.receipt_id=n.receipt_id AND a.client_id=n.client_id
 AND a.action_id='' AND a.stage='add' AND a.state='confirmed'
 AND (a.remote_id<>'' OR EXISTS(SELECT 1 FROM history h WHERE h.receipt_id=n.receipt_id)))),
 (SELECT COUNT(*) FROM inputs WHERE state='queued' AND queue_id<>'' AND queue_ordinal>0),
 (SELECT COUNT(*) FROM history),
 (SELECT COUNT(DISTINCT receipt_id) FROM steer),
 (SELECT COUNT(DISTINCT v.receipt_id) FROM steer v JOIN history h
 ON h.receipt_id=v.receipt_id AND h.turn_id=v.expected_turn_id),
 (SELECT COUNT(*) FROM submission_attempts a JOIN inputs n
 ON a.host_id=n.host_id AND a.app_id=n.app_id AND a.thread_id=n.thread_id
 AND a.receipt_id=n.receipt_id AND a.client_id=n.client_id
 WHERE a.state IN ('dispatching','unknown'))`,
		b.HostID, b.AppID, b.ThreadID, b.ChatID).Scan(
		&result.StagedInputs, &result.AddConfirmed, &result.QueuedSnapshot, &result.HistoryLinked,
		&result.SteerConfirmed, &result.SteerHistoryLinked, &result.UnresolvedAttempts)
	if err != nil {
		return VerificationInputEvidence{}, err
	}
	if err := tx.Commit(); err != nil {
		return VerificationInputEvidence{}, err
	}
	return result, nil
}
