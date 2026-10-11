package store

import (
	"context"
	"database/sql"
	"errors"

	"codex-feishu/internal/cards"
)

// QueueCardKey identifies bridge UI, not a native turn or item. These documents
// share the existing reply/update journal but never advance history watermarks,
// appear in native history, or participate in final-delivery checks. Native turn
// IDs are UUIDs, so this reserved UI namespace cannot identify a native turn.
func QueueCardKey(receiptID string) string { return "ui:queue:" + receiptID }

// StageQueueCard reads current receipt state inside the same short transaction
// as its card mutation. A racing callback cannot publish obsolete buttons. Idle
// inputs need only reactions; create a card when queued, uncertain or failed,
// then keep updating that same card as its operation completes.
func (s *Store) StageQueueCard(ctx context.Context, scope SubmissionScope, receiptID string) error {
	return s.StageQueueCardWithOptions(ctx, scope, receiptID, QueueCardOptions{})
}

// LocalPending creates the offline receipt card only when no native attempt
// exists. Offline disables native controls regardless of old hydrated state.
type QueueCardOptions struct{ LocalPending, Offline bool }

func (s *Store) StageQueueCardWithOptions(ctx context.Context, scope SubmissionScope, receiptID string, options QueueCardOptions) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := stageQueueCardTx(ctx, tx, scope, receiptID, options); err != nil {
		return err
	}
	return tx.Commit()
}

func stageQueueCardTx(ctx context.Context, tx *sql.Tx, scope SubmissionScope, receiptID string, options QueueCardOptions) error {
	var err error
	if _, err = submissionBinding(ctx, tx, scope); err != nil {
		return err
	}
	n, err := readSubmission(ctx, tx, scope, receiptID)
	if err != nil {
		return err
	}
	key := QueueCardKey(n.ID)
	old, err := readMirror(ctx, tx, scope.HostID, scope.ThreadID, key)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	mirrorMissing := errors.Is(err, sql.ErrNoRows)
	localPending, checkErr := localPendingSubmission(ctx, tx, scope, n)
	if checkErr != nil {
		return checkErr
	}
	uncertainReceived := n.State == "received" && !localPending
	localPending = localPending && options.LocalPending
	if mirrorMissing && n.State != "queued" && n.State != "unknown" && n.State != "failed" && !localPending && !uncertainReceived {
		return nil
	}
	view := cards.QueueView{State: n.State, Token: n.ActionToken, QueueID: n.QueueID, Revision: n.Revision, QueueOrdinal: n.QueueOrdinal, LocalPending: localPending}
	if uncertainReceived {
		view.State = "unknown"
	}
	if n.State == "queued" && !options.Offline {
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_controls WHERE host_id=? AND app_id=? AND thread_id=? AND native_status='inProgress' AND kind='stop' AND state='available'`, scope.HostID, scope.AppID, scope.ThreadID).Scan(&active); err != nil {
			return err
		}
		view.Active = active == 1
		if !view.Active {
			var status string
			err := tx.QueryRowContext(ctx, `SELECT native_status FROM task_controls WHERE host_id=? AND app_id=? AND thread_id=? ORDER BY rowid DESC LIMIT 1`, scope.HostID, scope.AppID, scope.ThreadID).Scan(&status)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			view.Stopped = status == "interrupted"
		}
	}
	card := cards.QueueInputView(view)
	if options.Offline && n.State == "queued" {
		card = cards.Progress("Codex 连接已断开 · 队列已保留", "重新连接后核验队列；原生操作暂不可用。")
	}
	terminal := n.State == "consumed" || n.State == "steered" || n.State == "removed" || n.State == "failed"
	if mirrorMissing {
		_, err = tx.ExecContext(ctx, `INSERT INTO mirrors(host_id,thread_id,mirror_key,turn_id,kind,card_json,terminal,parent_message_id) VALUES(?,?,?,?,'progress',?,?,?)`, scope.HostID, scope.ThreadID, key, key, card, terminal, n.MessageID)
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO outbound(host_id,thread_id,mirror_key,state) VALUES(?,?,?,'ready')`, scope.HostID, scope.ThreadID, key)
		}
	} else {
		if old.Kind != "progress" || old.TurnID != key || old.ParentMessageID != n.MessageID || old.ParentKey != "" {
			return ErrConflict
		}
		if old.Terminal && !terminal {
			return ErrConflict
		}
		if old.Card != card || old.Terminal != terminal {
			_, err = tx.ExecContext(ctx, `UPDATE mirrors SET card_json=?,terminal=?,revision=revision+1 WHERE host_id=? AND thread_id=? AND mirror_key=?`, card, terminal, scope.HostID, scope.ThreadID, key)
			if err == nil {
				_, err = tx.ExecContext(ctx, `UPDATE outbound SET state='ready' WHERE host_id=? AND thread_id=? AND mirror_key=? AND state='confirmed'`, scope.HostID, scope.ThreadID, key)
			}
		}
	}
	if err != nil {
		return err
	}
	return nil
}
