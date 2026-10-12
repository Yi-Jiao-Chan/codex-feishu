package main

import (
	"context"
	"errors"

	"codex-feishu/internal/config"
	"codex-feishu/internal/control"
	"codex-feishu/internal/verification"
)

// Each supplied allow flag authorizes just that component. A read-only request
// queries available journals; an absent component never starts a new probe.
func inspectManagedVerification(ctx context.Context, cfg config.Config, identity string, opts verifyOptions, live control.Snapshot, deps verifyDependencies, o output, pendingCode int) (output, int) {
	writes := opts.Card || opts.Group
	cardWanted := opts.Card || !writes && live.VerificationCardSupported
	groupWanted := opts.Group || !writes && live.VerificationGroupSupported
	// Check every requested capability before the first mutation. In particular,
	// a combined request against an older card-only build must not partially send.
	if !cardWanted && !groupWanted || cardWanted && (!live.VerificationCardSupported || deps.managed == nil) || groupWanted && (!live.VerificationGroupSupported || deps.managedGroup == nil) {
		o.State = "verification-managed-tests-pending"
		o.Checks = append(o.Checks, check{"verification.managed_components", "pending", true, "running build lacks a requested managed verifier; no test mutation performed"})
		return o, pendingCode
	}
	if !verification.ValidRequestID(opts.RequestID) {
		o.State = "verification-request-id-required"
		o.NextSteps = append(o.NextSteps, "generate and retain a cryptographically random 32-character lowercase hexadecimal request ID; rerun with --request-id and only the explicitly authorized card/group flags")
		return o, 2
	}
	data := o.Data.(map[string]any)
	data["read_only"] = !writes
	data["request_id"] = opts.RequestID
	cardSeen, groupSeen := false, false
	allObserved, changed, failed, unavailable := true, false, false, false
	if cardWanted {
		p, err := deps.managed(ctx, cfg, identity, opts.RequestID, opts.Card)
		switch {
		case !opts.Card && errors.Is(err, verification.ErrNotFound):
			o.Checks = append(o.Checks, check{"verification.managed_card", "skipped", false, "no card journal for this request ID; query performed no mutation"})
		case err != nil:
			unavailable = true
			allObserved = false
			o.Checks = append(o.Checks, check{"verification.managed_card", "pending", true, "managed result unavailable; retain original request ID and query without authorization flags; no assumption of rejection or replacement request"})
		default:
			cardSeen = true
			data["verification_card"] = p
			o.Checks = append(o.Checks, check{"verification.test_card", p.CardStatus, true, "journaled owner-only card send"}, check{"verification.card_callback", p.CallbackStatus, true, "actual owner callback matching the confirmed card message ID"}, check{"verification.private_message", p.PrivateMessageStatus, true, "actual owner private-chat command matching this probe token"})
			observed := p.CallbackStatus == "pass" && p.PrivateMessageStatus == "pass" && p.CardStatus == "pass"
			allObserved = allObserved && observed
			changed = changed || p.BuildChanged
			failed = failed || p.CardStatus == "fail"
			switch {
			case p.BuildChanged:
				o.NextSteps = append(o.NextSteps, "card probe belongs to another binary build; retain historical evidence without re-sending its request ID")
			case p.CardStatus == "fail":
				o.NextSteps = append(o.NextSteps, "test card was explicitly rejected; fix prerequisites before another explicitly authorized probe; retain this immutable record")
			case p.State == "unknown":
				o.NextSteps = append(o.NextSteps, "card send outcome is unknown; retain original request ID and query without authorization flags; no resend or standalone fallback")
			case observed:
				o.NextSteps = append(o.NextSteps, "owner-only card, callback and private-message components observed; full P1 scenarios and UI acceptance remain pending")
			case p.Expired:
				o.NextSteps = append(o.NextSteps, "card probe expired without all observations; retain its record; a new send requires a new ID and explicit card authorization")
			default:
				o.NextSteps = append(o.NextSteps, "click the private test card and send its /verify command within 5 minutes, then query the original --request-id without authorization flags")
			}
		}
	}
	if groupWanted {
		var p verification.GroupResult
		var err error
		if ctx.Err() != nil {
			err = ctx.Err()
		} else {
			p, err = deps.managedGroup(ctx, cfg, identity, opts.RequestID, opts.Group)
		}
		switch {
		case !opts.Group && errors.Is(err, verification.ErrNotFound):
			o.Checks = append(o.Checks, check{"verification.managed_group", "skipped", false, "no group journal for this request ID; query performed no mutation"})
		case err != nil:
			unavailable = true
			allObserved = false
			o.Checks = append(o.Checks, check{"verification.managed_group", "pending", true, "managed group result unavailable; retain original request ID and query without authorization flags; no standalone fallback or assumed rejection"})
		default:
			groupSeen = true
			data["verification_group"] = p
			o.Checks = append(o.Checks, check{"verification.test_group", p.GroupStatus, true, "journaled owner-and-bot group creation"}, check{"verification.group_membership", p.MembershipStatus, true, "positive GET evidence of bot and owner with no additional people"}, check{"verification.group_message", p.GroupMessageStatus, true, "actual owner @ message in this exact confirmed test group"})
			observed := p.GroupStatus == "pass" && p.MembershipStatus == "pass" && p.GroupMessageStatus == "pass"
			allObserved = allObserved && observed
			changed = changed || p.BuildChanged
			failed = failed || p.GroupStatus == "fail"
			switch {
			case p.BuildChanged:
				o.NextSteps = append(o.NextSteps, "group probe belongs to another binary build; retain historical evidence without reusing its intent for current-build verification")
			case p.GroupStatus == "fail":
				o.NextSteps = append(o.NextSteps, "test group creation was explicitly rejected; retain this immutable request and fix prerequisites before any new authorized intent")
			case p.State == "unknown":
				o.NextSteps = append(o.NextSteps, "group creation outcome is unknown; retain original ID and query without authorization flags; never search by name or create a replacement automatically")
			case observed:
				o.NextSteps = append(o.NextSteps, "test group, initial membership and owner @ components observed; this group has no Codex thread binding and does not prove App-origin automatic discovery")
			case p.Expired:
				o.NextSteps = append(o.NextSteps, "group probe expired without all observations; retain the test group and evidence; another creation needs explicit authorization and a new ID")
			case p.MembershipStatus != "pass":
				o.NextSteps = append(o.NextSteps, "group is confirmed but owner-and-bot membership is not proven; a repeated authorized group request may retry GET checks only, never group creation")
			default:
				o.NextSteps = append(o.NextSteps, "in the confirmed test group @ the bot with /verify-group "+opts.RequestID+" within 5 minutes, then query the original --request-id without authorization flags")
			}
		}
	}
	kind := "components"
	scope := "managed-owner-components"
	if cardSeen && !groupSeen && !unavailable {
		kind = "card"
		scope = "managed-owner-card-components"
	} else if groupSeen && !cardSeen && !unavailable {
		kind = "group"
		scope = "managed-owner-group-components"
	} else if cardWanted && !groupWanted {
		kind = "card"
		scope = "managed-owner-card-components"
	} else if groupWanted && !cardWanted {
		kind = "group"
		scope = "managed-owner-group-components"
	}
	data["verification_scope"] = scope
	o.State = "verification-managed-" + kind + "-pending"
	if changed {
		o.State = "verification-managed-" + kind + "-other-build"
	} else if allObserved && (cardSeen || groupSeen) {
		o.State = "verification-managed-" + kind + "-observed"
	} else if !cardSeen && !groupSeen && !unavailable {
		o.State = "verification-managed-probe-not-found"
	}
	if failed {
		return o, 4
	}
	return o, pendingCode
}
