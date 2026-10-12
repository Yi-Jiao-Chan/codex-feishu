package main

import (
	"context"
	"testing"

	"codex-feishu/internal/config"
	"codex-feishu/internal/store"
	"codex-feishu/internal/verification"
)

func inputCheckStatuses(o output) map[string]string {
	result := map[string]string{}
	for _, c := range o.Checks {
		result[c.ID] = c.Status
	}
	return result
}

func TestManagedInputEvidenceReportsOnlyPersistedComponentFacts(t *testing.T) {
	for _, scenario := range []string{"old-provider", "read-failed", "empty", "add-only", "history", "delete-only", "steer-response", "steer-history", "other-build"} {
		t.Run(scenario, func(t *testing.T) {
			path, _, deps := bindingCLIFixture(t)
			id := store.ID()
			p := bindingCLIResult(id)
			want := map[string]string{
				"verification.input_evidence_snapshot": "pass",
				"verification.input_queue_add":         "pending",
				"verification.input_history_link":      "pending",
				"verification.input_steer":             "pending",
				"verification.input_steer_history":     "pending",
			}
			p.InputEvidenceStatus = "pass"
			p.InputEvidence = &store.VerificationInputEvidence{StagedInputs: 1}
			switch scenario {
			case "old-provider":
				p.InputEvidenceStatus, p.InputEvidence = "", nil
				want["verification.input_evidence_snapshot"] = "pending"
			case "read-failed":
				p.InputEvidenceStatus, p.InputEvidence = "pending", nil
				want["verification.input_evidence_snapshot"] = "pending"
			case "empty":
				p.InputEvidence.StagedInputs = 0
			case "add-only":
				p.InputEvidence.AddConfirmed, p.InputEvidence.QueuedSnapshot = 1, 1
				want["verification.input_queue_add"] = "pass"
			case "history":
				p.InputEvidence.HistoryLinked = 1
				want["verification.input_history_link"] = "pass"
			case "delete-only":
				p.InputEvidence.UnresolvedAttempts = 1
			case "steer-response":
				p.InputEvidence.SteerConfirmed = 1
				want["verification.input_steer"] = "pass"
			case "steer-history":
				p.InputEvidence.SteerConfirmed, p.InputEvidence.SteerHistoryLinked, p.InputEvidence.HistoryLinked = 1, 1, 1
				want["verification.input_history_link"], want["verification.input_steer"], want["verification.input_steer_history"] = "pass", "pass", "pass"
			case "other-build":
				p.BuildChanged = true
				p.InputEvidence.AddConfirmed, p.InputEvidence.HistoryLinked, p.InputEvidence.SteerConfirmed, p.InputEvidence.SteerHistoryLinked = 1, 1, 1, 1
				want["verification.input_evidence_snapshot"] = "pending"
			}
			calls := 0
			deps.managedBinding = func(_ context.Context, _ config.Config, _, request string, begin bool) (verification.BindingResult, error) {
				calls++
				if begin || request != id {
					t.Fatal("input evidence query requested mutation")
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
					t.Fatal("component count promoted acceptance gate", c)
				}
			}
		})
	}
}
