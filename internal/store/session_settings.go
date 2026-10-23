package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"

	"codex-feishu/internal/cards"
)

const SessionSettingsSchema = `
CREATE TABLE IF NOT EXISTS session_settings_requests (
 id TEXT PRIMARY KEY, host_id TEXT NOT NULL REFERENCES deployment(host_id),
 app_id TEXT NOT NULL, message_id TEXT NOT NULL, thread_id TEXT NOT NULL,
 setting TEXT NOT NULL CHECK(setting IN ('model','reasoning','fast')),
 value TEXT NOT NULL, payload TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL CHECK(state IN ('ready','dispatching','unknown','confirmed','rejected')),
 created_at_ms INTEGER NOT NULL, next_check_at_ms INTEGER NOT NULL DEFAULT 0,
 UNIQUE(app_id,message_id)
);
CREATE INDEX IF NOT EXISTS session_settings_pending ON session_settings_requests(host_id,app_id,thread_id,state);
CREATE TABLE IF NOT EXISTS reply_settings_sources (
 host_id TEXT NOT NULL REFERENCES deployment(host_id), thread_id TEXT NOT NULL,
 turn_id TEXT NOT NULL, source TEXT NOT NULL,
 PRIMARY KEY(host_id,thread_id,turn_id)
);
`

type SessionSettingsRequest struct {
	ID, HostID, AppID, MessageID, ThreadID string
	Setting, Value, Payload, State         string
	CreatedAtMS, NextCheckAtMS             int64
}

const sessionSettingsColumns = `id,host_id,app_id,message_id,thread_id,setting,value,payload,state,created_at_ms,next_check_at_ms`

func scanSessionSettings(row rowScanner) (r SessionSettingsRequest, err error) {
	err = row.Scan(&r.ID, &r.HostID, &r.AppID, &r.MessageID, &r.ThreadID, &r.Setting, &r.Value, &r.Payload, &r.State, &r.CreatedAtMS, &r.NextCheckAtMS)
	return
}

func (s *Store) SessionSettingsRequests(ctx context.Context, scope SubmissionScope) ([]SessionSettingsRequest, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+sessionSettingsColumns+` FROM session_settings_requests r WHERE host_id=? AND app_id=? AND thread_id=? AND state IN ('ready','dispatching','unknown') AND NOT EXISTS(SELECT 1 FROM session_settings_forms f WHERE f.request_id=r.id AND f.phase IN ('editing','closed')) ORDER BY r.rowid LIMIT 64`, scope.HostID, scope.AppID, scope.ThreadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []SessionSettingsRequest
	for rows.Next() {
		r, err := scanSessionSettings(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// ClaimSessionSettings freezes the exact native request before the only write.
// A crash leaves dispatching for readback; it never makes the request ready.
func (s *Store) ClaimSessionSettings(ctx context.Context, r SessionSettingsRequest, payload string) (bool, error) {
	if r.Value == "" || payload == "" || len(payload) > 4096 || !json.Valid([]byte(payload)) {
		return false, ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := submissionBinding(ctx, tx, SubmissionScope{r.HostID, r.AppID, r.ThreadID}); err != nil {
		return false, err
	}
	saved, err := scanSessionSettings(tx.QueryRowContext(ctx, `SELECT `+sessionSettingsColumns+` FROM session_settings_requests WHERE id=?`, r.ID))
	if err != nil {
		return false, err
	}
	if saved != r || saved.State != "ready" {
		return false, nil
	}
	var formAllows bool
	if err := tx.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM session_settings_forms WHERE request_id=? AND (phase!='saving' OR action!='save'))`, r.ID).Scan(&formAllows); err != nil {
		return false, err
	}
	if !formAllows {
		return false, ErrConflict
	}
	var blocked bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_settings_requests WHERE host_id=? AND app_id=? AND thread_id=? AND state IN ('dispatching','unknown'))`, r.HostID, r.AppID, r.ThreadID).Scan(&blocked); err != nil {
		return false, err
	}
	if blocked {
		return false, nil
	}
	if err := commandWriteOne(ctx, tx, `UPDATE session_settings_requests SET state='dispatching',payload=? WHERE id=? AND state='ready'`, payload, r.ID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func validSessionSettingsFinish(state string, nowMS int64) bool {
	return (state == "confirmed" || state == "rejected" || state == "unknown") && nowMS > 0 && (state != "unknown" || nowMS <= math.MaxInt64-30000)
}

func finishSessionSettingsTx(ctx context.Context, tx *sql.Tx, r SessionSettingsRequest, state string, nowMS int64) error {
	saved, err := scanSessionSettings(tx.QueryRowContext(ctx, `SELECT `+sessionSettingsColumns+` FROM session_settings_requests WHERE id=? AND host_id=? AND app_id=? AND thread_id=?`, r.ID, r.HostID, r.AppID, r.ThreadID))
	if err != nil {
		return err
	}
	if saved.MessageID != r.MessageID || saved.Setting != r.Setting || saved.Value != r.Value {
		return ErrConflict
	}
	if saved.State == "confirmed" || saved.State == "rejected" {
		if saved.State == state {
			return nil
		}
		return ErrConflict
	}
	if state == "unknown" && saved.State == "ready" {
		return ErrConflict
	}
	next := int64(0)
	if state == "unknown" {
		next = nowMS + 30000
	}
	return commandWriteOne(ctx, tx, `UPDATE session_settings_requests SET state=?,next_check_at_ms=? WHERE id=?`, state, next, r.ID)
}

// FinishSessionSettings releases the original command reply atomically with
// the native result. Already-dispatched replies stay immutable; /model reads
// the current settings even while an older unknown write is being reconciled.
func (s *Store) FinishSessionSettings(ctx context.Context, r SessionSettingsRequest, state, card string, nowMS int64) error {
	if !validSessionSettingsFinish(state, nowMS) || len(card) > cards.DefaultMaxCardBytes || !json.Valid([]byte(card)) {
		return ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := finishSessionSettingsTx(ctx, tx, r, state, nowMS); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE command_replies SET card_json=? WHERE host_id=? AND app_id=? AND message_id=? AND state='ready' AND NOT EXISTS(SELECT 1 FROM command_reply_attempts a WHERE a.app_id=command_replies.app_id AND a.message_id=command_replies.message_id)`, card, r.HostID, r.AppID, r.MessageID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ReplySettingsSources(ctx context.Context, host, thread string) (map[string]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT turn_id,source FROM reply_settings_sources WHERE host_id=? AND thread_id=?`, host, thread)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]string{}
	for rows.Next() {
		var turn, source string
		if err := rows.Scan(&turn, &source); err != nil {
			return nil, err
		}
		result[turn] = source
	}
	return result, rows.Err()
}

func (s *Store) SaveReplySettingsSource(ctx context.Context, host, thread, turn, source string) (string, error) {
	if host == "" || thread == "" || turn == "" || source == "" || len(source) > 2048 {
		return "", ErrConflict
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO reply_settings_sources(host_id,thread_id,turn_id,source) VALUES(?,?,?,?) ON CONFLICT(host_id,thread_id,turn_id) DO NOTHING`, host, thread, turn, source)
	if err != nil {
		return "", err
	}
	err = s.DB.QueryRowContext(ctx, `SELECT source FROM reply_settings_sources WHERE host_id=? AND thread_id=? AND turn_id=?`, host, thread, turn).Scan(&source)
	return source, err
}
