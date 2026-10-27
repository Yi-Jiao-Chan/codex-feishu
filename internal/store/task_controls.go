package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TaskControlSchema stores card authorization and local claims. Native write
// attempts still belong exclusively to the existing submission journals.
const TaskControlSchema = `
CREATE TABLE IF NOT EXISTS task_controls (
 host_id TEXT NOT NULL, app_id TEXT NOT NULL, thread_id TEXT NOT NULL, turn_id TEXT NOT NULL,
 token TEXT NOT NULL UNIQUE, revision INTEGER NOT NULL DEFAULT 1,
 native_status TEXT NOT NULL, kind TEXT NOT NULL, state TEXT NOT NULL, reason TEXT NOT NULL,
 queue_count INTEGER NOT NULL DEFAULT -1, waiting INTEGER NOT NULL DEFAULT 0,
 fingerprint TEXT NOT NULL, PRIMARY KEY(host_id,app_id,thread_id,turn_id),
 FOREIGN KEY(host_id,thread_id) REFERENCES thread_observations(host_id,thread_id)
);
CREATE TABLE IF NOT EXISTS task_control_claims (
 host_id TEXT NOT NULL, app_id TEXT NOT NULL, thread_id TEXT NOT NULL, turn_id TEXT NOT NULL,
 token TEXT NOT NULL, revision INTEGER NOT NULL, kind TEXT NOT NULL,
 chat_id TEXT NOT NULL, message_id TEXT NOT NULL, external_id TEXT NOT NULL UNIQUE,
 native_action_id TEXT NOT NULL DEFAULT '', state TEXT NOT NULL DEFAULT 'ready', result TEXT NOT NULL DEFAULT '',
 created_at_ms INTEGER NOT NULL, PRIMARY KEY(token,revision,kind),
 FOREIGN KEY(host_id,app_id,thread_id,turn_id) REFERENCES task_controls(host_id,app_id,thread_id,turn_id)
);
`

type TaskControl struct {
	ThreadID, TurnID, Token, NativeStatus, Kind, State, Reason, Fingerprint string
	Revision                                                                int64
	QueueCount                                                              int
	Waiting                                                                 bool
}

type TaskControlClaim struct {
	ThreadID, TurnID, Token, Kind, ChatID, MessageID string
	ExternalID, NativeActionID, State, Result        string
	Revision                                         int64
}

const taskControlColumns = `thread_id,turn_id,token,native_status,kind,state,reason,fingerprint,revision,queue_count,waiting`
const taskClaimColumns = `thread_id,turn_id,token,kind,chat_id,message_id,external_id,native_action_id,state,result,revision`

func scanTaskControl(row rowScanner) (v TaskControl, err error) {
	err = row.Scan(&v.ThreadID, &v.TurnID, &v.Token, &v.NativeStatus, &v.Kind, &v.State, &v.Reason, &v.Fingerprint, &v.Revision, &v.QueueCount, &v.Waiting)
	return
}

func scanTaskControlClaim(row rowScanner) (v TaskControlClaim, err error) {
	err = row.Scan(&v.ThreadID, &v.TurnID, &v.Token, &v.Kind, &v.ChatID, &v.MessageID, &v.ExternalID, &v.NativeActionID, &v.State, &v.Result, &v.Revision)
	return
}

// Preparing a public card is allowed before a confirmed group exists. Claims
// additionally require the real confirmed binding and delivered progress card.
func taskControlAuthority(ctx context.Context, q submissionQuerier, scope SubmissionScope) error {
	if scope.HostID == "" || scope.AppID == "" || scope.ThreadID == "" {
		return ErrConflict
	}
	var count int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM deployment d JOIN thread_observations o ON o.host_id=d.host_id
 WHERE d.host_id=? AND d.app_id=? AND o.thread_id=? AND (o.state='eligible' OR EXISTS
 (SELECT 1 FROM bindings b JOIN create_intents i ON i.intent_id=b.intent_id WHERE b.host_id=o.host_id AND b.thread_id=o.thread_id AND i.state='confirmed' AND i.chat_id=b.chat_id))`, scope.HostID, scope.AppID, scope.ThreadID).Scan(&count)
	if err == nil && count != 1 {
		return ErrConflict
	}
	return err
}

func (s *Store) TaskControls(ctx context.Context, scope SubmissionScope) ([]TaskControl, error) {
	if err := taskControlAuthority(ctx, s.DB, scope); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+taskControlColumns+` FROM task_controls WHERE host_id=? AND app_id=? AND thread_id=? ORDER BY rowid`, scope.HostID, scope.AppID, scope.ThreadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []TaskControl
	for rows.Next() {
		v, err := scanTaskControl(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

func (s *Store) StageTaskControls(ctx context.Context, scope SubmissionScope, views []TaskControl) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = taskControlAuthority(ctx, tx, scope); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, v := range views {
		if v.TurnID == "" || seen[v.TurnID] || v.ThreadID != "" && v.ThreadID != scope.ThreadID || v.QueueCount < -1 || v.Kind != "" && v.Kind != "stop" && v.Kind != "continue" && v.Kind != "retry" || v.State != "available" && v.State != "unavailable" && v.State != "pending" && v.State != "unknown" || v.NativeStatus != "inProgress" && v.NativeStatus != "completed" && v.NativeStatus != "failed" && v.NativeStatus != "interrupted" || v.State == "available" && !(v.Kind == "stop" && v.NativeStatus == "inProgress" || v.Kind == "continue" && v.NativeStatus == "interrupted" && v.QueueCount > 0 || v.Kind == "retry" && v.NativeStatus == "failed" && v.QueueCount == 0) {
			return ErrConflict
		}
		seen[v.TurnID] = true
		// A callback can claim after Prepare read its snapshot. Preserve that
		// claim in this same write transaction instead of making it clickable.
		claimed, claimErr := scanTaskControlClaim(tx.QueryRowContext(ctx, `SELECT `+taskClaimColumns+` FROM task_control_claims WHERE host_id=? AND app_id=? AND thread_id=? AND turn_id=? AND state IN ('ready','pending','unknown') ORDER BY rowid DESC LIMIT 1`, scope.HostID, scope.AppID, scope.ThreadID, v.TurnID))
		if claimErr == nil {
			v.Kind, v.State, v.Reason = claimed.Kind, "pending", "processing"
			if claimed.State == "unknown" {
				v.State, v.Reason = "unknown", "verifying"
			}
		} else if !errors.Is(claimErr, sql.ErrNoRows) {
			return claimErr
		}
		old, readErr := scanTaskControl(tx.QueryRowContext(ctx, `SELECT `+taskControlColumns+` FROM task_controls WHERE host_id=? AND app_id=? AND thread_id=? AND turn_id=?`, scope.HostID, scope.AppID, scope.ThreadID, v.TurnID))
		if errors.Is(readErr, sql.ErrNoRows) {
			_, err = tx.ExecContext(ctx, `INSERT INTO task_controls(host_id,app_id,thread_id,turn_id,token,native_status,kind,state,reason,queue_count,waiting,fingerprint) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, scope.HostID, scope.AppID, scope.ThreadID, v.TurnID, ID(), v.NativeStatus, v.Kind, v.State, v.Reason, v.QueueCount, v.Waiting, v.Fingerprint)
		} else if readErr != nil {
			return readErr
		} else {
			if old.NativeStatus != "inProgress" && v.NativeStatus == "inProgress" {
				v.NativeStatus, v.Kind, v.State, v.Reason = old.NativeStatus, "", "unavailable", "verifying"
			}
			_, err = tx.ExecContext(ctx, `UPDATE task_controls SET native_status=?,kind=?,state=?,reason=?,queue_count=?,waiting=?,fingerprint=?,revision=revision+1
 WHERE host_id=? AND app_id=? AND thread_id=? AND turn_id=? AND (native_status<>? OR kind<>? OR state<>? OR reason<>? OR queue_count<>? OR waiting<>? OR fingerprint<>?)`,
				v.NativeStatus, v.Kind, v.State, v.Reason, v.QueueCount, v.Waiting, v.Fingerprint, scope.HostID, scope.AppID, scope.ThreadID, v.TurnID,
				v.NativeStatus, v.Kind, v.State, v.Reason, v.QueueCount, v.Waiting, v.Fingerprint)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) TaskControlClaims(ctx context.Context, scope SubmissionScope) ([]TaskControlClaim, error) {
	if err := taskControlAuthority(ctx, s.DB, scope); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+taskClaimColumns+` FROM task_control_claims WHERE host_id=? AND app_id=? AND thread_id=? ORDER BY rowid`, scope.HostID, scope.AppID, scope.ThreadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []TaskControlClaim
	for rows.Next() {
		v, err := scanTaskControlClaim(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

// ClaimTaskControl validates the server-side token, binding and actual progress
// card identity. Caller has already authenticated the callback's operator.
func (s *Store) ClaimTaskControl(ctx context.Context, host, app, chat, message, kind, token string, revision int64) (TaskControlClaim, error) {
	if host == "" || app == "" || chat == "" || message == "" || token == "" || revision <= 0 || kind != "stop" && kind != "continue" && kind != "retry" {
		return TaskControlClaim{}, ErrSubmissionStale
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return TaskControlClaim{}, err
	}
	defer tx.Rollback()
	v, err := scanTaskControl(tx.QueryRowContext(ctx, `SELECT `+taskControlColumns+` FROM task_controls WHERE host_id=? AND app_id=? AND token=?`, host, app, token))
	if err != nil {
		return TaskControlClaim{}, ErrSubmissionStale
	}
	scope := SubmissionScope{host, app, v.ThreadID}
	bound, err := submissionBinding(ctx, tx, scope)
	if err != nil || bound != chat {
		return TaskControlClaim{}, ErrSubmissionStale
	}
	var count int
	mirrorKind := "progress"
	if kind == "retry" {
		mirrorKind = "final"
	}
	// A retry belongs to the actual first result segment carrying its button.
	// Stop/continue retain the unique per-turn progress-card requirement.
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mirrors WHERE host_id=? AND thread_id=? AND turn_id=? AND kind=? AND retired=0 AND (?<>'final' OR remote_id=?)`, host, v.ThreadID, v.TurnID, mirrorKind, mirrorKind, message).Scan(&count)
	if err != nil {
		return TaskControlClaim{}, err
	}
	if count != 1 {
		return TaskControlClaim{}, ErrSubmissionStale
	}
	var card, callbackProofs, remoteID, deliveryState string
	var mirrorRevision, deliveredRevision int64
	err = tx.QueryRowContext(ctx, `SELECT m.card_json,m.callback_proofs,m.remote_id,m.revision,m.delivered_revision,o.state FROM mirrors m JOIN outbound o USING(host_id,thread_id,mirror_key) WHERE m.host_id=? AND m.thread_id=? AND m.turn_id=? AND m.kind=? AND m.retired=0 AND (?<>'final' OR m.remote_id=?)`, host, v.ThreadID, v.TurnID, mirrorKind, mirrorKind, message).Scan(&card, &callbackProofs, &remoteID, &mirrorRevision, &deliveredRevision, &deliveryState)
	if err != nil {
		return TaskControlClaim{}, err
	}
	if callbackProofs == "" && card != "" {
		callbackProofs = cardControlProofs(card)
	}
	if remoteID != message {
		return TaskControlClaim{}, ErrSubmissionStale
	}
	c, err := scanTaskControlClaim(tx.QueryRowContext(ctx, `SELECT `+taskClaimColumns+` FROM task_control_claims WHERE host_id=? AND app_id=? AND token=? AND revision=? AND kind=?`, host, app, token, revision, kind))
	if err == nil {
		if c.ChatID != chat || c.MessageID != message {
			return TaskControlClaim{}, ErrSubmissionStale
		}
		return c, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return TaskControlClaim{}, err
	}
	if v.Revision != revision || v.Kind != kind || v.State != "available" || mirrorRevision != deliveredRevision || deliveryState != "confirmed" || !cardControlProofsContain(callbackProofs, kind, token, revision) {
		return TaskControlClaim{}, ErrSubmissionStale
	}
	if blocked, blockErr := submissionBlocked(ctx, tx, scope); blockErr != nil {
		return TaskControlClaim{}, blockErr
	} else if blocked {
		return TaskControlClaim{}, ErrSubmissionBusy
	}
	c = TaskControlClaim{ThreadID: v.ThreadID, TurnID: v.TurnID, Token: token, Kind: kind, ChatID: chat, MessageID: message, Revision: revision, State: "ready", ExternalID: fmt.Sprintf("task.%s.%d.%s", token, revision, kind)}
	action, err := claimThreadSubmissionActionTx(ctx, tx, scope, kind, c.ExternalID, c.TurnID)
	if err != nil {
		return TaskControlClaim{}, err
	}
	c.NativeActionID = action.ID
	_, err = tx.ExecContext(ctx, `INSERT INTO task_control_claims(host_id,app_id,thread_id,turn_id,token,revision,kind,chat_id,message_id,external_id,native_action_id,created_at_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, host, app, c.ThreadID, c.TurnID, token, revision, kind, chat, message, c.ExternalID, c.NativeActionID, time.Now().UnixMilli())
	if err != nil {
		return TaskControlClaim{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE task_controls SET state='pending',reason='processing',revision=revision+1 WHERE host_id=? AND app_id=? AND thread_id=? AND turn_id=?`, host, app, v.ThreadID, v.TurnID)
	if err != nil {
		return TaskControlClaim{}, err
	}
	return c, tx.Commit()
}

type taskCardElement struct {
	Tag       string
	Elements  []taskCardElement
	Columns   []taskCardElement
	Behaviors []struct {
		Type  string
		Value struct {
			Action, Token string
			Revision      int64
		}
	}
}

func cardControlProof(kind, token string, revision int64) string {
	return cardHash(kind + "\x00" + token + "\x00" + strconv.FormatInt(revision, 10))
}

func cardControlProofs(card string) string {
	var doc struct {
		Body struct {
			Elements []taskCardElement
		}
	}
	if json.Unmarshal([]byte(card), &doc) != nil {
		return ""
	}
	var proofs []string
	var visit func([]taskCardElement)
	visit = func(elements []taskCardElement) {
		for _, e := range elements {
			switch e.Tag {
			case "button":
				for _, b := range e.Behaviors {
					if b.Type == "callback" && b.Value.Action != "" && b.Value.Token != "" && b.Value.Revision > 0 {
						proofs = append(proofs, cardControlProof(b.Value.Action, b.Value.Token, b.Value.Revision))
					}
				}
			case "column_set":
				// The title-row stop button is a sibling of the disclosure.
				// Only traverse native columns, never arbitrary JSON fields or
				// text pretending to contain an actionable button.
				for _, column := range e.Columns {
					if column.Tag == "column" {
						visit(column.Elements)
					}
				}
			}
		}
	}
	visit(doc.Body.Elements)
	sort.Strings(proofs)
	return strings.Join(proofs, ",")
}

func cardControlProofsContain(proofs, kind, token string, revision int64) bool {
	target, count := cardControlProof(kind, token, revision), 0
	for _, proof := range strings.Split(proofs, ",") {
		if proof == target {
			count++
		}
	}
	return count == 1
}

func taskCardHasControl(card, kind, token string, revision int64) bool {
	return cardControlProofsContain(cardControlProofs(card), kind, token, revision)
}

func (s *Store) FinishTaskControlClaim(ctx context.Context, scope SubmissionScope, claim TaskControlClaim, action SubmissionAction) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = submissionBinding(ctx, tx, scope); err != nil {
		return err
	}
	actual, err := readSubmissionAction(ctx, tx, scope, action.ID)
	if err != nil || actual.Token != claim.ExternalID || actual.Kind != claim.Kind || actual.ExpectedTurnID != claim.TurnID {
		return ErrConflict
	}
	state := actual.State
	if state == "ready" || state == "running" {
		state = "pending"
	}
	result, err := tx.ExecContext(ctx, `UPDATE task_control_claims SET native_action_id=?,state=?,result=? WHERE host_id=? AND app_id=? AND thread_id=? AND token=? AND revision=? AND kind=? AND (native_action_id='' OR native_action_id=?)`, actual.ID, state, actual.Result, scope.HostID, scope.AppID, scope.ThreadID, claim.Token, claim.Revision, claim.Kind, actual.ID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
