package store

import (
	"context"
	"database/sql"
	"errors"
)

// FollowAttemptSchema journals targets for command/reminder sends in the same
// transaction as their immutable payload. Old attempts are never backfilled.
const FollowAttemptSchema = `
CREATE TABLE IF NOT EXISTS frozen_follow_attempts (
 attempt_id TEXT PRIMARY KEY,host_id TEXT NOT NULL,thread_id TEXT NOT NULL,chat_id TEXT NOT NULL,
 follow_mode TEXT NOT NULL CHECK(follow_mode IN ('group','topic')),
 root_message_id TEXT NOT NULL,platform_thread_id TEXT NOT NULL,parent_message_id TEXT NOT NULL CHECK(parent_message_id!=''),
 CHECK(follow_mode!='topic' OR root_message_id!='' OR platform_thread_id!='')
);
CREATE TABLE IF NOT EXISTS command_source_targets (
 app_id TEXT NOT NULL,message_id TEXT NOT NULL,host_id TEXT NOT NULL,chat_id TEXT NOT NULL,
 root_message_id TEXT NOT NULL,platform_thread_id TEXT NOT NULL,
 PRIMARY KEY(app_id,message_id),FOREIGN KEY(app_id,message_id) REFERENCES command_replies(app_id,message_id),
 CHECK(root_message_id!='' OR platform_thread_id!='')
);
`

type FollowAttemptTarget struct {
	AttemptID, HostID, ThreadID, ChatID, FollowMode, RootMessageID, PlatformThreadID, ParentMessageID string `json:"-"`
}

const followAttemptColumns = `attempt_id,host_id,thread_id,chat_id,follow_mode,root_message_id,platform_thread_id,parent_message_id`

func readFollowAttemptTarget(ctx context.Context, q submissionQuerier, id string) (t FollowAttemptTarget, err error) {
	err = q.QueryRowContext(ctx, `SELECT `+followAttemptColumns+` FROM frozen_follow_attempts WHERE attempt_id=?`, id).Scan(&t.AttemptID, &t.HostID, &t.ThreadID, &t.ChatID, &t.FollowMode, &t.RootMessageID, &t.PlatformThreadID, &t.ParentMessageID)
	return
}
func (s *Store) FollowAttemptTarget(ctx context.Context, id string) (FollowAttemptTarget, error) {
	return readFollowAttemptTarget(ctx, s.DB, id)
}

func freezeFollowAttemptTx(ctx context.Context, tx *sql.Tx, id, host, thread, chat, parent string) error {
	if id == "" || host == "" || chat == "" || parent == "" {
		return ErrConflict
	}
	t := FollowAttemptTarget{AttemptID: id, HostID: host, ThreadID: thread, ChatID: chat, FollowMode: "group", ParentMessageID: parent}
	var b Binding
	err := scanBinding(tx.QueryRowContext(ctx, `SELECT `+bindingColumns+` FROM bindings WHERE host_id=? AND thread_id=?`, host, thread), &b)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if b.ChatID != chat {
			return ErrConflict
		}
		if b.RepairRequired {
			return ErrTargetRepair
		}
		t.RootMessageID, t.PlatformThreadID = b.RootMessageID, b.PlatformThreadID
		if b.FollowMode == "topic" {
			t.FollowMode = "topic"
			known, err := followParentKnownTx(ctx, tx, b, parent)
			if err != nil {
				return err
			}
			if !known {
				return ErrConflict
			}
		}

	} else if thread == "" {
		// Source targets are trusted platform event fields saved atomically with
		// this command. A topic outside any native binding still stays in topic.
		err = tx.QueryRowContext(ctx, `SELECT source.root_message_id,source.platform_thread_id FROM command_source_targets source JOIN command_replies c USING(app_id,message_id) WHERE c.attempt_id=? AND c.host_id=? AND c.chat_id=? AND c.message_id=? AND c.thread_id='' AND source.host_id=c.host_id AND source.chat_id=c.chat_id`, id, host, chat, parent).Scan(&t.RootMessageID, &t.PlatformThreadID)
		if err == nil {
			t.FollowMode = "topic"
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	} else {
		return ErrConflict
	}

	return commandWriteOne(ctx, tx, `INSERT INTO frozen_follow_attempts(`+followAttemptColumns+`) VALUES(?,?,?,?,?,?,?,?)`, t.AttemptID, t.HostID, t.ThreadID, t.ChatID, t.FollowMode, t.RootMessageID, t.PlatformThreadID, t.ParentMessageID)
}

// Only platform messages already attributed to this exact binding are valid
// topic parents. This never accepts a client-provided native thread as proof.
func followParentKnownTx(ctx context.Context, tx *sql.Tx, b Binding, parent string) (bool, error) {
	if parent == b.RootMessageID {
		return true, nil
	}
	var n int
	err := tx.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM mirrors WHERE host_id=? AND thread_id=? AND remote_id=?)
 OR EXISTS(SELECT 1 FROM outbound_attempts WHERE host_id=? AND thread_id=? AND chat_id=? AND state='confirmed' AND remote_id=?)
 OR EXISTS(SELECT 1 FROM m0_receipts r JOIN deployment d ON d.app_id=r.app_id WHERE d.host_id=? AND r.thread_id=? AND r.chat_id=? AND (r.message_id=? OR r.reply_message_id=? OR r.card_id=?))
 OR EXISTS(SELECT 1 FROM command_replies WHERE host_id=? AND thread_id=? AND chat_id=? AND (message_id=? OR remote_id=?))
 OR EXISTS(SELECT 1 FROM request_reminder_attempts WHERE host_id=? AND thread_id=? AND chat_id=? AND state='confirmed' AND remote_id=?)`, b.HostID, b.ThreadID, parent, b.HostID, b.ThreadID, b.ChatID, parent, b.HostID, b.ThreadID, b.ChatID, parent, parent, parent, b.HostID, b.ThreadID, b.ChatID, parent, parent, b.HostID, b.ThreadID, b.ChatID, parent).Scan(&n)
	return n != 0, err
}

func recordCommandSourceTargetTx(ctx context.Context, tx *sql.Tx, req CommandRequest) error {
	if req.SourceRootID == "" && req.SourceThreadID == "" {
		return nil
	}
	if req.ChatType != "group" {
		return ErrConflict
	}
	return commandWriteOne(ctx, tx, `INSERT INTO command_source_targets(app_id,message_id,host_id,chat_id,root_message_id,platform_thread_id) VALUES(?,?,?,?,?,?)`, req.AppID, req.MessageID, req.HostID, req.ChatID, req.SourceRootID, req.SourceThreadID)
}
