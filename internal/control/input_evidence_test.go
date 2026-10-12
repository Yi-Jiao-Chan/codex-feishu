//go:build linux

package control

import (
	"context"
	"errors"
	"testing"

	"codex-feishu/internal/store"
)

func TestBindingControlRejectsInconsistentInputEvidence(t *testing.T) {
	for _, reason := range []string{"missing-pass", "unexpected-status", "pending-with-data", "unbound-data", "negative", "add-over-inputs", "queue-over-inputs", "history-over-inputs", "steer-over-inputs", "target-without-steer", "target-without-history", "negative-unresolved"} {
		t.Run(reason, func(t *testing.T) {
			id := store.ID()
			r := validBindingResult(id)
			r.InputEvidenceStatus, r.InputEvidence = "pass", &store.VerificationInputEvidence{StagedInputs: 1}
			switch reason {
			case "missing-pass":
				r.InputEvidence = nil
			case "unexpected-status":
				r.InputEvidenceStatus = "unknown"
			case "pending-with-data":
				r.InputEvidenceStatus = "pending"
			case "unbound-data":
				r.BindingStatus = "pending"
			case "negative":
				r.InputEvidence.StagedInputs = -1
			case "add-over-inputs":
				r.InputEvidence.AddConfirmed = 2
			case "queue-over-inputs":
				r.InputEvidence.QueuedSnapshot = 2
			case "history-over-inputs":
				r.InputEvidence.HistoryLinked = 2
			case "steer-over-inputs":
				r.InputEvidence.SteerConfirmed = 2
			case "target-without-steer":
				r.InputEvidence.SteerHistoryLinked, r.InputEvidence.HistoryLinked = 1, 1
			case "target-without-history":
				r.InputEvidence.SteerConfirmed, r.InputEvidence.SteerHistoryLinked = 1, 1
			case "negative-unresolved":
				r.InputEvidence.UnresolvedAttempts = -1
			}
			path, identity, stop := staticControlResponse(t, func(identity string) response {
				return response{Schema: protocol, Identity: identity, Code: "ok", VerificationBinding: &r}
			})
			defer stop()
			if _, err := VerifyBinding(context.Background(), path, identity, id, false); !errors.Is(err, ErrUnavailable) {
				t.Fatal("inconsistent snapshot accepted", err)
			}
		})
	}
}

func TestBindingControlPreservesValidAndMissingLocalInputEvidence(t *testing.T) {
	for _, mode := range []string{"legacy", "pending", "empty", "observed"} {
		t.Run(mode, func(t *testing.T) {
			id := store.ID()
			r := validBindingResult(id)
			switch mode {
			case "pending":
				r.InputEvidenceStatus = "pending"
			case "empty":
				r.InputEvidenceStatus, r.InputEvidence = "pass", &store.VerificationInputEvidence{}
			case "observed":
				r.InputEvidenceStatus, r.InputEvidence = "pass", &store.VerificationInputEvidence{StagedInputs: 2, AddConfirmed: 2, HistoryLinked: 1, SteerConfirmed: 1, SteerHistoryLinked: 1, QueuedSnapshot: 1, UnresolvedAttempts: 1}
			}
			path, identity, stop := staticControlResponse(t, func(identity string) response {
				return response{Schema: protocol, Identity: identity, Code: "ok", VerificationBinding: &r}
			})
			defer stop()
			got, err := VerifyBinding(context.Background(), path, identity, id, false)
			if err != nil || got.InputEvidenceStatus != r.InputEvidenceStatus || (got.InputEvidence == nil) != (r.InputEvidence == nil) || got.InputEvidence != nil && *got.InputEvidence != *r.InputEvidence {
				t.Fatal(got, err)
			}
		})
	}
}
