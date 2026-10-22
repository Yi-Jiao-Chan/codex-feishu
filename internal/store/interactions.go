package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

const InteractionsSchema = `
CREATE TABLE IF NOT EXISTS interaction_requests (
 request_key TEXT PRIMARY KEY, host_id TEXT NOT NULL, app_id TEXT NOT NULL, thread_id TEXT NOT NULL,
 turn_id TEXT NOT NULL, item_id TEXT NOT NULL, mode TEXT NOT NULL CHECK(mode IN ('native','async')),
 method TEXT NOT NULL, native_id TEXT NOT NULL, payload_json TEXT NOT NULL, connection_id TEXT NOT NULL,
 generation INTEGER NOT NULL, state TEXT NOT NULL CHECK(state IN ('pending','resolved','disconnected','unsupported')),
 action_token TEXT NOT NULL UNIQUE, revision INTEGER NOT NULL DEFAULT 1, created_at_ms INTEGER NOT NULL,
 event_at INTEGER NOT NULL DEFAULT 0, replay INTEGER NOT NULL DEFAULT 1,
 parent_key TEXT NOT NULL DEFAULT '',parent_message_id TEXT NOT NULL DEFAULT '',
 attempt_id TEXT NOT NULL DEFAULT '', submission_id TEXT NOT NULL DEFAULT '', operable INTEGER NOT NULL DEFAULT 0,
 FOREIGN KEY(host_id,thread_id) REFERENCES bindings(host_id,thread_id)
);
CREATE TABLE IF NOT EXISTS interaction_attempts (
 attempt_id TEXT PRIMARY KEY, request_key TEXT NOT NULL REFERENCES interaction_requests(request_key),
 user_id TEXT NOT NULL, connection_id TEXT NOT NULL,generation INTEGER NOT NULL,result_json TEXT NOT NULL,
 summary TEXT NOT NULL,state TEXT NOT NULL CHECK(state IN ('ready','dispatching','submitted','unknown','rejected')),
 created_at_ms INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS interactions_thread ON interaction_requests(host_id,app_id,thread_id);
`

type InteractionRequest struct {
	Operable                                                                                                  bool
	Key, HostID, AppID, ThreadID, TurnID, ItemID, Mode, Method, NativeID, Payload, ConnectionID, State, Token string
	Generation, Revision, CreatedAtMS, EventAt                                                                int64
	Replay                                                                                                    bool
	ParentKey, ParentMessageID, AttemptID, SubmissionID                                                       string
}
type InteractionAttempt struct {
	ID, Key, UserID, ConnectionID, Result, Summary, State string
	Generation, CreatedAtMS                               int64
}

const interactionColumns = `request_key,host_id,app_id,thread_id,turn_id,item_id,mode,method,native_id,payload_json,connection_id,generation,state,action_token,revision,created_at_ms,event_at,replay,parent_key,parent_message_id,attempt_id,submission_id,operable`

func scanInteraction(row rowScanner) (r InteractionRequest, err error) {
	err = row.Scan(&r.Key, &r.HostID, &r.AppID, &r.ThreadID, &r.TurnID, &r.ItemID, &r.Mode, &r.Method, &r.NativeID, &r.Payload, &r.ConnectionID, &r.Generation, &r.State, &r.Token, &r.Revision, &r.CreatedAtMS, &r.EventAt, &r.Replay, &r.ParentKey, &r.ParentMessageID, &r.AttemptID, &r.SubmissionID, &r.Operable)
	return
}
func (s *Store) Interaction(ctx context.Context, key string) (InteractionRequest, error) {
	return scanInteraction(s.DB.QueryRowContext(ctx, `SELECT `+interactionColumns+` FROM interaction_requests WHERE request_key=?`, key))
}
func (s *Store) InteractionByToken(ctx context.Context, token string) (InteractionRequest, error) {
	return scanInteraction(s.DB.QueryRowContext(ctx, `SELECT `+interactionColumns+` FROM interaction_requests WHERE action_token=?`, token))
}
func (s *Store) Interactions(ctx context.Context, scope SubmissionScope) ([]InteractionRequest, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+interactionColumns+` FROM interaction_requests WHERE host_id=? AND app_id=? AND thread_id=? ORDER BY created_at_ms,request_key`, scope.HostID, scope.AppID, scope.ThreadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InteractionRequest
	for rows.Next() {
		r, e := scanInteraction(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func InteractionCardKey(key string) string { return "ui:interaction:" + key }

// Observe freezes the first body and notification provenance. Replay rotates
// live reply authorization, never reopens a resolved request or resends an attempt.
func (s *Store) ObserveInteraction(ctx context.Context, r InteractionRequest) error {
	if r.Key == "" || r.TurnID == "" || r.ItemID == "" || r.ConnectionID == "" || r.Generation <= 0 || r.CreatedAtMS <= 0 || !json.Valid([]byte(r.Payload)) || (r.Mode != "native" && r.Mode != "async") || (r.State != "pending" && r.State != "unsupported") {
		return ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = submissionBinding(ctx, tx, SubmissionScope{r.HostID, r.AppID, r.ThreadID}); err != nil {
		return err
	}
	old, err := scanInteraction(tx.QueryRowContext(ctx, `SELECT `+interactionColumns+` FROM interaction_requests WHERE request_key=?`, r.Key))
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `INSERT INTO interaction_requests(`+interactionColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?,?,?,?,?,'','',0)`, r.Key, r.HostID, r.AppID, r.ThreadID, r.TurnID, r.ItemID, r.Mode, r.Method, r.NativeID, r.Payload, r.ConnectionID, r.Generation, r.State, ID(), r.CreatedAtMS, r.EventAt, r.Replay, "", "")
	} else if err == nil {
		if old.HostID != r.HostID || old.AppID != r.AppID || old.ThreadID != r.ThreadID || old.TurnID != r.TurnID || old.ItemID != r.ItemID || old.Mode != r.Mode || old.Method != r.Method || old.NativeID != r.NativeID {
			return ErrConflict
		}
		if old.State != "resolved" && (old.ConnectionID != r.ConnectionID || old.Generation != r.Generation || old.State == "disconnected") {
			if r.Mode == "native" && old.Payload != r.Payload {
				return ErrConflict
			}
			nextState := r.State
			if old.Mode == "async" && old.State == "unsupported" {
				// A partial answer or ambiguous call identity is durable. A
				// reconnect without the older evidence must not restore controls.
				nextState = old.State
			}
			_, err = tx.ExecContext(ctx, `UPDATE interaction_requests SET connection_id=?,generation=?,state=?,action_token=?,revision=revision+1 WHERE request_key=?`, r.ConnectionID, r.Generation, nextState, ID(), r.Key)
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) ResolveInteraction(ctx context.Context, key, connection string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE interaction_requests SET state='resolved',revision=revision+1 WHERE request_key=? AND connection_id=? AND state!='resolved'`, key, connection)
	return err
}

// Native history can prove that an async answer is already in this thread.
// Partial App answers disable the grouped bridge form without inventing
// completion of the remaining questions.
func (s *Store) CloseAsyncInteraction(ctx context.Context, key, connection string, complete bool) error {
	state := "unsupported"
	if complete {
		state = "resolved"
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE interaction_requests SET state=?,operable=0,revision=revision+1 WHERE request_key=? AND connection_id=? AND mode='async' AND state IN ('pending','unsupported') AND state!=?`, state, key, connection, state)
	return err
}
func (s *Store) RecoverInteractions(ctx context.Context, host string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `UPDATE interaction_attempts SET state='unknown' WHERE state='dispatching' AND request_key IN(SELECT request_key FROM interaction_requests WHERE host_id=?)`, host)
	if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE interaction_requests SET state='disconnected',revision=revision+1 WHERE host_id=? AND mode='native' AND state='pending'`, host)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) InteractionAttempt(ctx context.Context, id string) (a InteractionAttempt, err error) {
	err = s.DB.QueryRowContext(ctx, `SELECT attempt_id,request_key,user_id,connection_id,generation,result_json,summary,state,created_at_ms FROM interaction_attempts WHERE attempt_id=?`, id).Scan(&a.ID, &a.Key, &a.UserID, &a.ConnectionID, &a.Generation, &a.Result, &a.Summary, &a.State, &a.CreatedAtMS)
	return
}

// Claim binds an authorized operator to the actual unforwarded card. It freezes
// the complete answer before returning to Feishu; the thread actor sends later.
func (s *Store) ClaimInteraction(ctx context.Context, scope SubmissionScope, user string, operators []string, chat, message, token string, revision int64, connection string, generation int64, result, summary string, nowMS int64) (r InteractionRequest, err error) {
	return s.claimInteraction(ctx, scope, user, operators, chat, message, token, revision, connection, generation, result, summary, "", nowMS)
}

// ClaimAsyncInteraction atomically freezes the explicitly requested append,
// its answer receipt, and the existing native-operation journal. Old callbacks
// use ClaimInteraction and keep their original queued-delivery behavior.
func (s *Store) ClaimAsyncInteraction(ctx context.Context, scope SubmissionScope, user string, operators []string, chat, message, token string, revision int64, connection string, generation int64, result, summary, body string, nowMS int64) (InteractionRequest, error) {
	if body == "" {
		return InteractionRequest{}, ErrIneligible
	}
	return s.claimInteraction(ctx, scope, user, operators, chat, message, token, revision, connection, generation, result, summary, body, nowMS)
}

func (s *Store) claimInteraction(ctx context.Context, scope SubmissionScope, user string, operators []string, chat, message, token string, revision int64, connection string, generation int64, result, summary, appendBody string, nowMS int64) (r InteractionRequest, err error) {
	if !containsMentionID(operators, user) || user == "" || !json.Valid([]byte(result)) || nowMS <= 0 {
		return r, ErrIneligible
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return r, err
	}
	defer tx.Rollback()
	b, err := submissionBinding(ctx, tx, scope)
	if err != nil {
		return r, err
	}
	if b != chat {
		return r, ErrConflict
	}
	r, err = scanInteraction(tx.QueryRowContext(ctx, `SELECT `+interactionColumns+` FROM interaction_requests WHERE action_token=?`, token))
	if err != nil {
		return r, err
	}
	if r.HostID != scope.HostID || r.AppID != scope.AppID || r.ThreadID != scope.ThreadID || !r.Operable || r.State != "pending" || r.Revision != revision || r.ConnectionID != connection || r.Generation != generation {
		return r, ErrSubmissionStale
	}
	m, err := readMirror(ctx, tx, scope.HostID, scope.ThreadID, InteractionCardKey(r.Key))
	if err != nil {
		return r, err
	}
	if m.RemoteID == "" || m.RemoteID != message {
		return r, ErrSubmissionStale
	}
	if r.AttemptID != "" {
		return r, ErrSubmissionBusy
	}
	if appendBody != "" && (r.Mode != "async" || r.Method != "async") {
		return r, ErrConflict
	}
	attempt := ID()
	_, err = tx.ExecContext(ctx, `INSERT INTO interaction_attempts(attempt_id,request_key,user_id,connection_id,generation,result_json,summary,state,created_at_ms) VALUES(?,?,?,?,?,?,?,'ready',?)`, attempt, r.Key, user, connection, generation, result, summary, nowMS)
	if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE interaction_requests SET attempt_id=?,revision=revision+1 WHERE request_key=?`, attempt, r.Key)
	}
	if err != nil {
		return r, err
	}
	r.AttemptID = attempt
	r.Revision++
	if appendBody != "" {
		r.SubmissionID, err = submitAsyncInteractionTx(ctx, tx, r, appendBody, true, nowMS)
		if err != nil {
			return r, err
		}
		r.Revision++
	}
	err = tx.Commit()
	return r, err
}
func (s *Store) DispatchInteraction(ctx context.Context, key, connection string, generation int64) (a InteractionAttempt, ok bool, err error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return a, false, err
	}
	defer tx.Rollback()
	r, err := scanInteraction(tx.QueryRowContext(ctx, `SELECT `+interactionColumns+` FROM interaction_requests WHERE request_key=?`, key))
	if err != nil {
		return a, false, err
	}
	if r.State != "pending" || r.ConnectionID != connection || r.Generation != generation || r.AttemptID == "" {
		return a, false, nil
	}
	err = tx.QueryRowContext(ctx, `SELECT attempt_id,request_key,user_id,connection_id,generation,result_json,summary,state,created_at_ms FROM interaction_attempts WHERE attempt_id=?`, r.AttemptID).Scan(&a.ID, &a.Key, &a.UserID, &a.ConnectionID, &a.Generation, &a.Result, &a.Summary, &a.State, &a.CreatedAtMS)
	if err != nil {
		return a, false, err
	}
	if a.State != "ready" || a.ConnectionID != connection || a.Generation != generation {
		return a, false, nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE interaction_attempts SET state='dispatching' WHERE attempt_id=? AND state='ready'`, a.ID)
	if err != nil {
		return a, false, err
	}
	err = tx.Commit()
	a.State = "dispatching"
	return a, err == nil, err
}
func (s *Store) FinishInteraction(ctx context.Context, id, outcome string) error {
	if outcome != "submitted" && outcome != "unknown" && outcome != "rejected" {
		return ErrConflict
	}
	result, err := s.DB.ExecContext(ctx, `UPDATE interaction_attempts SET state=? WHERE attempt_id=? AND state='dispatching'`, outcome, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return ErrConflict
	}
	return err
}

// StageInteractionCard uses the same send/reply/PATCH journal as normal output.
// CAS prevents an event or callback racing rendering from restoring old controls.
func (s *Store) StageInteractionCard(ctx context.Context, r InteractionRequest, card string, operable bool) error {
	return s.StageInteractionSubmissionCard(ctx, r, card, operable, 0)
}

// A submission revision prevents a racing queue action from restoring stale
// controls on the original question card.
func (s *Store) StageInteractionSubmissionCard(ctx context.Context, r InteractionRequest, card string, operable bool, submissionRevision int64) error {
	if !json.Valid([]byte(card)) {
		return ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	actual, err := scanInteraction(tx.QueryRowContext(ctx, `SELECT `+interactionColumns+` FROM interaction_requests WHERE request_key=?`, r.Key))
	if err != nil {
		return err
	}
	if actual.Revision != r.Revision || actual.State != r.State {
		return ErrConflict
	}
	if submissionRevision > 0 {
		n, err := readSubmission(ctx, tx, SubmissionScope{r.HostID, r.AppID, r.ThreadID}, actual.SubmissionID)
		if err != nil {
			return err
		}
		if n.Revision != submissionRevision {
			return ErrConflict
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE interaction_requests SET operable=? WHERE request_key=?`, operable, r.Key); err != nil {
		return err
	}
	if actual.ParentKey == "" && actual.ParentMessageID == "" {
		err = tx.QueryRowContext(ctx, `SELECT parent_key,parent_message_id FROM mirrors WHERE host_id=? AND thread_id=? AND turn_id=? AND kind IN ('progress','final') AND mirror_key NOT LIKE 'ui:%' ORDER BY rowid LIMIT 1`, r.HostID, r.ThreadID, r.TurnID).Scan(&actual.ParentKey, &actual.ParentMessageID)
		if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRowContext(ctx, `SELECT mirror_key,'' FROM mirrors WHERE host_id=? AND thread_id=? AND turn_id=? AND kind='human' ORDER BY rowid LIMIT 1`, r.HostID, r.ThreadID, r.TurnID).Scan(&actual.ParentKey, &actual.ParentMessageID)
			if errors.Is(err, sql.ErrNoRows) {
				// Async-only turns may have no progress/final document. A
				// Feishu-origin input is deliberately not mirrored, so use its
				// exact hydrated-history link instead of inventing a plain send.
				err = tx.QueryRowContext(ctx, `SELECT '',CASE WHEN r.reply_message_id<>'' THEN r.reply_message_id ELSE r.message_id END FROM input_history_links l JOIN m0_receipts r ON r.id=l.receipt_id WHERE l.host_id=? AND l.thread_id=? AND l.turn_id=? AND r.app_id=? AND r.thread_id=l.thread_id ORDER BY l.rowid LIMIT 1`, r.HostID, r.ThreadID, r.TurnID, r.AppID).Scan(&actual.ParentKey, &actual.ParentMessageID)
				if errors.Is(err, sql.ErrNoRows) {
					return nil
				}
			}
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE interaction_requests SET parent_key=?,parent_message_id=? WHERE request_key=?`, actual.ParentKey, actual.ParentMessageID, r.Key)
		if err != nil {
			return err
		}
	}
	key := InteractionCardKey(r.Key)
	terminal := r.State == "resolved"
	old, err := readMirror(ctx, tx, r.HostID, r.ThreadID, key)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `INSERT INTO mirrors(host_id,thread_id,mirror_key,turn_id,kind,card_json,terminal,replay,parent_key,parent_message_id,event_type,event_at) VALUES(?,?,?,?,'progress',?,?,?,?,?,'needs_user',?)`, r.HostID, r.ThreadID, key, key, card, terminal, r.Replay, actual.ParentKey, actual.ParentMessageID, r.EventAt)
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO outbound(host_id,thread_id,mirror_key,state)VALUES(?,?,?,'ready')`, r.HostID, r.ThreadID, key)
		}
	} else if err == nil && old.Card != card {
		_, err = tx.ExecContext(ctx, `UPDATE mirrors SET card_json=?,terminal=?,revision=revision+1 WHERE host_id=? AND thread_id=? AND mirror_key=?`, card, terminal, r.HostID, r.ThreadID, key)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE outbound SET state='ready' WHERE host_id=? AND thread_id=? AND mirror_key=? AND state='confirmed'`, r.HostID, r.ThreadID, key)
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Async answers become ordinary durable inputs in a single local transaction.
// No queue RPC is made here; the established submission controller owns it.
func (s *Store) SubmitAsyncInteraction(ctx context.Context, key, body string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := scanInteraction(tx.QueryRowContext(ctx, `SELECT `+interactionColumns+` FROM interaction_requests WHERE request_key=?`, key))
	if err != nil {
		return err
	}
	if r.Mode != "async" || r.State != "pending" || r.AttemptID == "" || r.SubmissionID != "" {
		return nil
	}
	var state string
	if err = tx.QueryRowContext(ctx, `SELECT state FROM interaction_attempts WHERE attempt_id=?`, r.AttemptID).Scan(&state); err != nil {
		return err
	}
	if state != "ready" {
		return nil
	}
	if _, err = submitAsyncInteractionTx(ctx, tx, r, body, false, 0); err != nil {
		return err
	}
	return tx.Commit()
}

func submitAsyncInteractionTx(ctx context.Context, tx *sql.Tx, r InteractionRequest, body string, appendAnswer bool, nowMS int64) (string, error) {
	b, err := submissionBinding(ctx, tx, SubmissionScope{r.HostID, r.AppID, r.ThreadID})
	if err != nil {
		return "", err
	}
	m, err := readMirror(ctx, tx, r.HostID, r.ThreadID, InteractionCardKey(r.Key))
	if err != nil {
		return "", err
	}
	if m.RemoteID == "" {
		return "", ErrConflict
	}
	if appendAnswer {
		// Preserve FIFO for older receipts not yet adopted by the actor.
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO submission_order(receipt_id)
 SELECT id FROM m0_receipts WHERE app_id=? AND thread_id=? AND chat_id=? AND state<>'draft' ORDER BY rowid`, r.AppID, r.ThreadID, b)
		if err != nil {
			return "", err
		}
	}
	receipt, client, token := ID(), "cfb-"+ID(), ID()
	_, err = tx.ExecContext(ctx, `INSERT INTO m0_receipts(id,app_id,message_id,thread_id,chat_id,body,client_id,state,action_token) VALUES(?,?,?,?,?,?,?,'received',?)`, receipt, r.AppID, m.RemoteID, r.ThreadID, b, body, client, token)
	if err == nil && appendAnswer {
		_, err = tx.ExecContext(ctx, `INSERT INTO submission_order(receipt_id) VALUES(?)`, receipt)
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO submission_inputs(receipt_id,host_id,app_id,thread_id,state) VALUES(?,?,?,?,'steering')`, receipt, r.HostID, r.AppID, r.ThreadID)
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE m0_receipts SET state='steering' WHERE id=?`, receipt)
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO submission_actions(action_id,host_id,app_id,thread_id,receipt_id,token,revision,kind,stage,state,client_id,created_at_ms)
 VALUES(?,?,?,?,?,?,1,'steer','prepare','ready',?,?)`, ID(), r.HostID, r.AppID, r.ThreadID, receipt, token, client, nowMS)
		}
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE interaction_requests SET submission_id=?,revision=revision+1 WHERE request_key=?`, receipt, r.Key)
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE interaction_attempts SET state='submitted' WHERE attempt_id=? AND state='ready'`, r.AttemptID)
	}
	if err != nil {
		return "", err
	}
	return receipt, nil
}
