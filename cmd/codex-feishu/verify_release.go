package main

import (
	"context"
	"time"

	"codex-feishu/internal/config"
	"codex-feishu/internal/control"
	"codex-feishu/internal/verification"
)

func inspectManagedTurnRelease(ctx context.Context, cfg config.Config, identity string, opts verifyOptions, live control.Snapshot, deps verifyDependencies, o output, code int) (output, int) {
	if !verification.ValidRequestID(opts.RequestID) || opts.Card || opts.Group || opts.Thread || opts.Binding {
		o.State = "invalid-arguments"
		return o, 2
	}
	if !live.VerificationTurnReleaseSupported || deps.managedRelease == nil {
		o.State = "verification-turn-release-not-integrated"
		o.Checks = append(o.Checks, check{"verification.turn_release", "pending", true, "running build lacks controlled release; no mutation or standalone fallback performed"})
		return o, code
	}
	data := o.Data.(map[string]any)
	data["read_only"] = false
	data["request_id"] = opts.RequestID
	data["verification_scope"] = "managed-local-model-release-only"
	r, err := deps.managedRelease(ctx, cfg, identity, opts.RequestID)
	o.State = "verification-managed-turn-release-pending"
	status := "pending"
	if err == nil {
		data["verification_binding"] = r
		now := time.Now().UnixMilli()
		if r.RequestID == opts.RequestID && r.BindingStatus == "pass" && r.ModelReleased && !r.ActivationRequested && !r.BuildChanged && !r.Expired && r.BoundAtMS > 0 && r.BoundAtMS <= now && r.ExpiresAtMS > now && r.ResourcesReady && r.CreatorConnected {
			status = "pass"
			o.State = "verification-managed-turn-released"
		}
	}
	o.Checks = append(o.Checks, check{"verification.turn_release", status, true, "only the original local model response gate; no submission, activation or native turn completion claim"})
	o.NextSteps = append(o.NextSteps, "query the original request ID without flags and inspect actual native completion, group result and App UI; release alone does not prove any P1 scenario; never replace the original resources")
	return o, code
}
