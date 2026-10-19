package store

import (
	"context"
	"database/sql"
	"encoding/json"
)

// CreationAttachment is an immutable reference to the creating message's
// resources. Downloads begin only after the user confirms the creation draft.
type CreationAttachment struct {
	Kind        string `json:"kind"`
	ResourceKey string `json:"resource_key"`
	Name        string `json:"name,omitempty"`
}

func creationAttachmentsJSON(parts []CreationAttachment) (string, error) {
	if len(parts) == 0 {
		return "[]", nil
	}
	for _, part := range parts {
		if part.Kind != "image" && part.Kind != "file" || part.ResourceKey == "" || len(part.ResourceKey) > 512 {
			return "", ErrConflict
		}
	}
	raw, err := json.Marshal(parts)
	return string(raw), err
}

func materializeCreationAttachmentsTx(ctx context.Context, tx *sql.Tx, d CreationDraft, i CreationIntent, r Receipt) error {
	scope := SubmissionScope{HostID: i.HostID, AppID: i.AppID, ThreadID: i.NativeThreadID}
	if err := mirrorWriteOne(ctx, tx, `INSERT INTO submission_inputs(receipt_id,host_id,app_id,thread_id,state) VALUES(?,?,?,?,'collecting')`, r.ID, scope.HostID, scope.AppID, scope.ThreadID); err != nil {
		return err
	}
	if err := mirrorWriteOne(ctx, tx, `INSERT INTO submission_order(receipt_id) VALUES(?)`, r.ID); err != nil {
		return err
	}
	batch := ID()
	// Replies belong to the newly created target. Individual resource GETs
	// retain the original source message, even when it came from another chat.
	if err := mirrorWriteOne(ctx, tx, `INSERT INTO attachment_batches(batch_id,host_id,app_id,thread_id,chat_id,sender_id,token,state,first_message_id,receipt_id,created_at_ms) VALUES(?,?,?,?,?,?,?,'sealed',?,?,?)`, batch, scope.HostID, scope.AppID, scope.ThreadID, r.ChatID, d.CreatorID, ID(), i.RootMessageID, r.ID, i.CreatedAtMS); err != nil {
		return err
	}
	if err := mirrorWriteOne(ctx, tx, `INSERT INTO attachment_events(app_id,message_id,batch_id,receipt_id) VALUES(?,?,?,?)`, scope.AppID, d.SourceMessageID, batch, r.ID); err != nil {
		return err
	}
	if err := mirrorWriteOne(ctx, tx, `INSERT INTO attachment_parts(part_id,batch_id,ordinal,kind,text,message_id,state) VALUES(?,?,0,'text',?,?,'ready')`, ID(), batch, i.Body, d.SourceMessageID); err != nil {
		return err
	}
	for ordinal, part := range d.Attachments {
		if err := mirrorWriteOne(ctx, tx, `INSERT INTO attachment_parts(part_id,batch_id,ordinal,kind,message_id,resource_key,name,state) VALUES(?,?,?,?,?,?,?,'pending')`, ID(), batch, ordinal+1, part.Kind, d.SourceMessageID, part.ResourceKey, part.Name); err != nil {
			return err
		}
	}
	return nil
}

// Only a local download failure with no native attempt can re-enter collection.
// Already confirmed remote resources and the original receipt remain intact.
func retryCreationAttachmentsTx(ctx context.Context, tx *sql.Tx, receipt string) error {
	var batch string
	err := tx.QueryRowContext(ctx, `SELECT b.batch_id FROM attachment_batches b JOIN m0_receipts r ON r.id=b.receipt_id
 WHERE b.receipt_id=? AND b.state='failed' AND r.state='failed' AND r.input_json=''
 AND NOT EXISTS(SELECT 1 FROM submission_attempts WHERE receipt_id=?)`, receipt, receipt).Scan(&batch)
	if err == sql.ErrNoRows {
		return ErrSubmissionStale
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE attachment_parts SET state='pending',attempts=0,next_at_ms=0,error_code='' WHERE batch_id=? AND state='failed'`, batch); err != nil {
		return err
	}
	if err = mirrorWriteOne(ctx, tx, `UPDATE attachment_batches SET state='sealed',revision=revision+1 WHERE batch_id=? AND state='failed'`, batch); err != nil {
		return err
	}
	if err = mirrorWriteOne(ctx, tx, `UPDATE m0_receipts SET state='collecting' WHERE id=? AND state='failed' AND input_json=''`, receipt); err != nil {
		return err
	}
	return mirrorWriteOne(ctx, tx, `UPDATE submission_inputs SET state='collecting',revision=revision+1 WHERE receipt_id=? AND state='failed'`, receipt)
}
