package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"codex-feishu/internal/cards"
)

// Compare visible wire documents while keeping frozen payload assertions exact.
func unmarkedWireCard(t *testing.T, s *Store, id, wire string) string {
	t.Helper()
	marker, err := s.CardRequestMarker(context.Background(), id)
	if errors.Is(err, sql.ErrNoRows) {
		return wire
	}
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal([]byte(wire), &doc); err != nil {
		t.Fatal(err)
	}
	var strip func(any)
	strip = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if x["element_id"] == marker {
				delete(x, "element_id")
			}
			for _, child := range x {
				strip(child)
			}
		case []any:
			for _, child := range x {
				strip(child)
			}
		}
	}
	strip(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestReminderReadbackReservationAndCrashRecoveryKeepOnlyFrozenEvidence(t *testing.T) {
	ctx := context.Background()
	s, _, o, _, at := retryRequestFixture(t)
	if _, err := s.DB.Exec(`CREATE TRIGGER ignore_reminder_marker BEFORE INSERT ON card_request_markers WHEN NEW.kind='reminder' BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ClaimRequestReminder(ctx, "host", "app", "thread", at.UnixMilli(), o); ok || !errors.Is(err, ErrConflict) {
		t.Fatal("unrecorded reminder marker dispatched", ok, err)
	}
	if d := requestDecision(t, s); d.State != "eligible" || d.AttemptID != "" {
		t.Fatal(d)
	}
	if _, err := s.DB.Exec(`DROP TRIGGER ignore_reminder_marker`); err != nil {
		t.Fatal(err)
	}
	a := claimRequest(t, s, o, at)
	if err := s.RecoverRequestReminders(ctx, "host"); err != nil {
		t.Fatal(err)
	}
	r, ok, err := s.ClaimCardReadback(ctx, "reminder", "host", "app", "thread", at.UnixMilli(), 30000)
	if err != nil || !ok || r.ID != a.ID || r.Card != a.Card || r.ParentMessageID != a.ParentMessageID {
		t.Fatal(r, ok, err)
	}
	if _, ok, err := s.ClaimCardReadback(ctx, "reminder", "host", "app", "other-thread", at.Add(time.Minute).UnixMilli(), 30000); err != nil || ok {
		t.Fatal("foreign thread proof", ok, err)
	}
	if err := s.FinishRequestReminder(ctx, a.ID, "confirmed", "readback-card"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ClaimCardReadback(ctx, "reminder", "host", "app", "thread", at.Add(time.Minute).UnixMilli(), 30000); err != nil || ok {
		t.Fatal("confirmed request still readable", ok, err)
	}
}

func TestCommandReadbackCooldownAndMarkerSurviveSchema16Migration(t *testing.T) {
	ctx := context.Background()
	s, req, at := commandStoreRequest(t)
	if _, _, err := s.ApplyLocalCommand(ctx, req, at); err != nil {
		t.Fatal(err)
	}
	a := retryCommandClaim(t, s, at.UnixMilli())
	if err := s.FinishCommandReply(ctx, a.ID, "unknown", ""); err != nil {
		t.Fatal(err)
	}
	r, ok, err := s.ClaimCardReadback(ctx, "command", "host", "app", "", at.UnixMilli(), 30000)
	if err != nil || !ok || r.ID != a.ID || r.ParentMessageID != req.MessageID {
		t.Fatal(r, ok, err)
	}
	if _, ok, err := s.ClaimCardReadback(ctx, "command", "host", "app", "", at.Add(29*time.Second).UnixMilli(), 30000); err != nil || ok {
		t.Fatal("readback before deadline", ok, err)
	}
	if _, ok, err := s.ClaimCardReadback(ctx, "command", "foreign-host", "app", "", at.Add(time.Minute).UnixMilli(), 30000); err == nil || ok {
		t.Fatal("foreign deployment readback", ok, err)
	}
	h, err := s.Health(ctx, SchemaVersion, "host", "app")
	if err != nil || h.CommandReplies["readback_available"] != 1 {
		t.Fatal(h, err)
	}
	// Recreate actual schema16: mirror marker is authoritative there, while
	// old command/reminder payloads have no registry proof and must stay so.
	eligible(t, s, "thread")
	m := Mirror{Key: "human", TurnID: "turn", Kind: "human", Card: cards.Message("人类", "Codex", "public")}
	stage(t, s, at.UnixMilli(), m)
	bindMirrorThread(t, s)
	ma := claimMirror(t, s, "human", at.UnixMilli())
	if err := s.FinishMirrorAttempt(ctx, ma.ID, "unknown", ""); err != nil {
		t.Fatal(err)
	}
	marker, err := s.MirrorRequestMarker(ctx, ma.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`DROP TABLE card_request_markers; ALTER TABLE mirrors DROP COLUMN progress_observed_at_json; PRAGMA user_version=16`); err != nil {
		t.Fatal(err)
	}
	var seq int
	var name, path string
	if err := s.DB.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if saved, err := s.CardRequestMarker(ctx, ma.ID); err != nil || saved != marker {
		t.Fatal("schema16 mirror marker lost", saved, err)
	}
	if _, err := s.CardRequestMarker(ctx, a.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("old command marker invented", err)
	}
}

func TestCardMarkerRequiresExactSourceAndIgnoredReservationRollsBackCommand(t *testing.T) {
	ctx := context.Background()
	s, req, at := commandStoreRequest(t)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := freezeCardRequestMarker(ctx, tx, "command", "missing-source", cards.Message("操作", "飞书", "public")); !errors.Is(err, ErrConflict) {
		t.Fatal("missing source reserved", err)
	}
	tx.Rollback()
	if _, _, err := s.ApplyLocalCommand(ctx, req, at); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`CREATE TRIGGER ignore_marker BEFORE INSERT ON card_request_markers BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ClaimCommandReply(ctx, "host", at.UnixMilli()); ok || !errors.Is(err, ErrConflict) {
		t.Fatal("ignored reservation accepted", ok, err)
	}
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM command_reply_attempts`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial command claim", count, err)
	}
	r, err := s.CommandReply(ctx, "host", "app", req.MessageID)
	if err != nil || r.State != "ready" {
		t.Fatal(r, err)
	}
}
