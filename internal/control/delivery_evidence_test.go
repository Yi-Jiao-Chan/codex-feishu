//go:build linux

package control

import (
	"context"
	"errors"
	"testing"

	"codex-feishu/internal/store"
)

func TestBindingControlRejectsInconsistentDeliveryEvidence(t *testing.T) {
	for _, reason := range []string{"missing-pass", "unexpected-status", "pending-with-data", "unbound", "negative", "progress-over-cards", "final-over-cards", "turns-over-cards", "delivered-over-turns", "delivered-over-confirmed", "references-over-cards", "confirmed-without-reference", "negative-unresolved"} {
		t.Run(reason, func(t *testing.T) {
			id := store.ID()
			r := validBindingResult(id)
			r.DeliveryEvidenceStatus, r.DeliveryEvidence = "pass", &store.VerificationDeliveryEvidence{}
			switch reason {
			case "missing-pass":
				r.DeliveryEvidence = nil
			case "unexpected-status":
				r.DeliveryEvidenceStatus = "unknown"
			case "pending-with-data":
				r.DeliveryEvidenceStatus = "pending"
			case "unbound":
				r.BindingStatus = "pending"
			case "negative":
				r.DeliveryEvidence.ProgressCards = -1
			case "progress-over-cards":
				r.DeliveryEvidence.ProgressConfirmed = 1
			case "final-over-cards":
				r.DeliveryEvidence.FinalConfirmed = 1
			case "turns-over-cards":
				r.DeliveryEvidence.FinalTurns = 1
			case "delivered-over-turns":
				r.DeliveryEvidence.FinalTurnsDelivered = 1
			case "delivered-over-confirmed":
				r.DeliveryEvidence.FinalCards, r.DeliveryEvidence.FinalTurns, r.DeliveryEvidence.FinalTurnsDelivered = 1, 1, 1
			case "references-over-cards":
				r.DeliveryEvidence.ReferencedCards = 1
			case "confirmed-without-reference":
				r.DeliveryEvidence.ProgressCards, r.DeliveryEvidence.ProgressConfirmed = 1, 1
			case "negative-unresolved":
				r.DeliveryEvidence.UnresolvedAttempts = -1
			}
			path, identity, stop := staticControlResponse(t, func(identity string) response {
				return response{Schema: protocol, Identity: identity, Code: "ok", VerificationBinding: &r}
			})
			defer stop()
			if _, err := VerifyBinding(context.Background(), path, identity, id, false); !errors.Is(err, ErrUnavailable) {
				t.Fatal("inconsistent delivery snapshot accepted", err)
			}
		})
	}
}

func TestBindingControlPreservesValidAndMissingDeliveryEvidence(t *testing.T) {
	for _, mode := range []string{"legacy", "pending", "empty", "observed"} {
		t.Run(mode, func(t *testing.T) {
			id := store.ID()
			r := validBindingResult(id)
			switch mode {
			case "pending":
				r.DeliveryEvidenceStatus = "pending"
			case "empty":
				r.DeliveryEvidenceStatus, r.DeliveryEvidence = "pass", &store.VerificationDeliveryEvidence{}
			case "observed":
				r.DeliveryEvidenceStatus, r.DeliveryEvidence = "pass", &store.VerificationDeliveryEvidence{ProgressCards: 1, ProgressConfirmed: 1, FinalCards: 2, FinalConfirmed: 2, FinalTurns: 1, FinalTurnsDelivered: 1, ReferencedCards: 3, UnresolvedAttempts: 1}
			}
			path, identity, stop := staticControlResponse(t, func(identity string) response {
				return response{Schema: protocol, Identity: identity, Code: "ok", VerificationBinding: &r}
			})
			defer stop()
			got, err := VerifyBinding(context.Background(), path, identity, id, false)
			if err != nil || got.DeliveryEvidenceStatus != r.DeliveryEvidenceStatus || (got.DeliveryEvidence == nil) != (r.DeliveryEvidence == nil) || got.DeliveryEvidence != nil && *got.DeliveryEvidence != *r.DeliveryEvidence {
				t.Fatal(got, err)
			}
		})
	}
}
