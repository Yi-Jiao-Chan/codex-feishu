package store

import (
	"codex-feishu/internal/cards"
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"codex-feishu/internal/rules"
)

func mirrorProofFixture(t *testing.T) (*Store, MentionOptions, Mirror, time.Time) {
	t.Helper()
	p, _ := rules.DefaultPolicy("UTC")
	at := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	s, _, options, m := mentionFixture(t, p, at)
	if err := s.ScheduleMentions(context.Background(), "host", "thread", options); err != nil {
		t.Fatal(err)
	}
	bindMirrorThread(t, s)
	return s, options, m, at
}

func installIgnoredMirrorProof(t *testing.T, s *Store, statement string) {
	t.Helper()
	if _, err := s.DB.Exec(statement); err != nil {
		t.Fatal(err)
	}
}

func TestMirrorClaimRequiresEveryDurableProofWrite(t *testing.T) {
	for _, statement := range []string{
		`CREATE TRIGGER ignore_proof BEFORE INSERT ON outbound_attempts BEGIN SELECT RAISE(IGNORE); END`,
		`CREATE TRIGGER ignore_proof BEFORE UPDATE ON outbound WHEN NEW.state='dispatching' BEGIN SELECT RAISE(IGNORE); END`,
		`CREATE TRIGGER ignore_proof BEFORE UPDATE ON mentions WHEN NEW.state='dispatching' BEGIN SELECT RAISE(IGNORE); END`,
	} {
		t.Run(statement, func(t *testing.T) {
			ctx := context.Background()
			s, options, _, at := mirrorProofFixture(t)
			installIgnoredMirrorProof(t, s, statement)
			if _, ok, err := s.ClaimMirrorWithMentions(ctx, "host", "thread", "final", 1, at.UnixMilli(), options); ok || !errors.Is(err, ErrConflict) {
				t.Fatal("ignored proof accepted", ok, err)
			}
			var count int
			if err := s.DB.QueryRow(`SELECT COUNT(*) FROM outbound_attempts`).Scan(&count); err != nil || count != 0 {
				t.Fatal("partial claim retained", count, err)
			}
			m, err := s.Mirror(ctx, "host", "thread", "final")
			if err != nil || m.State != "ready" {
				t.Fatal(m, err)
			}
			if d := mentionDecision(t, s); d.State != "eligible" || d.AttemptID != "" {
				t.Fatal(d)
			}
		})
	}
}

func TestMirrorConfirmationRequiresEveryProofAndPreservesUnknownPayload(t *testing.T) {
	for _, statement := range []string{
		`CREATE TRIGGER ignore_proof BEFORE UPDATE ON mirrors BEGIN SELECT RAISE(IGNORE); END`,
		`CREATE TRIGGER ignore_proof BEFORE UPDATE ON outbound_attempts BEGIN SELECT RAISE(IGNORE); END`,
		`CREATE TRIGGER ignore_proof BEFORE UPDATE ON outbound BEGIN SELECT RAISE(IGNORE); END`,
		`CREATE TRIGGER ignore_proof BEFORE UPDATE ON mentions BEGIN SELECT RAISE(IGNORE); END`,
	} {
		t.Run(statement, func(t *testing.T) {
			ctx := context.Background()
			s, options, m, at := mirrorProofFixture(t)
			a := mentionClaim(t, s, options, at)
			m.Card = `{"body":"newer public revision"}`
			stage(t, s, at.UnixMilli()+2000, m)
			installIgnoredMirrorProof(t, s, statement)
			if err := s.FinishMirrorAttempt(ctx, a.ID, "confirmed", "remote"); !errors.Is(err, ErrConflict) {
				t.Fatal("ignored confirmation accepted", err)
			}
			frozen, err := s.MirrorAttempt(ctx, a.ID)
			if err != nil || frozen != a {
				t.Fatal("failed proof discarded exact journal", frozen, err)
			}
			current, err := s.Mirror(ctx, "host", "thread", "final")
			if err != nil || current.State != "dispatching" || current.RemoteID != "" || current.DeliveredRevision != 0 || current.Revision != 2 {
				t.Fatal(current, err)
			}
			if d := mentionDecision(t, s); d.State != "dispatching" || d.AttemptID != a.ID {
				t.Fatal(d)
			}
			if _, err := s.DB.Exec(`DROP TRIGGER ignore_proof`); err != nil {
				t.Fatal(err)
			}
			if err := s.RecoverMirrorDispatches(ctx, "host"); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := s.ClaimMirror(ctx, "host", "thread", "final", 2, at.Add(time.Hour).UnixMilli()); err != nil || ok {
				t.Fatal("unknown delivery retried", ok, err)
			}
			// An authoritative successful response/readback can still confirm A.
			if err := s.FinishMirrorAttempt(ctx, a.ID, "confirmed", "remote"); err != nil {
				t.Fatal(err)
			}
			current, err = s.Mirror(ctx, "host", "thread", "final")
			if err != nil || current.State != "ready" || current.DeliveredRevision != 1 || current.RemoteID != "remote" {
				t.Fatal(current, err)
			}
		})
	}
}

func TestMirrorDeferralRequiresEveryProofWithoutPartiallyReopeningRecipients(t *testing.T) {
	for _, statement := range []string{
		`CREATE TRIGGER ignore_proof BEFORE INSERT ON outbound_throttles BEGIN SELECT RAISE(IGNORE); END`,
		`CREATE TRIGGER ignore_proof BEFORE UPDATE ON outbound_attempts BEGIN SELECT RAISE(IGNORE); END`,
		`CREATE TRIGGER ignore_proof BEFORE UPDATE ON outbound BEGIN SELECT RAISE(IGNORE); END`,
		`CREATE TRIGGER ignore_proof BEFORE UPDATE ON mentions BEGIN SELECT RAISE(IGNORE); END`,
	} {
		t.Run(statement, func(t *testing.T) {
			ctx := context.Background()
			s, options, _, at := mirrorProofFixture(t)
			a := mentionClaim(t, s, options, at)
			installIgnoredMirrorProof(t, s, statement)
			if err := s.DeferMirrorAttempt(ctx, a.ID, at.UnixMilli(), 1000); !errors.Is(err, ErrConflict) {
				t.Fatal("ignored deferral accepted", err)
			}
			frozen, err := s.MirrorAttempt(ctx, a.ID)
			if err != nil || frozen != a {
				t.Fatal(frozen, err)
			}
			var count int
			if err := s.DB.QueryRow(`SELECT COUNT(*) FROM outbound_throttles`).Scan(&count); err != nil || count != 0 {
				t.Fatal("partial throttle committed", count, err)
			}
			if d := mentionDecision(t, s); d.State != "dispatching" || d.AttemptID != a.ID {
				t.Fatal(d)
			}
		})
	}
}

func expectedMarkedCard(t *testing.T, s *Store, id, base string) string {
	t.Helper()
	marker, err := s.MirrorRequestMarker(context.Background(), id)
	if errors.Is(err, sql.ErrNoRows) {
		return base
	}
	if err != nil {
		t.Fatal(err)
	}
	marked, ok, err := cards.WithRequestMarker(base, marker)
	if err != nil || !ok {
		t.Fatal("expected marked body", ok, err)
	}
	return marked
}
