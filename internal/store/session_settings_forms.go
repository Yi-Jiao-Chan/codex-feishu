package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"

	"codex-feishu/internal/cards"
)

const SessionSettingsFormsSchema = `
CREATE TABLE IF NOT EXISTS session_settings_forms (
 request_id TEXT PRIMARY KEY REFERENCES session_settings_requests(id),
 token TEXT NOT NULL UNIQUE,
 phase TEXT NOT NULL CHECK(phase IN ('loading','editing','saving','canceling','closed')),
 snapshot_json TEXT NOT NULL DEFAULT '{}', catalog_json TEXT NOT NULL DEFAULT '[]',
 selection_json TEXT NOT NULL DEFAULT '{}', action TEXT NOT NULL DEFAULT '' CHECK(action IN ('','save','cancel')),
 revision INTEGER NOT NULL DEFAULT 1 CHECK(revision>0),
 action_revision INTEGER NOT NULL DEFAULT 0 CHECK(action_revision>=0),
 expires_at_ms INTEGER NOT NULL CHECK(expires_at_ms>0)
);
`

// SessionSettingsCancelValue is local intent only, never a native settings patch.
const SessionSettingsCancelValue = "cancel"

// SessionSettingsForm keeps the editable UI separate from native write certainty.
// Opener identity and target are taken from its immutable command receipt.
type SessionSettingsForm struct {
	RequestID, Token, Phase, Snapshot, Catalog, Selection, Action string
	Revision, ActionRevision, ExpiresAtMS                         int64
}

const sessionSettingsFormColumns = `request_id,token,phase,snapshot_json,catalog_json,selection_json,action,revision,action_revision,expires_at_ms`

func scanSessionSettingsForm(row rowScanner) (f SessionSettingsForm, err error) {
	err = row.Scan(&f.RequestID, &f.Token, &f.Phase, &f.Snapshot, &f.Catalog, &f.Selection, &f.Action, &f.Revision, &f.ActionRevision, &f.ExpiresAtMS)
	return
}

func (s *Store) SessionSettingsForm(ctx context.Context, id string) (SessionSettingsForm, error) {
	return scanSessionSettingsForm(s.DB.QueryRowContext(ctx, `SELECT `+sessionSettingsFormColumns+` FROM session_settings_forms WHERE request_id=?`, id))
}

func (s *Store) SessionSettingsFormByToken(ctx context.Context, scope SubmissionScope, token string) (SessionSettingsForm, error) {
	return scanSessionSettingsForm(s.DB.QueryRowContext(ctx, `SELECT `+sessionSettingsFormColumns+` FROM session_settings_forms f WHERE token=? AND EXISTS(SELECT 1 FROM session_settings_requests r WHERE r.id=f.request_id AND r.host_id=? AND r.app_id=? AND r.thread_id=?)`, token, scope.HostID, scope.AppID, scope.ThreadID))
}

func SessionSettingsCardKey(id string) string { return "ui:session-settings:" + id }

func createSessionSettingsFormTx(ctx context.Context, tx *sql.Tx, r SessionSettingsRequest, nowMS int64) error {
	if nowMS <= 0 || nowMS > math.MaxInt64-int64(time.Hour/time.Millisecond) {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_settings_forms(request_id,token,phase,expires_at_ms) VALUES(?,?,'loading',?)`, r.ID, ID(), nowMS+int64(time.Hour/time.Millisecond)); err != nil {
		return err
	}
	key := SessionSettingsCardKey(r.ID)
	card := cards.Message("会话设置", "Codex", "正在读取当前会话设置…")
	if _, err := tx.ExecContext(ctx, `INSERT INTO mirrors(host_id,thread_id,mirror_key,turn_id,kind,card_json,card_hash,terminal,parent_message_id) VALUES(?,?,?,?,'progress',?,?,0,?)`, r.HostID, r.ThreadID, key, key, card, cardHash(card), r.MessageID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO outbound(host_id,thread_id,mirror_key,state) VALUES(?,?,?,'ready')`, r.HostID, r.ThreadID, key)
	return err
}

func sessionSettingsJSON(raw string, array bool, limit int) bool {
	if len(raw) > limit {
		return false
	}
	if array {
		var value []json.RawMessage
		return json.Unmarshal([]byte(raw), &value) == nil && value != nil
	}
	var value map[string]json.RawMessage
	return json.Unmarshal([]byte(raw), &value) == nil && value != nil
}
func validSessionSettingsForm(f SessionSettingsForm) bool {
	return f.RequestID != "" && f.Token != "" && f.Revision > 0 && f.ActionRevision >= 0 && f.ExpiresAtMS > 0 && sessionSettingsJSON(f.Snapshot, false, 32768) && sessionSettingsJSON(f.Catalog, true, 1<<20) && sessionSettingsJSON(f.Selection, false, 4096)
}
func validSessionSettingsCard(card string) bool {
	return len(card) <= cards.DefaultMaxCardBytes && sessionSettingsJSON(card, false, cards.DefaultMaxCardBytes)
}
func nextSessionSettingsForm(expected, next SessionSettingsForm) bool {
	return validSessionSettingsForm(next) && expected.RequestID == next.RequestID && expected.Token == next.Token && expected.ExpiresAtMS == next.ExpiresAtMS && expected.Revision < math.MaxInt64 && next.Revision == expected.Revision+1
}

// sessionSettingsIdentityTx revalidates the persisted target and opener receipt.
func sessionSettingsIdentityTx(ctx context.Context, tx *sql.Tx, r SessionSettingsRequest) (SessionSettingsRequest, CommandReply, error) {
	saved, err := scanSessionSettings(tx.QueryRowContext(ctx, `SELECT `+sessionSettingsColumns+` FROM session_settings_requests WHERE id=? AND host_id=? AND app_id=? AND thread_id=?`, r.ID, r.HostID, r.AppID, r.ThreadID))
	if err != nil {
		return saved, CommandReply{}, err
	}
	if saved.MessageID != r.MessageID || saved.Setting != r.Setting || saved.Value != r.Value {
		return saved, CommandReply{}, ErrConflict
	}
	chat, err := submissionBinding(ctx, tx, SubmissionScope{r.HostID, r.AppID, r.ThreadID})
	if err != nil {
		return saved, CommandReply{}, err
	}
	command, err := scanCommandReply(tx.QueryRowContext(ctx, `SELECT `+commandColumns+` FROM command_replies WHERE host_id=? AND app_id=? AND message_id=?`, r.HostID, r.AppID, r.MessageID))
	if err != nil {
		return saved, command, err
	}
	if command.Kind != "model_settings" || command.ThreadID != r.ThreadID || command.ChatID != chat || command.ChatType != "group" || command.UserID == "" {
		return saved, command, ErrConflict
	}
	return saved, command, nil
}

func writeSessionSettingsFormTx(ctx context.Context, tx *sql.Tx, f SessionSettingsForm) error {
	return commandWriteOne(ctx, tx, `UPDATE session_settings_forms SET phase=?,snapshot_json=?,catalog_json=?,selection_json=?,action=?,revision=?,action_revision=? WHERE request_id=?`, f.Phase, f.Snapshot, f.Catalog, f.Selection, f.Action, f.Revision, f.ActionRevision, f.RequestID)
}

// stageSessionSettingsCardTx owns only this UI mirror. It never advances a
// history watermark or reopens an unknown/rejected delivery attempt.
func stageSessionSettingsCardTx(ctx context.Context, tx *sql.Tx, r SessionSettingsRequest, card string, terminal bool) error {
	key := SessionSettingsCardKey(r.ID)
	old, err := readMirror(ctx, tx, r.HostID, r.ThreadID, key)
	if err != nil {
		return err
	}
	if old.Kind != "progress" || old.TurnID != key || old.ParentKey != "" || old.ParentMessageID != r.MessageID || old.Retired || old.Terminal && !terminal {
		return ErrConflict
	}
	if mirrorCardEqual(old, card) && old.Terminal == terminal {
		return nil
	}
	if err := commandWriteOne(ctx, tx, `UPDATE mirrors SET card_json=?,card_hash=?,projection_version=0,terminal=?,revision=revision+1 WHERE host_id=? AND thread_id=? AND mirror_key=?`, card, cardHash(card), terminal, r.HostID, r.ThreadID, key); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE outbound SET state='ready' WHERE host_id=? AND thread_id=? AND mirror_key=? AND state='confirmed'`, r.HostID, r.ThreadID, key)
	return err
}

// StageSessionSettingsForm publishes the loaded form by CAS, leaving its
// native request ready but excluded from the worker until the user saves.
func (s *Store) StageSessionSettingsForm(ctx context.Context, r SessionSettingsRequest, expected, next SessionSettingsForm, card string) error {
	if expected.RequestID != r.ID || expected.Phase != "loading" || next.Phase != "editing" || expected.Action != "" || next.Action != "" || expected.ActionRevision != 0 || next.ActionRevision != 0 || !nextSessionSettingsForm(expected, next) || !validSessionSettingsCard(card) {
		return ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	saved, _, err := sessionSettingsIdentityTx(ctx, tx, r)
	if err != nil {
		return err
	}
	if saved != r || saved.State != "ready" || saved.Value != "" || saved.Payload != "" {
		return ErrConflict
	}
	actual, err := scanSessionSettingsForm(tx.QueryRowContext(ctx, `SELECT `+sessionSettingsFormColumns+` FROM session_settings_forms WHERE request_id=?`, r.ID))
	if err != nil {
		return err
	}
	if actual != expected {
		return ErrConflict
	}
	if err := writeSessionSettingsFormTx(ctx, tx, next); err != nil {
		return err
	}
	if err := stageSessionSettingsCardTx(ctx, tx, r, card, false); err != nil {
		return err
	}
	return tx.Commit()
}

// ClaimSessionSettingsFormAction freezes the first save/cancel intent locally.
// Exact duplicates are acknowledged even after delivery/reconciliation advances;
// another action or selection at the consumed revision can never win later.
func (s *Store) ClaimSessionSettingsFormAction(ctx context.Context, scope SubmissionScope, user, chat, message, token string, revision int64, action, selection, pendingCard string, nowMS int64) (SessionSettingsForm, bool, error) {
	var f SessionSettingsForm
	if action == "cancel" {
		selection = "{}"
	}
	if user == "" || chat == "" || message == "" || token == "" || revision <= 0 || nowMS <= 0 || action != "save" && action != "cancel" || !sessionSettingsJSON(selection, false, 4096) || !validSessionSettingsCard(pendingCard) {
		return f, false, ErrSubmissionStale
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return f, false, err
	}
	defer tx.Rollback()
	if err := directoryDeployment(ctx, tx, scope.HostID, scope.AppID); err != nil {
		return f, false, err
	}
	f, err = scanSessionSettingsForm(tx.QueryRowContext(ctx, `SELECT `+sessionSettingsFormColumns+` FROM session_settings_forms f WHERE token=? AND EXISTS(SELECT 1 FROM session_settings_requests r WHERE r.id=f.request_id AND r.host_id=? AND r.app_id=? AND r.thread_id=?)`, token, scope.HostID, scope.AppID, scope.ThreadID))
	if errors.Is(err, sql.ErrNoRows) {
		return f, false, ErrSubmissionStale
	}
	if err != nil {
		return f, false, err
	}
	r, err := scanSessionSettings(tx.QueryRowContext(ctx, `SELECT `+sessionSettingsColumns+` FROM session_settings_requests WHERE id=?`, f.RequestID))
	if err != nil {
		return f, false, err
	}
	_, command, err := sessionSettingsIdentityTx(ctx, tx, r)
	if err != nil {
		return f, false, err
	}
	if command.UserID != user || command.ChatID != chat {
		return f, false, ErrSubmissionStale
	}
	m, err := readMirror(ctx, tx, scope.HostID, scope.ThreadID, SessionSettingsCardKey(r.ID))
	if err != nil {
		return f, false, err
	}
	if m.RemoteID == "" || m.RemoteID != message {
		return f, false, ErrSubmissionStale
	}
	if f.ActionRevision == revision {
		if f.Action != action || f.Selection != selection {
			return f, false, ErrSubmissionStale
		}
		return f, false, tx.Commit()
	}
	if f.Phase != "editing" || f.Revision != revision || f.Action != "" || f.ActionRevision != 0 || f.ExpiresAtMS <= nowMS || f.Revision == math.MaxInt64 || r.State != "ready" || r.Payload != "" || r.Value != "" || m.State != "confirmed" || m.DeliveredRevision != m.Revision || m.Retired {
		return f, false, ErrSubmissionStale
	}
	f.Action, f.Selection, f.ActionRevision = action, selection, revision
	f.Revision++
	value := selection
	f.Phase = "saving"
	if action == "cancel" {
		f.Phase = "canceling"
		value = SessionSettingsCancelValue
	}
	if err := writeSessionSettingsFormTx(ctx, tx, f); err != nil {
		return f, false, err
	}
	if err := commandWriteOne(ctx, tx, `UPDATE session_settings_requests SET value=? WHERE id=? AND state='ready' AND value='' AND payload=''`, value, r.ID); err != nil {
		return f, false, err
	}
	if err := stageSessionSettingsCardTx(ctx, tx, r, pendingCard, false); err != nil {
		return f, false, err
	}
	if err := tx.Commit(); err != nil {
		return f, false, err
	}
	return f, true, nil
}

// FinishSessionSettingsForm atomically records native certainty and replaces
// the original UI card. It never changes a frozen native request payload.
func (s *Store) FinishSessionSettingsForm(ctx context.Context, r SessionSettingsRequest, expected, next SessionSettingsForm, nativeState, card string, nowMS int64) error {
	allowed := expected.Phase == "loading" && nativeState == "rejected" && next.Phase == "closed" || expected.Phase == "saving" && (nativeState == "unknown" && next.Phase == "saving" || (nativeState == "confirmed" || nativeState == "rejected") && next.Phase == "closed") || expected.Phase == "canceling" && nativeState == "confirmed" && next.Phase == "closed"
	if !allowed || expected.RequestID != r.ID || !nextSessionSettingsForm(expected, next) || expected.Selection != next.Selection || expected.Action != next.Action || expected.ActionRevision != next.ActionRevision || !validSessionSettingsCard(card) || !validSessionSettingsFinish(nativeState, nowMS) {
		return ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	saved, _, err := sessionSettingsIdentityTx(ctx, tx, r)
	if err != nil {
		return err
	}
	actual, err := scanSessionSettingsForm(tx.QueryRowContext(ctx, `SELECT `+sessionSettingsFormColumns+` FROM session_settings_forms WHERE request_id=?`, r.ID))
	if err != nil {
		return err
	}
	if actual != expected {
		if actual == next && saved.State == nativeState {
			m, readErr := readMirror(ctx, tx, r.HostID, r.ThreadID, SessionSettingsCardKey(r.ID))
			if readErr == nil && mirrorCardEqual(m, card) {
				return nil
			}
		}
		return ErrConflict
	}
	if err := finishSessionSettingsTx(ctx, tx, r, nativeState, nowMS); err != nil {
		return err
	}
	if err := writeSessionSettingsFormTx(ctx, tx, next); err != nil {
		return err
	}
	if err := stageSessionSettingsCardTx(ctx, tx, r, card, next.Phase == "closed"); err != nil {
		return err
	}
	return tx.Commit()
}
