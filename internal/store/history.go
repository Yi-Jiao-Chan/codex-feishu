package store

import (
	"context"
	"database/sql"
	"errors"
)

// InputLink is exact history evidence; it never means that an absent queue item
// was deleted by the bridge. One logical receipt must not map to two inputs.
type InputLink struct{ ReceiptID, TurnID, ItemID string }

func stageInputLink(ctx context.Context, tx *sql.Tx, host, thread string, link InputLink) error {
	if link.ReceiptID == "" || link.TurnID == "" || link.ItemID == "" {
		return errors.New("invalid input history link")
	}
	var authorized int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM m0_receipts r JOIN deployment d ON d.app_id=r.app_id WHERE d.host_id=? AND r.thread_id=? AND r.id=?`, host, thread, link.ReceiptID).Scan(&authorized)
	if err != nil {
		return err
	}
	if authorized != 1 {
		return ErrConflict
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO input_history_links(host_id,thread_id,turn_id,item_id,receipt_id) VALUES(?,?,?,?,?)`, host, thread, link.TurnID, link.ItemID, link.ReceiptID)
	if err != nil {
		return err
	}
	var got InputLink
	var gotHost, gotThread string
	err = tx.QueryRowContext(ctx, `SELECT host_id,thread_id,turn_id,item_id,receipt_id FROM input_history_links WHERE receipt_id=?`, link.ReceiptID).Scan(&gotHost, &gotThread, &got.TurnID, &got.ItemID, &got.ReceiptID)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (got != link || gotHost != host || gotThread != thread) {
		return ErrConflict
	}
	return err
}

func (s *Store) InputLinks(ctx context.Context, host, thread string) ([]InputLink, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT receipt_id,turn_id,item_id FROM input_history_links WHERE host_id=? AND thread_id=? ORDER BY turn_id,item_id`, host, thread)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []InputLink
	for rows.Next() {
		var link InputLink
		if err := rows.Scan(&link.ReceiptID, &link.TurnID, &link.ItemID); err != nil {
			return nil, err
		}
		result = append(result, link)
	}
	return result, rows.Err()
}

func (s *Store) ReceiptsForDeployment(ctx context.Context, host, thread string) ([]Receipt, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT r.id,r.app_id,r.message_id,r.thread_id,r.chat_id,r.body,r.client_id,r.queue_id,r.state,r.card_id,r.action_token,r.reply_message_id FROM m0_receipts r JOIN deployment d ON d.app_id=r.app_id WHERE d.host_id=? AND r.thread_id=?`, host, thread)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Receipt
	for rows.Next() {
		var r Receipt
		if err := rows.Scan(&r.ID, &r.AppID, &r.MessageID, &r.ThreadID, &r.ChatID, &r.Body, &r.ClientID, &r.QueueID, &r.State, &r.CardID, &r.ActionToken, &r.ReplyMessageID); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// Mirrors returns the durable public delivery ledger, including frozen/retired
// segments. Settled Card bodies may be empty; their hash, projection version,
// route and remote identity still prevent reposts and allow later corrections.
func (s *Store) Mirrors(ctx context.Context, host, thread string) ([]Mirror, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+mirrorColumns+` FROM mirrors m JOIN outbound o USING(host_id,thread_id,mirror_key) WHERE m.host_id=? AND m.thread_id=? ORDER BY o.rowid`, host, thread)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Mirror
	for rows.Next() {
		m, err := scanMirror(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}
