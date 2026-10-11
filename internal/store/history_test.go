package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestHistoryLinksAndMirrorsCommitWithCheckpointAtomically(t *testing.T) {
	ctx := context.Background()
	s, _ := discoveryStore(t)
	eligible(t, s, "thread")
	r, _, err := s.Claim(ctx, Receipt{AppID: "app", ThreadID: "thread", MessageID: "original", ChatID: "chat", Body: "input"})
	if err != nil {
		t.Fatal(err)
	}
	link := InputLink{r.ID, "turn", "native-item"}
	doc := mirrorDoc("final", "final", "answer")
	doc.ParentKey, doc.ParentMessageID = "", r.MessageID
	if err := s.StageSnapshot(ctx, "host", "thread", []Mirror{doc}, []InputLink{link}, Watermark{ObservedAtMS: 100}); err != nil {
		t.Fatal(err)
	}
	link.ItemID = "conflicting-native-item"
	doc.Card = `{"body":"changed"}`
	if err := s.StageSnapshot(ctx, "host", "thread", []Mirror{doc}, []InputLink{link}, Watermark{ObservedAtMS: 200}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate logical input accepted: %v", err)
	}
	m, _ := s.Mirror(ctx, "host", "thread", "final")
	w, _ := s.Watermark(ctx, "host", "thread")
	links, err := s.InputLinks(ctx, "host", "thread")
	if err != nil || len(links) != 1 || links[0].ItemID != "native-item" || w.ObservedAtMS != 100 || m.Revision != 1 {
		t.Fatalf("link conflict partially committed: %+v %+v %+v %v", links, w, m, err)
	}
}

func TestHistoryLinksCannotUseOtherDeploymentOrThreadReceipts(t *testing.T) {
	ctx := context.Background()
	for _, other := range []Receipt{
		{AppID: "other-app", ThreadID: "thread", MessageID: "message"},
		{AppID: "app", ThreadID: "other-thread", MessageID: "message"},
	} {
		s, _ := discoveryStore(t)
		eligible(t, s, "thread")
		r, _, err := s.Claim(ctx, other)
		if err != nil {
			t.Fatal(err)
		}
		owned, err := s.ReceiptsForDeployment(ctx, "host", "thread")
		if err != nil || len(owned) != 0 {
			t.Fatalf("foreign receipt exposed to sync: %+v %v", owned, err)
		}
		if err := s.StageSnapshot(ctx, "host", "thread", []Mirror{mirrorDoc("human", "human", "question")}, []InputLink{{r.ID, "turn", "item"}}, Watermark{ObservedAtMS: 100}); !errors.Is(err, ErrConflict) {
			t.Fatalf("foreign receipt linked: %v", err)
		}
		if _, err := s.Watermark(ctx, "host", "thread"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("foreign receipt advanced scan: %v", err)
		}
	}
}

func TestRetiredUnknownSegmentMustReconcileBeforeClearingOriginalCard(t *testing.T) {
	ctx := context.Background()
	s, _ := discoveryStore(t)
	eligible(t, s, "thread")
	bindMirrorThread(t, s)
	doc := mirrorDoc("tail", "final", "old tail")
	doc.ParentKey, doc.ParentMessageID, doc.Terminal = "", "input", true
	stage(t, s, 100, doc)
	a := claimMirror(t, s, doc.Key, 1000)
	if err := s.FinishMirrorAttempt(ctx, a.ID, "unknown", ""); err != nil {
		t.Fatal(err)
	}
	doc.Retired, doc.Card = true, `{"body":"replacement notice"}`
	stage(t, s, 101, doc)
	if _, claimed, err := s.ClaimMirror(ctx, "host", "thread", doc.Key, 2, 2000); err != nil || claimed {
		t.Fatalf("retirement bypassed unknown: %v %v", claimed, err)
	}
	if err := s.FinishMirrorAttempt(ctx, a.ID, "confirmed", "old-card"); err != nil {
		t.Fatal(err)
	}
	update := claimMirror(t, s, doc.Key, 2001)
	if update.Operation != "update" || update.TargetMessageID != "old-card" || update.Card != doc.Card {
		t.Fatalf("retirement reposted or lost correction: %+v", update)
	}
}

func TestVersionThreeMigrationPreservesInflightMirrorJournal(t *testing.T) {
	ctx := context.Background()
	s, path := discoveryStore(t)
	eligible(t, s, "thread")
	bindMirrorThread(t, s)
	stage(t, s, 100, mirrorDoc("human", "human", "old-version-public-card"))
	a := claimMirror(t, s, "human", 1000)
	// Remove only the v4 extensions to reproduce an actual v3 outbox database.
	if _, err := s.DB.Exec(`DROP TABLE directory_rules; DROP TABLE submission_attempts; DROP TABLE submission_actions; DROP TABLE submission_inputs; ALTER TABLE mirrors DROP COLUMN retired; ALTER TABLE mirrors DROP COLUMN event_type; ALTER TABLE mirrors DROP COLUMN event_at; DROP TABLE input_history_links; DROP TABLE notification_rules; PRAGMA user_version=3;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.RecoverMirrorDispatches(ctx, "host"); err != nil {
		t.Fatal(err)
	}
	m, err := reopened.Mirror(ctx, "host", "thread", "human")
	if err != nil || m.State != "unknown" || m.Card != a.Card || m.Retired || m.EventAt != 0 {
		t.Fatalf("migration lost public state: %+v %v", m, err)
	}
	frozen, err := reopened.MirrorAttempt(ctx, a.ID)
	if err != nil || frozen.State != "unknown" || frozen.Card != a.Card || frozen.ID != a.ID {
		t.Fatalf("migration altered dispatch identity: %+v %v", frozen, err)
	}
}
