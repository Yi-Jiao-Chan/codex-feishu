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

const MirrorReadbackSchema = `
CREATE TABLE IF NOT EXISTS outbound_request_markers (
 marker TEXT PRIMARY KEY CHECK(length(marker)=20),
 attempt_id TEXT NOT NULL UNIQUE REFERENCES outbound_attempts(attempt_id),
 next_check_at_ms INTEGER NOT NULL DEFAULT 0 CHECK(next_check_at_ms>=0)
);
`

// Request markers are valid invisible markdown IDs, unique across this
// database. A collision aborts claim before any remote write. Old unmarked
// attempts keep their original documents and do not acquire invented proof.
func freezeMirrorRequestMarker(ctx context.Context, tx *sql.Tx, a *MirrorAttempt) error {
	hash := sha256.Sum256([]byte(a.ID))
	marker := "c" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(hash[:])[:19]
	card, marked, err := cards.WithRequestMarker(a.Card, marker)
	if err != nil || !marked {
		return err
	}
	if err := mirrorWriteOne(ctx, tx, `INSERT INTO outbound_request_markers(marker,attempt_id) VALUES(?,?)`, marker, a.ID); err != nil {
		return err
	}
	a.Card = card
	return nil
}

func (s *Store) MirrorRequestMarker(ctx context.Context, id string) (string, error) {
	var marker string
	err := s.DB.QueryRowContext(ctx, `SELECT marker FROM outbound_request_markers WHERE attempt_id=?`, id).Scan(&marker)
	return marker, err
}

// MirrorReadback is private worker data, not diagnostic/log output. Its full
// frozen card is needed to verify a platform read without exposing content.
type MirrorReadback struct {
	Attempt MirrorAttempt `json:"-"`
	Marker  string        `json:"-"`
	AppID   string        `json:"-"`
}

// ClaimMirrorReadback reserves a bounded GET-only check. A restart preserves
// the interval; no read can reopen a mutation or reset an unknown payload.
func (s *Store) ClaimMirrorReadback(ctx context.Context, host, thread string, nowMS, intervalMS int64) (MirrorReadback, bool, error) {
	var r MirrorReadback
	if nowMS <= 0 || intervalMS <= 0 || intervalMS > math.MaxInt64-nowMS {
		return r, false, ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return r, false, err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT a.attempt_id,k.marker,d.app_id FROM outbound_attempts a JOIN outbound_request_markers k USING(attempt_id) JOIN outbound o USING(host_id,thread_id,mirror_key) JOIN deployment d USING(host_id) JOIN bindings b USING(host_id,thread_id) WHERE a.host_id=? AND a.thread_id=? AND a.state='unknown' AND o.state='unknown' AND b.chat_id=a.chat_id AND k.next_check_at_ms<=? ORDER BY k.next_check_at_ms,a.created_at_ms,a.attempt_id LIMIT 1`, host, thread, nowMS).Scan(&id, &r.Marker, &r.AppID)
	if errors.Is(err, sql.ErrNoRows) {
		return MirrorReadback{}, false, nil
	}
	if err != nil {
		return r, false, err
	}
	r.Attempt, err = scanAttempt(tx.QueryRowContext(ctx, `SELECT `+attemptColumns+` FROM outbound_attempts WHERE attempt_id=?`, id))
	if err != nil {
		return r, false, err
	}
	if err := mirrorWriteOne(ctx, tx, `UPDATE outbound_request_markers SET next_check_at_ms=? WHERE attempt_id=? AND next_check_at_ms<=?`, nowMS+intervalMS, id, nowMS); err != nil {
		return r, false, err
	}
	if err := tx.Commit(); err != nil {
		return MirrorReadback{}, false, err
	}
	return r, true, nil
}
