package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

func discoveryStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "discovery.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func eligible(t *testing.T, s *Store, thread string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.BeginBaseline(ctx, "host", "app", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveNew(ctx, "host", thread, "/project", 101); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkEligible(ctx, "host", thread, "first-input", "/project", "/project"); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationPreservesM0AndNeverDowngrades(t *testing.T) {
	ctx := context.Background()
	s, path := discoveryStore(t)
	if fresh, err := s.Begin(ctx, "lost-response", "queue.add"); err != nil || !fresh {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`DROP TABLE mentions; DROP TABLE mention_events; DROP TABLE notification_policy_history; DROP TABLE directory_rules; DROP TABLE submission_attempts; DROP TABLE submission_actions; DROP TABLE submission_inputs; DROP TABLE notification_rules; DROP TABLE input_history_links; DROP TABLE outbound_attempts; DROP TABLE outbound; DROP TABLE mirrors; DROP TABLE history_watermarks; DROP TABLE bindings; DROP TABLE create_intents; DROP TABLE thread_observations; DROP TABLE deployment; PRAGMA user_version=1;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state, _, err := s.Operation(ctx, "lost-response")
	if err != nil || state != "sending" {
		t.Fatalf("journal lost: %s %v", state, err)
	}
	var version int
	if err = s.DB.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("version %d: %v", version, err)
	}
	if _, err = s.DB.Exec("PRAGMA user_version=99"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if future, err := Open(path); err == nil {
		future.Close()
		t.Fatal("future schema opened")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 99 {
		t.Fatalf("future schema downgraded: %d %v", version, err)
	}
}

func TestBaselineIsAtomicPersistentAndDoesNotHideLiveBirths(t *testing.T) {
	ctx := context.Background()
	s, path := discoveryStore(t)
	if needed, err := s.BeginBaseline(ctx, "host", "app", 100); err != nil || !needed {
		t.Fatalf("baseline: %v %v", needed, err)
	}
	if err := s.ObserveNew(ctx, "host", "live-birth", "/project", 101); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteBaseline(ctx, "host", []string{"old", ""}); err == nil {
		t.Fatal("invalid partial baseline accepted")
	}
	if _, err := s.Observation(ctx, "host", "old"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("snapshot partially committed: %v", err)
	}
	if err := s.CompleteBaseline(ctx, "host", []string{"old", "live-birth"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveNew(ctx, "host", "old", "/project", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkEligible(ctx, "host", "old", "new-turn-input", "/project", "/project"); !errors.Is(err, ErrIneligible) {
		t.Fatalf("history promoted: %v", err)
	}
	o, err := s.Observation(ctx, "host", "live-birth")
	if err != nil || o.Origin != "new" || o.State != "pending" {
		t.Fatalf("live birth ignored: %+v %v", o, err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if needed, err := s.BeginBaseline(ctx, "host", "app", 200); err != nil || needed {
		t.Fatalf("baseline restarted: %v %v", needed, err)
	}
	if _, err = s.BeginBaseline(ctx, "host", "other-app", 200); !errors.Is(err, ErrConflict) {
		t.Fatalf("deployment reassigned: %v", err)
	}
	if err = s.CompleteBaseline(ctx, "host", []string{"live-birth"}); err != nil {
		t.Fatal(err)
	}
	o, err = s.Observation(ctx, "host", "live-birth")
	if err != nil || o.State != "pending" {
		t.Fatalf("pending draft lost: %+v %v", o, err)
	}
}

func TestGroupIntentAndDispatchAreUniqueUnderReplay(t *testing.T) {
	ctx := context.Background()
	s, _ := discoveryStore(t)
	eligible(t, s, "thread")
	want, err := s.PrepareGroup(ctx, "host", "thread", "owner", "original-name", []string{"member"})
	if err != nil {
		t.Fatal(err)
	}
	var claims atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.PrepareGroup(ctx, "host", "thread", "other-owner", "changed-name", nil)
			if err != nil {
				t.Error(err)
				return
			}
			// The dispatch state may advance concurrently; persisted request
			// parameters and UUID must remain identical.
			got.State = want.State
			if !reflect.DeepEqual(got, want) {
				t.Errorf("parameters/UUID changed: %+v", got)
			}
			claimed, err := s.ClaimGroupDispatch(ctx, got.ID)
			if err != nil {
				t.Error(err)
			}
			if claimed {
				claims.Add(1)
			}
		}()
	}
	wg.Wait()
	if claims.Load() != 1 {
		t.Fatalf("remote dispatch claims: %d", claims.Load())
	}
}

func TestCrashUnknownDoesNotCreateAgainAndConfirmedBindingRecovers(t *testing.T) {
	ctx := context.Background()
	s, path := discoveryStore(t)
	eligible(t, s, "thread")
	i, err := s.PrepareGroup(ctx, "host", "thread", "owner", "name", nil)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.ClaimGroupDispatch(ctx, i.ID); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.RecoverGroupDispatches(ctx, "host"); err != nil {
		t.Fatal(err)
	}
	i, err = s.PrepareGroup(ctx, "host", "thread", "owner", "name", nil)
	if err != nil || i.State != "unknown" {
		t.Fatalf("lost dispatch not unknown: %+v %v", i, err)
	}
	if claimed, err := s.ClaimGroupDispatch(ctx, i.ID); err != nil || claimed {
		t.Fatalf("unknown redispatched: %v %v", claimed, err)
	}
	if err = s.ConfirmGroup(ctx, i.ID, "chat"); err != nil {
		t.Fatal(err)
	}
	// Simulate success persisted before binding was saved, then recover locally.
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.BindConfirmedGroup(ctx, i.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.BindConfirmedGroup(ctx, i.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmGroup(ctx, i.ID, "different-chat"); !errors.Is(err, ErrConflict) {
		t.Fatalf("remote ID replaced: %v", err)
	}
	i, err = s.PrepareGroup(ctx, "host", "thread", "owner", "new-name", nil)
	if err != nil || i.ChatID != "chat" || i.State != "confirmed" {
		t.Fatalf("confirmed intent lost: %+v %v", i, err)
	}
}

func TestBindingConflictPreservesRemoteResultAndExcludedThreadsStayExcluded(t *testing.T) {
	ctx := context.Background()
	s, _ := discoveryStore(t)
	for _, thread := range []string{"one", "two"} {
		eligible(t, s, thread)
		i, err := s.PrepareGroup(ctx, "host", thread, "owner", "name", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.ClaimGroupDispatch(ctx, i.ID); err != nil {
			t.Fatal(err)
		}
		if err = s.ConfirmGroup(ctx, i.ID, "same-chat"); err != nil {
			t.Fatal(err)
		}
		err = s.BindConfirmedGroup(ctx, i.ID)
		if thread == "one" && err != nil {
			t.Fatal(err)
		}
		if thread == "two" && !errors.Is(err, ErrConflict) {
			t.Fatalf("target reused: %v", err)
		}
		got, err := s.PrepareGroup(ctx, "host", thread, "owner", "name", nil)
		if err != nil || got.State != "confirmed" || got.ChatID != "same-chat" {
			t.Fatalf("creation discarded after binding failure: %+v %v", got, err)
		}
	}
	eligible(t, s, "muted")
	if err := s.Exclude(ctx, "host", "muted", "directory_muted"); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveNew(ctx, "host", "muted", "/project", 300); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkEligible(ctx, "host", "muted", "later-input", "/project", "/project"); !errors.Is(err, ErrIneligible) {
		t.Fatalf("muted history reactivated: %v", err)
	}
	if _, err := s.PrepareGroup(ctx, "host", "muted", "owner", "name", nil); !errors.Is(err, ErrIneligible) {
		t.Fatalf("excluded group prepared: %v", err)
	}
}
