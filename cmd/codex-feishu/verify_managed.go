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
	writes := opts.Card || opts.Group || opts.Thread
	cardWanted := opts.Card || !writes && live.VerificationCardSupported
	groupWanted := opts.Group || !writes && live.VerificationGroupSupported
	threadWanted := opts.Thread || !writes && live.VerificationThreadCreationSupported
	// Check every requested capability before the first mutation. In particular,
	// a combined request against an older card-only build must not partially send.
	if !cardWanted && !groupWanted && !threadWanted || cardWanted && (!live.VerificationCardSupported || deps.managed == nil) || groupWanted && (!live.VerificationGroupSupported || deps.managedGroup == nil) || threadWanted && (!live.VerificationThreadCreationSupported || deps.managedThread == nil) {
		o.State = "verification-managed-tests-pending"
		o.Checks = append(o.Checks, check{"verification.managed_components", "pending", true, "running build lacks a requested managed verifier; no test mutation performed"})
		return o, pendingCode
	}
	if !verification.ValidRequestID(opts.RequestID) {
		o.State = "verification-request-id-required"
		o.NextSteps = append(o.NextSteps, "generate and retain a cryptographically random 32-character lowercase hexadecimal request ID; rerun with --request-id and only the explicitly authorized component flags")
		return o, 2
	}
	data := o.Data.(map[string]any)
	data["read_only"] = !writes
	data["request_id"] = opts.RequestID
	cardSeen, groupSeen, threadSeen := false, false, false
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
	if threadWanted {
		var p verification.NativeSessionResult
		var err error
		if ctx.Err() != nil {
			err = ctx.Err()
		} else {
			p, err = deps.managedThread(ctx, cfg, identity, opts.RequestID, opts.Thread)
		}
		switch {
		case !opts.Thread && errors.Is(err, verification.ErrNotFound):
			o.Checks = append(o.Checks, check{"verification.managed_thread_creation", "skipped", false, "no thread journal for this request ID; query performed no mutation"})
		case err != nil:
			unavailable, allObserved = true, false
			o.Checks = append(o.Checks, check{"verification.managed_thread_creation", "pending", true, "managed thread result unavailable; retain original ID and query without authorization flags; no new connection, replacement workspace or standalone fallback"})
		default:
			threadSeen = true
			data["verification_thread_creation"] = p
			resources, connection := "pending", "pending"
			if p.ResourcesReady && !p.Expired {
				resources = "pass"
			}
			if p.CreatorConnected {
				connection = "pass"
			}
			o.Checks = append(o.Checks, check{"verification.thread_birth", p.CreationStatus, true, "journaled isolated readonly local-model thread creation; no turn, input or group binding"}, check{"verification.thread_resources", resources, true, "original private workspace and local-model resources in the current verification window"}, check{"verification.thread_creator_connection", connection, true, "original creator connection identity is still current; this does not prove App UI or attachment recovery"})
			observed := p.CreationStatus == "pass" && resources == "pass" && connection == "pass"
			allObserved = allObserved && observed
			changed = changed || p.BuildChanged
			failed = failed || p.CreationStatus == "fail"
			switch {
			case p.BuildChanged:
				o.NextSteps = append(o.NextSteps, "thread birth belongs to another binary build; retain historical proof without reusing its intent for a current-build test")
			case p.CreationStatus == "fail":
				o.NextSteps = append(o.NextSteps, "native creation was explicitly rejected; retain this record and repair prerequisites before any new explicitly authorized intent")
			case p.State == "unknown":
				o.NextSteps = append(o.NextSteps, "native creation outcome is unknown; query original ID without authorization flags; never recreate or archive an unproven thread")
			case p.ResourceRecoveryRequired || !p.CreatorConnected:
				o.NextSteps = append(o.NextSteps, "original model resources or creator connection unavailable; preserve birth history, private workspace and journal; do not replace its endpoint or infer fresh binding")
			case p.Expired:
				o.NextSteps = append(o.NextSteps, "thread creation verification window expired; retain original resources and evidence; a new creation requires a new ID and explicit thread authorization")
			case observed:
				o.NextSteps = append(o.NextSteps, "isolated thread birth and original local resources observed; group binding, queue/steer, remote cleanup, actual App UI and complete P1 acceptance remain pending")
			default:
				o.NextSteps = append(o.NextSteps, "thread creation remains pending; retain original ID and inspect without authorization flags; no turn or input was started")
			}
		}
	}
	kind := "components"
	scope := "managed-owner-components"
	if cardSeen && !groupSeen && !threadSeen && !unavailable {
		kind = "card"
		scope = "managed-owner-card-components"
	} else if groupSeen && !cardSeen && !threadSeen && !unavailable {
		kind = "group"
		scope = "managed-owner-group-components"
	} else if threadSeen && !cardSeen && !groupSeen && !unavailable {
		kind = "thread-creation"
		scope = "managed-local-thread-creation-components"
	} else if cardWanted && !groupWanted && !threadWanted {
		kind = "card"
		scope = "managed-owner-card-components"
	} else if groupWanted && !cardWanted && !threadWanted {
		kind = "group"
		scope = "managed-owner-group-components"
	} else if threadWanted && !cardWanted && !groupWanted {
		kind = "thread-creation"
		scope = "managed-local-thread-creation-components"
	}
	data["verification_scope"] = scope
	o.State = "verification-managed-" + kind + "-pending"
	if changed {
		o.State = "verification-managed-" + kind + "-other-build"
	} else if allObserved && (cardSeen || groupSeen || threadSeen) {
		o.State = "verification-managed-" + kind + "-observed"
	} else if !cardSeen && !groupSeen && !threadSeen && !unavailable {
		o.State = "verification-managed-probe-not-found"
	}
	if failed {
		return o, 4
	}
	return o, pendingCode
}
