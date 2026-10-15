package store

import (
	"context"
	"database/sql"
	"errors"
)

var ErrTargetRepair = errors.New("follow target requires repair")

const bindingColumns = `host_id,thread_id,intent_id,chat_id,follow_mode,root_message_id,platform_thread_id,repair_required,repair_reason`

func scanBinding(row rowScanner, b *Binding) error {
	err := row.Scan(&b.HostID, &b.ThreadID, &b.IntentID, &b.ChatID, &b.FollowMode, &b.RootMessageID, &b.PlatformThreadID, &b.RepairRequired, &b.RepairReason)
	if b.FollowMode == "group" {
		b.FollowMode = ""
	}
	return err
}

// BindFollowTarget requires the existing, confirmed creation journal. A target
// is immutable: repair cannot silently move a native conversation elsewhere.
func (s *Store) BindFollowTarget(ctx context.Context, b Binding) error {
	mode := b.FollowMode
	if mode == "" {
		mode = "group"
	}
	if b.HostID == "" || b.ThreadID == "" || b.IntentID == "" || b.ChatID == "" || mode != "group" && mode != "topic" || mode == "topic" && b.RootMessageID == "" || mode == "group" && b.PlatformThreadID != "" {
		return ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state, chat, host, thread string
	if err = tx.QueryRowContext(ctx, `SELECT state,chat_id,host_id,thread_id FROM create_intents WHERE intent_id=?`, b.IntentID).Scan(&state, &chat, &host, &thread); err != nil {
		return err
	}
	if state != "confirmed" || chat != b.ChatID || host != b.HostID || thread != b.ThreadID {
		return ErrConflict
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO bindings(host_id,thread_id,intent_id,chat_id,follow_mode,root_message_id,platform_thread_id) VALUES(?,?,?,?,?,?,?) ON CONFLICT(host_id,thread_id) DO NOTHING`, b.HostID, b.ThreadID, b.IntentID, b.ChatID, mode, b.RootMessageID, b.PlatformThreadID)
	if err != nil {
		return err
	}
	var actual Binding
	if err = scanBinding(tx.QueryRowContext(ctx, `SELECT `+bindingColumns+` FROM bindings WHERE host_id=? AND thread_id=?`, b.HostID, b.ThreadID), &actual); err != nil {
		return err
	}
	if actual.IntentID != b.IntentID || actual.ChatID != b.ChatID || (actual.FollowMode == "topic") != (mode == "topic") || actual.RootMessageID != b.RootMessageID || actual.PlatformThreadID != b.PlatformThreadID {
		return ErrConflict
	}
	return tx.Commit()
}

// BindingByTarget never guesses a topic from the chat's latest activity. When
// both platform identities are known they must agree. An exact root can prove
// a new platform thread before the first reply response has recorded its ID.
func (s *Store) BindingByTarget(ctx context.Context, host, chat, root, platformThread string) (Binding, error) {
	group, err := s.BindingByChat(ctx, host, chat)
	if err == nil {
		return group, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Binding{}, err
	}
	if root == "" && platformThread == "" {
		return Binding{}, sql.ErrNoRows
	}
	var b Binding
	err = scanBinding(s.DB.QueryRowContext(ctx, `SELECT `+bindingColumns+` FROM bindings WHERE host_id=? AND chat_id=? AND follow_mode='topic' AND (?='' OR root_message_id=?) AND (?='' OR platform_thread_id=? OR (platform_thread_id='' AND root_message_id=? AND ?!=''))`, host, chat, root, root, platformThread, platformThread, root, root), &b)
	return b, err
}

// BindingByMessage accepts only durable platform message identities, never a
// client-provided native thread. Ambiguous accounting fails closed.
func (s *Store) BindingByMessage(ctx context.Context, host, chat, message string) (Binding, error) {
	if host == "" || chat == "" || message == "" {
		return Binding{}, sql.ErrNoRows
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+bindingColumns+` FROM bindings b WHERE b.host_id=? AND b.chat_id=? AND (
 b.root_message_id=? OR EXISTS(SELECT 1 FROM mirrors m WHERE m.host_id=b.host_id AND m.thread_id=b.thread_id AND m.remote_id=?)
 OR EXISTS(SELECT 1 FROM outbound_attempts a WHERE a.host_id=b.host_id AND a.thread_id=b.thread_id AND a.chat_id=b.chat_id AND a.state='confirmed' AND a.remote_id=?)
 OR EXISTS(SELECT 1 FROM m0_receipts r JOIN deployment d ON d.app_id=r.app_id WHERE d.host_id=b.host_id AND r.thread_id=b.thread_id AND r.chat_id=b.chat_id AND (r.message_id=? OR r.card_id=?))
 OR EXISTS(SELECT 1 FROM command_replies c WHERE c.host_id=b.host_id AND c.thread_id=b.thread_id AND c.chat_id=b.chat_id AND (c.message_id=? OR c.remote_id=?))
 OR EXISTS(SELECT 1 FROM request_reminder_attempts a WHERE a.host_id=b.host_id AND a.thread_id=b.thread_id AND a.chat_id=b.chat_id AND a.state='confirmed' AND a.remote_id=?)
 )`, host, chat, message, message, message, message, message, message, message, message)
	if err != nil {
		return Binding{}, err
	}
	defer rows.Close()
	var b Binding
	count := 0
	for rows.Next() {
		if err = scanBinding(rows, &b); err != nil {
			return Binding{}, err
		}
		count++
	}
	if err = rows.Err(); err != nil {
		return Binding{}, err
	}
	if count == 0 {
		return Binding{}, sql.ErrNoRows
	}
	if count != 1 {
		return Binding{}, ErrConflict
	}
	return b, nil
}

func (s *Store) MarkBindingRepair(ctx context.Context, host, thread, reason string) error {
	if reason == "" {
		return ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `UPDATE bindings SET repair_required=1,repair_reason=? WHERE host_id=? AND thread_id=?`, reason, host, thread)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	var schema int
	if err = tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&schema); err != nil {
		return err
	}
	if schema < 28 {
		return tx.Commit()
	}
	var draft string
	err = tx.QueryRowContext(ctx, `SELECT d.draft_id
FROM bindings b
JOIN creation_workflows i ON i.intent_id=b.intent_id
JOIN creation_drafts d ON d.draft_id=i.draft_id
WHERE b.host_id=? AND b.thread_id=?
  AND i.host_id=b.host_id AND i.native_thread_id=b.thread_id
  AND i.target_chat_id=b.chat_id AND i.root_message_id=b.root_message_id
  AND i.follow_mode='topic' AND b.follow_mode='topic'
  AND d.host_id=i.host_id AND d.app_id=i.app_id
  AND d.state IN ('creating','created')`, host, thread).Scan(&draft)
	if errors.Is(err, sql.ErrNoRows) {
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE creation_drafts SET error_code='target-root',revision=revision+1 WHERE draft_id=? AND error_code<>'target-root'`, draft); err != nil {
		return err
	}
	return tx.Commit()
}

const FollowTargetsSchema = `
PRAGMA defer_foreign_keys=ON;
CREATE TABLE bindings_v27 (
 host_id TEXT NOT NULL,thread_id TEXT NOT NULL,intent_id TEXT NOT NULL UNIQUE REFERENCES create_intents(intent_id),
 chat_id TEXT NOT NULL,follow_mode TEXT NOT NULL DEFAULT 'group' CHECK(follow_mode IN ('group','topic')),
 root_message_id TEXT NOT NULL DEFAULT '',platform_thread_id TEXT NOT NULL DEFAULT '',
 repair_required INTEGER NOT NULL DEFAULT 0 CHECK(repair_required IN (0,1)),repair_reason TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(host_id,thread_id),CHECK(follow_mode!='topic' OR root_message_id!=''),CHECK(follow_mode!='group' OR platform_thread_id='')
);
INSERT INTO bindings_v27(host_id,thread_id,intent_id,chat_id) SELECT host_id,thread_id,intent_id,chat_id FROM bindings;
DROP TABLE bindings;
ALTER TABLE bindings_v27 RENAME TO bindings;
CREATE UNIQUE INDEX bindings_group_chat ON bindings(host_id,chat_id) WHERE follow_mode='group';
CREATE UNIQUE INDEX bindings_topic_root ON bindings(host_id,chat_id,root_message_id) WHERE follow_mode='topic';
CREATE UNIQUE INDEX bindings_topic_thread ON bindings(host_id,chat_id,platform_thread_id) WHERE follow_mode='topic' AND platform_thread_id!='';
CREATE TRIGGER bindings_no_mixed_insert BEFORE INSERT ON bindings WHEN EXISTS(SELECT 1 FROM bindings b WHERE b.host_id=NEW.host_id AND b.chat_id=NEW.chat_id AND b.follow_mode!=NEW.follow_mode) BEGIN SELECT RAISE(ABORT,'mixed group/topic target'); END;
CREATE TRIGGER bindings_no_mixed_update BEFORE UPDATE ON bindings WHEN EXISTS(SELECT 1 FROM bindings b WHERE b.host_id=NEW.host_id AND b.chat_id=NEW.chat_id AND b.follow_mode!=NEW.follow_mode AND (b.host_id!=OLD.host_id OR b.thread_id!=OLD.thread_id)) BEGIN SELECT RAISE(ABORT,'mixed group/topic target'); END;
`

// RecordBindingPlatformThread accepts only the first platform-proven identity.
// A conflicting response cannot retarget an existing conversation.
func (s *Store) RecordBindingPlatformThread(ctx context.Context, host, thread, platformThread string) error {
	if platformThread == "" {
		return ErrConflict
	}
	r, err := s.DB.ExecContext(ctx, `UPDATE bindings SET platform_thread_id=? WHERE host_id=? AND thread_id=? AND follow_mode='topic' AND (platform_thread_id='' OR platform_thread_id=?)`, platformThread, host, thread, platformThread)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		return ErrConflict
	}
	return err
}
