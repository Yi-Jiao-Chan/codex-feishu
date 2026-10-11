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
	key := QueueCardKey(n.ID)
	old, err := readMirror(ctx, tx, scope.HostID, scope.ThreadID, key)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) && n.State != "queued" && n.State != "unknown" && n.State != "failed" {
		return nil
	}
	card := cards.QueueInput(n.State, n.ActionToken, n.QueueID, n.Revision)
	terminal := n.State == "consumed" || n.State == "steered" || n.State == "removed" || n.State == "failed"
	if errors.Is(err, sql.ErrNoRows) {
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
	return tx.Commit()
}
