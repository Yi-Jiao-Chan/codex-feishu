package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"codex-feishu/internal/cards"
)

// Resources contain metadata only. Binary data lives in the private asset cache.
const AttachmentsSchema = `
CREATE TABLE IF NOT EXISTS submission_order (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,
 receipt_id TEXT NOT NULL UNIQUE REFERENCES m0_receipts(id)
);
INSERT OR IGNORE INTO submission_order(receipt_id) SELECT id FROM m0_receipts ORDER BY rowid;
CREATE TABLE IF NOT EXISTS attachment_batches (
 batch_id TEXT PRIMARY KEY, host_id TEXT NOT NULL, app_id TEXT NOT NULL, thread_id TEXT NOT NULL,
 chat_id TEXT NOT NULL, sender_id TEXT NOT NULL, token TEXT NOT NULL UNIQUE,
 revision INTEGER NOT NULL DEFAULT 1, state TEXT NOT NULL CHECK(state IN ('open','sealed','failed','cleared')),
 first_message_id TEXT NOT NULL, receipt_id TEXT NOT NULL DEFAULT '', created_at_ms INTEGER NOT NULL,
 card_sealed_at_ns INTEGER NOT NULL DEFAULT 0,
 FOREIGN KEY(host_id,thread_id) REFERENCES bindings(host_id,thread_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS attachment_open_sender ON attachment_batches(host_id,app_id,thread_id,chat_id,sender_id) WHERE state='open';
CREATE TABLE IF NOT EXISTS attachment_events (
 app_id TEXT NOT NULL, message_id TEXT NOT NULL, batch_id TEXT NOT NULL REFERENCES attachment_batches(batch_id),
 receipt_id TEXT NOT NULL UNIQUE REFERENCES m0_receipts(id), PRIMARY KEY(app_id,message_id)
);
CREATE TABLE IF NOT EXISTS attachment_parts (
 part_id TEXT PRIMARY KEY, batch_id TEXT NOT NULL REFERENCES attachment_batches(batch_id), ordinal INTEGER NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('text','image','file')), text TEXT NOT NULL DEFAULT '',
 message_id TEXT NOT NULL, resource_key TEXT NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL CHECK(state IN ('pending','downloading','ready','failed','removed')),
 attempts INTEGER NOT NULL DEFAULT 0, next_at_ms INTEGER NOT NULL DEFAULT 0,
 size INTEGER NOT NULL DEFAULT 0, digest TEXT NOT NULL DEFAULT '', file_id TEXT NOT NULL DEFAULT '',
 content_type TEXT NOT NULL DEFAULT '', error_code TEXT NOT NULL DEFAULT '', UNIQUE(batch_id,ordinal)
);
CREATE INDEX IF NOT EXISTS attachment_download_due ON attachment_parts(state,next_at_ms);
`

var ErrAttachmentLimit = errors.New("attachment batch exceeds configured resource count")

type AttachmentPart struct {
	ID, BatchID, Kind, Text, MessageID, ResourceKey, Name string
	State, Digest, FileID, ContentType, ErrorCode         string
	Ordinal, Attempts                                     int
	Size, NextAtMS                                        int64
	Scope                                                 SubmissionScope
}

type AttachmentBatch struct {
	ID, ChatID, SenderID, Token, State, FirstMessageID, ReceiptID string
	Scope                                                         SubmissionScope
	Revision                                                      int64
	Parts                                                         []AttachmentPart
}

type AttachmentInput struct {
	Scope                             SubmissionScope
	ChatID, SenderID, MessageID, Body string
	// ExpectedBatch freezes the draft observed before processing an instruction.
	// A concurrent button that seals it makes this instruction stale.
	ExpectedBatch    string
	IngressStartedNS int64 // Local handler entry, for overlapping card/message arbitration.
	Parts            []AttachmentPart
}

const attachmentBatchColumns = `batch_id,host_id,app_id,thread_id,chat_id,sender_id,token,revision,state,first_message_id,receipt_id`
const attachmentPartColumns = `part_id,batch_id,ordinal,kind,text,message_id,resource_key,name,state,attempts,next_at_ms,size,digest,file_id,content_type,error_code`

func scanAttachmentBatch(row rowScanner) (b AttachmentBatch, err error) {
	err = row.Scan(&b.ID, &b.Scope.HostID, &b.Scope.AppID, &b.Scope.ThreadID, &b.ChatID, &b.SenderID, &b.Token, &b.Revision, &b.State, &b.FirstMessageID, &b.ReceiptID)
	return
}
func scanAttachmentPart(row rowScanner) (p AttachmentPart, err error) {
	err = row.Scan(&p.ID, &p.BatchID, &p.Ordinal, &p.Kind, &p.Text, &p.MessageID, &p.ResourceKey, &p.Name, &p.State, &p.Attempts, &p.NextAtMS, &p.Size, &p.Digest, &p.FileID, &p.ContentType, &p.ErrorCode)
	return
}

func attachmentParts(ctx context.Context, tx *sql.Tx, id string) ([]AttachmentPart, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+attachmentPartColumns+` FROM attachment_parts WHERE batch_id=? ORDER BY ordinal`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var parts []AttachmentPart
	for rows.Next() {
		p, err := scanAttachmentPart(rows)
		if err != nil {
			return nil, err
		}
		parts = append(parts, p)
	}
	return parts, rows.Err()
}

func (s *Store) OpenAttachmentBatch(ctx context.Context, scope SubmissionScope, sender string) (AttachmentBatch, error) {
	chat, err := submissionBinding(ctx, s.DB, scope)
	if err != nil {
		return AttachmentBatch{}, err
	}
	return scanAttachmentBatch(s.DB.QueryRowContext(ctx, `SELECT `+attachmentBatchColumns+` FROM attachment_batches WHERE host_id=? AND app_id=? AND thread_id=? AND chat_id=? AND sender_id=? AND state='open'`, scope.HostID, scope.AppID, scope.ThreadID, chat, sender))
}

func claimInputTx(ctx context.Context, tx *sql.Tx, scope SubmissionScope, chat, message, body, state string) (Receipt, bool, error) {
	if state != "draft" {
		if err := adoptSubmissionOrderTx(ctx, tx, scope, chat); err != nil {
			return Receipt{}, false, err
		}
	}
	r := Receipt{ID: ID(), AppID: scope.AppID, ThreadID: scope.ThreadID, ChatID: chat, MessageID: message, Body: body, State: state, ActionToken: ID()}
	r.ClientID = "cfb-" + r.ID
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO m0_receipts(id,app_id,message_id,thread_id,chat_id,body,client_id,state,action_token) VALUES(?,?,?,?,?,?,?,?,?)`, r.ID, r.AppID, message, r.ThreadID, chat, body, r.ClientID, state, r.ActionToken)
	if err != nil {
		return r, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return r, false, err
	}
	if n != 1 {
		return r, false, ErrConflict
	}
	err = mirrorWriteOne(ctx, tx, `INSERT INTO submission_inputs(receipt_id,host_id,app_id,thread_id,state) VALUES(?,?,?,?,?)`, r.ID, scope.HostID, scope.AppID, scope.ThreadID, state)
	if err == nil && state != "draft" {
		err = mirrorWriteOne(ctx, tx, `INSERT INTO submission_order(receipt_id) VALUES(?)`, r.ID)
	}
	return r, true, err
}

// ReceiveAttachmentInput commits event deduplication, append/seal and the input
// receipt together. It performs no network or filesystem work.
func (s *Store) ReceiveAttachmentInput(ctx context.Context, in AttachmentInput, maxResources, maxImages int) (AttachmentBatch, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return AttachmentBatch{}, false, err
	}
	defer tx.Rollback()
	chat, err := submissionBinding(ctx, tx, in.Scope)
	if err != nil || chat != in.ChatID || in.SenderID == "" || in.MessageID == "" {
		return AttachmentBatch{}, false, ErrConflict
	}
	var seen int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM m0_receipts WHERE app_id=? AND message_id=?)+(SELECT COUNT(*) FROM command_replies WHERE app_id=? AND message_id=?)`, in.Scope.AppID, in.MessageID, in.Scope.AppID, in.MessageID).Scan(&seen); err != nil {
		return AttachmentBatch{}, false, err
	}
	if seen != 0 {
		return AttachmentBatch{}, false, nil
	}
	reject := func(b AttachmentBatch, reason error) (AttachmentBatch, bool, error) {
		body := "本批附件已发送；这条与按钮同时到达的说明尚未提交，请确认后重新发送。"
		if errors.Is(reason, ErrAttachmentLimit) {
			body = fmt.Sprintf("本条附件未加入草稿，也未提交 Codex：每批最多 %d 个附件、其中最多 %d 张图片。请先发送或清空原草稿，再重发本条。", maxResources, maxImages)
		}
		// Terminal rejection and its dedup marker share the admission transaction.
		// A concurrent replay cannot be accepted before failure feedback commits.
		if err := commandWriteOne(ctx, tx, `INSERT INTO command_replies(app_id,message_id,host_id,chat_id,chat_type,user_id,thread_id,kind,attempt_id,card_json,state,created_at_ms) VALUES(?,?,?,?,'group',?,?,'feedback',?,?,'ready',?)`, in.Scope.AppID, in.MessageID, in.Scope.HostID, chat, in.SenderID, in.Scope.ThreadID, ID(), cards.Message("附件未提交", "本部署", body), time.Now().UnixMilli()); err != nil {
			return b, false, err
		}
		if err := tx.Commit(); err != nil {
			return b, false, err
		}
		return b, false, reason
	}
	if strings.TrimSpace(in.Body) != "" && in.IngressStartedNS > 0 {
		var overlapping int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM attachment_batches WHERE host_id=? AND app_id=? AND thread_id=? AND chat_id=? AND sender_id=? AND card_sealed_at_ns>=?`, in.Scope.HostID, in.Scope.AppID, in.Scope.ThreadID, chat, in.SenderID, in.IngressStartedNS).Scan(&overlapping); err != nil {
			return AttachmentBatch{}, false, err
		}
		if overlapping != 0 {
			return reject(AttachmentBatch{}, ErrSubmissionStale)
		}
	}
	b, err := scanAttachmentBatch(tx.QueryRowContext(ctx, `SELECT `+attachmentBatchColumns+` FROM attachment_batches WHERE host_id=? AND app_id=? AND thread_id=? AND chat_id=? AND sender_id=? AND state='open'`, in.Scope.HostID, in.Scope.AppID, in.Scope.ThreadID, chat, in.SenderID))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return b, false, err
	}
	if in.ExpectedBatch != "" && in.ExpectedBatch != b.ID {
		return reject(b, ErrSubmissionStale)
	}
	resources := 0
	for _, p := range in.Parts {
		if p.Kind != "text" {
			resources++
		}
		if p.Kind != "text" && p.Kind != "image" && p.Kind != "file" || p.Kind != "text" && p.ResourceKey == "" {
			return b, false, ErrConflict
		}
	}
	if b.ID == "" && resources == 0 {
		r, fresh, err := claimInputTx(ctx, tx, in.Scope, chat, in.MessageID, in.Body, "received")
		if err != nil {
			return b, false, err
		}
		if err = stageWorktreeRestoreGrantTx(ctx, tx, in.Scope, r.ID); err != nil {
			return b, false, err
		}
		if err = stageInputContinueTx(ctx, tx, in.Scope, r); err != nil {
			return b, false, err
		}
		return b, fresh, tx.Commit()
	}
	parts, err := attachmentParts(ctx, tx, b.ID)
	if err != nil {
		return b, false, err
	}
	count, images := 0, 0
	for _, p := range append(append([]AttachmentPart{}, parts...), in.Parts...) {
		if p.State == "removed" {
			continue
		}
		if p.Kind != "text" {
			count++
		}
		if p.Kind == "image" {
			images++
		}
	}
	if count > maxResources || images > maxImages {
		return reject(b, ErrAttachmentLimit)
	}
	if b.ID == "" {
		b = AttachmentBatch{ID: ID(), Scope: in.Scope, ChatID: chat, SenderID: in.SenderID, Token: ID(), State: "open", Revision: 1, FirstMessageID: in.MessageID}
		if err := mirrorWriteOne(ctx, tx, `INSERT INTO attachment_batches(batch_id,host_id,app_id,thread_id,chat_id,sender_id,token,state,first_message_id,created_at_ms) VALUES(?,?,?,?,?,?,?,'open',?,?)`, b.ID, in.Scope.HostID, in.Scope.AppID, in.Scope.ThreadID, chat, in.SenderID, b.Token, in.MessageID, time.Now().UnixMilli()); err != nil {
			return b, false, err
		}
	}
	r, fresh, err := claimInputTx(ctx, tx, in.Scope, chat, in.MessageID, in.Body, "draft")
	if err != nil || !fresh {
		return b, false, err
	}
	if err = stageWorktreeRestoreGrantTx(ctx, tx, in.Scope, r.ID); err != nil {
		return b, false, err
	}
	if err = mirrorWriteOne(ctx, tx, `INSERT INTO attachment_events(app_id,message_id,batch_id,receipt_id) VALUES(?,?,?,?)`, in.Scope.AppID, in.MessageID, b.ID, r.ID); err != nil {
		return b, false, err
	}
	for i, p := range in.Parts {
		state := "pending"
		if p.Kind == "text" {
			state = "ready"
		}
		if err = mirrorWriteOne(ctx, tx, `INSERT INTO attachment_parts(part_id,batch_id,ordinal,kind,text,message_id,resource_key,name,state) VALUES(?,?,?,?,?,?,?,?,?)`, ID(), b.ID, len(parts)+i, p.Kind, p.Text, in.MessageID, p.ResourceKey, p.Name, state); err != nil {
			return b, false, err
		}
	}
	if err = mirrorWriteOne(ctx, tx, `UPDATE attachment_batches SET revision=revision+1 WHERE batch_id=?`, b.ID); err != nil {
		return b, false, err
	}
	b.Revision++
	if strings.TrimSpace(in.Body) != "" {
		if err = sealAttachmentTx(ctx, tx, &b, r.ID, in.Body); err != nil {
			return b, false, err
		}
		if err = stageInputContinueTx(ctx, tx, in.Scope, r); err != nil {
			return b, false, err
		}
	}
	if err := stageAttachmentCardTx(ctx, tx, b); err != nil {
		return b, false, err
	}
	return b, true, tx.Commit()
}

func sealAttachmentTx(ctx context.Context, tx *sql.Tx, b *AttachmentBatch, receiptID, body string) error {
	if b.State != "open" {
		return ErrSubmissionStale
	}
	if err := adoptSubmissionOrderTx(ctx, tx, b.Scope, b.ChatID); err != nil {
		return err
	}
	if body == "" {
		body = "请查看附件。"
	}
	if err := mirrorWriteOne(ctx, tx, `UPDATE m0_receipts SET state='collecting',body=? WHERE id=? AND state='draft'`, body, receiptID); err != nil {
		return err
	}
	if err := mirrorWriteOne(ctx, tx, `UPDATE submission_inputs SET state='collecting',revision=revision+1 WHERE receipt_id=? AND state='draft'`, receiptID); err != nil {
		return err
	}
	if err := mirrorWriteOne(ctx, tx, `INSERT INTO submission_order(receipt_id) VALUES(?)`, receiptID); err != nil {
		return err
	}
	b.State, b.ReceiptID = "sealed", receiptID
	b.Revision++
	return mirrorWriteOne(ctx, tx, `UPDATE attachment_batches SET state='sealed',receipt_id=?,revision=? WHERE batch_id=? AND state='open'`, receiptID, b.Revision, b.ID)
}

// AttachmentAction checks the actual delivered card, revision, sender and full
// binding. It can only modify local drafts or batches with no native attempt.
func (s *Store) AttachmentAction(ctx context.Context, scope SubmissionScope, sender, chat, card, token string, revision int64, action, partID, body string) (AttachmentBatch, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return AttachmentBatch{}, err
	}
	defer tx.Rollback()
	boundChat, err := submissionBinding(ctx, tx, scope)
	if err != nil || chat != boundChat {
		return AttachmentBatch{}, ErrConflict
	}
	b, err := scanAttachmentBatch(tx.QueryRowContext(ctx, `SELECT `+attachmentBatchColumns+` FROM attachment_batches WHERE token=? AND host_id=? AND app_id=? AND thread_id=? AND chat_id=? AND sender_id=?`, token, scope.HostID, scope.AppID, scope.ThreadID, chat, sender))
	if err != nil {
		return b, ErrSubmissionStale
	}
	m, err := readMirror(ctx, tx, scope.HostID, scope.ThreadID, AttachmentCardKey(b.ID))
	if err != nil || m.RemoteID != card || card == "" || b.Revision != revision {
		return b, ErrSubmissionStale
	}
	if b.State != "open" && b.State != "failed" {
		return b, ErrSubmissionStale
	}
	if b.State == "failed" {
		var attempted int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM submission_attempts WHERE receipt_id=?`, b.ReceiptID).Scan(&attempted); err != nil {
			return b, err
		}
		if attempted != 0 {
			return b, ErrSubmissionBusy
		}
	}
	switch action {
	case "attachment_send":
		if b.State != "open" || len(body) > 16000 {
			return b, ErrSubmissionStale
		}
		var receipt string
		if err := tx.QueryRowContext(ctx, `SELECT receipt_id FROM attachment_events WHERE app_id=? AND message_id=? AND batch_id=?`, scope.AppID, b.FirstMessageID, b.ID).Scan(&receipt); err != nil {
			return b, err
		}
		if strings.TrimSpace(body) != "" {
			var next int
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(ordinal)+1,0) FROM attachment_parts WHERE batch_id=?`, b.ID).Scan(&next); err != nil {
				return b, err
			}
			if err := mirrorWriteOne(ctx, tx, `INSERT INTO attachment_parts(part_id,batch_id,ordinal,kind,text,message_id,state) VALUES(?,?,?,'text',?,?,'ready')`, ID(), b.ID, next, body, b.FirstMessageID); err != nil {
				return b, err
			}
		}
		if err := sealAttachmentTx(ctx, tx, &b, receipt, body); err != nil {
			return b, err
		}
		if err := mirrorWriteOne(ctx, tx, `UPDATE attachment_batches SET card_sealed_at_ns=? WHERE batch_id=?`, time.Now().UnixNano(), b.ID); err != nil {
			return b, err
		}
	case "attachment_clear":
		b.State = "cleared"
		if _, err := tx.ExecContext(ctx, `UPDATE attachment_parts SET state='removed' WHERE batch_id=?`, b.ID); err != nil {
			return b, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE m0_receipts SET state='removed' WHERE id IN (SELECT receipt_id FROM attachment_events WHERE batch_id=?)`, b.ID); err != nil {
			return b, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE submission_inputs SET state='removed',revision=revision+1 WHERE receipt_id IN (SELECT receipt_id FROM attachment_events WHERE batch_id=?)`, b.ID); err != nil {
			return b, err
		}
	case "attachment_retry", "attachment_remove":
		if action == "attachment_remove" {
			res, err := tx.ExecContext(ctx, `UPDATE attachment_parts SET state='removed' WHERE batch_id=? AND part_id=? AND kind<>'text' AND state='failed'`, b.ID, partID)
			if err != nil {
				return b, err
			}
			if count, _ := res.RowsAffected(); count != 1 {
				return b, ErrSubmissionStale
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE attachment_parts SET state='pending',attempts=0,next_at_ms=0,error_code='' WHERE batch_id=? AND state='failed'`, b.ID); err != nil {
			return b, err
		}
		if b.State == "failed" {
			b.State = "sealed"
			if err := mirrorWriteOne(ctx, tx, `UPDATE m0_receipts SET state='collecting' WHERE id=? AND state='failed' AND input_json=''`, b.ReceiptID); err != nil {
				return b, err
			}
			if err := mirrorWriteOne(ctx, tx, `UPDATE submission_inputs SET state='collecting',revision=revision+1 WHERE receipt_id=? AND state='failed'`, b.ReceiptID); err != nil {
				return b, err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM submission_order WHERE receipt_id=?`, b.ReceiptID); err != nil {
				return b, err
			}
			if err := mirrorWriteOne(ctx, tx, `INSERT INTO submission_order(receipt_id) VALUES(?)`, b.ReceiptID); err != nil {
				return b, err
			}
		}
	default:
		return b, ErrConflict
	}
	b.Revision++
	if err := mirrorWriteOne(ctx, tx, `UPDATE attachment_batches SET state=?,revision=? WHERE batch_id=?`, b.State, b.Revision, b.ID); err != nil {
		return b, err
	}
	if err := stageAttachmentCardTx(ctx, tx, b); err != nil {
		return b, err
	}
	return b, tx.Commit()
}

func (s *Store) RecoverAttachmentDownloads(ctx context.Context, host, app string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE attachment_parts SET state='pending',next_at_ms=0 WHERE state='downloading' AND batch_id IN (SELECT batch_id FROM attachment_batches WHERE host_id=? AND app_id=? AND state IN ('open','sealed','failed'))`, host, app)
	return err
}

func (s *Store) ClaimAttachmentDownload(ctx context.Context, host, app string, nowMS int64) (AttachmentPart, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return AttachmentPart{}, false, err
	}
	defer tx.Rollback()
	p, err := scanAttachmentPart(tx.QueryRowContext(ctx, `SELECT `+attachmentPartColumns+` FROM attachment_parts WHERE state='pending' AND next_at_ms<=? AND batch_id IN (SELECT batch_id FROM attachment_batches WHERE host_id=? AND app_id=? AND state IN ('open','sealed'))
 AND NOT EXISTS(SELECT 1 FROM attachment_parts active JOIN attachment_batches ab ON ab.batch_id=active.batch_id
 WHERE active.state='downloading' AND ab.state IN ('open','sealed') AND ab.host_id=? AND ab.app_id=? AND ab.thread_id=(SELECT thread_id FROM attachment_batches WHERE batch_id=attachment_parts.batch_id))
 ORDER BY next_at_ms,rowid LIMIT 1`, nowMS, host, app, host, app))
	if errors.Is(err, sql.ErrNoRows) {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	b, err := scanAttachmentBatch(tx.QueryRowContext(ctx, `SELECT `+attachmentBatchColumns+` FROM attachment_batches WHERE batch_id=?`, p.BatchID))
	if err != nil {
		return p, false, err
	}
	if chat, err := submissionBinding(ctx, tx, b.Scope); err != nil || chat != b.ChatID {
		return p, false, ErrConflict
	}
	p.Scope, p.State, p.Attempts, p.NextAtMS = b.Scope, "downloading", p.Attempts+1, nowMS+90_000
	if err := mirrorWriteOne(ctx, tx, `UPDATE attachment_parts SET state='downloading',attempts=?,next_at_ms=? WHERE part_id=? AND state='pending'`, p.Attempts, p.NextAtMS, p.ID); err != nil {
		return p, false, err
	}
	return p, true, tx.Commit()
}

// FinishAttachmentDownload applies only to the claimed generation of a GET.
// Retries are restricted to the GET; this method never retries a native write.
func (s *Store) FinishAttachmentDownload(ctx context.Context, p AttachmentPart, code string, retry bool, nowMS int64) error {
	state, next := "ready", int64(0)
	if code != "" {
		state = "failed"
		if retry && p.Attempts < 3 {
			state, next = "pending", nowMS+int64(p.Attempts)*2000
		}
	}
	res, err := s.DB.ExecContext(ctx, `UPDATE attachment_parts SET state=?,next_at_ms=?,size=?,digest=?,file_id=?,content_type=?,error_code=? WHERE part_id=? AND batch_id=? AND state='downloading' AND attempts=?`, state, next, p.Size, p.Digest, p.FileID, p.ContentType, code, p.ID, p.BatchID, p.Attempts)
	if err != nil {
		return err
	}
	if count, err := res.RowsAffected(); err != nil || count != 1 {
		return ErrSubmissionStale
	}
	return nil
}

// RefreshAttachmentBatches freezes complete native inputs only after every
// non-removed resource passed validation. resolve returns a trusted cache path.
func (s *Store) RefreshAttachmentBatches(ctx context.Context, host, app string, maxBytes, maxImageBytes int64, resolve func(AttachmentPart) (string, error)) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT batch_id FROM attachment_batches WHERE host_id=? AND app_id=? AND state IN ('open','sealed','failed') ORDER BY rowid`, host, app)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var wake []string
	for _, id := range ids {
		thread, err := s.refreshAttachmentBatch(ctx, id, maxBytes, maxImageBytes, resolve)
		if err != nil {
			return wake, err
		}
		wake = append(wake, thread)
	}
	return wake, nil
}

func (s *Store) refreshAttachmentBatch(ctx context.Context, id string, maxBytes, maxImageBytes int64, resolve func(AttachmentPart) (string, error)) (string, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	b, err := scanAttachmentBatch(tx.QueryRowContext(ctx, `SELECT `+attachmentBatchColumns+` FROM attachment_batches WHERE batch_id=?`, id))
	if err != nil {
		return "", err
	}
	if _, err := submissionBinding(ctx, tx, b.Scope); err != nil {
		return "", err
	}
	parts, err := attachmentParts(ctx, tx, id)
	if err != nil {
		return "", err
	}
	ready, failed := true, false
	var total, images int64
	for _, p := range parts {
		if p.State == "removed" {
			continue
		}
		ready = ready && p.State == "ready"
		failed = failed || p.State == "failed"
		total += p.Size
		if p.Kind == "image" {
			images += p.Size
		}
	}
	if total > maxBytes || images > maxImageBytes {
		failed, ready = true, false
		if _, err := tx.ExecContext(ctx, `UPDATE attachment_parts SET state='failed',error_code='batch-size-limit' WHERE batch_id=? AND kind<>'text' AND state='ready'`, id); err != nil {
			return "", err
		}
	}
	changed := false
	if b.State == "sealed" {
		n, err := readSubmission(ctx, tx, b.Scope, b.ReceiptID)
		if err != nil {
			return "", err
		}
		if n.State == "collecting" && failed {
			changed = true
			b.State = "failed"
			if err := finishAttachmentReceiptTx(ctx, tx, b.Scope, n.ID, "failed", ""); err != nil {
				return "", err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE attachment_batches SET state='failed',revision=revision+1 WHERE batch_id=?`, id); err != nil {
				return "", err
			}
			b.Revision++
		} else if n.State == "collecting" && ready {
			changed = true
			var input []map[string]any
			hasText := false
			for _, p := range parts {
				if p.State == "removed" {
					continue
				}
				if p.Kind == "text" {
					if p.Text != "" {
						hasText = hasText || strings.TrimSpace(p.Text) != ""
						input = append(input, map[string]any{"type": "text", "text": p.Text, "text_elements": []any{}})
					}
					continue
				}
				p.Scope = b.Scope
				path, err := resolve(p)
				if err != nil {
					if err := mirrorWriteOne(ctx, tx, `UPDATE attachment_parts SET state='failed',error_code='integrity' WHERE part_id=? AND state='ready'`, p.ID); err != nil {
						return "", err
					}
					b.State = "failed"
					n.State = "failed"
					if err := finishAttachmentReceiptTx(ctx, tx, b.Scope, n.ID, "failed", ""); err != nil {
						return "", err
					}
					if err := mirrorWriteOne(ctx, tx, `UPDATE attachment_batches SET state='failed',revision=revision+1 WHERE batch_id=?`, b.ID); err != nil {
						return "", err
					}
					b.Revision++
					if err := stageAttachmentCardTx(ctx, tx, b); err != nil {
						return "", err
					}
					return b.Scope.ThreadID, tx.Commit()
				}
				if p.Kind == "image" {
					input = append(input, map[string]any{"type": "localImage", "path": path})
				} else {
					name, _ := json.Marshal(p.Name)
					quotedPath, _ := json.Marshal(path)
					input = append(input, map[string]any{"type": "text", "text": string(name) + ": " + string(quotedPath), "text_elements": []any{}})
				}
			}
			if strings.TrimSpace(n.Body) != "" && !hasText {
				input = append(input, map[string]any{"type": "text", "text": n.Body, "text_elements": []any{}})
			}
			raw, err := json.Marshal(input)
			if err != nil {
				return "", err
			}
			// The daemon's queued representation embeds image bytes. Reserve
			// headroom above the 20 MiB image budget inside its 32 MiB frame.
			if len(raw) > 1<<20 {
				if _, err := tx.ExecContext(ctx, `UPDATE attachment_parts SET state='failed',error_code='input-size-limit' WHERE batch_id=? AND kind<>'text' AND state<>'removed'`, b.ID); err != nil {
					return "", err
				}
				n.State, b.State = "failed", "failed"
				if err := finishAttachmentReceiptTx(ctx, tx, b.Scope, n.ID, "failed", ""); err != nil {
					return "", err
				}
				if err := mirrorWriteOne(ctx, tx, `UPDATE attachment_batches SET state='failed',revision=revision+1 WHERE batch_id=?`, b.ID); err != nil {
					return "", err
				}
				b.Revision++
				if err := stageAttachmentCardTx(ctx, tx, b); err != nil {
					return "", err
				}
				return b.Scope.ThreadID, tx.Commit()
			}
			if err := finishAttachmentReceiptTx(ctx, tx, b.Scope, n.ID, "received", string(raw)); err != nil {
				return "", err
			}
		}
	}
	if err := stageAttachmentCardTx(ctx, tx, b); err != nil {
		return "", err
	}
	if changed {
		return b.Scope.ThreadID, tx.Commit()
	}
	return "", tx.Commit()
}

func finishAttachmentReceiptTx(ctx context.Context, tx *sql.Tx, scope SubmissionScope, receipt, state, inputJSON string) error {
	if err := mirrorWriteOne(ctx, tx, `UPDATE m0_receipts SET state=?,input_json=? WHERE id=? AND app_id=? AND thread_id=? AND state='collecting' AND input_json=''`, state, inputJSON, receipt, scope.AppID, scope.ThreadID); err != nil {
		return err
	}
	return mirrorWriteOne(ctx, tx, `UPDATE submission_inputs SET state=?,revision=revision+1 WHERE receipt_id=? AND host_id=? AND app_id=? AND thread_id=? AND state='collecting'`, state, receipt, scope.HostID, scope.AppID, scope.ThreadID)
}

// ReleaseAttachmentDownload returns a claim that never issued a GET, without
// spending the retry budget (for example, a revoked verification lease).
func (s *Store) ReleaseAttachmentDownload(ctx context.Context, p AttachmentPart, nextMS int64) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE attachment_parts SET state='pending',attempts=attempts-1,next_at_ms=? WHERE part_id=? AND batch_id=? AND state='downloading' AND attempts=?`, nextMS, p.ID, p.BatchID, p.Attempts)
	return err
}

func (s *Store) RecoverExpiredAttachmentDownloads(ctx context.Context, host, app string, nowMS int64) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE attachment_parts SET state=CASE WHEN attempts>=3 THEN 'failed' ELSE 'pending' END,next_at_ms=0,error_code='download-interrupted'
 WHERE state='downloading' AND next_at_ms<=? AND batch_id IN (SELECT batch_id FROM attachment_batches WHERE host_id=? AND app_id=? AND state IN ('open','sealed','failed'))`, nowMS, host, app)
	return err
}

func (s *Store) HasCollectingAttachments(ctx context.Context, host, app string) (bool, error) {
	var count int
	// A confirmed creation can still be waiting for its binding or an explicit
	// local download retry. Keep the worker available when intake is disabled.
	err := s.DB.QueryRowContext(ctx, `SELECT
 (SELECT COUNT(*) FROM attachment_batches b JOIN m0_receipts r ON r.id=b.receipt_id WHERE b.host_id=? AND b.app_id=? AND b.state='sealed' AND r.state='collecting')+
 (SELECT COUNT(*) FROM creation_drafts d WHERE d.host_id=? AND d.app_id=? AND d.state='creating' AND d.attachments_json<>'[]')`, host, app, host, app).Scan(&count)
	return count != 0, err
}
