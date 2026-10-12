package main

import (
	"context"
	"errors"
	"testing"

	"codex-feishu/internal/config"
	"codex-feishu/internal/control"
	"codex-feishu/internal/store"
	"codex-feishu/internal/verification"
)

func releaseCLIFixture(t *testing.T) (string, verifyDependencies) {
	t.Helper()
	path, _, deps := bindingCLIFixture(t)
	health := deps.doctor.health
	deps.doctor.health = func(ctx context.Context, path string, cfg config.Config) (control.Snapshot, error) {
		snapshot, err := health(ctx, path, cfg)
		snapshot.VerificationTurnReleaseSupported = true
		return snapshot, err
	}
	return path, deps
}

func TestVerifyReleaseRequiresItsOwnFlagAndDoesNotCreateBindOrActivate(t *testing.T) {
	path, deps := releaseCLIFixture(t)
	id := store.ID()
	calls := 0
	deps.managedRelease = func(_ context.Context, _ config.Config, identity, request string) (verification.BindingResult, error) {
		if request != id || identity == "" {
			t.Fatal("wrong release scope")
		}
		calls++
		result := bindingCLIResult(id)
		result.ModelReleased = true
		return result, nil
	}
	var result output
	deps.emit = func(o output) { result = o }
	if code := runVerifyWithDependencies(context.Background(), []string{"--config", path, "--release-test-turn", "--request-id", id}, deps); code != 5 || calls != 1 || result.State != "verification-managed-turn-released" {
		t.Fatal(result, code, calls)
	}
	assertBindingAcceptancePending(t, result)
	data := result.Data.(map[string]any)
	if data["read_only"] != false || data["verification_scope"] != "managed-local-model-release-only" {
		t.Fatal(data)
	}
	if code := runVerifyWithDependencies(context.Background(), []string{"--config", path, "--request-id", id}, deps); code != 5 || calls != 1 || result.Data.(map[string]any)["read_only"] != true {
		t.Fatal("query implicitly released", result, code, calls)
	}
	assertBindingAcceptancePending(t, result)
	if code := runVerifyWithDependencies(context.Background(), []string{"--config", path, "--allow-test-binding", "--request-id", id}, deps); code != 5 || calls != 1 {
		t.Fatal("binding implicitly released", result, code, calls)
	}
}

func TestVerifyReleaseRejectsMissingIDAndCombinedAuthorizationBeforeDiagnostics(t *testing.T) {
	for _, extra := range [][]string{nil, {"--request-id", "bad"}, {"--request-id", store.ID(), "--allow-test-card"}, {"--request-id", store.ID(), "--allow-test-group"}, {"--request-id", store.ID(), "--allow-test-thread"}, {"--request-id", store.ID(), "--allow-test-binding"}} {
		args := append([]string{"--config", "/unused", "--release-test-turn"}, extra...)
		deps := verifyDependencies{emit: func(o output) {
			if o.State != "invalid-arguments" {
				t.Fatal(o)
			}
		}}
		if code := runVerifyWithDependencies(context.Background(), args, deps); code != 2 {
			t.Fatal(args, code)
		}
	}
}

func TestVerifyReleaseMissingCapabilityOrDependencyDoesNotFallback(t *testing.T) {
	for _, missing := range []string{"capability", "dependency"} {
		t.Run(missing, func(t *testing.T) {
			path, deps := releaseCLIFixture(t)
			deps.managedRelease = func(context.Context, config.Config, string, string) (verification.BindingResult, error) {
				t.Fatal("unsupported release invoked")
				return verification.BindingResult{}, nil
			}
			if missing == "dependency" {
				deps.managedRelease = nil
			} else {
				health := deps.doctor.health
				deps.doctor.health = func(ctx context.Context, path string, cfg config.Config) (control.Snapshot, error) {
					live, err := health(ctx, path, cfg)
					live.VerificationTurnReleaseSupported = false
					return live, err
				}
			}
			o, code := inspectVerify(context.Background(), path, verifyOptions{Release: true, RequestID: store.ID()}, deps)
			if code != 5 || o.State != "verification-turn-release-not-integrated" || o.Data.(map[string]any)["read_only"] != true {
				t.Fatal(o, code)
			}
			assertBindingAcceptancePending(t, o)
		})
	}
}

func TestVerifyReleaseFailureNeverClaimsTurnCompletion(t *testing.T) {
	for _, reason := range []string{"not-found", "not-started", "not-released", "new-build", "expired", "activation", "resource-loss"} {
		t.Run(reason, func(t *testing.T) {
			path, deps := releaseCLIFixture(t)
			id := store.ID()
			calls := 0
			deps.managedRelease = func(context.Context, config.Config, string, string) (verification.BindingResult, error) {
				calls++
				r := bindingCLIResult(id)
				r.ModelReleased = true
				switch reason {
				case "not-found":
					return verification.BindingResult{}, verification.ErrNotFound
				case "not-started":
					return verification.BindingResult{}, errors.New("not ready")
				case "not-released":
					r.ModelReleased = false
				case "new-build":
					r.BuildChanged = true
				case "expired":
					r.Expired = true
				case "activation":
					r.ActivationRequested = true
				case "resource-loss":
					r.ResourcesReady = false
				}
				return r, nil
			}
			o, code := inspectVerify(context.Background(), path, verifyOptions{Release: true, RequestID: id}, deps)
			if code != 5 || calls != 1 || o.State != "verification-managed-turn-release-pending" {
				t.Fatal(o, code, calls)
			}
			assertBindingAcceptancePending(t, o)
		})
	}
}
