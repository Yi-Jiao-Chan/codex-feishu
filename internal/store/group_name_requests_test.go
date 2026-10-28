package store

import (
	"context"
	"errors"
	"testing"
)

func requireNameRequests(t *testing.T, s *Store, scope GroupNameScope, desired ...string) []GroupNameRequest {
	t.Helper()
	requests, err := s.PendingGroupNameRequests(context.Background(), scope.Binding.HostID, scope.AppID, scope.OwnerID)
	if err != nil || len(requests) != len(desired) {
		t.Fatalf("pending names count=%d want=%d err=%v", len(requests), len(desired), err)
	}
	for i, want := range desired {
		if requests[i].Scope != scope || requests[i].Desired != want {
			t.Fatal("pending request lost its exact scope or desired title")
		}
	}
	return requests
}

func TestGroupNameRequestsCoalesceAndCompleteWithCAS(t *testing.T) {
	ctx := context.Background()
	s, _, scope := groupNameStore(t)
	a, err := s.StageGroupNameRequest(ctx, scope, "A", 1000)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.StageGroupNameRequest(ctx, scope, "B", 2000)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteGroupNameRequest(ctx, a); err != nil {
		t.Fatal(err)
	}
	requireNameRequests(t, s, scope, "B")
	if err := s.CompleteGroupNameRequest(ctx, b); err != nil {
		t.Fatal(err)
	}
	requireNameRequests(t, s, scope)
	if err := s.CompleteGroupNameRequest(ctx, b); err != nil {
		t.Fatal("duplicate completion was not benign", err)
	}
	if _, err := s.StageGroupNameRequest(ctx, scope, "B", 3000); err != nil {
		t.Fatal(err)
	}
	requireNameRequests(t, s, scope, "B")
	var attempts int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM m0_operations WHERE kind=?`, GroupNameOperationKind).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatal("staging a desired title created a dispatch attempt", err)
	}
}

func TestGroupNameRequestsPersistAcrossRestart(t *testing.T) {
	ctx := context.Background()
	s, path, scope := groupNameStore(t)
	if _, err := s.StageGroupNameRequest(ctx, scope, "Restored title", 1000); err != nil {
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
	r := requireNameRequests(t, s, scope, "Restored title")[0]
	if err := s.CompleteGroupNameRequest(ctx, r); err != nil {
		t.Fatal(err)
	}
	requireNameRequests(t, s, scope)
}

func TestGroupNameRequestsValidateExactRouteAndScope(t *testing.T) {
	ctx := context.Background()
	s, _, scope := groupNameStore(t)
	for _, alter := range []func(*GroupNameScope){
		func(s *GroupNameScope) { s.AppID = "another-app" },
		func(s *GroupNameScope) { s.OwnerID = "" },
		func(s *GroupNameScope) { s.Binding.HostID = "another-host" },
		func(s *GroupNameScope) { s.Binding.ThreadID = "another-thread" },
		func(s *GroupNameScope) { s.Binding.ChatID = "another-chat" },
		func(s *GroupNameScope) { s.Binding.IntentID = "another-intent" },
		func(s *GroupNameScope) { s.Binding.FollowMode = "topic"; s.Binding.RootMessageID = "root" },
		func(s *GroupNameScope) { s.Binding.RootMessageID = "stale-root" },
		func(s *GroupNameScope) { s.Binding.PlatformThreadID = "stale-platform-thread" },
		func(s *GroupNameScope) { s.Binding.RepairRequired = true },
		func(s *GroupNameScope) { s.Binding.RepairReason = "stale-repair" },
	} {
		stale := scope
		alter(&stale)
		if _, err := s.StageGroupNameRequest(ctx, stale, "Title", 1000); !errors.Is(err, ErrConflict) {
			t.Fatal("stale/non-group scope was staged", err)
		}
	}
	for _, desired := range []string{"", " \n\t", string([]byte{0xff})} {
		if _, err := s.StageGroupNameRequest(ctx, scope, desired, 1000); !errors.Is(err, ErrConflict) {
			t.Fatal("invalid title staged", err)
		}
	}
	if _, err := s.StageGroupNameRequest(ctx, scope, "Title", 0); !errors.Is(err, ErrConflict) {
		t.Fatal("invalid timestamp staged", err)
	}
	if _, err := s.StageGroupNameRequest(ctx, scope, "Title", 1000); err != nil {
		t.Fatal(err)
	}
	for _, other := range []GroupNameScope{
		{AppID: "another-app", OwnerID: scope.OwnerID, Binding: scope.Binding},
		{AppID: scope.AppID, OwnerID: "another-owner", Binding: scope.Binding},
	} {
		requireNameRequests(t, s, other)
	}
	if requests, err := s.PendingGroupNameRequests(ctx, "another-host", scope.AppID, scope.OwnerID); err != nil || len(requests) != 0 {
		t.Fatal("request crossed hosts", err)
	}
	// Scope owner follows the configured caller, as in the existing rename
	// journal; authoritative group ownership is checked by reconciliation.
	otherOwner := scope
	otherOwner.OwnerID = "another-owner"
	if _, err := s.StageGroupNameRequest(ctx, otherOwner, "Other owner's title", 2000); err != nil {
		t.Fatal(err)
	}
	requireNameRequests(t, s, scope)
	requireNameRequests(t, s, otherOwner, "Other owner's title")
}

func TestGroupNameRequestsIgnoreStaleBindingsAndRejectMalformedRelevantData(t *testing.T) {
	ctx := context.Background()
	s, _, scope := groupNameStore(t)
	if _, err := s.StageGroupNameRequest(ctx, scope, "Title", 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO m0_operations(key,kind,state,remote_id,created_at) VALUES('unrelated',?,'ready','{',1000)`, GroupNameRequestKind); err != nil {
		t.Fatal(err)
	}
	requireNameRequests(t, s, scope, "Title")
	if _, err := s.DB.ExecContext(ctx, `UPDATE m0_operations SET remote_id='{' WHERE key=?`, groupNamePrefix(scope)+"request"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PendingGroupNameRequests(ctx, scope.Binding.HostID, scope.AppID, scope.OwnerID); !errors.Is(err, ErrConflict) {
		t.Fatal("malformed current request did not fail closed", err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE bindings SET chat_id='replacement-chat' WHERE host_id=? AND thread_id=?`, scope.Binding.HostID, scope.Binding.ThreadID); err != nil {
		t.Fatal(err)
	}
	// Malformed orphaned data does not block a different exact binding.
	requireNameRequests(t, s, scope)
	if _, err := s.StageGroupNameRequest(ctx, scope, "Stale title", 2000); !errors.Is(err, ErrConflict) {
		t.Fatal("stale binding request was restaged", err)
	}
}

func TestGroupNameRequestsIgnoreChangedRouteFields(t *testing.T) {
	ctx := context.Background()
	s, _, scope := groupNameStore(t)
	nameClaim(t, s, scope, "", "Frozen title", "Previous")
	if _, err := s.StageGroupNameRequest(ctx, scope, "Desired title", 2000); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkBindingRepair(ctx, scope.Binding.HostID, scope.Binding.ThreadID, "fixture-repair"); err != nil {
		t.Fatal(err)
	}
	// Repair fields are outside the deterministic prefix but still belong to
	// exact binding identity, for both queue requests and legacy attempts.
	requireNameRequests(t, s, scope)
}

func TestGroupNameRequestsRecoverOnlyLegacyPendingAttempts(t *testing.T) {
	for _, state := range []string{"dispatching", "unknown", "deferred", "confirmed", "rejected"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			s, _, scope := groupNameStore(t)
			a := nameClaim(t, s, scope, "", "Desired", "Previous")
			switch state {
			case "deferred":
				if _, err := s.DeferGroupName(ctx, a, 2000, 5000); err != nil {
					t.Fatal(err)
				}
			case "dispatching":
			default:
				if err := s.FinishGroupName(ctx, a, state); err != nil {
					t.Fatal(err)
				}
			}
			if state == "confirmed" || state == "rejected" {
				requireNameRequests(t, s, scope)
				return
			}
			r := requireNameRequests(t, s, scope, "Desired")[0]
			if err := s.CompleteGroupNameRequest(ctx, r); err != nil {
				t.Fatal("legacy completion without request row failed", err)
			}
			requireNameRequests(t, s, scope, "Desired")
			if _, err := s.StageGroupNameRequest(ctx, scope, "Desired", 3000); err != nil {
				t.Fatal(err)
			}
			if err := s.CompleteGroupNameRequest(ctx, r); err != nil {
				t.Fatal(err)
			}
			requests := requireNameRequests(t, s, scope, "Desired")
			if requests[0].encoded == "" {
				t.Fatal("legacy completion deleted a newly staged same-name request")
			}
		})
	}
}

func TestGroupNameRequestsLatestDesiredOverridesFrozenUnknown(t *testing.T) {
	ctx := context.Background()
	s, _, scope := groupNameStore(t)
	a := nameClaim(t, s, scope, "", "Frozen title", "Previous")
	if err := s.FinishGroupName(ctx, a, "unknown"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StageGroupNameRequest(ctx, scope, "Latest desired title", 2000); err != nil {
		t.Fatal(err)
	}
	requireNameRequests(t, s, scope, "Latest desired title")
	recovered, err := s.GroupNameAttempt(ctx, scope)
	if err != nil || recovered.State != "unknown" || recovered.Plan.Desired != "Frozen title" {
		t.Fatal("desired queue overwrote the frozen ambiguous attempt", err)
	}
}

func TestGroupNameRequestsDoNotReviveSupersededLegacyDeferral(t *testing.T) {
	ctx := context.Background()
	s, _, scope := groupNameStore(t)
	a := nameClaim(t, s, scope, "", "Older desired", "Previous")
	if _, err := s.DeferGroupName(ctx, a, 2000, 0); err != nil {
		t.Fatal(err)
	}
	b, claimed, err := s.ClaimGroupName(ctx, GroupNamePlan{Scope: scope, Desired: "Latest desired", Previous: "Previous"}, a.Key, 4000)
	if err != nil || !claimed {
		t.Fatal("newer due attempt not claimed", err)
	}
	if err := s.FinishGroupName(ctx, b, "confirmed"); err != nil {
		t.Fatal(err)
	}
	requireNameRequests(t, s, scope)
}
