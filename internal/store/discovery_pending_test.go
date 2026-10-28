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

func TestCandidateSuspensionPreservesEvidenceAndTerminalStates(t *testing.T) {
	s, _ := discoveryStore(t)
	ctx := context.Background()
	for _, thread := range []string{"candidate", "muted", "bound"} {
		eligible(t, s, thread)
	}
	before, err := s.Observation(ctx, "host", "candidate")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Exclude(ctx, "host", "muted", "directory_muted"); err != nil {
		t.Fatal(err)
	}
	intent, err := s.PrepareGroup(ctx, "host", "bound", "owner", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimGroupDispatch(ctx, intent.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmGroup(ctx, intent.ID, "chat"); err != nil {
		t.Fatal(err)
	}
	if err := s.BindConfirmedGroup(ctx, intent.ID); err != nil {
		t.Fatal(err)
	}
	for _, thread := range []string{"candidate", "muted", "bound"} {
		if err := s.SuspendCandidate(ctx, "host", thread); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.NotePending(ctx, "host", "candidate", "metadata_unavailable"); err != nil {
		t.Fatal(err)
	}
	if err := s.HoldCandidate(ctx, "host", "candidate", "active_unconfirmed"); err != nil {
		t.Fatal(err)
	}
	after, err := s.Observation(ctx, "host", "candidate")
	if err != nil || !after.Suspended() || after.State != "pending" {
		t.Fatalf("candidate not suspended: %+v %v", after, err)
	}
	after.State, after.ExclusionReason = before.State, before.ExclusionReason
	if after != before {
		t.Fatalf("suspension destroyed evidence: before=%+v after=%+v", before, after)
	}
	if err := s.HoldCandidate(ctx, "host", "candidate", "archived"); err != nil {
		t.Fatal(err)
	}
	after, _ = s.Observation(ctx, "host", "candidate")
	if !after.Suspended() || !after.Archived() {
		t.Fatal("archive observation replaced missing-cwd hold")
	}
	pending, err := s.PendingObservations(ctx, "host")
	if err != nil || len(pending) != 0 {
		t.Fatalf("suspended/terminal candidate still pending retry: %+v %v", pending, err)
	}
	if err := s.ResumeCandidate(ctx, "host", "candidate"); err != nil {
		t.Fatal(err)
	}
	after, _ = s.Observation(ctx, "host", "candidate")
	if after.Suspended() || !after.Archived() || after.State != "pending" {
		t.Fatal("resume bypassed archive evidence or granted eligibility")
	}
	for thread, want := range map[string]string{"muted": "excluded", "bound": "eligible"} {
		if err := s.ResumeCandidate(ctx, "host", thread); err != nil {
			t.Fatal(err)
		}
		o, _ := s.Observation(ctx, "host", thread)
		if o.State != want || o.Suspended() {
			t.Fatalf("suspension rewrote terminal/bound route: %+v", o)
		}
	}
}
