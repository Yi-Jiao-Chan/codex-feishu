package main

import (
	"context"
	"testing"

	"codex-feishu/internal/config"
	"codex-feishu/internal/store"
	"codex-feishu/internal/verification"
)

func TestManagedDeliveryEvidenceRequiresCompleteCurrentSegmentsAndReplyProof(t *testing.T) {
	for _, scenario := range []string{"old-provider", "read-failed", "empty", "progress", "final-pending", "partial-final", "final-delivered", "other-build"} {
		t.Run(scenario, func(t *testing.T) {
			path, _, deps := bindingCLIFixture(t)
			id := store.ID()
			p := bindingCLIResult(id)
			p.DeliveryEvidenceStatus, p.DeliveryEvidence = "pass", &store.VerificationDeliveryEvidence{}
			want := map[string]string{
				"verification.delivery_evidence_snapshot": "pass",
				"verification.progress_delivery":          "pending",
				"verification.final_delivery":             "pending",
				"verification.delivery_reply_relation":    "pending",
			}
			switch scenario {
			case "old-provider":
				p.DeliveryEvidenceStatus, p.DeliveryEvidence = "", nil
				want["verification.delivery_evidence_snapshot"] = "pending"
			case "read-failed":
				p.DeliveryEvidenceStatus, p.DeliveryEvidence = "pending", nil
				want["verification.delivery_evidence_snapshot"] = "pending"
			case "progress":
				p.DeliveryEvidence.ProgressCards, p.DeliveryEvidence.ProgressConfirmed, p.DeliveryEvidence.ReferencedCards = 1, 1, 1
				want["verification.progress_delivery"], want["verification.delivery_reply_relation"] = "pass", "pass"
			case "final-pending":
				p.DeliveryEvidence.FinalCards, p.DeliveryEvidence.FinalTurns, p.DeliveryEvidence.UnresolvedAttempts = 1, 1, 1
			case "partial-final":
				p.DeliveryEvidence.FinalCards, p.DeliveryEvidence.FinalConfirmed, p.DeliveryEvidence.FinalTurns, p.DeliveryEvidence.ReferencedCards = 2, 1, 1, 1
			case "final-delivered":
				p.DeliveryEvidence.FinalCards, p.DeliveryEvidence.FinalConfirmed, p.DeliveryEvidence.FinalTurns, p.DeliveryEvidence.FinalTurnsDelivered, p.DeliveryEvidence.ReferencedCards = 2, 2, 1, 1, 2
				want["verification.final_delivery"], want["verification.delivery_reply_relation"] = "pass", "pass"
			case "other-build":
				p.BuildChanged = true
				p.DeliveryEvidence.FinalCards, p.DeliveryEvidence.FinalConfirmed, p.DeliveryEvidence.FinalTurns, p.DeliveryEvidence.FinalTurnsDelivered, p.DeliveryEvidence.ReferencedCards = 1, 1, 1, 1, 1
				want["verification.delivery_evidence_snapshot"] = "pending"
			}
			calls := 0
			deps.managedBinding = func(_ context.Context, _ config.Config, _, request string, begin bool) (verification.BindingResult, error) {
				calls++
				if begin || request != id {
					t.Fatal("delivery evidence query requested mutation")
				}
				return p, nil
			}
			o, code := inspectVerify(context.Background(), path, verifyOptions{RequestID: id}, deps)
			if code != 5 || calls != 1 || o.Data.(map[string]any)["read_only"] != true {
				t.Fatal(o, code, calls)
			}
			assertBindingAcceptancePending(t, o)
			statuses := inputCheckStatuses(o)
			for key, expected := range want {
				if statuses[key] != expected {
					t.Fatal(key, statuses[key], expected)
				}
			}
			for _, c := range o.Checks {
				if _, found := want[c.ID]; found && c.Required {
					t.Fatal("delivery count promoted a P1 gate", c)
				}
			}
		})
	}
}
