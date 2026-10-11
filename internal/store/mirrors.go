package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Mirror is a public card document. Key includes the turn and stable segment
// index; native item IDs alone may be reused by legacy history readers.
type Mirror struct {
	HostID, ThreadID, Key, TurnID, Kind, Card string
	Revision, DeliveredRevision               int64
	Terminal, Replay                          bool
	ParentKey, ParentMessageID, RemoteID      string
	State                                     string
}

// Watermark records the end of a complete native history scan. It is a local
// reconciliation checkpoint, not a cursor that permits skipping older items.
type Watermark struct {
	TurnID, ItemID string
	ObservedAtMS   int64
}

type MirrorAttempt struct {
	ID, HostID, ThreadID, Key, Operation                  string
	ChatID, ParentMessageID, TargetMessageID, Card, State string
	RemoteID                                              string
	Revision, CreatedAtMS                                 int64
}

const mirrorColumns = `m.host_id,m.thread_id,m.mirror_key,m.turn_id,m.kind,m.card_json,m.revision,m.delivered_revision,m.terminal,m.replay,m.parent_key,m.parent_message_id,m.remote_id,o.state`

type rowScanner interface{ Scan(...any) error }

func scanMirror(row rowScanner) (m Mirror, err error) {
	err = row.Scan(&m.HostID, &m.ThreadID, &m.Key, &m.TurnID, &m.Kind, &m.Card, &m.Revision, &m.DeliveredRevision, &m.Terminal, &m.Replay, &m.ParentKey, &m.ParentMessageID, &m.RemoteID, &m.State)
	return
}

func readMirror(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, host, thread, key string) (Mirror, error) {
	return scanMirror(q.QueryRowContext(ctx, `SELECT `+mirrorColumns+` FROM mirrors m JOIN outbound o USING(host_id,thread_id,mirror_key) WHERE m.host_id=? AND m.thread_id=? AND m.mirror_key=?`, host, thread, key))
}

func (s *Store) Mirror(ctx context.Context, host, thread, key string) (Mirror, error) {
	return readMirror(ctx, s.DB, host, thread, key)
}

// StageMirrors commits public documents, outbox work and the complete-scan
// checkpoint together. It also works before the selected thread has a group.
// Caller serializes complete snapshots per thread; an older checkpoint is
// rejected so delayed snapshots cannot overwrite newer public content.
func (s *Store) StageMirrors(ctx context.Context, host, thread string, docs []Mirror, mark Watermark) error {
	if host == "" || thread == "" || mark.ObservedAtMS <= 0 {
		return errors.New("invalid mirror snapshot")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	err = tx.QueryRowContext(ctx, `SELECT state FROM thread_observations WHERE host_id=? AND thread_id=?`, host, thread).Scan(&state)
	if err != nil {
		return err
	}
	if state != "eligible" {
		// A later mute does not stop an existing binding's mirror stream.
		var bound int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM bindings WHERE host_id=? AND thread_id=?`, host, thread).Scan(&bound); err != nil {
			return err
		}
		if bound == 0 {
			return ErrIneligible
		}
	}
	var previous int64
	err = tx.QueryRowContext(ctx, `SELECT observed_at_ms FROM history_watermarks WHERE host_id=? AND thread_id=?`, host, thread).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if previous > mark.ObservedAtMS {
		return ErrConflict
	}
	seen := make(map[string]bool, len(docs))
	for _, m := range docs {
		if m.Key == "" || m.TurnID == "" || !json.Valid([]byte(m.Card)) || seen[m.Key] {
			return errors.New("invalid or duplicate public mirror")
		}
		seen[m.Key] = true
		if m.HostID != "" && m.HostID != host || m.ThreadID != "" && m.ThreadID != thread {
			return ErrConflict
		}
		if m.Kind == "human" {
			if m.ParentKey != "" || m.ParentMessageID != "" {
				return errors.New("human mirror cannot have a reply parent")
			}
		} else if m.Kind == "progress" || m.Kind == "final" {
			if (m.ParentKey == "") == (m.ParentMessageID == "") || m.ParentKey == m.Key {
				return errors.New("reply mirror requires exactly one parent")
			}
		} else {
			return errors.New("unsupported public mirror kind")
		}
		old, readErr := readMirror(ctx, tx, host, thread, m.Key)
		if errors.Is(readErr, sql.ErrNoRows) {
			_, err = tx.ExecContext(ctx, `INSERT INTO mirrors(host_id,thread_id,mirror_key,turn_id,kind,card_json,terminal,replay,parent_key,parent_message_id) VALUES(?,?,?,?,?,?,?,?,?,?)`, host, thread, m.Key, m.TurnID, m.Kind, m.Card, m.Terminal, m.Replay, m.ParentKey, m.ParentMessageID)
			if err == nil {
				_, err = tx.ExecContext(ctx, `INSERT INTO outbound(host_id,thread_id,mirror_key,state) VALUES(?,?,?,'ready')`, host, thread, m.Key)
			}
		} else if readErr != nil {
			return readErr
		} else {
			if old.Kind != m.Kind || old.TurnID != m.TurnID || old.ParentKey != m.ParentKey || old.ParentMessageID != m.ParentMessageID {
				return ErrConflict
			}
			if old.Terminal && !m.Terminal || old.Card == m.Card && old.Terminal == m.Terminal {
				continue
			}
			// Replay provenance belongs to the first observation. Reconnect
			// must not replace a live event's decision, or create a new one.
			_, err = tx.ExecContext(ctx, `UPDATE mirrors SET card_json=?,terminal=?,revision=revision+1 WHERE host_id=? AND thread_id=? AND mirror_key=?`, m.Card, m.Terminal, host, thread, m.Key)
			if err == nil {
				_, err = tx.ExecContext(ctx, `UPDATE outbound SET state='ready' WHERE host_id=? AND thread_id=? AND mirror_key=? AND state='confirmed'`, host, thread, m.Key)
			}
		}
		if err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO history_watermarks(host_id,thread_id,turn_id,item_id,observed_at_ms) VALUES(?,?,?,?,?) ON CONFLICT(host_id,thread_id) DO UPDATE SET turn_id=excluded.turn_id,item_id=excluded.item_id,observed_at_ms=excluded.observed_at_ms`, host, thread, mark.TurnID, mark.ItemID, mark.ObservedAtMS)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Watermark(ctx context.Context, host, thread string) (m Watermark, err error) {
	err = s.DB.QueryRowContext(ctx, `SELECT turn_id,item_id,observed_at_ms FROM history_watermarks WHERE host_id=? AND thread_id=?`, host, thread).Scan(&m.TurnID, &m.ItemID, &m.ObservedAtMS)
	return
}

// ReadyMirrors only selects work; ClaimMirror performs all dispatch guards in
// a transaction. Ordering is deterministic, and unresolved optional progress
// does not prevent an independent final reply from being selected.
func (s *Store) ReadyMirrors(ctx context.Context, host, thread string) ([]Mirror, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+mirrorColumns+` FROM mirrors m JOIN outbound o USING(host_id,thread_id,mirror_key) WHERE m.host_id=? AND m.thread_id=? AND o.state='ready' ORDER BY o.rowid`, host, thread)
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

// ClaimMirror freezes target, parent, payload and UUID before a remote write.
// A false claim means not ready, stale revision, no binding/parent yet, or the
// per-thread writer/progress interval is busy. Unknown work is never claimed.
func (s *Store) ClaimMirror(ctx context.Context, host, thread, key string, revision, nowMS int64) (a MirrorAttempt, claimed bool, err error) {
	if nowMS <= 0 {
		return a, false, errors.New("invalid dispatch time")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return a, false, err
	}
	defer tx.Rollback()
	m, err := readMirror(ctx, tx, host, thread, key)
	if err != nil {
		return a, false, err
	}
	if m.State != "ready" || m.Revision != revision {
		return a, false, nil
	}
	var busy int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound WHERE host_id=? AND thread_id=? AND state='dispatching'`, host, thread).Scan(&busy); err != nil || busy != 0 {
		return a, false, err
	}
	a.ChatID = ""
	err = tx.QueryRowContext(ctx, `SELECT chat_id FROM bindings WHERE host_id=? AND thread_id=?`, host, thread).Scan(&a.ChatID)
	if errors.Is(err, sql.ErrNoRows) {
		return MirrorAttempt{}, false, nil
	}
	if err != nil {
		return a, false, err
	}
	if m.Kind == "progress" && !m.Terminal {
		var last sql.NullInt64
		if err = tx.QueryRowContext(ctx, `SELECT MAX(created_at_ms) FROM outbound_attempts WHERE host_id=? AND thread_id=? AND mirror_key=?`, host, thread, key).Scan(&last); err != nil {
			return a, false, err
		}
		if last.Valid && nowMS-last.Int64 < 2000 {
			return MirrorAttempt{}, false, nil
		}
	}
	a.ParentMessageID = m.ParentMessageID
	if m.ParentKey != "" {
		var kind string
		err = tx.QueryRowContext(ctx, `SELECT kind,remote_id FROM mirrors WHERE host_id=? AND thread_id=? AND mirror_key=?`, host, thread, m.ParentKey).Scan(&kind, &a.ParentMessageID)
		if errors.Is(err, sql.ErrNoRows) || err == nil && a.ParentMessageID == "" {
			return MirrorAttempt{}, false, nil
		}
		if err != nil {
			return a, false, err
		}
		if kind != "human" {
			return a, false, ErrConflict
		}
	}
	a.ID, a.HostID, a.ThreadID, a.Key = ID(), host, thread, key
	a.Revision, a.Card, a.CreatedAtMS, a.State = m.Revision, m.Card, nowMS, "dispatching"
	a.TargetMessageID = m.RemoteID
	switch {
	case m.RemoteID != "":
		a.Operation = "update"
	case m.Kind == "human":
		a.Operation = "send"
	default:
		a.Operation = "reply"
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO outbound_attempts(attempt_id,host_id,thread_id,mirror_key,revision,operation,chat_id,parent_message_id,target_message_id,card_json,created_at_ms,state) VALUES(?,?,?,?,?,?,?,?,?,?,?,'dispatching')`, a.ID, host, thread, key, a.Revision, a.Operation, a.ChatID, a.ParentMessageID, a.TargetMessageID, a.Card, nowMS)
	if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE outbound SET state='dispatching' WHERE host_id=? AND thread_id=? AND mirror_key=?`, host, thread, key)
	}
	if err != nil {
		return MirrorAttempt{}, false, err
	}
	err = tx.Commit()
	return a, err == nil, err
}

const attemptColumns = `attempt_id,host_id,thread_id,mirror_key,revision,operation,chat_id,parent_message_id,target_message_id,card_json,created_at_ms,state,remote_id`

func scanAttempt(row rowScanner) (a MirrorAttempt, err error) {
	err = row.Scan(&a.ID, &a.HostID, &a.ThreadID, &a.Key, &a.Revision, &a.Operation, &a.ChatID, &a.ParentMessageID, &a.TargetMessageID, &a.Card, &a.CreatedAtMS, &a.State, &a.RemoteID)
	return
}

func (s *Store) MirrorAttempt(ctx context.Context, id string) (MirrorAttempt, error) {
	return scanAttempt(s.DB.QueryRowContext(ctx, `SELECT `+attemptColumns+` FROM outbound_attempts WHERE attempt_id=?`, id))
}

// FinishMirrorAttempt persists the remote evidence and scheduling transition
// atomically. Unknown/rejected attempts stay frozen even if new content arrives.
// Confirmation after response loss is allowed, but only with an actual ID.
func (s *Store) FinishMirrorAttempt(ctx context.Context, id, outcome, remoteID string) error {
	if outcome != "confirmed" && outcome != "unknown" && outcome != "rejected" {
		return errors.New("invalid mirror outcome")
	}
	if outcome == "confirmed" && remoteID == "" || outcome != "confirmed" && remoteID != "" {
		return errors.New("mirror outcome lacks or contradicts remote evidence")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	a, err := scanAttempt(tx.QueryRowContext(ctx, `SELECT `+attemptColumns+` FROM outbound_attempts WHERE attempt_id=?`, id))
	if err != nil {
		return err
	}
	if a.State == "confirmed" || a.State == "rejected" {
		if a.State == outcome && a.RemoteID == remoteID {
			return nil
		}
		return ErrConflict
	}
	m, err := readMirror(ctx, tx, a.HostID, a.ThreadID, a.Key)
	if err != nil {
		return err
	}
	if m.State != "dispatching" && m.State != "unknown" {
		return ErrConflict
	}
	state := outcome
	if outcome == "confirmed" {
		if a.Operation == "update" && remoteID != a.TargetMessageID || m.RemoteID != "" && remoteID != m.RemoteID {
			return ErrConflict
		}
		_, err = tx.ExecContext(ctx, `UPDATE mirrors SET remote_id=?,delivered_revision=? WHERE host_id=? AND thread_id=? AND mirror_key=?`, remoteID, a.Revision, a.HostID, a.ThreadID, a.Key)
		if m.Revision > a.Revision {
			state = "ready"
		}
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE outbound_attempts SET state=?,remote_id=? WHERE attempt_id=?`, outcome, remoteID, id)
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE outbound SET state=? WHERE host_id=? AND thread_id=? AND mirror_key=?`, state, a.HostID, a.ThreadID, a.Key)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// RecoverMirrorDispatches is called once after acquiring the deployment lock,
// before starting workers. Dispatching may mean accepted with a lost response.
func (s *Store) RecoverMirrorDispatches(ctx context.Context, host string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE outbound_attempts SET state='unknown' WHERE host_id=? AND state='dispatching'`, host); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE outbound SET state='unknown' WHERE host_id=? AND state='dispatching'`, host); err != nil {
		return err
	}
	return tx.Commit()
}
