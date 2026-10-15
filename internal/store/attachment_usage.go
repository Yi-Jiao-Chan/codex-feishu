package store

import (
	"context"
	"database/sql"
)

// AttachmentUsage contains counts only; task materials and resource identities
// must not be included in management output.
type AttachmentUsage struct {
	BatchStates      map[string]int64 `json:"batch_states"`
	ResourceStates   map[string]int64 `json:"resource_states"`
	SubmissionStates map[string]int64 `json:"submission_states"`
	RecordedBytes    int64            `json:"recorded_bytes"`
}

// AttachmentCacheReferences includes prospective paths before the ready DB
// commit, as well as sealed/queued/unknown inputs and failed downloads. Invalid
// scope or binding evidence fails closed instead of making their files orphans.
func (s *Store) AttachmentCacheReferences(ctx context.Context, host, app string) ([]AttachmentPart, error) {
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	parts, err := attachmentCacheReferencesTx(ctx, tx, host, app)
	if err != nil {
		return nil, err
	}
	return parts, tx.Commit()
}

func attachmentCacheReferencesTx(ctx context.Context, tx *sql.Tx, host, app string) ([]AttachmentPart, error) {
	if err := directoryDeployment(ctx, tx, host, app); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT p.part_id,p.kind,p.state,p.size,p.file_id,b.host_id,b.app_id,b.thread_id,b.chat_id
 FROM attachment_parts p JOIN attachment_batches b ON b.batch_id=p.batch_id
 WHERE b.state<>'cleared' AND p.state<>'removed' ORDER BY b.batch_id,p.ordinal`)
	if err != nil {
		return nil, err
	}
	parts := []AttachmentPart{}
	chats := map[SubmissionScope]string{}
	for rows.Next() {
		var p AttachmentPart
		var chat string
		if err := rows.Scan(&p.ID, &p.Kind, &p.State, &p.Size, &p.FileID, &p.Scope.HostID, &p.Scope.AppID, &p.Scope.ThreadID, &chat); err != nil {
			rows.Close()
			return nil, err
		}
		if p.Scope.HostID != host || p.Scope.AppID != app {
			rows.Close()
			return nil, ErrConflict
		}
		if previous, ok := chats[p.Scope]; ok && previous != chat {
			rows.Close()
			return nil, ErrConflict
		}
		chats[p.Scope] = chat
		parts = append(parts, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for scope, chat := range chats {
		actual, err := submissionBinding(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		if chat != actual {
			return nil, ErrConflict
		}
	}
	return parts, nil
}

func (s *Store) AttachmentUsage(ctx context.Context, host, app string) (AttachmentUsage, error) {
	usage := AttachmentUsage{BatchStates: map[string]int64{}, ResourceStates: map[string]int64{}, SubmissionStates: map[string]int64{}}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return usage, err
	}
	defer tx.Rollback()
	if _, err := attachmentCacheReferencesTx(ctx, tx, host, app); err != nil {
		return usage, err
	}
	queries := []struct {
		query  string
		target map[string]int64
	}{
		{`SELECT state,COUNT(*) FROM attachment_batches WHERE host_id=? AND app_id=? GROUP BY state`, usage.BatchStates},
		{`SELECT p.state,COUNT(*) FROM attachment_parts p JOIN attachment_batches b ON b.batch_id=p.batch_id WHERE b.host_id=? AND b.app_id=? AND p.kind<>'text' GROUP BY p.state`, usage.ResourceStates},
		{`SELECT n.state,COUNT(*) FROM attachment_batches b JOIN submission_inputs n ON n.receipt_id=b.receipt_id AND n.host_id=b.host_id AND n.app_id=b.app_id AND n.thread_id=b.thread_id WHERE b.host_id=? AND b.app_id=? GROUP BY n.state`, usage.SubmissionStates},
	}
	for _, q := range queries {
		rows, err := tx.QueryContext(ctx, q.query, host, app)
		if err != nil {
			return usage, err
		}
		for rows.Next() {
			var state string
			var count int64
			if err := rows.Scan(&state, &count); err != nil {
				rows.Close()
				return usage, err
			}
			q.target[state] = count
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return usage, err
		}
	}
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(p.size),0) FROM attachment_parts p JOIN attachment_batches b ON b.batch_id=p.batch_id WHERE b.host_id=? AND b.app_id=? AND b.state<>'cleared' AND p.state<>'removed' AND p.kind<>'text'`, host, app).Scan(&usage.RecordedBytes)
	if err != nil {
		return usage, err
	}
	return usage, tx.Commit()
}
