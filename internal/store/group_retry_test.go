package store

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
)

func groupRetryIntent(t *testing.T) (*Store, string, GroupIntent) {
	t.Helper()
	s, path := discoveryStore(t)
	eligible(t, s, "thread")
	i, err := s.PrepareGroup(context.Background(), "host", "thread", "owner", "frozen name", []string{"owner", "extra"})
	if err != nil {
		t.Fatal(err)
	}
	return s, path, i
}
func groupRetryClaim(t *testing.T, s *Store, i GroupIntent, at int64) GroupAttempt {
	t.Helper()
	a, claimed, err := s.ClaimGroupAttempt(context.Background(), i.ID, at)
	if err != nil || !claimed {
		t.Fatal("not claimed", err)
	}
	return a
}

func TestGroupRetryDurableExactAttemptsAndFrozenIntent(t *testing.T) {
	ctx := context.Background()
	s, path, i := groupRetryIntent(t)
	a := groupRetryClaim(t, s, i, 1000)
	invalid := a
	invalid.ID = ""
	if err := s.FinishGroupDispatch(ctx, invalid, "confirmed", "wrong-chat"); !errors.Is(err, ErrConflict) {
		t.Fatal("exact outcome accepted missing attempt ID", err)
	}
	if err := s.DeferGroupAttempt(ctx, a, 2000, 7000); err != nil {
		t.Fatal(err)
	}
	if err := s.DeferGroupAttempt(ctx, a, 9999, 99000); err != nil {
		t.Fatal(err)
	}
	var due, count int64
	if err := s.DB.QueryRow("SELECT next_retry_at_ms,rejection_count FROM group_throttles WHERE intent_id=?", i.ID).Scan(&due, &count); err != nil || due != 9000 || count != 1 {
		t.Fatal("replay changed deferral", due, count, err)
	}
	h, err := s.Health(ctx, SchemaVersion, "host", "app")
	if err != nil || h.GroupIntents["deferred"] != 1 || h.GroupAttempts["rejected"] != 1 {
		t.Fatal("waiting lost in health", err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.RecoverGroupDispatches(ctx, "host"); err != nil {
		t.Fatal(err)
	}
	ready, err := s.GroupRetryReady(ctx, i.ID, 8999)
	if err != nil || ready {
		t.Fatal("restart ignored rate floor", err)
	}
	if _, claimed, err := s.ClaimGroupAttempt(ctx, i.ID, 8999); err != nil || claimed {
		t.Fatal("early retry dispatched", err)
	}
	same, err := s.PrepareGroup(ctx, "host", "thread", "different owner", "different title", nil)
	if err != nil || same.ID != i.ID || same.OwnerID != i.OwnerID || same.Name != i.Name || len(same.Members) != 2 {
		t.Fatal("retry changed logical group", err)
	}
	b := groupRetryClaim(t, s, i, 9000)
	if b.ID == a.ID {
		t.Fatal("rejected attempt overwritten")
	}
	for _, state := range []string{"unknown", "rejected", "confirmed"} {
		chat := ""
		if state == "confirmed" {
			chat = "stale-chat"
		}
		if err := s.FinishGroupDispatch(ctx, a, state, chat); !errors.Is(err, ErrConflict) {
			t.Fatal("stale attempt finished retry", state, err)
		}
	}
	if err := s.ConfirmGroup(ctx, i.ID, "stale-chat"); !errors.Is(err, ErrConflict) {
		t.Fatal("old intent-only confirmation replaced retry", err)
	}
	if err := s.FinishGroupAttempt(ctx, i.ID, "unknown"); !errors.Is(err, ErrConflict) {
		t.Fatal("old intent-only failure replaced retry", err)
	}
	if err := s.FinishGroupDispatch(ctx, b, "confirmed", "correct-chat"); err != nil {
		t.Fatal(err)
	}
	if err := s.BindConfirmedGroup(ctx, i.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.BindConfirmedGroup(ctx, i.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.Binding(ctx, "host", "thread")
	if err != nil || got.ChatID != "correct-chat" {
		t.Fatal("bound stale outcome", err)
	}
	var rejected int
	if err := s.DB.QueryRow("SELECT count(*) FROM group_attempts WHERE state='rejected' AND attempt_id=?", a.ID).Scan(&rejected); err != nil || rejected != 1 {
		t.Fatal("old journal lost", err)
	}
	if err := s.DB.QueryRow("SELECT count(*) FROM group_throttles").Scan(&count); err != nil || count != 0 {
		t.Fatal("finished group kept throttle", err)
	}
}

func TestGroupRetryRollbackAndCrashFreezeUnknown(t *testing.T) {
	ctx := context.Background()
	for _, stage := range []string{"defer", "confirm"} {
		t.Run(stage, func(t *testing.T) {
			s, path, i := groupRetryIntent(t)
			a := groupRetryClaim(t, s, i, 1000)
			if _, err := s.DB.Exec(`CREATE TRIGGER fail_group_outcome BEFORE UPDATE ON create_intents BEGIN SELECT RAISE(FAIL,'fixture'); END;`); err != nil {
				t.Fatal(err)
			}
			var err error
			if stage == "defer" {
				err = s.DeferGroupAttempt(ctx, a, 1000, 0)
			} else {
				err = s.FinishGroupDispatch(ctx, a, "confirmed", "created-chat")
			}
			if err == nil {
				t.Fatal("SQL failure accepted")
			}
			var state string
			if err := s.DB.QueryRow("SELECT state FROM group_attempts WHERE attempt_id=?", a.ID).Scan(&state); err != nil || state != "dispatching" {
				t.Fatal("half committed outcome", err)
			}
			var n int
			if err := s.DB.QueryRow("SELECT count(*) FROM group_throttles").Scan(&n); err != nil || n != 0 {
				t.Fatal("half committed retry", err)
			}
			if _, err := s.DB.Exec("DROP TRIGGER fail_group_outcome"); err != nil {
				t.Fatal(err)
			}
			s.Close()
			s, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err := s.RecoverGroupDispatches(ctx, "host"); err != nil {
				t.Fatal(err)
			}
			if _, claimed, err := s.ClaimGroupAttempt(ctx, i.ID, 99999); err != nil || claimed {
				t.Fatal("crash repeated remote creation", err)
			}
			if err := s.DeferGroupAttempt(ctx, a, 99999, 0); !errors.Is(err, ErrConflict) {
				t.Fatal("unknown was reopened", err)
			}
			if err := s.DB.QueryRow("SELECT state FROM group_attempts WHERE attempt_id=?", a.ID).Scan(&state); err != nil || state != "unknown" {
				t.Fatal("crash proof missing", err)
			}
		})
	}
}

func TestGroupRetryClaimRespectsExclusionAndConcurrentDeadline(t *testing.T) {
	s, _, i := groupRetryIntent(t)
	ctx := context.Background()
	a := groupRetryClaim(t, s, i, 1000)
	if err := s.DeferGroupAttempt(ctx, a, 1000, 2000); err != nil {
		t.Fatal(err)
	}
	var claims atomic.Int32
	var wg sync.WaitGroup
	for n := 0; n < 10; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, claimed, err := s.ClaimGroupAttempt(ctx, i.ID, 3000)
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
		t.Fatal("concurrent duplicate dispatches", claims.Load())
	}
	var b GroupAttempt
	if err := s.DB.QueryRow("SELECT attempt_id,intent_id,created_at_ms,state,chat_id FROM group_attempts WHERE state='dispatching'").Scan(&b.ID, &b.IntentID, &b.CreatedAtMS, &b.State, &b.ChatID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeferGroupAttempt(ctx, b, 3000, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Exclude(ctx, "host", "thread", "directory_muted"); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := s.ClaimGroupAttempt(ctx, i.ID, 99999); err != nil || claimed {
		t.Fatal("excluded candidate resumed", err)
	}
}

func TestGroupRetryBackoffServerFloorAndOverflow(t *testing.T) {
	s, _, i := groupRetryIntent(t)
	ctx := context.Background()
	at := int64(1000)
	for n, delay := range []int64{1000, 2000, 4000, 8000, 16000, 32000, 60000, 60000, 90000} {
		a := groupRetryClaim(t, s, i, at)
		floor := int64(0)
		if n == 8 {
			floor = 90000
		}
		if err := s.DeferGroupAttempt(ctx, a, at, floor); err != nil {
			t.Fatal(err)
		}
		var due, count int64
		if err := s.DB.QueryRow("SELECT next_retry_at_ms,rejection_count FROM group_throttles WHERE intent_id=?", i.ID).Scan(&due, &count); err != nil || due != at+delay || count != int64(n+1) {
			t.Fatal("wrong backoff", n, due, count, err)
		}
		at = due
	}
	a := groupRetryClaim(t, s, i, at)
	if err := s.DeferGroupAttempt(ctx, a, math.MaxInt64, 1000); !errors.Is(err, ErrConflict) {
		t.Fatal("retry overflow accepted", err)
	}
}

func TestGroupRetryMigrationDoesNotInventLegacyDispatchProof(t *testing.T) {
	s, path, i := groupRetryIntent(t)
	ctx := context.Background()
	if _, err := s.DB.Exec("UPDATE create_intents SET state='dispatching'; DROP TABLE group_throttles; DROP TABLE group_attempts; ALTER TABLE mirrors DROP COLUMN progress_observed_at_json; PRAGMA user_version=12"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.RecoverGroupDispatches(ctx, "host"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB.QueryRow("SELECT count(*) FROM group_attempts").Scan(&n); err != nil || n != 0 {
		t.Fatal("legacy attempt guessed", err)
	}
	if _, claimed, err := s.ClaimGroupAttempt(ctx, i.ID, 99999); err != nil || claimed {
		t.Fatal("legacy unknown redispatched", err)
	}
	if err := s.ConfirmGroup(ctx, i.ID, "readback-chat"); err != nil {
		t.Fatal(err)
	}
	if err := s.BindConfirmedGroup(ctx, i.ID); err != nil {
		t.Fatal(err)
	}
}
