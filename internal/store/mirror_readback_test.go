package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"
	"testing"

	"codex-feishu/internal/cards"
)

func TestMirrorMarkerIsFrozenWithExactAttemptAndReadbackCooldownSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	s, path := discoveryStore(t)
	eligible(t, s, "thread")
	bindMirrorThread(t, s)
	m := Mirror{Key: "human", TurnID: "turn", Kind: "human", Card: cards.Message("人类发送", "Codex", "public input")}
	stage(t, s, 100, m)
	a := claimMirror(t, s, "human", 1000)
	marker, err := s.MirrorRequestMarker(ctx, a.ID)
	if err != nil || len(marker) != 20 || !strings.Contains(a.Card, marker) || strings.Contains(m.Card, marker) {
		t.Fatal(marker, err, a)
	}
	if a.Card != expectedMarkedCard(t, s, a.ID, m.Card) {
		t.Fatal("public document not preserved")
	}
	if err := s.FinishMirrorAttempt(ctx, a.ID, "unknown", ""); err != nil {
		t.Fatal(err)
	}
	h, err := s.Health(ctx, SchemaVersion, "host", "app")
	if err != nil || h.Outbound["readback_available"] != 1 {
		t.Fatal(h, err)
	}
	r, ok, err := s.ClaimMirrorReadback(ctx, "host", "thread", 1001, 30000)
	if err != nil || !ok || r.Attempt.ID != a.ID || r.AppID != "app" || r.Marker != marker {
		t.Fatal(r, ok, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, ok, err := s.ClaimMirrorReadback(ctx, "host", "thread", 31000, 30000); err != nil || ok {
		t.Fatal("early check after restart", ok, err)
	}
	if _, ok, err := s.ClaimMirrorReadback(ctx, "host", "thread", 31001, 30000); err != nil || !ok {
		t.Fatal("persisted deadline lost", ok, err)
	}
	if _, ok, err := s.ClaimMirrorReadback(ctx, "other-host", "thread", 61001, 30000); err != nil || ok {
		t.Fatal("foreign host evidence", ok, err)
	}
	if _, ok, err := s.ClaimMirrorReadback(ctx, "host", "other-thread", 61001, 30000); err != nil || ok {
		t.Fatal("foreign thread evidence", ok, err)
	}
	if _, ok, err := s.ClaimMirrorReadback(ctx, "host", "thread", math.MaxInt64, 30000); !errors.Is(err, ErrConflict) || ok {
		t.Fatal("overflow", ok, err)
	}
}

func TestMirrorIgnoredMarkerWriteRollsBackClaimAndGlobalMarkerCannotBeReused(t *testing.T) {
	ctx := context.Background()
	s, options, _, at := mirrorProofFixture(t)
	if _, err := s.DB.Exec(`CREATE TRIGGER ignore_marker BEFORE INSERT ON outbound_request_markers BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ClaimMirrorWithMentions(ctx, "host", "thread", "final", 1, at.UnixMilli(), options); ok || !errors.Is(err, ErrConflict) {
		t.Fatal("ignored marker accepted", ok, err)
	}
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM outbound_attempts`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial attempt retained", count, err)
	}
	if d := mentionDecision(t, s); d.State != "eligible" || d.AttemptID != "" {
		t.Fatal(d)
	}
	if _, err := s.DB.Exec(`DROP TRIGGER ignore_marker`); err != nil {
		t.Fatal(err)
	}
	a := mentionClaim(t, s, options, at)
	marker, err := s.MirrorRequestMarker(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`INSERT INTO outbound_attempts(attempt_id,host_id,thread_id,mirror_key,revision,operation,chat_id,parent_message_id,target_message_id,card_json,created_at_ms,state) SELECT 'other-attempt',host_id,thread_id,mirror_key,revision,operation,chat_id,parent_message_id,target_message_id,card_json,created_at_ms,'rejected' FROM outbound_attempts WHERE attempt_id=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`INSERT INTO outbound_request_markers(marker,attempt_id) VALUES(?,?)`, marker, "other-attempt"); err == nil {
		t.Fatal("global marker reused by another attempt")
	}
}

func TestMirrorSchema15UpgradeDoesNotInventMarkersForFrozenUnknown(t *testing.T) {
	ctx := context.Background()
	s, path := discoveryStore(t)
	eligible(t, s, "thread")
	bindMirrorThread(t, s)
	stage(t, s, 100, mirrorDoc("human", "human", "legacy frozen document"))
	a := claimMirror(t, s, "human", 1000)
	if _, err := s.DB.Exec(`DROP TABLE outbound_request_markers; PRAGMA user_version=15`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.RecoverMirrorDispatches(ctx, "host"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MirrorRequestMarker(ctx, a.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("legacy proof invented", err)
	}
	if _, ok, err := s.ClaimMirrorReadback(ctx, "host", "thread", 1001, 30000); err != nil || ok {
		t.Fatal("legacy marker guessed", ok, err)
	}
	frozen, err := s.MirrorAttempt(ctx, a.ID)
	if err != nil || frozen.State != "unknown" || frozen.Card != a.Card {
		t.Fatal(frozen, err)
	}
}
