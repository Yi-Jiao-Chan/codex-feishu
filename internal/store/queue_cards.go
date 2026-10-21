package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"codex-feishu/internal/cards"
)

// QueueCardKey identifies bridge UI, not a native turn or item. These documents
// share the existing reply/update journal but never advance history watermarks,
// appear in native history, or participate in final-delivery checks. Native turn
// IDs are UUIDs, so this reserved UI namespace cannot identify a native turn.
func QueueCardKey(receiptID string) string { return "ui:queue:" + receiptID }

type submissionCardIdentity struct{ key, parent, parentKey string }

// Keep existing queue journals authoritative, including ambiguous deliveries.
// New attachment submissions retain their original card and immutable reply
// parent; a later instruction receipt must not change that physical relation.
func submissionCardIdentityTx(ctx context.Context, tx *sql.Tx, scope SubmissionScope, n Submission) (submissionCardIdentity, error) {
	identity := submissionCardIdentity{key: QueueCardKey(n.ID), parent: n.ReplyID()}
	if _, err := readMirror(ctx, tx, scope.HostID, scope.ThreadID, identity.key); err == nil {
		return identity, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return identity, err
	}
	var request, parentKey, parentMessage string
	err := tx.QueryRowContext(ctx, `SELECT request_key,parent_key,parent_message_id FROM interaction_requests
 WHERE host_id=? AND app_id=? AND thread_id=? AND mode='async' AND submission_id=?`, scope.HostID, scope.AppID, scope.ThreadID, n.ID).Scan(&request, &parentKey, &parentMessage)
	if err == nil {
		return submissionCardIdentity{key: InteractionCardKey(request), parent: parentMessage, parentKey: parentKey}, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return identity, err
	}
	var count int
	var batch, parent string
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MIN(batch_id),''),COALESCE(MIN(first_message_id),'') FROM attachment_batches
 WHERE host_id=? AND app_id=? AND thread_id=? AND chat_id=? AND receipt_id=? AND state<>'cleared'`, scope.HostID, scope.AppID, scope.ThreadID, n.ChatID, n.ID).Scan(&count, &batch, &parent)
	if err != nil {
		return identity, err
	}
	if count > 1 || count == 1 && parent == "" {
		return identity, ErrConflict
	}
	if count == 1 {
		identity = submissionCardIdentity{key: AttachmentCardKey(batch), parent: parent}
	}
	return identity, nil
}

func validSubmissionCard(m Mirror, identity submissionCardIdentity) bool {
	return m.Key == identity.key && m.Kind == "progress" && m.TurnID == identity.key && m.ParentMessageID == identity.parent && m.ParentKey == identity.parentKey && !m.Retired
}

// SubmissionCard resolves and validates the one authoritative UI document in
// the same snapshot as its confirmed binding and receipt. Callers additionally
// compare its nonempty RemoteID to the authenticated callback's message ID.
func (s *Store) SubmissionCard(ctx context.Context, scope SubmissionScope, receiptID string) (Mirror, error) {
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Mirror{}, err
	}
	defer tx.Rollback()
	if _, err := submissionBinding(ctx, tx, scope); err != nil {
		return Mirror{}, err
	}
	n, err := readSubmission(ctx, tx, scope, receiptID)
	if err != nil {
		return Mirror{}, err
	}
	identity, err := submissionCardIdentityTx(ctx, tx, scope, n)
	if err != nil {
		return Mirror{}, err
	}
	m, err := readMirror(ctx, tx, scope.HostID, scope.ThreadID, identity.key)
	if err != nil {
		return Mirror{}, err
	}
	if !validSubmissionCard(m, identity) {
		return Mirror{}, ErrConflict
	}
	return m, tx.Commit()
}

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

// SubmissionQueueView shares verified queue facts with an asynchronous question card.
func (s *Store) SubmissionQueueView(ctx context.Context, scope SubmissionScope, receiptID string, options QueueCardOptions) (cards.QueueView, error) {
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return cards.QueueView{}, err
	}
	defer tx.Rollback()
	if _, err = submissionBinding(ctx, tx, scope); err != nil {
		return cards.QueueView{}, err
	}
	n, err := readSubmission(ctx, tx, scope, receiptID)
	if err != nil {
		return cards.QueueView{}, err
	}
	view, err := submissionQueueViewTx(ctx, tx, scope, n, options)
	if err != nil {
		return view, err
	}
	return view, tx.Commit()
}

func submissionQueueViewTx(ctx context.Context, tx *sql.Tx, scope SubmissionScope, n Submission, options QueueCardOptions) (cards.QueueView, error) {
	localPending, err := localPendingSubmission(ctx, tx, scope, n)
	if err != nil {
		return cards.QueueView{}, err
	}
	view := cards.QueueView{State: n.State, Token: n.ActionToken, QueueID: n.QueueID, Revision: n.Revision, QueueOrdinal: n.QueueOrdinal, LocalPending: localPending && options.LocalPending}
	if n.State == "received" && !localPending {
		view.State = "unknown"
	}
	if options.Offline {
		if !view.LocalPending {
			view.Token = ""
		}
	} else if n.State == "queued" {
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_controls WHERE host_id=? AND app_id=? AND thread_id=? AND native_status='inProgress' AND kind='stop' AND state='available'`, scope.HostID, scope.AppID, scope.ThreadID).Scan(&active); err != nil {
			return view, err
		}
		view.Active = active == 1
		if !view.Active {
			var status string
			err := tx.QueryRowContext(ctx, `SELECT native_status FROM task_controls WHERE host_id=? AND app_id=? AND thread_id=? ORDER BY rowid DESC LIMIT 1`, scope.HostID, scope.AppID, scope.ThreadID).Scan(&status)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return view, err
			}
			view.Stopped = status == "interrupted"
		}
	}
	return view, nil
}

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
	if n.State == "failed" || n.State == "collecting" {
		var localBatch int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM attachment_batches WHERE receipt_id=? AND state IN ('sealed','failed') AND NOT EXISTS(SELECT 1 FROM submission_attempts WHERE receipt_id=?)`, receiptID, receiptID).Scan(&localBatch); err != nil {
			return err
		}
		if localBatch != 0 {
			return nil
		} // The mutable attachment card owns pre-submit failures/retries.
	}
	identity, err := submissionCardIdentityTx(ctx, tx, scope, n)
	if err != nil {
		return err
	}
	key := identity.key
	if strings.HasPrefix(key, "ui:interaction:") {
		return nil // The question renderer owns this card and its queue controls.
	}
	old, err := readMirror(ctx, tx, scope.HostID, scope.ThreadID, key)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	mirrorMissing := errors.Is(err, sql.ErrNoRows)
	view, err := submissionQueueViewTx(ctx, tx, scope, n, options)
	if err != nil {
		return err
	}
	if mirrorMissing && view.State != "queued" && view.State != "unknown" && view.State != "failed" && !view.LocalPending {
		return nil
	}
	card := cards.QueueInputView(view)
	if options.Offline && n.State == "queued" {
		card = cards.Progress("Codex 连接已断开 · 队列已保留", "重新连接后核验队列；原生操作暂不可用。")
	}
	terminal := n.State == "consumed" || n.State == "steered" || n.State == "removed" || n.State == "failed"
	if mirrorMissing {
		_, err = tx.ExecContext(ctx, `INSERT INTO mirrors(host_id,thread_id,mirror_key,turn_id,kind,card_json,terminal,parent_message_id) VALUES(?,?,?,?,'progress',?,?,?)`, scope.HostID, scope.ThreadID, key, key, card, terminal, identity.parent)
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO outbound(host_id,thread_id,mirror_key,state) VALUES(?,?,?,'ready')`, scope.HostID, scope.ThreadID, key)
		}
	} else {
		if !validSubmissionCard(old, identity) {
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
