package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/cot"
)

// Mirror is a public delivery ledger entry. Card is present only while the
// payload is needed for delivery or readback. Key includes the turn and stable
// segment index; legacy readers may reuse native item IDs.
type Mirror struct {
	HostID, ThreadID, Key, TurnID, Kind, Card, CardHash, CallbackProofs string
	Revision, DeliveredRevision                                         int64
	ProjectionVersion                                                   int
	HasCallback, Terminal, Retired, Replay                              bool
	// Direct sends a progress/final card to the binding itself. The durable
	// encoding is an empty pair of parent fields, previously forbidden for AI.
	Direct                               bool
	ParentKey, ParentMessageID, RemoteID string
	State                                string
	EventType                            string
	EventAt                              int64            // Original terminal event, Unix seconds; zero means unknown.
	ProgressObservedAt                   map[string]int64 // First local reception per public item; zero means unrecorded.
}

// Watermark records the end of a complete native history scan. It is a local
// reconciliation checkpoint, not a cursor that permits skipping older items.
type Watermark struct {
	TurnID, ItemID string
	ObservedAtMS   int64
}

type MirrorAttempt struct {
	COTStep, COTID, COTEvents, COTState, COTStateBefore, COTPayloadHash string
	COTCaughtUp                                                         bool
	FollowMode, RootMessageID, PlatformThreadID                         string
	RetryOfAttemptID, RejectionRequestID                                string
	ID, HostID, ThreadID, Key, Operation                                string
	ChatID, ParentMessageID, TargetMessageID, Card, CardHash, State     string
	RemoteID                                                            string
	Revision, CreatedAtMS                                               int64
	RejectionCode, RejectionHTTPStatus                                  int
}

type MirrorRejection struct {
	Code, HTTPStatus int
	RequestID        string
}

const mirrorColumns = `m.host_id,m.thread_id,m.mirror_key,m.turn_id,m.kind,m.card_json,m.card_hash,m.projection_version,m.has_callback,m.callback_proofs,m.revision,m.delivered_revision,m.terminal,m.retired,m.replay,m.parent_key,m.parent_message_id,m.remote_id,o.state,m.event_type,m.event_at,m.progress_observed_at_json`

func cardHash(card string) string {
	sum := sha256.Sum256([]byte(card))
	return hex.EncodeToString(sum[:])
}

func cotPayloadHash(events, state string) string {
	return cardHash(events + "\x00" + state)
}

func cardMetadata(card string) (hash string, projectionVersion int, err error) {
	hash = cardHash(card)
	doc, nativeCOT, err := cot.Parse(card)
	if err != nil {
		return "", 0, err
	}
	if nativeCOT {
		projectionVersion = doc.Version
	}
	return hash, projectionVersion, nil
}

func mirrorCardEqual(m Mirror, card string) bool {
	if m.Card != "" {
		return m.Card == card
	}
	return m.CardHash != "" && m.CardHash == cardHash(card)
}

type rowScanner interface{ Scan(...any) error }

func scanMirror(row rowScanner) (m Mirror, err error) {
	var clocks string
	err = row.Scan(&m.HostID, &m.ThreadID, &m.Key, &m.TurnID, &m.Kind, &m.Card, &m.CardHash, &m.ProjectionVersion, &m.HasCallback, &m.CallbackProofs, &m.Revision, &m.DeliveredRevision, &m.Terminal, &m.Retired, &m.Replay, &m.ParentKey, &m.ParentMessageID, &m.RemoteID, &m.State, &m.EventType, &m.EventAt, &clocks)
	if err == nil {
		err = json.Unmarshal([]byte(clocks), &m.ProgressObservedAt)
		if err == nil && m.Card != "" {
			var hash string
			var version int
			hash, version, err = cardMetadata(m.Card)
			if err == nil {
				m.CardHash = hash
				m.ProjectionVersion = version
				m.HasCallback = (m.Kind == "progress" || m.Kind == "final") && cardHasCallback(m.Card)
				if m.Kind == "progress" || m.Kind == "final" {
					m.CallbackProofs = cardControlProofs(m.Card)
				} else {
					m.CallbackProofs = ""
				}
			}
		}
		m.Direct = (m.Kind == "progress" || m.Kind == "final") && m.ParentKey == "" && m.ParentMessageID == ""
	}
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
	return s.StageSnapshot(ctx, host, thread, docs, nil, mark)
}

// StageSnapshot also records exact native receipt links in the same transaction.
// Link conflicts or a bad public document roll back the checkpoint and all work.
func (s *Store) StageSnapshot(ctx context.Context, host, thread string, docs []Mirror, links []InputLink, mark Watermark) error {
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
		if m.Key == "" || m.TurnID == "" || !json.Valid([]byte(m.Card)) || seen[m.Key] || m.Retired && !m.Terminal {
			return errors.New("invalid or duplicate public mirror")
		}
		if doc, nativeCOT, parseErr := cot.Parse(m.Card); parseErr != nil {
			return parseErr
		} else if nativeCOT && (m.Kind != "progress" || doc.ThreadID != thread || doc.TurnID != m.TurnID || m.Terminal != (doc.Status != "inProgress")) {
			return ErrConflict
		} else {
			m.CardHash = cardHash(m.Card)
			m.HasCallback = (m.Kind == "progress" || m.Kind == "final") && cardHasCallback(m.Card)
			if m.Kind == "progress" || m.Kind == "final" {
				m.CallbackProofs = cardControlProofs(m.Card)
			} else {
				m.CallbackProofs = ""
			}
			if nativeCOT {
				m.ProjectionVersion = doc.Version
			}
		}
		seen[m.Key] = true
		for id, at := range m.ProgressObservedAt {
			if m.Kind != "progress" || id == "" || at < 0 {
				return errors.New("invalid progress reception time")
			}
		}
		if m.EventAt < 0 || m.EventType == "" && m.EventAt != 0 || m.EventType != "" && (m.Kind != "final" || m.EventType != "completed" && m.EventType != "failed") {
			return errors.New("invalid public notification event")
		}
		if m.HostID != "" && m.HostID != host || m.ThreadID != "" && m.ThreadID != thread {
			return ErrConflict
		}
		if m.Kind == "human" {
			if m.Direct || m.ParentKey != "" || m.ParentMessageID != "" {
				return errors.New("human mirror cannot have a reply parent")
			}
		} else if m.Kind == "progress" || m.Kind == "final" {
			if m.Direct {
				if m.ParentKey != "" || m.ParentMessageID != "" {
					return errors.New("direct mirror cannot have a reply parent")
				}
			} else if (m.ParentKey == "") == (m.ParentMessageID == "") || m.ParentKey == m.Key {
				return errors.New("reply mirror requires exactly one parent")
			}
		} else {
			return errors.New("unsupported public mirror kind")
		}
		old, readErr := readMirror(ctx, tx, host, thread, m.Key)
		if errors.Is(readErr, sql.ErrNoRows) {
			if m.Retired {
				return ErrConflict // Only an existing obsolete segment can retire.
			}
			clocks, marshalErr := json.Marshal(m.ProgressObservedAt)
			if marshalErr != nil {
				return marshalErr
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO mirrors(host_id,thread_id,mirror_key,turn_id,kind,card_json,card_hash,projection_version,has_callback,callback_proofs,terminal,replay,parent_key,parent_message_id,event_type,event_at,progress_observed_at_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, host, thread, m.Key, m.TurnID, m.Kind, m.Card, m.CardHash, m.ProjectionVersion, m.HasCallback, m.CallbackProofs, m.Terminal, m.Replay, m.ParentKey, m.ParentMessageID, m.EventType, m.EventAt, string(clocks))
			if err == nil {
				_, err = tx.ExecContext(ctx, `INSERT INTO outbound(host_id,thread_id,mirror_key,state) VALUES(?,?,?,'ready')`, host, thread, m.Key)
			}
		} else if readErr != nil {
			return readErr
		} else {
			if old.Kind != m.Kind || old.TurnID != m.TurnID || old.Direct != m.Direct || old.ParentKey != m.ParentKey || old.ParentMessageID != m.ParentMessageID {
				return ErrConflict
			}
			oldVersion, oldCOT := old.ProjectionVersion, old.ProjectionVersion != 0
			if old.Card != "" {
				oldDocument, nativeCOT, parseErr := cot.Parse(old.Card)
				if parseErr != nil {
					return parseErr
				}
				oldCOT = nativeCOT
				if nativeCOT {
					oldVersion = oldDocument.Version
				}
			}
			newCOT := m.ProjectionVersion != 0
			if oldCOT != newCOT || oldCOT && oldVersion != m.ProjectionVersion {
				return ErrConflict
			}
			if oldCOT {
				var closed bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM cot_deliveries WHERE host_id=? AND thread_id=? AND mirror_key=? AND (completed=1 OR json_extract(state_json,'$.finished')=1))`, host, thread, m.Key).Scan(&closed); err != nil {
					return err
				}
				if closed {
					continue
				}
			}
			if old.Terminal && !m.Terminal {
				continue
			}
			// Presentation updates and callbacks may omit this metadata. Keep
			// the first positive reception time, allowing a legacy zero to be
			// filled once without retiming already recorded items.
			if m.ProgressObservedAt != nil {
				if old.ProgressObservedAt == nil {
					old.ProgressObservedAt = make(map[string]int64)
				}
				for id, at := range m.ProgressObservedAt {
					if previous, seen := old.ProgressObservedAt[id]; !seen || previous == 0 && at > 0 {
						old.ProgressObservedAt[id] = at
					}
				}
				clocks, marshalErr := json.Marshal(old.ProgressObservedAt)
				if marshalErr != nil {
					return marshalErr
				}
				if _, err = tx.ExecContext(ctx, `UPDATE mirrors SET progress_observed_at_json=? WHERE host_id=? AND thread_id=? AND mirror_key=?`, string(clocks), host, thread, m.Key); err != nil {
					return err
				}
			}
			if mirrorCardEqual(old, m.Card) && old.Terminal == m.Terminal && old.Retired == m.Retired {
				continue
			}
			// Replay provenance belongs to the first observation. Reconnect
			// must not replace a live event's decision, or create a new one.
			_, err = tx.ExecContext(ctx, `UPDATE mirrors SET card_json=?,card_hash=?,projection_version=?,has_callback=?,callback_proofs=?,terminal=?,retired=?,revision=revision+1 WHERE host_id=? AND thread_id=? AND mirror_key=?`, m.Card, m.CardHash, m.ProjectionVersion, m.HasCallback, m.CallbackProofs, m.Terminal, m.Retired, host, thread, m.Key)
			if err == nil {
				if old.State == "confirmed" {
					_, err = tx.ExecContext(ctx, `UPDATE outbound SET state='ready',retry_of_attempt_id='' WHERE host_id=? AND thread_id=? AND mirror_key=? AND state='confirmed'`, host, thread, m.Key)
				} else if old.State == "rejected" {
					err = reopenCorrectedFinal(ctx, tx, old, m)
				}
			}
		}
		if err != nil {
			return err
		}
		if m.Retired {
			if err = retireUnacceptedMirror(ctx, tx, host, thread, m.Key); err != nil {
				return err
			}
			if err = compactMirrorPayloadTx(ctx, tx, host, thread, m.Key); err != nil {
				return err
			}
		}
	}
	for _, link := range links {
		if err = stageInputLink(ctx, tx, host, thread, link); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO history_watermarks(host_id,thread_id,turn_id,item_id,observed_at_ms) VALUES(?,?,?,?,?) ON CONFLICT(host_id,thread_id) DO UPDATE SET turn_id=excluded.turn_id,item_id=excluded.item_id,observed_at_ms=excluded.observed_at_ms`, host, thread, mark.TurnID, mark.ItemID, mark.ObservedAtMS)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// reopenCorrectedFinal permits one fresh request only after the source payload
// actually changes and the preceding creation was definitively rejected. The
// rejected attempt stays immutable and is linked from the next attempt. Unknown
// requests, updates of an existing remote card and unchanged snapshots remain
// frozen. Rejected mentions are released for dispatch-time reevaluation.
func reopenCorrectedFinal(ctx context.Context, tx *sql.Tx, old, next Mirror) error {
	if old.Kind != "final" || !old.Terminal || !next.Terminal || next.Retired || old.RemoteID != "" {
		return nil
	}
	var source string
	err := tx.QueryRowContext(ctx, `SELECT attempt_id FROM outbound_attempts
	 WHERE host_id=? AND thread_id=? AND mirror_key=? AND revision<=? AND state='rejected' AND remote_id=''
	 ORDER BY created_at_ms DESC,rowid DESC LIMIT 1`, old.HostID, old.ThreadID, old.Key, old.Revision).Scan(&source)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var unresolved, throttled int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound_attempts WHERE host_id=? AND thread_id=? AND mirror_key=? AND state IN ('dispatching','unknown')`, old.HostID, old.ThreadID, old.Key).Scan(&unresolved); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound_throttles WHERE host_id=? AND thread_id=? AND mirror_key=?`, old.HostID, old.ThreadID, old.Key).Scan(&throttled); err != nil {
		return err
	}
	if unresolved != 0 || throttled != 0 {
		return nil
	}
	if err = mirrorWriteOne(ctx, tx, `UPDATE outbound SET state='ready',retry_of_attempt_id=? WHERE host_id=? AND thread_id=? AND mirror_key=? AND state='rejected'`, source, old.HostID, old.ThreadID, old.Key); err != nil {
		return err
	}
	var selected int64
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mentions WHERE host_id=? AND thread_id=? AND mirror_key=? AND attempt_id=? AND state='rejected'`, old.HostID, old.ThreadID, old.Key, source).Scan(&selected); err != nil {
		return err
	}
	return mirrorWriteCount(ctx, tx, selected, `UPDATE mentions SET state='eligible',reason='',attempt_id=NULL WHERE host_id=? AND thread_id=? AND mirror_key=? AND attempt_id=? AND state='rejected'`, old.HostID, old.ThreadID, old.Key, source)
}

func (s *Store) Watermark(ctx context.Context, host, thread string) (m Watermark, err error) {
	err = s.DB.QueryRowContext(ctx, `SELECT turn_id,item_id,observed_at_ms FROM history_watermarks WHERE host_id=? AND thread_id=?`, host, thread).Scan(&m.TurnID, &m.ItemID, &m.ObservedAtMS)
	return
}

// ReadyMirrors only selects work; ClaimMirror performs all dispatch guards in
// a transaction. Ordering is deterministic, and unresolved optional progress
// does not prevent an independent final reply from being selected.
func (s *Store) ReadyMirrors(ctx context.Context, host, thread string) ([]Mirror, error) {
	return s.readyMirrors(ctx, host, thread, 0)
}

// ReadyMirrorsAt excludes future deferrals before any dispatch-time remote
// lookups. ClaimMirror still rechecks the deadline transactionally.
func (s *Store) ReadyMirrorsAt(ctx context.Context, host, thread string, nowMS int64) ([]Mirror, error) {
	if nowMS <= 0 {
		return nil, errors.New("invalid mirror selection time")
	}
	return s.readyMirrors(ctx, host, thread, nowMS)
}

func (s *Store) readyMirrors(ctx context.Context, host, thread string, nowMS int64) ([]Mirror, error) {
	query := `SELECT ` + mirrorColumns + ` FROM mirrors m JOIN outbound o USING(host_id,thread_id,mirror_key) WHERE m.host_id=? AND m.thread_id=? AND o.state='ready'`
	args := []any{host, thread}
	if nowMS > 0 {
		query += ` AND NOT EXISTS(SELECT 1 FROM outbound_throttles t WHERE t.host_id=m.host_id AND t.thread_id=m.thread_id AND t.mirror_key=m.mirror_key AND t.next_retry_at_ms>?)`
		args = append(args, nowMS)
	}
	rows, err := s.DB.QueryContext(ctx, query+` ORDER BY o.rowid`, args...)
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
// per-thread writer/progress interval is busy, or a definite rate rejection is
// still deferred. Unknown work is never claimed.
func (s *Store) ClaimMirror(ctx context.Context, host, thread, key string, revision, nowMS int64) (a MirrorAttempt, claimed bool, err error) {
	return s.ClaimMirrorWithMentions(ctx, host, thread, key, revision, nowMS, MentionOptions{})
}

// ClaimMirrorWithMentions additionally freezes scheduled, reachable mentions in
// the same attempt transaction. It never creates notification eligibility.
func (s *Store) ClaimMirrorWithMentions(ctx context.Context, host, thread, key string, revision, nowMS int64, mentions MentionOptions, progressIntervalMS ...int64) (a MirrorAttempt, claimed bool, err error) {
	if nowMS <= 0 {
		return a, false, errors.New("invalid dispatch time")
	}
	interval := int64(2000)
	if len(progressIntervalMS) > 1 {
		return a, false, errors.New("invalid progress interval")
	}
	if len(progressIntervalMS) == 1 && progressIntervalMS[0] != 0 {
		interval = progressIntervalMS[0]
		if interval < 100 || interval > 10000 {
			return a, false, errors.New("invalid progress interval")
		}
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
	if m.Card == "" {
		return a, false, ErrConflict
	}
	if err = tx.QueryRowContext(ctx, `SELECT retry_of_attempt_id FROM outbound WHERE host_id=? AND thread_id=? AND mirror_key=?`, host, thread, key).Scan(&a.RetryOfAttemptID); err != nil {
		return a, false, err
	}
	if a.RetryOfAttemptID != "" {
		var sourceState, sourceRemote string
		var sourceRevision int64
		err = tx.QueryRowContext(ctx, `SELECT state,remote_id,revision FROM outbound_attempts WHERE attempt_id=? AND host_id=? AND thread_id=? AND mirror_key=?`, a.RetryOfAttemptID, host, thread, key).Scan(&sourceState, &sourceRemote, &sourceRevision)
		if err != nil || sourceState != "rejected" || sourceRemote != "" || sourceRevision > revision {
			if err != nil {
				return a, false, err
			}
			return a, false, ErrConflict
		}
		if sourceRevision == revision {
			var rateRetry int
			if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound_throttles WHERE host_id=? AND thread_id=? AND mirror_key=?`, host, thread, key).Scan(&rateRetry); err != nil {
				return a, false, err
			}
			if rateRetry != 1 {
				return a, false, ErrConflict
			}
		}
	}
	var deferred int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound_throttles WHERE host_id=? AND thread_id=? AND mirror_key=? AND next_retry_at_ms>?`, host, thread, key, nowMS).Scan(&deferred); err != nil || deferred != 0 {
		return a, false, err
	}
	var busy int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound WHERE host_id=? AND thread_id=? AND state='dispatching'`, host, thread).Scan(&busy); err != nil || busy != 0 {
		return a, false, err
	}
	a.ChatID = ""
	var binding Binding
	err = scanBinding(tx.QueryRowContext(ctx, `SELECT `+bindingColumns+` FROM bindings WHERE host_id=? AND thread_id=?`, host, thread), &binding)
	a.ChatID = binding.ChatID
	if err == nil && binding.RepairRequired {
		return a, false, ErrTargetRepair
	}
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
		if last.Valid && nowMS-last.Int64 < interval {
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
	case m.Kind == "human" || m.Direct:
		a.Operation = "send"
	default:
		a.Operation = "reply"
	}
	if binding.FollowMode == "topic" {
		a.FollowMode, a.RootMessageID, a.PlatformThreadID = "topic", binding.RootMessageID, binding.PlatformThreadID
		if a.Operation != "update" {
			a.Operation = "reply"
			a.ParentMessageID = binding.RootMessageID
		}
	}
	_, isCOT, err := cot.Parse(m.Card)
	if err != nil {
		return MirrorAttempt{}, false, err
	}
	if isCOT {
		work, err := prepareCOTAttempt(ctx, tx, m, &a)
		if err != nil {
			return MirrorAttempt{}, false, err
		}
		if !work {
			return MirrorAttempt{}, false, tx.Commit()
		}
	}
	var mentionIDs []string
	if !isCOT {
		mentionIDs, err = prepareMirrorMentions(ctx, tx, m, &a, mentions)
	}
	if err != nil {
		return MirrorAttempt{}, false, err
	}
	err = mirrorWriteOne(ctx, tx, `INSERT INTO outbound_attempts(attempt_id,host_id,thread_id,mirror_key,revision,operation,chat_id,parent_message_id,target_message_id,card_json,created_at_ms,state,follow_mode,root_message_id,platform_thread_id,retry_of_attempt_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,'dispatching',?,?,?,?)`, a.ID, host, thread, key, a.Revision, a.Operation, a.ChatID, a.ParentMessageID, a.TargetMessageID, a.Card, nowMS, a.FollowMode, a.RootMessageID, a.PlatformThreadID, a.RetryOfAttemptID)
	if err == nil && !isCOT {
		err = freezeMirrorRequestMarker(ctx, tx, &a)
	}
	if err == nil && isCOT {
		a.COTPayloadHash = cotPayloadHash(a.COTEvents, a.COTState)
		err = mirrorWriteOne(ctx, tx, `UPDATE outbound_attempts SET cot_step=?,cot_id=?,cot_events_json=?,cot_state_json=?,cot_state_before=?,cot_caught_up=?,cot_payload_hash=? WHERE attempt_id=?`, a.COTStep, a.COTID, a.COTEvents, a.COTState, a.COTStateBefore, a.COTCaughtUp, a.COTPayloadHash, a.ID)
	}
	if err == nil {
		a.CardHash = cardHash(a.Card)
		err = mirrorWriteOne(ctx, tx, `UPDATE outbound_attempts SET card_json=?,card_hash=? WHERE attempt_id=?`, a.Card, a.CardHash, a.ID)
	}
	if err == nil {
		err = linkMirrorMentions(ctx, tx, a, mentionIDs)
	}
	if err == nil {
		err = mirrorWriteOne(ctx, tx, `UPDATE outbound SET state='dispatching' WHERE host_id=? AND thread_id=? AND mirror_key=?`, host, thread, key)
	}
	if err != nil {
		return MirrorAttempt{}, false, err
	}
	err = tx.Commit()
	return a, err == nil, err
}

const attemptColumns = `attempt_id,host_id,thread_id,mirror_key,revision,operation,chat_id,parent_message_id,target_message_id,card_json,card_hash,created_at_ms,state,remote_id,follow_mode,root_message_id,platform_thread_id,cot_step,cot_id,cot_events_json,cot_state_json,cot_state_before,cot_caught_up,cot_payload_hash,retry_of_attempt_id,rejection_code,rejection_http_status,rejection_request_id`

func scanAttempt(row rowScanner) (a MirrorAttempt, err error) {
	err = row.Scan(&a.ID, &a.HostID, &a.ThreadID, &a.Key, &a.Revision, &a.Operation, &a.ChatID, &a.ParentMessageID, &a.TargetMessageID, &a.Card, &a.CardHash, &a.CreatedAtMS, &a.State, &a.RemoteID, &a.FollowMode, &a.RootMessageID, &a.PlatformThreadID, &a.COTStep, &a.COTID, &a.COTEvents, &a.COTState, &a.COTStateBefore, &a.COTCaughtUp, &a.COTPayloadHash, &a.RetryOfAttemptID, &a.RejectionCode, &a.RejectionHTTPStatus, &a.RejectionRequestID)
	if err == nil && a.Card != "" && a.CardHash == "" {
		a.CardHash = cardHash(a.Card)
	}
	if err == nil && a.COTStep != "" && a.COTPayloadHash == "" {
		a.COTPayloadHash = cotPayloadHash(a.COTEvents, a.COTState)
	}
	if a.COTStep == "" {
		a.COTEvents, a.COTState = "", ""
	}
	return
}

func (s *Store) MirrorAttempt(ctx context.Context, id string) (MirrorAttempt, error) {
	return scanAttempt(s.DB.QueryRowContext(ctx, `SELECT `+attemptColumns+` FROM outbound_attempts WHERE attempt_id=?`, id))
}

// FinishMirrorAttempt persists the remote evidence and scheduling transition
// atomically. Unknown attempts always retain their frozen payload. Settled
// attempts keep their hash and delivery evidence but release the full card body.
// A definitely rejected final can be superseded only when StageSnapshot
// observes a different safe payload. Confirmation after response loss is
// allowed, but only with an actual ID.
func (s *Store) FinishMirrorAttempt(ctx context.Context, id, outcome, remoteID string) error {
	return s.finishMirrorAttempt(ctx, id, outcome, remoteID, "", false, MirrorRejection{})
}

func (s *Store) FinishRejectedMirrorAttempt(ctx context.Context, id string, rejection MirrorRejection) error {
	return s.finishMirrorAttempt(ctx, id, "rejected", "", "", false, rejection)
}

func (s *Store) FinishCOTAttempt(ctx context.Context, id, outcome, remoteID, cotID string) error {
	return s.finishMirrorAttempt(ctx, id, outcome, remoteID, cotID, true, MirrorRejection{})
}

func (s *Store) finishMirrorAttempt(ctx context.Context, id, outcome, remoteID, cotID string, nativeCOT bool, rejection MirrorRejection) error {
	if outcome != "confirmed" && outcome != "unknown" && outcome != "rejected" {
		return errors.New("invalid mirror outcome")
	}
	if outcome == "confirmed" && remoteID == "" || outcome != "confirmed" && remoteID != "" {
		return errors.New("mirror outcome lacks or contradicts remote evidence")
	}
	if rejection.Code < 0 || rejection.HTTPStatus < 0 || rejection.HTTPStatus > 999 || outcome != "rejected" && rejection != (MirrorRejection{}) {
		return errors.New("invalid mirror rejection evidence")
	}
	rejection.RequestID = safeMirrorDiagnosticID(rejection.RequestID)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	a, err := scanAttempt(tx.QueryRowContext(ctx, `SELECT `+attemptColumns+` FROM outbound_attempts WHERE attempt_id=?`, id))
	if err != nil {
		return err
	}
	if (a.COTStep != "") != nativeCOT {
		return ErrConflict
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
	if nativeCOT {
		state, err = finishCOTMirror(ctx, tx, a, m, outcome, remoteID, cotID)
	} else if outcome == "confirmed" {
		if a.Operation == "update" && remoteID != a.TargetMessageID || m.RemoteID != "" && remoteID != m.RemoteID {
			return ErrConflict
		}
		err = mirrorWriteOne(ctx, tx, `UPDATE mirrors SET remote_id=?,delivered_revision=? WHERE host_id=? AND thread_id=? AND mirror_key=?`, remoteID, a.Revision, a.HostID, a.ThreadID, a.Key)
		if m.Revision > a.Revision {
			state = "ready"
		}
	}
	if err == nil {
		hash := a.CardHash
		if hash == "" && a.Card != "" {
			hash = cardHash(a.Card)
		}
		cotHash := a.COTPayloadHash
		if cotHash == "" && a.COTStep != "" {
			cotHash = cotPayloadHash(a.COTEvents, a.COTState)
		}
		if outcome == "unknown" {
			err = mirrorWriteOne(ctx, tx, `UPDATE outbound_attempts SET state=?,remote_id=?,rejection_code=?,rejection_http_status=?,rejection_request_id=?,card_hash=?,cot_payload_hash=? WHERE attempt_id=?`, outcome, remoteID, rejection.Code, rejection.HTTPStatus, rejection.RequestID, hash, cotHash, id)
		} else {
			err = mirrorWriteOne(ctx, tx, `UPDATE outbound_attempts SET state=?,remote_id=?,rejection_code=?,rejection_http_status=?,rejection_request_id=?,card_hash=?,cot_payload_hash=?,card_json='',cot_events_json='[]',cot_state_json='{}' WHERE attempt_id=?`, outcome, remoteID, rejection.Code, rejection.HTTPStatus, rejection.RequestID, hash, cotHash, id)
		}
	}
	if err == nil {
		err = mirrorWriteOne(ctx, tx, `UPDATE outbound SET state=?,retry_of_attempt_id='' WHERE host_id=? AND thread_id=? AND mirror_key=?`, state, a.HostID, a.ThreadID, a.Key)
	}
	if err == nil {
		var selected int64
		err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mentions WHERE attempt_id=? AND state IN ('dispatching','unknown')`, id).Scan(&selected)
		if err == nil {
			err = mirrorWriteCount(ctx, tx, selected, `UPDATE mentions SET state=? WHERE attempt_id=? AND state IN ('dispatching','unknown')`, outcome, id)
		}
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `DELETE FROM outbound_throttles WHERE host_id=? AND thread_id=? AND mirror_key=?`, a.HostID, a.ThreadID, a.Key)
	}
	if err == nil && !nativeCOT && outcome == "rejected" && a.RetryOfAttemptID != "" && m.Kind == "final" && !m.Retired {
		err = stageFinalDeliveryFailure(ctx, tx, a, m, rejection)
	}
	if err == nil {
		err = compactMirrorPayloadTx(ctx, tx, a.HostID, a.ThreadID, a.Key)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func safeMirrorDiagnosticID(value string) string {
	if value == "" || len(value) > 128 {
		return ""
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.:", r) {
			continue
		}
		return ""
	}
	return value
}

func stageFinalDeliveryFailure(ctx context.Context, tx *sql.Tx, a MirrorAttempt, m Mirror, rejection MirrorRejection) error {
	key := "ui:delivery-failure:" + a.ID
	card := cards.DeliveryFailure(rejection.Code, rejection.HTTPStatus, rejection.RequestID, a.ID)
	if err := mirrorWriteOne(ctx, tx, `INSERT INTO mirrors(host_id,thread_id,mirror_key,turn_id,kind,card_json,card_hash,terminal,replay,parent_key,parent_message_id) VALUES(?,?,?,?,'progress',?,?,1,?,?,?)`, a.HostID, a.ThreadID, key, key, card, cardHash(card), m.Replay, m.ParentKey, m.ParentMessageID); err != nil {
		return err
	}
	return mirrorWriteOne(ctx, tx, `INSERT INTO outbound(host_id,thread_id,mirror_key,state) VALUES(?,?,?,'ready')`, a.HostID, a.ThreadID, key)
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
	if _, err = tx.ExecContext(ctx, `UPDATE mentions SET state='unknown' WHERE host_id=? AND state='dispatching'`, host); err != nil {
		return err
	}
	return tx.Commit()
}
