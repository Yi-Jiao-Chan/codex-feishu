package main

import "codex-feishu/internal/verification"

func appendManagedInputChecks(o *output, p verification.BindingResult) {
	read := "pending"
	if p.InputEvidenceStatus == "pass" && p.InputEvidence != nil && !p.BuildChanged && p.BindingStatus == "pass" {
		read = "pass"
	}
	o.Checks = append(o.Checks, check{"verification.input_evidence_snapshot", read, false, "read-only persisted input evidence for the original managed test binding; no new queue/history GET or App UI observation"})
	add, history, steer, target := "pending", "pending", "pending", "pending"
	if read == "pass" {
		e := p.InputEvidence
		if e.AddConfirmed > 0 {
			add = "pass"
		}
		if e.HistoryLinked > 0 {
			history = "pass"
		}
		if e.SteerConfirmed > 0 {
			steer = "pass"
		}
		if e.SteerHistoryLinked > 0 {
			target = "pass"
		}
	}
	o.Checks = append(o.Checks,
		check{"verification.input_queue_add", add, false, "at least one original default add confirmed in its scoped journal; not proof of busy ordering or no duplicates"},
		check{"verification.input_history_link", history, false, "at least one native user item linked to a fixture receipt; not proof of App page visibility"},
		check{"verification.input_steer", steer, false, "same explicit action has confirmed queue deletion and steer to its captured turn; queue disappearance alone is insufficient"},
		check{"verification.input_steer_history", target, false, "the steered receipt also maps to a native user item in the captured target turn; not complete P1-05 or UI acceptance"})
}
