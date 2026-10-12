package store

import "context"

// VerificationDeliveryEvidence is a local persisted snapshot, not proof of
// client rendering, one progress card per turn, or complete P1 acceptance.
type VerificationDeliveryEvidence struct {
	ProgressCards       int `json:"progress_cards"`
	ProgressConfirmed   int `json:"progress_confirmed"`
	FinalCards          int `json:"final_cards"`
	FinalConfirmed      int `json:"final_confirmed"`
	FinalTurns          int `json:"final_turns"`
	FinalTurnsDelivered int `json:"final_turns_delivered"`
	ReferencedCards     int `json:"referenced_cards"`
	UnresolvedAttempts  int `json:"unresolved_attempts"`
}

// VerificationDeliveryEvidence requires exact current revision and original
// reply creation proof. Only routing metadata is selected; card bodies, tokens
// and frozen wire requests never leave or enter this query's projection.
func (s *Store) VerificationDeliveryEvidence(ctx context.Context, b VerificationBinding) (VerificationDeliveryEvidence, error) {
	var result VerificationDeliveryEvidence
	tx, err := s.beginVerificationEvidenceTx(ctx, b)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `WITH ai AS (
 SELECT m.host_id,m.thread_id,m.mirror_key,m.turn_id,m.kind,m.revision,m.delivered_revision,
 m.retired,m.parent_key,m.parent_message_id,m.remote_id,COALESCE(o.state,'') AS state
 FROM mirrors m LEFT JOIN outbound o USING(host_id,thread_id,mirror_key)
 WHERE m.host_id=? AND m.thread_id=? AND m.kind IN ('progress','final') AND m.turn_id<>''
 AND substr(m.mirror_key,1,3)<>'ui:' AND substr(m.turn_id,1,3)<>'ui:'
), attempts AS (
 SELECT mirror_key,revision,operation,parent_message_id,target_message_id,remote_id,state
 FROM outbound_attempts WHERE host_id=? AND thread_id=? AND chat_id=?
), anchors AS (
 SELECT m.mirror_key,m.parent_message_id AS parent FROM ai m
 JOIN m0_receipts r ON r.message_id=m.parent_message_id AND r.app_id=? AND r.thread_id=m.thread_id AND r.chat_id=?
 JOIN input_history_links h ON h.receipt_id=r.id AND h.host_id=m.host_id AND h.thread_id=m.thread_id AND h.turn_id=m.turn_id
 WHERE m.parent_key='' AND m.parent_message_id<>'' AND r.client_id<>'' AND h.item_id<>''
 UNION
 SELECT m.mirror_key,h.remote_id AS parent FROM ai m JOIN mirrors h
 ON h.host_id=m.host_id AND h.thread_id=m.thread_id AND h.mirror_key=m.parent_key AND h.turn_id=m.turn_id
 WHERE m.parent_key<>'' AND m.parent_message_id='' AND h.kind='human' AND h.remote_id<>''
 AND EXISTS(SELECT 1 FROM attempts p WHERE p.mirror_key=h.mirror_key AND p.state='confirmed'
 AND p.operation IN ('send','reply') AND p.target_message_id='' AND p.remote_id=h.remote_id AND p.revision<=h.revision)
), referenced AS (
 SELECT m.* FROM ai m WHERE m.remote_id<>'' AND EXISTS(
 SELECT 1 FROM attempts a JOIN anchors h ON h.mirror_key=m.mirror_key AND h.parent=a.parent_message_id
 WHERE a.mirror_key=m.mirror_key AND a.state='confirmed' AND a.operation='reply'
 AND a.target_message_id='' AND a.remote_id=m.remote_id AND a.revision>0 AND a.revision<=m.revision)
), confirmed AS (
 SELECT m.mirror_key FROM referenced m WHERE m.state='confirmed' AND m.revision>0 AND m.delivered_revision=m.revision
 AND EXISTS(SELECT 1 FROM attempts a WHERE a.mirror_key=m.mirror_key AND a.revision=m.revision
 AND a.state='confirmed' AND a.remote_id=m.remote_id AND (
 a.operation='update' AND a.target_message_id=m.remote_id OR
 a.operation='reply' AND a.target_message_id='' AND EXISTS(SELECT 1 FROM anchors h WHERE h.mirror_key=m.mirror_key AND h.parent=a.parent_message_id)))
), final_turns AS (
 SELECT DISTINCT turn_id FROM ai WHERE kind='final' AND retired=0
)
 SELECT
 (SELECT COUNT(*) FROM ai WHERE kind='progress' AND retired=0),
 (SELECT COUNT(*) FROM ai m JOIN confirmed c USING(mirror_key) WHERE m.kind='progress' AND m.retired=0),
 (SELECT COUNT(*) FROM ai WHERE kind='final' AND retired=0),
 (SELECT COUNT(*) FROM ai m JOIN confirmed c USING(mirror_key) WHERE m.kind='final' AND m.retired=0),
 (SELECT COUNT(*) FROM final_turns),
 (SELECT COUNT(*) FROM final_turns t WHERE NOT EXISTS(SELECT 1 FROM ai m WHERE m.kind='final' AND m.turn_id=t.turn_id AND NOT (
 EXISTS(SELECT 1 FROM confirmed c WHERE c.mirror_key=m.mirror_key) OR
 m.retired=1 AND m.state='confirmed' AND m.remote_id='' AND NOT EXISTS(SELECT 1 FROM outbound_attempts a
 WHERE a.host_id=m.host_id AND a.thread_id=m.thread_id AND a.mirror_key=m.mirror_key
 AND (a.state IN ('dispatching','unknown') OR a.state='confirmed' AND a.remote_id<>'' AND a.operation IN ('send','reply')))))),
 (SELECT COUNT(*) FROM referenced WHERE retired=0),
 (SELECT COUNT(*) FROM attempts WHERE state IN ('dispatching','unknown'))`,
		b.HostID, b.ThreadID, b.HostID, b.ThreadID, b.ChatID, b.AppID, b.ChatID).Scan(
		&result.ProgressCards, &result.ProgressConfirmed, &result.FinalCards, &result.FinalConfirmed,
		&result.FinalTurns, &result.FinalTurnsDelivered, &result.ReferencedCards, &result.UnresolvedAttempts)
	if err != nil {
		return VerificationDeliveryEvidence{}, err
	}
	if err := tx.Commit(); err != nil {
		return VerificationDeliveryEvidence{}, err
	}
	return result, nil
}
