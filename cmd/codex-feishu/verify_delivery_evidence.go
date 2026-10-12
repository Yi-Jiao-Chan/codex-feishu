package main

import "codex-feishu/internal/verification"

func appendManagedDeliveryChecks(o *output, p verification.BindingResult) {
	read := "pending"
	if p.DeliveryEvidenceStatus == "pass" && p.DeliveryEvidence != nil && !p.BuildChanged && p.BindingStatus == "pass" {
		read = "pass"
	}
	progress, final, reply := "pending", "pending", "pending"
	if read == "pass" {
		e := p.DeliveryEvidence
		if e.ProgressConfirmed > 0 {
			progress = "pass"
		}
		if e.FinalTurnsDelivered > 0 {
			final = "pass"
		}
		if e.ReferencedCards > 0 && e.ReferencedCards == e.ProgressCards+e.FinalCards {
			reply = "pass"
		}
	}
	o.Checks = append(o.Checks,
		check{"verification.delivery_evidence_snapshot", read, false, "read-only persisted delivery evidence for the original test binding; no message GET, update or resend"},
		check{"verification.progress_delivery", progress, false, "at least one native-turn progress card confirmed at its current revision with original reply proof; not proof of one card per turn or client layout"},
		check{"verification.final_delivery", final, false, "at least one turn's final segments and sent retired tails are all confirmed at current revisions with original reply proof; not proof of App UI or all turns"},
		check{"verification.delivery_reply_relation", reply, false, "all current native progress/final documents have confirmed original replies to their scoped input anchors; not a rendered-client observation"})
}
