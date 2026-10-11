package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type ReactionTransition struct {
	ReceiptID, MessageID, PreviousID, Desired string
	Revision                                  int64
}

// ClaimReactionTransition serializes optional feedback with the receipt's
// current durable state. An old received snapshot cannot replace newer running
// feedback. Ambiguous transitions stay frozen and never affect native input.
func (s *Store) ClaimReactionTransition(ctx context.Context, scope SubmissionScope, receiptID, expectedState, desired string) (ReactionTransition, bool, error) {
	return s.ClaimReactionTransitionAt(ctx, scope, receiptID, expectedState, desired, time.Now().UnixMilli())
}

func (s *Store) ClaimReactionTransitionAt(ctx context.Context, scope SubmissionScope, receiptID, expectedState, desired string, nowMS int64) (ReactionTransition, bool, error) {
	if nowMS <= 0 {
		return ReactionTransition{}, false, ErrConflict
	}
	switch desired {
	case "", "Get", "THINKING", "OnIt", "CheckMark", "CrossMark":
	default:
		return ReactionTransition{}, false, ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return ReactionTransition{}, false, err
	}
	defer tx.Rollback()
	chat, err := submissionBinding(ctx, tx, scope)
	if err != nil {
		return ReactionTransition{}, false, err
	}
	var message, inputState string
	if err := tx.QueryRowContext(ctx, `SELECT message_id,state FROM m0_receipts WHERE id=? AND app_id=? AND thread_id=? AND chat_id=?`, receiptID, scope.AppID, scope.ThreadID, chat).Scan(&message, &inputState); err != nil {
		return ReactionTransition{}, false, err
	}
	if inputState != expectedState {
		return ReactionTransition{}, false, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO m0_reactions(receipt_id) VALUES(?) ON CONFLICT(receipt_id) DO NOTHING`, receiptID); err != nil {
		return ReactionTransition{}, false, err
	}
	var emoji, previous, state string
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT emoji,reaction_id,revision,state FROM m0_reactions WHERE receipt_id=?`, receiptID).Scan(&emoji, &previous, &revision, &state); err != nil {
		return ReactionTransition{}, false, err
	}
	if state != "confirmed" {
		return ReactionTransition{}, false, nil
	}
	if emoji == desired {
		if _, err := tx.ExecContext(ctx, "DELETE FROM reaction_throttles WHERE receipt_id=?", receiptID); err != nil {
			return ReactionTransition{}, false, err
		}
		return ReactionTransition{}, false, tx.Commit()
	}
	var due int64
	err = tx.QueryRowContext(ctx, "SELECT next_retry_at_ms FROM reaction_throttles WHERE receipt_id=?", receiptID).Scan(&due)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ReactionTransition{}, false, err
	}
	if due > nowMS {
		return ReactionTransition{}, false, nil
	}

	if revision < 0 || revision == 1<<63-1 {
		return ReactionTransition{}, false, ErrConflict
	}
	revision++
	if _, err := tx.ExecContext(ctx, `UPDATE m0_reactions SET revision=?,state='changing' WHERE receipt_id=?`, revision, receiptID); err != nil {
		return ReactionTransition{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return ReactionTransition{}, false, err
	}
	return ReactionTransition{receiptID, message, previous, desired, revision}, true, nil
}

func (s *Store) FinishReactionTransition(ctx context.Context, transition ReactionTransition, state, remoteID string) error {
	if state != "confirmed" && state != "unknown" && state != "rejected" || state == "confirmed" && (transition.Desired != "") != (remoteID != "") {
		return ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var result sql.Result
	if state == "confirmed" {
		result, err = tx.ExecContext(ctx, `UPDATE m0_reactions SET emoji=?,reaction_id=?,state=? WHERE receipt_id=? AND revision=? AND state='changing'`, transition.Desired, remoteID, state, transition.ReceiptID, transition.Revision)
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE m0_reactions SET state=? WHERE receipt_id=? AND revision=? AND state='changing'`, state, transition.ReceiptID, transition.Revision)
	}
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
	if _, err := tx.ExecContext(ctx, "DELETE FROM reaction_throttles WHERE receipt_id=?", transition.ReceiptID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecoverReactionTransitions(ctx context.Context, host, app string) error {
	var installed string
	if err := s.DB.QueryRowContext(ctx, "SELECT app_id FROM deployment WHERE host_id=?", host).Scan(&installed); err != nil {
		return err
	}
	if app != installed {
		return ErrConflict
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE m0_reactions SET state='unknown' WHERE state='changing' AND receipt_id IN (SELECT r.id FROM m0_receipts r JOIN bindings b ON b.thread_id=r.thread_id AND b.chat_id=r.chat_id WHERE b.host_id=? AND r.app_id=?)`, host, app)
	return err
}

// ReceivedFeedbackBindings selects only pending acknowledgements. The service
// must not poll the membership API for every idle/history-only task group.
func (s *Store) ReceivedFeedbackBindings(ctx context.Context, host, app string) ([]Binding, error) {
	return s.ReceivedFeedbackBindingsAt(ctx, host, app, time.Now().UnixMilli())
}

func (s *Store) ReceivedFeedbackBindingsAt(ctx context.Context, host, app string, nowMS int64) ([]Binding, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT b.thread_id,b.intent_id,b.chat_id FROM bindings b JOIN deployment d USING(host_id) JOIN m0_receipts r ON r.thread_id=b.thread_id AND r.chat_id=b.chat_id AND r.app_id=d.app_id LEFT JOIN m0_reactions x ON x.receipt_id=r.id WHERE b.host_id=? AND d.app_id=? AND r.state='received' AND (x.receipt_id IS NULL OR x.state='confirmed' AND x.emoji<>'Get') AND NOT EXISTS(SELECT 1 FROM reaction_throttles t WHERE t.receipt_id=r.id AND t.next_retry_at_ms>?)`, host, app, nowMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Binding
	for rows.Next() {
		b := Binding{HostID: host}
		if err := rows.Scan(&b.ThreadID, &b.IntentID, &b.ChatID); err != nil {
			return nil, err
		}
		result = append(result, b)
	}
	return result, rows.Err()
}

// ReactionFeedbackState is metadata for diagnostics/tests; it never includes
// receipt text. Missing rows retain sql.ErrNoRows.
func (s *Store) ReactionFeedbackState(ctx context.Context, receiptID string) (emoji, state string, err error) {
	err = s.DB.QueryRowContext(ctx, "SELECT emoji,state FROM m0_reactions WHERE receipt_id=?", receiptID).Scan(&emoji, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", sql.ErrNoRows
	}
	return
}
