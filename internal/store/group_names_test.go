package store

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func groupNameStore(t *testing.T) (*Store, string, GroupNameScope) {
	t.Helper()
	s, path, i := groupRetryIntent(t)
	ctx := context.Background()
	a := groupRetryClaim(t, s, i, 1000)
	if err := s.FinishGroupDispatch(ctx, a, "confirmed", "chat"); err != nil {
		t.Fatal(err)
	}
	if err := s.BindConfirmedGroup(ctx, i.ID); err != nil {
		t.Fatal(err)
	}
	b, err := s.Binding(ctx, "host", "thread")
	if err != nil {
		t.Fatal(err)
	}
	return s, path, GroupNameScope{AppID: "app", OwnerID: "owner", Binding: b}
}

func nameClaim(t *testing.T, s *Store, scope GroupNameScope, expected, desired, previous string) GroupNameAttempt {
	t.Helper()
	a, claimed, err := s.ClaimGroupName(context.Background(), GroupNamePlan{Scope: scope, Desired: desired, Previous: previous}, expected, 1000)
	if err != nil || !claimed {
		t.Fatal("name not claimed", err)
	}
	return a
}

func TestGroupNamesStoreCASAndFrozenRecovery(t *testing.T) {
	ctx := context.Background()
	s, path, scope := groupNameStore(t)
	a := nameClaim(t, s, scope, "", "B", "A")
	if err := s.FinishGroupName(ctx, a, "unknown"); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := s.ClaimGroupName(ctx, GroupNamePlan{Scope: scope, Desired: "C", Previous: "A"}, a.Key, 2000); err != nil || claimed {
		t.Fatal("new title bypassed unknown", err)
	}
	if err := s.FinishGroupName(ctx, a, "rejected"); !errors.Is(err, ErrConflict) {
		t.Fatal("stale completion accepted", err)
	}
	s.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recovered, err := s.GroupNameAttempt(ctx, scope)
	if err != nil || recovered.State != "unknown" || recovered.Plan.Desired != "B" || recovered.Plan.Previous != "A" || recovered.Plan.Scope != scope {
		t.Fatal("frozen plan lost", recovered, err)
	}
	if err := s.FinishGroupName(ctx, recovered, "rejected"); !errors.Is(err, ErrConflict) {
		t.Fatal("unknown became retryable", err)
	}
	if err := s.FinishGroupName(ctx, recovered, "confirmed"); err != nil {
		t.Fatal(err)
	}
	b := nameClaim(t, s, scope, a.Key, "A", "B")
	if b.Key == a.Key {
		t.Fatal("A to B to A reused attempt")
	}
	if err := s.FinishGroupName(ctx, recovered, "confirmed"); !errors.Is(err, ErrConflict) {
		t.Fatal("stale CAS accepted", err)
	}
	h, err := s.GroupNameHealth(ctx, "host", "app")
	if err != nil || h["confirmed"] != 1 || h["dispatching"] != 1 {
		t.Fatal(h, err)
	}
	other, err := s.GroupNameHealth(ctx, "host", "other")
	if err != nil || len(other) != 0 {
		t.Fatal("health crossed app", other, err)
	}
}

func TestGroupNamesStoreAtomicClaim(t *testing.T) {
	s, _, scope := groupNameStore(t)
	var successes atomic.Int32
	var wg sync.WaitGroup
	for n := 0; n < 20; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, claimed, err := s.ClaimGroupName(context.Background(), GroupNamePlan{Scope: scope, Desired: "B", Previous: "A"}, "", 1000)
			if err != nil {
				t.Error(err)
			}
			if claimed {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatal("multiple claims", successes.Load())
	}
}

func TestGroupNamesStoreRejectsStaleScopeAndWaits(t *testing.T) {
	s, _, scope := groupNameStore(t)
	ctx := context.Background()
	for _, alter := range []func(*GroupNameScope){func(s *GroupNameScope) { s.AppID = "other" }, func(s *GroupNameScope) { s.Binding.ChatID = "other" }, func(s *GroupNameScope) { s.Binding.IntentID = "other" }} {
		stale := scope
		alter(&stale)
		_, claimed, err := s.ClaimGroupName(ctx, GroupNamePlan{Scope: stale, Desired: "B", Previous: "A"}, "", 1000)
		if err != nil || claimed {
			t.Fatal("stale scope journaled", err)
		}
	}
	a := nameClaim(t, s, scope, "", "B", "A")
	due, err := s.DeferGroupName(ctx, a, 2000, 5000)
	if err != nil || due != 7000 {
		t.Fatal(due, err)
	}
	if _, err := s.DeferGroupName(ctx, a, 4000, 8000); !errors.Is(err, ErrConflict) {
		t.Fatal("stale defer accepted", err)
	}
	if _, claimed, err := s.ClaimGroupName(ctx, GroupNamePlan{Scope: scope, Desired: "C", Previous: "A"}, a.Key, 6999); err != nil || claimed {
		t.Fatal("early retry", err)
	}
	b, claimed, err := s.ClaimGroupName(ctx, GroupNamePlan{Scope: scope, Desired: "C", Previous: "A", RejectionCount: 1}, a.Key, 7000)
	if err != nil || !claimed {
		t.Fatal("due changed title blocked", err)
	}
	if err := s.FinishGroupName(ctx, b, "rejected"); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := s.ClaimGroupName(ctx, GroupNamePlan{Scope: scope, Desired: "C", Previous: "A"}, b.Key, 8000); err != nil || claimed {
		t.Fatal("permanent same title retried", err)
	}
	nameClaim(t, s, scope, b.Key, "D", "A")
}
