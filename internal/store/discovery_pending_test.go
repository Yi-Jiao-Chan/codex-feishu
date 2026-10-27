package store

import (
	"context"
	"testing"
)

func TestPendingObservationsExcludesOnlyBoundRoutes(t *testing.T) {
	s, _ := discoveryStore(t)
	ctx := context.Background()
	for _, thread := range []string{"bound", "confirmed-unbound", "eligible"} {
		eligible(t, s, thread)
	}
	if err := s.ObserveNew(ctx, "host", "pending", "/project", 102); err != nil {
		t.Fatal(err)
	}
	for _, thread := range []string{"bound", "confirmed-unbound"} {
		i, err := s.PrepareGroup(ctx, "host", thread, "owner", "fixture", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimGroupDispatch(ctx, i.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.ConfirmGroup(ctx, i.ID, "chat-"+thread); err != nil {
			t.Fatal(err)
		}
		if thread == "bound" {
			if err := s.BindConfirmedGroup(ctx, i.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A binding for a different host must not hide this candidate.
	if _, err := s.BeginBaseline(ctx, "other-host", "other-app", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveNew(ctx, "other-host", "eligible", "/project", 101); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkEligible(ctx, "other-host", "eligible", "input", "/project", "/project"); err != nil {
		t.Fatal(err)
	}
	other, err := s.PrepareGroup(ctx, "other-host", "eligible", "owner", "other", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimGroupDispatch(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmGroup(ctx, other.ID, "other-chat"); err != nil {
		t.Fatal(err)
	}
	if err := s.BindConfirmedGroup(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingObservations(ctx, "host")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, o := range pending {
		seen[o.ThreadID] = true
	}
	if len(seen) != 3 || !seen["pending"] || !seen["eligible"] || !seen["confirmed-unbound"] {
		t.Fatal("discovery reactivated a bound route or lost an unbound recovery candidate", seen)
	}
	observation, err := s.Observation(ctx, "host", "bound")
	if err != nil || observation.State != "eligible" {
		t.Fatal("candidate filtering rewrote discovery provenance", err)
	}
}
