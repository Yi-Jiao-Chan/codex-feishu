package store

import (
	"context"
	"database/sql"
	"errors"

	"codex-feishu/internal/cards"
)

func AttachmentCardKey(id string) string { return "ui:attachments:" + id }

func stageAttachmentCardTx(ctx context.Context, tx *sql.Tx, b AttachmentBatch) error {
	state := b.State
	if b.State == "sealed" {
		n, err := readSubmission(ctx, tx, b.Scope, b.ReceiptID)
		if err != nil {
			return err
		}
		if n.State != "collecting" {
			// Feedback.Stage/StageReceived now owns this same card's QueueView.
			// A periodic resource refresh must not restore online buttons after
			// the native generation has disconnected.
			return nil
		}
		state = n.State
	}
	parts, err := attachmentParts(ctx, tx, b.ID)
	if err != nil {
		return err
	}
	v := cards.AttachmentDraftView{State: state, Token: b.Token, Revision: b.Revision}
	seen := map[string]bool{}
	for _, p := range parts {
		if p.Kind == "text" || p.State == "removed" {
			continue
		}
		v.Items = append(v.Items, cards.AttachmentItem{ID: p.ID, Name: p.Name, State: p.State, Error: p.ErrorCode})
		if !seen[p.MessageID] {
			v.Sources = append(v.Sources, p.MessageID)
			seen[p.MessageID] = true
		}
	}
	return stageAttachmentMirrorTx(ctx, tx, b, cards.AttachmentDraft(v))
}

func stageAttachmentMirrorTx(ctx context.Context, tx *sql.Tx, b AttachmentBatch, card string) error {
	key := AttachmentCardKey(b.ID)
	old, err := readMirror(ctx, tx, b.Scope.HostID, b.Scope.ThreadID, key)
	if errors.Is(err, sql.ErrNoRows) {
		if err := mirrorWriteOne(ctx, tx, `INSERT INTO mirrors(host_id,thread_id,mirror_key,turn_id,kind,card_json,parent_message_id) VALUES(?,?,?,?,'progress',?,?)`, b.Scope.HostID, b.Scope.ThreadID, key, key, card, b.FirstMessageID); err != nil {
			return err
		}
		return mirrorWriteOne(ctx, tx, `INSERT INTO outbound(host_id,thread_id,mirror_key,state) VALUES(?,?,?,'ready')`, b.Scope.HostID, b.Scope.ThreadID, key)
	}
	if err != nil {
		return err
	}
	if old.Card == card {
		return nil
	}
	if err := mirrorWriteOne(ctx, tx, `UPDATE mirrors SET card_json=?,revision=revision+1 WHERE host_id=? AND thread_id=? AND mirror_key=?`, card, b.Scope.HostID, b.Scope.ThreadID, key); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE outbound SET state='ready' WHERE host_id=? AND thread_id=? AND mirror_key=? AND state='confirmed'`, b.Scope.HostID, b.Scope.ThreadID, key)
	return err
}

// AttachmentSourceSubmission lets reactions on every original attachment
// follow the sealed receipt, without creating another native submission.
func (s *Store) AttachmentSourceSubmission(ctx context.Context, scope SubmissionScope, receipt string) (Submission, bool, error) {
	var target string
	err := s.DB.QueryRowContext(ctx, `SELECT b.receipt_id FROM attachment_events e JOIN attachment_batches b ON b.batch_id=e.batch_id WHERE e.receipt_id=? AND b.host_id=? AND b.app_id=? AND b.thread_id=? AND b.state<>'cleared'`, receipt, scope.HostID, scope.AppID, scope.ThreadID).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) || err == nil && target == "" {
		return Submission{}, false, nil
	}
	if err != nil {
		return Submission{}, false, err
	}
	n, err := s.Submission(ctx, scope, target)
	return n, err == nil, err
}
