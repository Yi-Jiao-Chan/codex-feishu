package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

const inputContinuePrefix = "input-continue."

// Ordinary admission and attachment sealing must first adopt older receipts
// that the actor has not staged yet, including upgrade-era and async inputs.
func adoptSubmissionOrderTx(ctx context.Context, tx *sql.Tx, scope SubmissionScope, chat string) error {
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO submission_order(receipt_id) SELECT id FROM m0_receipts WHERE app_id=? AND thread_id=? AND chat_id=? AND state<>'draft' ORDER BY rowid`, scope.AppID, scope.ThreadID, chat)
	return err
}

func serverID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, ch := range id {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}

// InputContinueSource recognizes only server-generated, receipt-bound ordinary
// input intents. An empty source means an already observed App interruption;
// otherwise the token freezes the earlier bridge stop's immutable action ID.
func InputContinueSource(a SubmissionAction) (string, bool) {
	if a.Kind != "continue" || !serverID(a.ReceiptID) || !strings.HasPrefix(a.Token, inputContinuePrefix) {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(a.Token, inputContinuePrefix), ".")
	if len(parts) != 2 || parts[0] != a.ReceiptID || parts[1] != "app" && !serverID(parts[1]) {
		return "", false
	}
	if parts[1] == "app" {
		return "", a.ExpectedTurnID != ""
	}
	return parts[1], true
}

// ReceiveSubmissionInput is the ordinary-message admission boundary. Recovery,
// creation and legacy Claim do not retroactively acquire a continue intention.
func (s *Store) ReceiveSubmissionInput(ctx context.Context, scope SubmissionScope, in Receipt) (Receipt, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Receipt{}, false, err
	}
	defer tx.Rollback()
	chat, err := submissionBinding(ctx, tx, scope)
	if err != nil || in.AppID != scope.AppID || in.ThreadID != scope.ThreadID || in.ChatID != chat || in.MessageID == "" {
		return Receipt{}, false, ErrConflict
	}
	var seen int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM m0_receipts WHERE app_id=? AND message_id=?`, scope.AppID, in.MessageID).Scan(&seen); err != nil {
		return Receipt{}, false, err
	}
	if seen != 0 {
		return Receipt{}, false, tx.Commit()
	}
	r, fresh, err := claimInputTx(ctx, tx, scope, chat, in.MessageID, in.Body, "received")
	if err == nil {
		err = stageInputContinueTx(ctx, tx, scope, r)
	}
	if err != nil {
		return Receipt{}, false, err
	}
	return r, fresh, tx.Commit()
}

// Capture permission at fresh input admission, never when download, actor tick
// or replay eventually makes the receipt ready. Database commit order arbitrates
// overlapping ingress callbacks; a subsequent accepted stop supersedes it.
func stageInputContinueTx(ctx context.Context, tx *sql.Tx, scope SubmissionScope, r Receipt) error {
	var observedTurn string
	err := tx.QueryRowContext(ctx, `SELECT t.turn_id FROM history_watermarks w JOIN task_controls t ON t.host_id=w.host_id AND t.thread_id=w.thread_id AND t.turn_id=w.turn_id
 WHERE w.host_id=? AND t.app_id=? AND w.thread_id=? AND t.native_status='interrupted'
 AND NOT EXISTS(SELECT 1 FROM task_controls active WHERE active.host_id=t.host_id AND active.app_id=t.app_id AND active.thread_id=t.thread_id AND active.native_status='inProgress')`, scope.HostID, scope.AppID, scope.ThreadID).Scan(&observedTurn)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	source, target := "", observedTurn
	// A later Continue may still be pending, rejected as empty, or unknown. It
	// cannot erase the earlier stop as the fresh input's source of permission.
	control, err := scanSubmissionAction(tx.QueryRowContext(ctx, `SELECT `+actionColumns+` FROM submission_actions WHERE host_id=? AND app_id=? AND thread_id=? AND receipt_id='' AND kind='stop' ORDER BY rowid DESC LIMIT 1`, scope.HostID, scope.AppID, scope.ThreadID))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	redundantCandidate := control.State == "rejected" && control.ExpectedTurnID != "" && (control.Result == "idle" || control.Result == "ended")
	if err == nil && control.Kind == "stop" && (control.State != "rejected" || redundantCandidate) {
		// A confirmed historical stop cannot override a newer observed App turn.
		var latest string
		readErr := tx.QueryRowContext(ctx, `SELECT turn_id FROM history_watermarks WHERE host_id=? AND thread_id=?`, scope.HostID, scope.ThreadID).Scan(&latest)
		if readErr != nil && !errors.Is(readErr, sql.ErrNoRows) {
			return readErr
		}
		if control.State != "confirmed" && control.State != "rejected" || latest == "" || latest == control.ExpectedTurnID {
			source, target = control.ID, control.ExpectedTurnID
		}
	}
	if source == "" && target == "" {
		return nil
	}
	part := source
	if part == "" {
		part = "app"
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO submission_actions(action_id,host_id,app_id,thread_id,receipt_id,token,revision,kind,stage,state,expected_turn_id,client_id,created_at_ms) VALUES(?,?,?,?,?,?,0,'continue','prepare','ready',?,?,?)`, ID(), scope.HostID, scope.AppID, scope.ThreadID, r.ID, inputContinuePrefix+r.ID+"."+part, target, r.ClientID, time.Now().UnixMilli())
	return err
}

// inputContinueReadyTx also runs in the final dispatch-claim transaction. The
// input must be accepted, its source stop confirmed, and no later stop admitted.
func inputContinueReadyTx(ctx context.Context, tx *sql.Tx, scope SubmissionScope, op *SubmissionAction) (bool, error) {
	source, ok := InputContinueSource(*op)
	if !ok {
		return false, ErrSubmissionStale
	}
	reject := func(reason string) (bool, error) {
		_, err := tx.ExecContext(ctx, `UPDATE submission_actions SET state='rejected',result=? WHERE action_id=? AND state='ready'`, reason, op.ID)
		return false, err
	}
	var newer int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM submission_actions WHERE host_id=? AND app_id=? AND thread_id=? AND kind='stop' AND rowid>(SELECT rowid FROM submission_actions WHERE action_id=?)`, scope.HostID, scope.AppID, scope.ThreadID, op.ID).Scan(&newer); err != nil {
		return false, err
	}
	if newer != 0 {
		return reject("superseded")
	}
	if source != "" {
		stop, err := readSubmissionAction(ctx, tx, scope, source)
		if err != nil {
			return false, err
		}
		if stop.Kind != "stop" {
			return reject("changed")
		}
		redundant := stop.State == "rejected" && stop.ExpectedTurnID != "" && (stop.Result == "idle" || stop.Result == "ended")
		if redundant {
			// Repeated card/CoT stops can target the same turn. If the second
			// one found it already ended without dispatching, retain only that
			// frozen target; live canContinue must still prove it interrupted.
			var attempts int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM submission_attempts WHERE action_id=?`, stop.ID).Scan(&attempts); err != nil {
				return false, err
			}
			redundant = attempts == 0
		}
		if stop.State == "rejected" && !redundant {
			return reject("changed")
		}
		if stop.State != "confirmed" && !redundant {
			return false, nil
		}
		if !redundant && stop.Result != "stopped" || stop.ExpectedTurnID == "" || op.ExpectedTurnID != "" && op.ExpectedTurnID != stop.ExpectedTurnID {
			return reject("changed")
		}
		if op.ExpectedTurnID == "" {
			op.ExpectedTurnID = stop.ExpectedTurnID
			if _, err = tx.ExecContext(ctx, `UPDATE submission_actions SET expected_turn_id=? WHERE action_id=? AND state='ready'`, op.ExpectedTurnID, op.ID); err != nil {
				return false, err
			}
		}
	}
	n, err := readSubmission(ctx, tx, scope, op.ReceiptID)
	if err != nil {
		return false, err
	}
	switch n.State {
	case "removed", "failed", "consumed", "steered":
		return reject(n.State)
	case "accepted", "queued":
		var confirmed int
		err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM submission_attempts WHERE host_id=? AND app_id=? AND thread_id=? AND receipt_id=? AND action_id='' AND stage='add' AND state='confirmed'`, scope.HostID, scope.AppID, scope.ThreadID, n.ID).Scan(&confirmed)
		return confirmed != 0, err
	default:
		return false, nil
	}
}

func (s *Store) PrepareInputContinue(ctx context.Context, scope SubmissionScope, actionID string) (bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = submissionBinding(ctx, tx, scope); err != nil {
		return false, err
	}
	op, err := readSubmissionAction(ctx, tx, scope, actionID)
	if err != nil || op.State != "ready" {
		return false, err
	}
	ready, err := inputContinueReadyTx(ctx, tx, scope, &op)
	if err != nil {
		return false, err
	}
	return ready, tx.Commit()
}
