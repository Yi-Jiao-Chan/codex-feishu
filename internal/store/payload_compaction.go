package store

import (
	"context"
	"database/sql"
	"errors"
)

const maxCompactionPayloadBytes = 8 << 20

// compactMirrorPayloadTx releases a settled mirror body only after its remote
// result is unambiguous. Callback capability is retained as metadata, while
// unresolved writes keep the exact body needed for dispatch and readback.
func compactMirrorPayloadTx(ctx context.Context, tx *sql.Tx, host, thread, key string) error {
	m, err := readMirror(ctx, tx, host, thread, key)
	if err != nil {
		return err
	}
	if m.Card == "" || m.State != "confirmed" && m.State != "rejected" {
		return nil
	}
	if m.State == "confirmed" && m.DeliveredRevision != m.Revision && !(m.Retired && m.RemoteID == "") {
		return nil
	}
	var unresolved int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound_attempts WHERE host_id=? AND thread_id=? AND mirror_key=? AND state IN ('dispatching','unknown')`, host, thread, key).Scan(&unresolved); err != nil {
		return err
	}
	if unresolved != 0 {
		return nil
	}
	hash, version, err := cardMetadata(m.Card)
	if err != nil {
		return err
	}
	if m.CardHash != "" && m.CardHash != hash || m.ProjectionVersion != 0 && m.ProjectionVersion != version {
		return ErrConflict
	}
	if version != 0 && m.Terminal && m.State == "confirmed" {
		var completed bool
		err := tx.QueryRowContext(ctx, `SELECT completed FROM cot_deliveries WHERE host_id=? AND thread_id=? AND mirror_key=?`, host, thread, key).Scan(&completed)
		if errors.Is(err, sql.ErrNoRows) || err == nil && !completed {
			return nil
		}
		if err != nil {
			return err
		}
	}
	hasCallback, callbackProofs := false, ""
	if m.Kind == "progress" || m.Kind == "final" {
		hasCallback = cardHasCallback(m.Card)
		callbackProofs = cardControlProofs(m.Card)
	}
	return mirrorWriteOne(ctx, tx, `UPDATE mirrors SET card_json='',card_hash=?,projection_version=?,has_callback=?,callback_proofs=? WHERE host_id=? AND thread_id=? AND mirror_key=? AND card_json<>''`, hash, version, hasCallback, callbackProofs, host, thread, key)
}

// CompactDeliveryPayloads incrementally removes payloads written by older
// versions. The row limit and byte budget independently bound settled attempts,
// completed CoT states, and mirror batches; callers repeat while the count is
// non-zero. A single oversized payload still makes progress by itself.
func (s *Store) CompactDeliveryPayloads(ctx context.Context, host string, limit int) (int, error) {
	if host == "" || limit < 1 || limit > 512 {
		return 0, errors.New("invalid delivery payload compaction")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	type attemptPayload struct {
		id, card, hash, cotStep, cotEvents, cotState, cotHash string
	}
	rows, err := tx.QueryContext(ctx, `SELECT attempt_id,card_json,card_hash,cot_step,cot_events_json,cot_state_json,cot_payload_hash FROM outbound_attempts
	 WHERE host_id=? AND state IN ('confirmed','rejected') AND (card_json<>'' OR cot_events_json<>'[]' OR cot_state_json<>'{}') ORDER BY rowid LIMIT ?`, host, limit)
	if err != nil {
		return 0, err
	}
	var attempts []attemptPayload
	attemptBytes := 0
	for rows.Next() {
		var p attemptPayload
		if err := rows.Scan(&p.id, &p.card, &p.hash, &p.cotStep, &p.cotEvents, &p.cotState, &p.cotHash); err != nil {
			rows.Close()
			return 0, err
		}
		size := len(p.card) + len(p.cotEvents) + len(p.cotState)
		if len(attempts) != 0 && attemptBytes+size > maxCompactionPayloadBytes {
			break
		}
		attempts = append(attempts, p)
		attemptBytes += size
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	compacted := 0
	for _, p := range attempts {
		hash := p.hash
		if p.card != "" {
			actual := cardHash(p.card)
			if hash != "" && hash != actual {
				return 0, ErrConflict
			}
			hash = actual
		}
		cotHash := p.cotHash
		if p.cotStep != "" {
			actual := cotPayloadHash(p.cotEvents, p.cotState)
			if cotHash != "" && cotHash != actual {
				return 0, ErrConflict
			}
			cotHash = actual
		}
		if err := mirrorWriteOne(ctx, tx, `UPDATE outbound_attempts SET card_json='',card_hash=?,cot_events_json='[]',cot_state_json='{}',cot_payload_hash=?
		 WHERE attempt_id=? AND state IN ('confirmed','rejected') AND (card_json<>'' OR cot_events_json<>'[]' OR cot_state_json<>'{}')`, hash, cotHash, p.id); err != nil {
			return 0, err
		}
		compacted++
	}

	rows, err = tx.QueryContext(ctx, `SELECT d.host_id,d.thread_id,d.mirror_key,length(d.state_json) FROM cot_deliveries d JOIN mirrors m USING(host_id,thread_id,mirror_key)
	 WHERE d.host_id=? AND d.completed=1 AND d.state_json<>'{}' ORDER BY m.rowid LIMIT ?`, host, limit)
	if err != nil {
		return 0, err
	}
	var deliveries [][3]string
	deliveryBytes := 0
	for rows.Next() {
		var identity [3]string
		var size int
		if err := rows.Scan(&identity[0], &identity[1], &identity[2], &size); err != nil {
			rows.Close()
			return 0, err
		}
		if len(deliveries) != 0 && deliveryBytes+size > maxCompactionPayloadBytes {
			break
		}
		deliveries = append(deliveries, identity)
		deliveryBytes += size
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, identity := range deliveries {
		if err := mirrorWriteOne(ctx, tx, `UPDATE cot_deliveries SET state_json='{}' WHERE host_id=? AND thread_id=? AND mirror_key=? AND completed=1 AND state_json<>'{}'`, identity[0], identity[1], identity[2]); err != nil {
			return 0, err
		}
		compacted++
	}

	rows, err = tx.QueryContext(ctx, `SELECT m.thread_id,m.mirror_key,length(m.card_json) FROM mirrors m JOIN outbound o USING(host_id,thread_id,mirror_key)
	 WHERE m.host_id=? AND m.card_json<>'' AND json_valid(m.card_json) AND o.state IN ('confirmed','rejected')
	 AND (o.state='rejected' OR m.delivered_revision=m.revision OR (m.retired=1 AND m.remote_id=''))
	 AND NOT EXISTS(SELECT 1 FROM outbound_attempts a WHERE a.host_id=m.host_id AND a.thread_id=m.thread_id AND a.mirror_key=m.mirror_key AND a.state IN ('dispatching','unknown'))
	 AND (o.state='rejected' OR m.terminal=0 OR (m.projection_version=0 AND json_type(m.card_json,'$.codex_feishu_cot') IS NULL)
	      OR EXISTS(SELECT 1 FROM cot_deliveries d WHERE d.host_id=m.host_id AND d.thread_id=m.thread_id AND d.mirror_key=m.mirror_key AND d.completed=1))
	 ORDER BY m.rowid LIMIT ?`, host, limit)
	if err != nil {
		return 0, err
	}
	var mirrors [][2]string
	mirrorBytes := 0
	for rows.Next() {
		var identity [2]string
		var size int
		if err := rows.Scan(&identity[0], &identity[1], &size); err != nil {
			rows.Close()
			return 0, err
		}
		if len(mirrors) != 0 && mirrorBytes+size > maxCompactionPayloadBytes {
			break
		}
		mirrors = append(mirrors, identity)
		mirrorBytes += size
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, identity := range mirrors {
		before := compacted
		var card string
		if err := tx.QueryRowContext(ctx, `SELECT card_json FROM mirrors WHERE host_id=? AND thread_id=? AND mirror_key=?`, host, identity[0], identity[1]).Scan(&card); err != nil {
			return 0, err
		}
		if err := compactMirrorPayloadTx(ctx, tx, host, identity[0], identity[1]); err != nil {
			return 0, err
		}
		var remaining int
		if err := tx.QueryRowContext(ctx, `SELECT card_json<>'' FROM mirrors WHERE host_id=? AND thread_id=? AND mirror_key=?`, host, identity[0], identity[1]).Scan(&remaining); err != nil {
			return 0, err
		}
		if card != "" && remaining == 0 {
			compacted = before + 1
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return compacted, nil
}
