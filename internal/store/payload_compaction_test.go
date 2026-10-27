package store

import (
	"context"
	"testing"
)

func TestSettledMirrorPayloadsCompactButUnknownStaysFrozen(t *testing.T) {
	ctx := context.Background()
	s, _ := discoveryStore(t)
	eligible(t, s, "thread")
	bindMirrorThread(t, s)
	doc := Mirror{Key: "human", TurnID: "turn", Kind: "human", Card: `{"body":"bounded payload"}`, Terminal: true}
	stage(t, s, 100, doc)
	a := claimMirror(t, s, doc.Key, 1000)
	if err := s.FinishMirrorAttempt(ctx, a.ID, "unknown", ""); err != nil {
		t.Fatal(err)
	}
	unknownAttempt, err := s.MirrorAttempt(ctx, a.ID)
	if err != nil || unknownAttempt.Card != a.Card || unknownAttempt.CardHash != a.CardHash {
		t.Fatalf("unknown attempt lost its frozen payload: %+v %v", unknownAttempt, err)
	}
	unknownMirror, err := s.Mirror(ctx, "host", "thread", doc.Key)
	if err != nil || unknownMirror.Card != doc.Card {
		t.Fatalf("unknown mirror lost its current payload: %+v %v", unknownMirror, err)
	}

	if err := s.FinishMirrorAttempt(ctx, a.ID, "confirmed", "remote"); err != nil {
		t.Fatal(err)
	}
	settledAttempt, err := s.MirrorAttempt(ctx, a.ID)
	if err != nil || settledAttempt.Card != "" || settledAttempt.CardHash != a.CardHash || settledAttempt.State != "confirmed" {
		t.Fatalf("settled attempt retained its body or lost evidence: %+v %v", settledAttempt, err)
	}
	settledMirror, err := s.Mirror(ctx, "host", "thread", doc.Key)
	if err != nil || settledMirror.Card != "" || settledMirror.CardHash != cardHash(doc.Card) || settledMirror.RemoteID != "remote" {
		t.Fatalf("settled mirror retained its body or lost routing evidence: %+v %v", settledMirror, err)
	}
	stage(t, s, 101, doc)
	settledMirror, err = s.Mirror(ctx, "host", "thread", doc.Key)
	if err != nil || settledMirror.Card != "" || settledMirror.Revision != 1 || settledMirror.State != "confirmed" {
		t.Fatalf("hash-identical replay restored or resent the payload: %+v %v", settledMirror, err)
	}
}

func TestLegacyPayloadCompactionKeepsCallbackMetadataForDisconnectedUpdate(t *testing.T) {
	ctx := context.Background()
	s, _ := discoveryStore(t)
	eligible(t, s, "thread")
	bindMirrorThread(t, s)
	settled := Mirror{Key: "human", TurnID: "turn", Kind: "human", Card: `{"body":"legacy settled"}`, Terminal: true}
	callback := Mirror{Key: "control", TurnID: "control", Kind: "progress", Card: `{"schema":"2.0","body":{"elements":[{"tag":"markdown","content":"control"},{"tag":"button","behaviors":[{"type":"callback","value":{"action":"stop"}}]}]}}`, Terminal: true, ParentMessageID: "input"}
	stage(t, s, 100, settled, callback)
	var attemptIDs []string
	for n, key := range []string{settled.Key, callback.Key} {
		a := claimMirror(t, s, key, int64(1000+n))
		if err := s.FinishMirrorAttempt(ctx, a.ID, "confirmed", "remote-"+key); err != nil {
			t.Fatal(err)
		}
		attemptIDs = append(attemptIDs, a.ID)
		if _, err := s.DB.Exec(`UPDATE outbound_attempts SET card_json=?,card_hash='' WHERE attempt_id=?`, a.Card, a.ID); err != nil {
			t.Fatal(err)
		}
	}
	legacyEvents, legacyState := `[{"event_type":"fixture"}]`, `{"finished":true,"fixture":"legacy state"}`
	if _, err := s.DB.Exec(`UPDATE outbound_attempts SET cot_step='append',cot_events_json=?,cot_state_json=?,cot_payload_hash='' WHERE attempt_id=?`, legacyEvents, legacyState, attemptIDs[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`INSERT INTO cot_deliveries(host_id,thread_id,mirror_key,cot_id,state_json,completed) VALUES('host','thread',?,'legacy-cot',?,1)`, callback.Key, legacyState); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE mirrors SET card_json=?,card_hash='',projection_version=0 WHERE mirror_key=?`, settled.Card, settled.Key); err != nil {
		t.Fatal(err)
	}

	compacted, err := s.CompactDeliveryPayloads(ctx, "host", 8)
	if err != nil || compacted != 4 {
		t.Fatalf("legacy batch compaction: count=%d err=%v", compacted, err)
	}
	var attemptBodies int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM outbound_attempts WHERE card_json<>''`).Scan(&attemptBodies); err != nil || attemptBodies != 0 {
		t.Fatalf("settled attempt bodies remain: %d %v", attemptBodies, err)
	}
	var cotEvents, cotState, cotHash, deliveryState string
	if err := s.DB.QueryRow(`SELECT cot_events_json,cot_state_json,cot_payload_hash FROM outbound_attempts WHERE attempt_id=?`, attemptIDs[0]).Scan(&cotEvents, &cotState, &cotHash); err != nil || cotEvents != "[]" || cotState != "{}" || cotHash != cotPayloadHash(legacyEvents, legacyState) {
		t.Fatalf("settled CoT attempt payload remains or lost its hash: events=%q state=%q hash=%q err=%v", cotEvents, cotState, cotHash, err)
	}
	if err := s.DB.QueryRow(`SELECT state_json FROM cot_deliveries WHERE mirror_key=?`, callback.Key).Scan(&deliveryState); err != nil || deliveryState != "{}" {
		t.Fatalf("completed CoT delivery state remains: state=%q err=%v", deliveryState, err)
	}
	plain, err := s.Mirror(ctx, "host", "thread", settled.Key)
	if err != nil || plain.Card != "" || plain.CardHash != cardHash(settled.Card) {
		t.Fatalf("legacy settled mirror was not compacted: %+v %v", plain, err)
	}
	actionable, err := s.Mirror(ctx, "host", "thread", callback.Key)
	if err != nil || actionable.Card != "" || actionable.CardHash != cardHash(callback.Card) || !actionable.HasCallback {
		t.Fatalf("actionable callback metadata was not compacted safely: %+v %v", actionable, err)
	}
	if err := s.StageDisconnectedCards(ctx, SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}, "disconnected"); err != nil {
		t.Fatal(err)
	}
	actionable, err = s.Mirror(ctx, "host", "thread", callback.Key)
	if err != nil || actionable.Card == "" || actionable.State != "ready" || actionable.HasCallback {
		t.Fatalf("compacted callback card was not replaced on disconnect: %+v %v", actionable, err)
	}
}

func TestConfirmedNonterminalMirrorCompactsAndCanTransitionOffline(t *testing.T) {
	ctx := context.Background()
	s, _ := discoveryStore(t)
	eligible(t, s, "thread")
	bindMirrorThread(t, s)
	m := Mirror{Key: "progress", TurnID: "turn", Kind: "progress", Card: `{"body":"running"}`, ParentMessageID: "input"}
	stage(t, s, 100, m)
	a := claimMirror(t, s, m.Key, 1000)
	if err := s.FinishMirrorAttempt(ctx, a.ID, "confirmed", "remote"); err != nil {
		t.Fatal(err)
	}
	settled, err := s.Mirror(ctx, "host", "thread", m.Key)
	if err != nil || settled.Card != "" || settled.CardHash != cardHash(m.Card) || settled.Terminal {
		t.Fatalf("confirmed progress body was retained: %+v %v", settled, err)
	}
	if err := s.StageDisconnectedCards(ctx, SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}, "disconnected"); err != nil {
		t.Fatal(err)
	}
	offline, err := s.Mirror(ctx, "host", "thread", m.Key)
	if err != nil || offline.Card == "" || offline.State != "ready" || offline.Revision != settled.Revision+1 {
		t.Fatalf("compacted progress could not transition offline: %+v %v", offline, err)
	}
}

func TestLegacyPayloadCompactionSkipsIncompleteCOTWithoutStarvingLaterRows(t *testing.T) {
	ctx := context.Background()
	s, _ := discoveryStore(t)
	eligible(t, s, "thread")
	bindMirrorThread(t, s)
	cotMirror := cotTestMirror(t, cotTestDocument("completed", true), true)
	plain := Mirror{Key: "plain", TurnID: "plain-turn", Kind: "human", Card: `{"body":"later settled"}`, Terminal: true}
	stage(t, s, 100, cotMirror)
	stage(t, s, 101, plain)
	if _, err := s.DB.Exec(`UPDATE mirrors SET remote_id='remote',delivered_revision=revision; UPDATE outbound SET state='confirmed'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`INSERT INTO cot_deliveries(host_id,thread_id,mirror_key,cot_id) VALUES('host','thread',?,'cot')`, cotMirror.Key); err != nil {
		t.Fatal(err)
	}

	compacted, err := s.CompactDeliveryPayloads(ctx, "host", 1)
	if err != nil || compacted != 1 {
		t.Fatalf("later eligible row was starved: count=%d err=%v", compacted, err)
	}
	incomplete, err := s.Mirror(ctx, "host", "thread", cotMirror.Key)
	if err != nil || incomplete.Card != cotMirror.Card {
		t.Fatalf("incomplete CoT payload was compacted: %+v %v", incomplete, err)
	}
	settled, err := s.Mirror(ctx, "host", "thread", plain.Key)
	if err != nil || settled.Card != "" || settled.CardHash != cardHash(plain.Card) {
		t.Fatalf("later settled payload was not compacted: %+v %v", settled, err)
	}
}
