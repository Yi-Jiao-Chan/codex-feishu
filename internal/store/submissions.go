package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SubmissionSchema is included by migration v5. These are operation journals,
// not a second execution queue or a second namespace for input receipts.
const SubmissionSchema = `
CREATE TABLE IF NOT EXISTS submission_inputs (
 receipt_id TEXT PRIMARY KEY REFERENCES m0_receipts(id),
 host_id TEXT NOT NULL, app_id TEXT NOT NULL, thread_id TEXT NOT NULL,
 state TEXT NOT NULL, queue_id TEXT NOT NULL DEFAULT '',
 queue_ordinal INTEGER NOT NULL DEFAULT 0, revision INTEGER NOT NULL DEFAULT 1,
 turn_id TEXT NOT NULL DEFAULT '', item_id TEXT NOT NULL DEFAULT '',
 FOREIGN KEY(host_id,thread_id) REFERENCES bindings(host_id,thread_id)
);
CREATE TABLE IF NOT EXISTS submission_actions (
 action_id TEXT PRIMARY KEY, host_id TEXT NOT NULL, app_id TEXT NOT NULL, thread_id TEXT NOT NULL,
 receipt_id TEXT NOT NULL DEFAULT '', token TEXT NOT NULL, revision INTEGER NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('steer','remove','stop','continue')),
 stage TEXT NOT NULL, state TEXT NOT NULL CHECK(state IN ('ready','running','unknown','confirmed','rejected')),
 queue_id TEXT NOT NULL DEFAULT '', expected_turn_id TEXT NOT NULL DEFAULT '',
 client_id TEXT NOT NULL DEFAULT '', result TEXT NOT NULL DEFAULT '', created_at_ms INTEGER NOT NULL,
 UNIQUE(host_id,app_id,thread_id,token,revision,kind),
 FOREIGN KEY(host_id,thread_id) REFERENCES bindings(host_id,thread_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS submission_one_active_item_action
 ON submission_actions(host_id,app_id,thread_id,receipt_id)
 WHERE receipt_id<>'' AND state IN ('ready','running','unknown');
CREATE TABLE IF NOT EXISTS submission_attempts (
 attempt_id TEXT PRIMARY KEY, host_id TEXT NOT NULL, app_id TEXT NOT NULL, thread_id TEXT NOT NULL,
 receipt_id TEXT NOT NULL DEFAULT '', action_id TEXT NOT NULL DEFAULT '', stage TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('dispatching','unknown','confirmed','rejected')),
 queue_id TEXT NOT NULL DEFAULT '', expected_turn_id TEXT NOT NULL DEFAULT '',
 client_id TEXT NOT NULL DEFAULT '', remote_id TEXT NOT NULL DEFAULT '',
 deleted INTEGER NOT NULL DEFAULT -1 CHECK(deleted IN (-1,0,1)), created_at_ms INTEGER NOT NULL,
 UNIQUE(host_id,app_id,thread_id,receipt_id,action_id,stage),
 FOREIGN KEY(host_id,thread_id) REFERENCES bindings(host_id,thread_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS submission_one_dispatching_thread
 ON submission_attempts(host_id,app_id,thread_id) WHERE state='dispatching';
`

var ErrSubmissionStale = errors.New("input state changed; refresh its card")
var ErrSubmissionBusy = errors.New("thread has an input operation awaiting verification")

type SubmissionScope struct{ HostID, AppID, ThreadID string }

// Submission embeds the existing M0 receipt, so cards, reactions and history
// suppression keep using exactly the same receipt/client/message identities.
type Submission struct {
	Receipt
	Revision     int64
	QueueOrdinal int
	TurnID       string
	ItemID       string
}

type SubmissionAction struct {
	ID, ReceiptID, Token, Kind, Stage, State string
	QueueID, ExpectedTurnID, ClientID        string
	Result                                   string
	Revision                                 int64
}

type SubmissionAttempt struct {
	ID, ReceiptID, ActionID, Stage, State string
	QueueID, ExpectedTurnID, ClientID     string
	RemoteID                              string
	Deleted                               int
}

type submissionQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func submissionBinding(ctx context.Context, q submissionQuerier, scope SubmissionScope) (string, error) {
	if scope.HostID == "" || scope.AppID == "" || scope.ThreadID == "" {
		return "", ErrConflict
	}
	var chat string
	err := q.QueryRowContext(ctx, `SELECT b.chat_id FROM bindings b
 JOIN deployment d ON d.host_id=b.host_id
 JOIN create_intents i ON i.intent_id=b.intent_id AND i.host_id=b.host_id AND i.thread_id=b.thread_id
 WHERE b.host_id=? AND d.app_id=? AND b.thread_id=? AND i.state='confirmed' AND i.chat_id=b.chat_id`,
		scope.HostID, scope.AppID, scope.ThreadID).Scan(&chat)
	if errors.Is(err, sql.ErrNoRows) || err == nil && chat == "" {
		return "", ErrConflict
	}
	return chat, err
}

// SubmissionBinding verifies the complete confirmed binding before native reads
// as well as writes. A thread ID by itself is never authorization.
func (s *Store) SubmissionBinding(ctx context.Context, scope SubmissionScope) (string, error) {
	return submissionBinding(ctx, s.DB, scope)
}

const submissionColumns = `r.id,r.app_id,r.message_id,r.thread_id,r.chat_id,r.body,r.client_id,n.queue_id,n.state,r.card_id,r.action_token,n.revision,n.queue_ordinal,n.turn_id,n.item_id`
const actionColumns = `action_id,receipt_id,token,kind,stage,state,queue_id,expected_turn_id,client_id,result,revision`
const submissionAttemptColumns = `attempt_id,receipt_id,action_id,stage,state,queue_id,expected_turn_id,client_id,remote_id,deleted`

func scanSubmission(row rowScanner) (n Submission, err error) {
	err = row.Scan(&n.ID, &n.AppID, &n.MessageID, &n.ThreadID, &n.ChatID, &n.Body, &n.ClientID, &n.QueueID, &n.State, &n.CardID, &n.ActionToken, &n.Revision, &n.QueueOrdinal, &n.TurnID, &n.ItemID)
	return
}

func scanSubmissionAction(row rowScanner) (a SubmissionAction, err error) {
	err = row.Scan(&a.ID, &a.ReceiptID, &a.Token, &a.Kind, &a.Stage, &a.State, &a.QueueID, &a.ExpectedTurnID, &a.ClientID, &a.Result, &a.Revision)
	return
}

func scanSubmissionAttempt(row rowScanner) (a SubmissionAttempt, err error) {
	err = row.Scan(&a.ID, &a.ReceiptID, &a.ActionID, &a.Stage, &a.State, &a.QueueID, &a.ExpectedTurnID, &a.ClientID, &a.RemoteID, &a.Deleted)
	return
}

func readSubmission(ctx context.Context, q submissionQuerier, scope SubmissionScope, id string) (Submission, error) {
	return scanSubmission(q.QueryRowContext(ctx, `SELECT `+submissionColumns+` FROM submission_inputs n
 JOIN m0_receipts r ON r.id=n.receipt_id JOIN bindings b ON b.host_id=n.host_id AND b.thread_id=n.thread_id AND b.chat_id=r.chat_id
 WHERE n.host_id=? AND n.app_id=? AND n.thread_id=? AND r.app_id=n.app_id AND r.thread_id=n.thread_id AND n.receipt_id=?`,
		scope.HostID, scope.AppID, scope.ThreadID, id))
}

func readSubmissionAction(ctx context.Context, q submissionQuerier, scope SubmissionScope, id string) (SubmissionAction, error) {
	return scanSubmissionAction(q.QueryRowContext(ctx, `SELECT `+actionColumns+` FROM submission_actions WHERE host_id=? AND app_id=? AND thread_id=? AND action_id=?`, scope.HostID, scope.AppID, scope.ThreadID, id))
}

func saveSubmission(ctx context.Context, tx *sql.Tx, scope SubmissionScope, n Submission) error {
	r, err := tx.ExecContext(ctx, `UPDATE submission_inputs SET state=?,queue_id=?,queue_ordinal=?,turn_id=?,item_id=?,revision=revision+1
 WHERE host_id=? AND app_id=? AND thread_id=? AND receipt_id=?
 AND (state<>? OR queue_id<>? OR queue_ordinal<>? OR turn_id<>? OR item_id<>?)`,
		n.State, n.QueueID, n.QueueOrdinal, n.TurnID, n.ItemID, scope.HostID, scope.AppID, scope.ThreadID, n.ID,
		n.State, n.QueueID, n.QueueOrdinal, n.TurnID, n.ItemID)
	if err != nil {
		return err
	}
	if _, err = r.RowsAffected(); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE m0_receipts SET state=?,queue_id=? WHERE id=? AND app_id=? AND thread_id=? AND chat_id=(SELECT chat_id FROM bindings WHERE host_id=? AND thread_id=?)`,
		n.State, n.QueueID, n.ID, scope.AppID, scope.ThreadID, scope.HostID, scope.ThreadID)
	return err
}

// StageSubmissions adopts only receipts in this deployment's confirmed chat.
// Old M0 writes are never mistaken for a new, safe-to-dispatch input.
func (s *Store) StageSubmissions(ctx context.Context, scope SubmissionScope) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	chat, err := submissionBinding(ctx, tx, scope)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO submission_inputs(receipt_id,host_id,app_id,thread_id,state,queue_id)
 SELECT r.id,?,?,?,CASE
 WHEN r.state='received' AND EXISTS(SELECT 1 FROM m0_operations WHERE key='m0.add.'||r.id) THEN 'unknown'
 WHEN r.state NOT IN ('removed','consumed','steered','failed') AND EXISTS(SELECT 1 FROM m0_actions WHERE receipt_id=r.id AND state='processing') THEN 'unknown'
 ELSE r.state END,r.queue_id
 FROM m0_receipts r WHERE r.app_id=? AND r.thread_id=? AND r.chat_id=?`,
		scope.HostID, scope.AppID, scope.ThreadID, scope.AppID, scope.ThreadID, chat)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Submissions(ctx context.Context, scope SubmissionScope) ([]Submission, error) {
	chat, err := submissionBinding(ctx, s.DB, scope)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+submissionColumns+` FROM submission_inputs n JOIN m0_receipts r ON r.id=n.receipt_id
 WHERE n.host_id=? AND n.app_id=? AND n.thread_id=? AND r.app_id=? AND r.thread_id=? AND r.chat_id=? ORDER BY r.rowid`,
		scope.HostID, scope.AppID, scope.ThreadID, scope.AppID, scope.ThreadID, chat)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Submission
	for rows.Next() {
		n, err := scanSubmission(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, n)
	}
	return result, rows.Err()
}

func (s *Store) SubmissionActions(ctx context.Context, scope SubmissionScope) ([]SubmissionAction, error) {
	if _, err := submissionBinding(ctx, s.DB, scope); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+actionColumns+` FROM submission_actions WHERE host_id=? AND app_id=? AND thread_id=? ORDER BY rowid`, scope.HostID, scope.AppID, scope.ThreadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []SubmissionAction
	for rows.Next() {
		a, err := scanSubmissionAction(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

func (s *Store) SubmissionAction(ctx context.Context, scope SubmissionScope, id string) (SubmissionAction, error) {
	if _, err := submissionBinding(ctx, s.DB, scope); err != nil {
		return SubmissionAction{}, err
	}
	return readSubmissionAction(ctx, s.DB, scope, id)
}

// SaveSubmissionCard saves only the presentation identity. It cannot overwrite
// a receipt state or queue identity that advanced while a card was being sent.
func (s *Store) SaveSubmissionCard(ctx context.Context, scope SubmissionScope, receiptID, cardID string) error {
	if cardID == "" {
		return ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = submissionBinding(ctx, tx, scope); err != nil {
		return err
	}
	n, err := readSubmission(ctx, tx, scope, receiptID)
	if err != nil {
		return err
	}
	if n.CardID != "" && n.CardID != cardID {
		return ErrConflict
	}
	_, err = tx.ExecContext(ctx, `UPDATE m0_receipts SET card_id=? WHERE id=? AND app_id=? AND thread_id=? AND chat_id=?`, cardID, receiptID, scope.AppID, scope.ThreadID, n.ChatID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func submissionBlocked(ctx context.Context, q submissionQuerier, scope SubmissionScope) (bool, error) {
	var count int
	err := q.QueryRowContext(ctx, `SELECT
 (SELECT COUNT(*) FROM submission_inputs WHERE host_id=? AND app_id=? AND thread_id=? AND state IN ('submitting','unknown'))+
 (SELECT COUNT(*) FROM submission_actions WHERE host_id=? AND app_id=? AND thread_id=? AND state IN ('running','unknown'))`,
		scope.HostID, scope.AppID, scope.ThreadID, scope.HostID, scope.AppID, scope.ThreadID).Scan(&count)
	return count != 0, err
}

func (s *Store) SubmissionBlocked(ctx context.Context, scope SubmissionScope) (bool, error) {
	if _, err := submissionBinding(ctx, s.DB, scope); err != nil {
		return false, err
	}
	return submissionBlocked(ctx, s.DB, scope)
}

// ClaimSubmissionAction atomically checks card revision and exact queue identity.
// Replays return the original result even after the card revision has advanced.
func (s *Store) ClaimSubmissionAction(ctx context.Context, scope SubmissionScope, receiptID, kind, queueID string, revision int64) (SubmissionAction, error) {
	if kind != "steer" && kind != "remove" || revision <= 0 {
		return SubmissionAction{}, ErrSubmissionStale
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return SubmissionAction{}, err
	}
	defer tx.Rollback()
	if _, err = submissionBinding(ctx, tx, scope); err != nil {
		return SubmissionAction{}, err
	}
	n, err := readSubmission(ctx, tx, scope, receiptID)
	if err != nil {
		return SubmissionAction{}, ErrSubmissionStale
	}
	a, err := scanSubmissionAction(tx.QueryRowContext(ctx, `SELECT `+actionColumns+` FROM submission_actions WHERE host_id=? AND app_id=? AND thread_id=? AND token=? AND revision=? AND kind=?`,
		scope.HostID, scope.AppID, scope.ThreadID, n.ActionToken, revision, kind))
	if err == nil {
		if a.QueueID != queueID {
			return SubmissionAction{}, ErrSubmissionStale
		}
		return a, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return SubmissionAction{}, err
	}
	if n.Revision != revision || n.QueueID != queueID || n.State != "queued" && !(kind == "remove" && n.State == "received" && queueID == "") {
		return SubmissionAction{}, ErrSubmissionStale
	}
	if n.State != "received" {
		if blocked, err := submissionBlocked(ctx, tx, scope); err != nil {
			return SubmissionAction{}, err
		} else if blocked {
			return SubmissionAction{}, ErrSubmissionBusy
		}
	}
	if n.State == "received" {
		if safe, err := localPendingSubmission(ctx, tx, scope, n); err != nil {
			return SubmissionAction{}, err
		} else if !safe {
			return SubmissionAction{}, ErrSubmissionStale
		}
	}
	a = SubmissionAction{ID: ID(), ReceiptID: n.ID, Token: n.ActionToken, Kind: kind, Stage: "prepare", State: "ready", QueueID: queueID, ClientID: n.ClientID, Revision: revision}
	if n.State == "received" {
		a.State, a.Stage, a.Result = "confirmed", "local", "removed"
		n.State = "removed"
	} else if kind == "steer" {
		n.State = "steering"
	} else {
		n.State = "removing"
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO submission_actions(action_id,host_id,app_id,thread_id,receipt_id,token,revision,kind,stage,state,queue_id,client_id,result,created_at_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, scope.HostID, scope.AppID, scope.ThreadID, a.ReceiptID, a.Token, a.Revision, a.Kind, a.Stage, a.State, a.QueueID, a.ClientID, a.Result, time.Now().UnixMilli())
	if err != nil {
		return SubmissionAction{}, err
	}
	if err = saveSubmission(ctx, tx, scope, n); err != nil {
		return SubmissionAction{}, err
	}
	return a, tx.Commit()
}

func (s *Store) ClaimThreadSubmissionAction(ctx context.Context, scope SubmissionScope, kind, token string) (SubmissionAction, error) {
	return s.ClaimThreadSubmissionActionExpected(ctx, scope, kind, token, "")
}

// ClaimThreadSubmissionActionExpected seeds a task card's immutable target at
// claim time. Replays return the original action, including its original target.
func (s *Store) ClaimThreadSubmissionActionExpected(ctx context.Context, scope SubmissionScope, kind, token, expectedTurnID string) (SubmissionAction, error) {
	if token == "" || kind != "stop" && kind != "continue" {
		return SubmissionAction{}, ErrSubmissionStale
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return SubmissionAction{}, err
	}
	defer tx.Rollback()
	if _, err = submissionBinding(ctx, tx, scope); err != nil {
		return SubmissionAction{}, err
	}
	a, err := scanSubmissionAction(tx.QueryRowContext(ctx, `SELECT `+actionColumns+` FROM submission_actions WHERE host_id=? AND app_id=? AND thread_id=? AND token=? AND revision=0`, scope.HostID, scope.AppID, scope.ThreadID, token))
	if err == nil {
		if a.Kind != kind {
			return SubmissionAction{}, ErrSubmissionStale
		}
		return a, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return SubmissionAction{}, err
	}
	if blocked, err := submissionBlocked(ctx, tx, scope); err != nil {
		return SubmissionAction{}, err
	} else if blocked {
		return SubmissionAction{}, ErrSubmissionBusy
	}
	a = SubmissionAction{ID: ID(), Token: token, Kind: kind, Stage: "prepare", State: "ready", ExpectedTurnID: expectedTurnID}
	_, err = tx.ExecContext(ctx, `INSERT INTO submission_actions(action_id,host_id,app_id,thread_id,token,revision,kind,stage,state,expected_turn_id,created_at_ms) VALUES(?,?,?,?,?,0,?,'prepare','ready',?,?)`, a.ID, scope.HostID, scope.AppID, scope.ThreadID, token, kind, expectedTurnID, time.Now().UnixMilli())
	if err != nil {
		return SubmissionAction{}, err
	}
	return a, tx.Commit()
}

// PrepareSubmissionAction captures a target once. Later stages cannot retarget
// an expected turn when App moves to a different turn.
func (s *Store) PrepareSubmissionAction(ctx context.Context, scope SubmissionScope, id, stage, turnID, queueID, clientID string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = submissionBinding(ctx, tx, scope); err != nil {
		return err
	}
	a, err := readSubmissionAction(ctx, tx, scope, id)
	if err != nil {
		return err
	}
	if a.State != "ready" || a.Stage != "prepare" || a.ExpectedTurnID != "" && turnID != a.ExpectedTurnID ||
		(a.Kind == "steer" || a.Kind == "remove") && (stage != "delete" || a.QueueID != queueID || a.ClientID != clientID) ||
		a.Kind == "stop" && (stage != "interrupt" || turnID == "") ||
		a.Kind == "continue" && stage != "start" || a.Kind == "steer" && turnID == "" {
		return ErrSubmissionStale
	}
	_, err = tx.ExecContext(ctx, `UPDATE submission_actions SET stage=?,expected_turn_id=?,queue_id=?,client_id=? WHERE host_id=? AND app_id=? AND thread_id=? AND action_id=?`, stage, turnID, queueID, clientID, scope.HostID, scope.AppID, scope.ThreadID, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ClaimSubmissionAttempt commits dispatching before its caller sends an RPC.
// A stage can be dispatched only once; a different restore stage is not a retry
// of a steer with an uncertain result.
func (s *Store) ClaimSubmissionAttempt(ctx context.Context, scope SubmissionScope, receiptID, actionID, stage string) (SubmissionAttempt, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return SubmissionAttempt{}, false, err
	}
	defer tx.Rollback()
	if _, err = submissionBinding(ctx, tx, scope); err != nil {
		return SubmissionAttempt{}, false, err
	}
	existing, err := scanSubmissionAttempt(tx.QueryRowContext(ctx, `SELECT `+submissionAttemptColumns+` FROM submission_attempts WHERE host_id=? AND app_id=? AND thread_id=? AND receipt_id=? AND action_id=? AND stage=?`, scope.HostID, scope.AppID, scope.ThreadID, receiptID, actionID, stage))
	if err == nil {
		return existing, false, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return SubmissionAttempt{}, false, err
	}
	if blocked, err := submissionBlocked(ctx, tx, scope); err != nil {
		return SubmissionAttempt{}, false, err
	} else if blocked {
		return SubmissionAttempt{}, false, ErrSubmissionBusy
	}
	a := SubmissionAttempt{ID: ID(), ReceiptID: receiptID, ActionID: actionID, Stage: stage, State: "dispatching", Deleted: -1}
	if actionID == "" {
		n, err := readSubmission(ctx, tx, scope, receiptID)
		if err != nil {
			return a, false, err
		}
		if stage != "add" || n.State != "received" {
			return a, false, ErrSubmissionStale
		}
		a.ClientID = n.ClientID
		n.State = "submitting"
		if err = saveSubmission(ctx, tx, scope, n); err != nil {
			return a, false, err
		}
	} else {
		op, err := readSubmissionAction(ctx, tx, scope, actionID)
		if err != nil {
			return a, false, err
		}
		if op.State != "ready" || op.Stage != stage || op.ReceiptID != receiptID || stage == "prepare" || stage == "local" {
			return a, false, ErrSubmissionStale
		}
		a.QueueID, a.ExpectedTurnID, a.ClientID = op.QueueID, op.ExpectedTurnID, op.ClientID
		_, err = tx.ExecContext(ctx, `UPDATE submission_actions SET state='running' WHERE host_id=? AND app_id=? AND thread_id=? AND action_id=?`, scope.HostID, scope.AppID, scope.ThreadID, actionID)
		if err != nil {
			return a, false, err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO submission_attempts(attempt_id,host_id,app_id,thread_id,receipt_id,action_id,stage,state,queue_id,expected_turn_id,client_id,created_at_ms) VALUES(?,?,?,?,?,?,?,'dispatching',?,?,?,?)`,
		a.ID, scope.HostID, scope.AppID, scope.ThreadID, receiptID, actionID, stage, a.QueueID, a.ExpectedTurnID, a.ClientID, time.Now().UnixMilli())
	if err != nil {
		return a, false, err
	}
	return a, true, tx.Commit()
}

func (s *Store) SubmissionAttempts(ctx context.Context, scope SubmissionScope) ([]SubmissionAttempt, error) {
	if _, err := submissionBinding(ctx, s.DB, scope); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+submissionAttemptColumns+` FROM submission_attempts WHERE host_id=? AND app_id=? AND thread_id=? ORDER BY rowid`, scope.HostID, scope.AppID, scope.ThreadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []SubmissionAttempt
	for rows.Next() {
		a, err := scanSubmissionAttempt(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

// FinishSubmissionAttempt saves response and the next local stage together.
// Failure to commit leaves dispatching, which startup treats as unknown.
func (s *Store) FinishSubmissionAttempt(ctx context.Context, scope SubmissionScope, id, outcome, remoteID string, deleted int) error {
	if outcome != "confirmed" && outcome != "rejected" && outcome != "unknown" {
		return ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = submissionBinding(ctx, tx, scope); err != nil {
		return err
	}
	a, err := scanSubmissionAttempt(tx.QueryRowContext(ctx, `SELECT `+submissionAttemptColumns+` FROM submission_attempts WHERE host_id=? AND app_id=? AND thread_id=? AND attempt_id=?`, scope.HostID, scope.AppID, scope.ThreadID, id))
	if err != nil {
		return err
	}
	if a.State != "dispatching" && a.State != "unknown" ||
		a.Stage == "delete" && outcome == "confirmed" && deleted != 0 && deleted != 1 {
		return ErrConflict
	}
	_, err = tx.ExecContext(ctx, `UPDATE submission_attempts SET state=?,remote_id=?,deleted=? WHERE host_id=? AND app_id=? AND thread_id=? AND attempt_id=?`, outcome, remoteID, deleted, scope.HostID, scope.AppID, scope.ThreadID, id)
	if err != nil {
		return err
	}
	var n Submission
	if a.ReceiptID != "" {
		n, err = readSubmission(ctx, tx, scope, a.ReceiptID)
		if err != nil {
			return err
		}
		if (a.Stage == "add" || a.Stage == "restore") && outcome == "confirmed" && remoteID == "" && n.ItemID == "" {
			return ErrConflict
		}
	}
	var op SubmissionAction
	if a.ActionID != "" {
		op, err = readSubmissionAction(ctx, tx, scope, a.ActionID)
		if err != nil {
			return err
		}
		op.State, op.Result = outcome, ""
	}
	if outcome == "unknown" {
		n.State = "unknown"
	} else {
		switch a.Stage {
		case "add", "restore":
			if outcome == "confirmed" {
				n.State, n.QueueOrdinal = "accepted", 0
				if remoteID != "" {
					n.QueueID = remoteID
				}
				if n.ItemID != "" {
					n.State = "consumed"
				}
				if a.Stage == "restore" {
					op.Result = "restored"
				}
			} else {
				n.State = "failed"
				op.Result = "restoration_failed"
			}
		case "delete":
			if outcome == "confirmed" && deleted == 1 {
				n.QueueOrdinal = 0
				if op.Kind == "steer" {
					op.State, op.Stage, n.State = "ready", "steer", "steering"
				} else {
					op.Result, n.State = "removed", "removed"
				}
			} else {
				op.State, op.Result, n.State, n.QueueOrdinal = "rejected", "changed", "accepted", 0
			}
		case "steer":
			if outcome == "confirmed" {
				op.Result, n.State, n.TurnID, n.QueueOrdinal = "steered", "steered", a.ExpectedTurnID, 0
			} else {
				// Only a definite native rejection permits this new add stage.
				op.State, op.Stage, n.State = "ready", "restore", "steering"
			}
		case "interrupt":
			if outcome == "confirmed" {
				op.Result = "stopped"
			} else {
				op.Result = "rejected"
			}
		case "start":
			if outcome == "confirmed" {
				op.Result = "continued"
			} else {
				op.Result = "rejected"
			}
		default:
			return ErrConflict
		}
	}
	if a.ActionID != "" {
		_, err = tx.ExecContext(ctx, `UPDATE submission_actions SET state=?,stage=?,result=? WHERE host_id=? AND app_id=? AND thread_id=? AND action_id=?`, op.State, op.Stage, op.Result, scope.HostID, scope.AppID, scope.ThreadID, op.ID)
		if err != nil {
			return err
		}
	}
	if a.ReceiptID != "" {
		if err = saveSubmission(ctx, tx, scope, n); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) RecoverSubmissionDispatches(ctx context.Context, scope SubmissionScope) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = submissionBinding(ctx, tx, scope); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE submission_inputs SET state='unknown',revision=revision+1 WHERE host_id=? AND app_id=? AND thread_id=? AND receipt_id IN
 (SELECT receipt_id FROM submission_attempts WHERE host_id=? AND app_id=? AND thread_id=? AND state='dispatching')`, scope.HostID, scope.AppID, scope.ThreadID, scope.HostID, scope.AppID, scope.ThreadID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE m0_receipts SET state='unknown' WHERE id IN (SELECT receipt_id FROM submission_inputs WHERE host_id=? AND app_id=? AND thread_id=? AND state='unknown') AND app_id=? AND thread_id=?`, scope.HostID, scope.AppID, scope.ThreadID, scope.AppID, scope.ThreadID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE submission_actions SET state='unknown' WHERE host_id=? AND app_id=? AND thread_id=? AND state='running'`, scope.HostID, scope.AppID, scope.ThreadID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE submission_attempts SET state='unknown' WHERE host_id=? AND app_id=? AND thread_id=? AND state='dispatching'`, scope.HostID, scope.AppID, scope.ThreadID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ObserveSubmission saves exact native evidence and its existing history link.
// Callers must not use absence of evidence to report removed or steered.
func (s *Store) ObserveSubmission(ctx context.Context, scope SubmissionScope, id, state, queueID string, ordinal int, turnID, itemID string, actionID ...string) error {
	if ordinal < 0 || (turnID == "") != (itemID == "") || state != "queued" && state != "accepted" && state != "consumed" && state != "steered" {
		return ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = submissionBinding(ctx, tx, scope); err != nil {
		return err
	}
	n, err := readSubmission(ctx, tx, scope, id)
	if err != nil {
		return err
	}
	if n.State == "removed" || n.State == "failed" {
		return ErrSubmissionStale
	}
	var activeID string
	err = tx.QueryRowContext(ctx, `SELECT action_id FROM submission_actions WHERE host_id=? AND app_id=? AND thread_id=? AND receipt_id=? AND state IN ('ready','running','unknown')`, scope.HostID, scope.AppID, scope.ThreadID, id).Scan(&activeID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if activeID != "" && (len(actionID) != 1 || actionID[0] != activeID) {
		return ErrSubmissionStale
	}
	if len(actionID) != 0 {
		if len(actionID) != 1 {
			return ErrConflict
		}
		op, err := readSubmissionAction(ctx, tx, scope, actionID[0])
		if err != nil || op.ReceiptID != id {
			return ErrConflict
		}
	}
	n.State, n.QueueID, n.QueueOrdinal = state, queueID, ordinal
	if turnID != "" {
		n.TurnID, n.ItemID = turnID, itemID
		if err = stageInputLink(ctx, tx, scope.HostID, scope.ThreadID, InputLink{id, turnID, itemID}); err != nil {
			return err
		}
	}
	if err = saveSubmission(ctx, tx, scope, n); err != nil {
		return err
	}
	return tx.Commit()
}

// ResolveSubmissionAction records an observable result without inventing a
// successful RPC response (for example, an unknown delete followed by native
// consumption). Unknown attempts remain an immutable account of that ambiguity.
func (s *Store) ResolveSubmissionAction(ctx context.Context, scope SubmissionScope, id, state, result string) error {
	if state != "confirmed" && state != "rejected" {
		return ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = submissionBinding(ctx, tx, scope); err != nil {
		return err
	}
	op, err := readSubmissionAction(ctx, tx, scope, id)
	if err != nil {
		return err
	}
	if op.State != "ready" && op.State != "unknown" {
		return ErrSubmissionStale
	}
	_, err = tx.ExecContext(ctx, `UPDATE submission_actions SET state=?,result=? WHERE host_id=? AND app_id=? AND thread_id=? AND action_id=?`, state, result, scope.HostID, scope.AppID, scope.ThreadID, id)
	if err != nil {
		return err
	}
	if op.ReceiptID != "" {
		n, err := readSubmission(ctx, tx, scope, op.ReceiptID)
		if err != nil {
			return err
		}
		// Evidence may already have changed the receipt to consumed/queued.
		if n.State == "steering" || n.State == "removing" || n.State == "unknown" {
			n.State, n.QueueOrdinal = "accepted", 0
			if err = saveSubmission(ctx, tx, scope, n); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
