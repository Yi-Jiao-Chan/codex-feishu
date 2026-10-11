package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"errors"
	"math"

	"codex-feishu/internal/cards"
)

const CardReadbackSchema = `
CREATE TABLE IF NOT EXISTS card_request_markers (
 marker TEXT PRIMARY KEY CHECK(length(marker)=20),
 kind TEXT NOT NULL CHECK(kind IN ('mirror','command','reminder')),
 attempt_id TEXT NOT NULL UNIQUE,
 next_check_at_ms INTEGER NOT NULL DEFAULT 0 CHECK(next_check_at_ms>=0)
);
INSERT INTO card_request_markers(marker,kind,attempt_id) SELECT marker,'mirror',attempt_id FROM outbound_request_markers;

`

func freezeCardRequestMarker(ctx context.Context, tx *sql.Tx, kind, id, base string) (string, string, error) {
	if kind != "mirror" && kind != "command" && kind != "reminder" || id == "" {
		return "", "", ErrConflict
	}
	hash := sha256.Sum256([]byte(id))
	marker := "c" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(hash[:])[:19]
	card, marked, err := cards.WithRequestMarker(base, marker)
	if err != nil {
		return "", "", err
	}
	if !marked {
		return base, "", nil
	}
	source := "outbound_attempts"
	if kind == "command" {
		source = "command_reply_attempts"
	}
	if kind == "reminder" {
		source = "request_reminder_attempts"
	}
	// The shared namespace outlives the immutable source journals. Reserve
	// only after the exact source exists in this same claim transaction.
	query := `INSERT INTO card_request_markers(marker,kind,attempt_id) SELECT ?,?,? WHERE EXISTS(SELECT 1 FROM ` + source + ` WHERE attempt_id=?)`
	if err := commandWriteOne(ctx, tx, query, marker, kind, id, id); err != nil {
		return "", "", err
	}
	return card, marker, nil
}

// CardReadback carries private frozen worker evidence, never status or logs.
type CardReadback struct {
	Kind, ID, HostID, AppID, ThreadID, ChatID, ParentMessageID, Card, Marker string `json:"-"`
}

func (s *Store) CardRequestMarker(ctx context.Context, id string) (string, error) {
	var marker string
	err := s.DB.QueryRowContext(ctx, `SELECT marker FROM card_request_markers WHERE attempt_id=?`, id).Scan(&marker)
	return marker, err
}

// Only exact unknown attempts that are still the logical current attempt are
// readable. A resolved native request may still confirm an already sent card;
// that does not reopen pending state, eligibility or any native operation.
func (s *Store) ClaimCardReadback(ctx context.Context, kind, host, app, thread string, nowMS, intervalMS int64) (CardReadback, bool, error) {
	var r CardReadback
	if kind != "command" && kind != "reminder" || nowMS <= 0 || intervalMS <= 0 || intervalMS > math.MaxInt64-nowMS {
		return r, false, ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return r, false, err
	}
	defer tx.Rollback()
	if err := requestScope(ctx, tx, host, app); err != nil {
		return r, false, err
	}
	var query string
	args := []any{host, app}
	if kind == "command" {
		query = `SELECT a.attempt_id,a.host_id,a.app_id,a.thread_id,a.chat_id,a.message_id,a.card_json,k.marker FROM command_reply_attempts a JOIN command_replies r USING(app_id,message_id) JOIN card_request_markers k ON k.kind='command' AND k.attempt_id=a.attempt_id WHERE a.host_id=? AND a.app_id=? AND r.host_id=a.host_id AND r.attempt_id=a.attempt_id AND r.state='unknown' AND a.state='unknown' AND k.next_check_at_ms<=? ORDER BY k.next_check_at_ms,a.dispatched_at_ms,a.attempt_id LIMIT 1`
	} else {
		query = `SELECT a.attempt_id,a.host_id,a.app_id,a.thread_id,a.chat_id,a.parent_message_id,a.card_json,k.marker FROM request_reminder_attempts a JOIN app_requests r USING(host_id,app_id,thread_id,request_key) JOIN card_request_markers k ON k.kind='reminder' AND k.attempt_id=a.attempt_id JOIN bindings b USING(host_id,thread_id) WHERE a.host_id=? AND a.app_id=? AND a.thread_id=? AND b.chat_id=a.chat_id AND a.state='unknown' AND r.reminder_state='unknown' AND k.next_check_at_ms<=? ORDER BY k.next_check_at_ms,a.created_at_ms,a.attempt_id LIMIT 1`
		args = append(args, thread)
	}
	args = append(args, nowMS)
	err = tx.QueryRowContext(ctx, query, args...).Scan(&r.ID, &r.HostID, &r.AppID, &r.ThreadID, &r.ChatID, &r.ParentMessageID, &r.Card, &r.Marker)
	if errors.Is(err, sql.ErrNoRows) {
		return CardReadback{}, false, nil
	}
	if err != nil {
		return r, false, err
	}
	r.Kind = kind
	if err := commandWriteOne(ctx, tx, `UPDATE card_request_markers SET next_check_at_ms=? WHERE attempt_id=? AND kind=? AND next_check_at_ms<=?`, nowMS+intervalMS, r.ID, kind, nowMS); err != nil {
		return r, false, err
	}
	if err := tx.Commit(); err != nil {
		return CardReadback{}, false, err
	}
	return r, true, nil
}
