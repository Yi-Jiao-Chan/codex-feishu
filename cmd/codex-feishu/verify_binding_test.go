package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/config"
	"codex-feishu/internal/control"
	"codex-feishu/internal/store"
	"codex-feishu/internal/verification"
)

func bindingCLIResult(id string) verification.BindingResult {
	now := time.Now()
	return verification.BindingResult{RequestID: id, BridgeCommit: "build", BinarySHA256: strings.Repeat("b", 64), BindingStatus: "pass", ThreadID: "existing-thread", ChatID: "existing-chat", BoundAtMS: now.Add(-time.Minute).UnixMilli(), ExpiresAtMS: now.Add(4 * time.Minute).UnixMilli(), ResourcesReady: true, CreatorConnected: true}
}

func bindingCLIFixture(t *testing.T) (string, config.Config, verifyDependencies) {
	t.Helper()
	path, cfg, doctor, _ := doctorFixture(t)
	original := doctor.health
	doctor.health = func(ctx context.Context, path string, cfg config.Config) (control.Snapshot, error) {
		live, err := original(ctx, path, cfg)
		live.VerificationCardSupported, live.VerificationGroupSupported, live.VerificationThreadCreationSupported, live.VerificationBindingSupported = true, true, true, true
		return live, err
	}
	deps := readonlyVerifyDependencies(t, doctor)
	deps.managed = func(context.Context, config.Config, string, string, bool) (verification.Result, error) {
		return verification.Result{}, verification.ErrNotFound
	}
	deps.managedGroup = func(context.Context, config.Config, string, string, bool) (verification.GroupResult, error) {
		return verification.GroupResult{}, verification.ErrNotFound
	}
	deps.managedThread = func(context.Context, config.Config, string, string, bool) (verification.NativeSessionResult, error) {
		return verification.NativeSessionResult{}, verification.ErrNotFound
	}
	deps.managedBinding = func(_ context.Context, _ config.Config, _, id string, _ bool) (verification.BindingResult, error) {
		return bindingCLIResult(id), nil
	}
	return path, cfg, deps
}

func assertBindingAcceptancePending(t *testing.T, o output) {
	t.Helper()
	if o.OK {
		t.Fatal("binding promoted acceptance")
	}
	count := 0
	for _, check := range o.Checks {
		if strings.HasPrefix(check.ID, "acceptance.P1-") {
			count++
			if !check.Required || check.Status != "pending" {
				t.Fatal("binding promoted a P1 scenario", check)
			}
		}
		if check.ID == "verification.live_ui" && check.Status != "pending" {
			t.Fatal("binding promoted real UI", check)
		}
	}
	if count != 12 {
		t.Fatalf("P1 checks=%d", count)
	}
}

func TestVerifyBindingFlagIsIndependentOfComponentBirth(t *testing.T) {
	path, cfg, deps := bindingCLIFixture(t)
	id := store.ID()
	var begins [4]int
	deps.managed = func(_ context.Context, _ config.Config, _, request string, begin bool) (verification.Result, error) {
		if request != id || !begin {
			t.Fatal("unexpected card operation")
		}
		begins[0]++
		return verification.Result{RequestID: id, CardStatus: "pass", CallbackStatus: "pass", PrivateMessageStatus: "pass"}, nil
	}
	deps.managedGroup = func(_ context.Context, _ config.Config, _, request string, begin bool) (verification.GroupResult, error) {
		if request != id || !begin {
			t.Fatal("unexpected group operation")
		}
		begins[1]++
		return verification.GroupResult{RequestID: id, GroupStatus: "pass", MembershipStatus: "pass", GroupMessageStatus: "pass"}, nil
	}
	deps.managedThread = func(_ context.Context, _ config.Config, _, request string, begin bool) (verification.NativeSessionResult, error) {
		if request != id || !begin {
			t.Fatal("unexpected thread operation")
		}
		begins[2]++
		return threadCLIResult(id), nil
	}
	deps.managedBinding = func(_ context.Context, got config.Config, identity, request string, begin bool) (verification.BindingResult, error) {
		if request != id || !begin || identity == "" || got.Access.OwnerOpenID != cfg.Access.OwnerOpenID {
			t.Fatal("unexpected binding operation")
		}
		begins[3]++
		result := bindingCLIResult(id)
		result.ActivationRequested = true
		return result, nil
	}
	var emitted output
	deps.emit = func(o output) { emitted = o }
	if code := runVerifyWithDependencies(context.Background(), []string{"--config", path, "--allow-test-binding", "--request-id", id}, deps); code != 5 || emitted.State != "verification-managed-binding-observed" || begins != [4]int{0, 0, 0, 1} {
		t.Fatal("binding flag authorized component creation", emitted, code, begins)
	}
	assertBindingAcceptancePending(t, emitted)
	if data := emitted.Data.(map[string]any); data["verification_scope"] != "managed-owner-binding-components" || data["read_only"] != false || data["verification_binding"] == nil {
		t.Fatal(data)
	}
	o, code := inspectVerify(context.Background(), path, verifyOptions{Group: true, Thread: true, RequestID: id}, deps)
	if code != 5 || begins != [4]int{0, 1, 1, 1} || o.Data.(map[string]any)["verification_binding"] != nil {
		t.Fatal("paired creation implicitly bound", o, code, begins)
	}
	o, code = inspectVerify(context.Background(), path, verifyOptions{Card: true, Group: true, Thread: true, Binding: true, RequestID: id}, deps)
	if code != 5 || begins != [4]int{1, 2, 2, 2} || o.State != "verification-managed-components-observed" {
		t.Fatal("combined independent flags", o, code, begins)
	}
	assertBindingAcceptancePending(t, o)
	if _, err := os.Stat(cfg.Storage.DatabaseFile); !os.IsNotExist(err) {
		t.Fatal("CLI created a journal", err)
	}
}

func TestVerifyBindingPreflightPreventsAllPartialMutations(t *testing.T) {
	for _, missing := range []string{"card-cap", "group-cap", "thread-cap", "binding-cap", "card-dep", "group-dep", "thread-dep", "binding-dep"} {
		t.Run(missing, func(t *testing.T) {
			path, _, deps := bindingCLIFixture(t)
			original := deps.doctor.health
			deps.doctor.health = func(ctx context.Context, path string, cfg config.Config) (control.Snapshot, error) {
				live, err := original(ctx, path, cfg)
				switch missing {
				case "card-cap":
					live.VerificationCardSupported = false
				case "group-cap":
					live.VerificationGroupSupported = false
				case "thread-cap":
					live.VerificationThreadCreationSupported = false
				case "binding-cap":
					live.VerificationBindingSupported = false
				}
				return live, err
			}
			deps.managed = func(context.Context, config.Config, string, string, bool) (verification.Result, error) {
				t.Fatal("partial card operation")
				return verification.Result{}, nil
			}
			deps.managedGroup = func(context.Context, config.Config, string, string, bool) (verification.GroupResult, error) {
				t.Fatal("partial group operation")
				return verification.GroupResult{}, nil
			}
			deps.managedThread = func(context.Context, config.Config, string, string, bool) (verification.NativeSessionResult, error) {
				t.Fatal("partial thread operation")
				return verification.NativeSessionResult{}, nil
			}
			deps.managedBinding = func(context.Context, config.Config, string, string, bool) (verification.BindingResult, error) {
				t.Fatal("partial binding operation")
				return verification.BindingResult{}, nil
			}
			switch missing {
			case "card-dep":
				deps.managed = nil
			case "group-dep":
				deps.managedGroup = nil
			case "thread-dep":
				deps.managedThread = nil
			case "binding-dep":
				deps.managedBinding = nil
			}
			o, code := inspectVerify(context.Background(), path, verifyOptions{Card: true, Group: true, Thread: true, Binding: true, RequestID: store.ID()}, deps)
			if code != 5 || o.State != "verification-managed-tests-pending" {
				t.Fatal(o, code)
			}
			assertBindingAcceptancePending(t, o)
		})
	}
}

func TestVerifyBindingQueriesAllSupportedWithoutActivation(t *testing.T) {
	path, _, deps := bindingCLIFixture(t)
	id := store.ID()
	var reads [4]int
	deps.managed = func(_ context.Context, _ config.Config, _, _ string, begin bool) (verification.Result, error) {
		if begin {
			t.Fatal("query created card")
		}
		reads[0]++
		return verification.Result{}, verification.ErrNotFound
	}
	deps.managedGroup = func(_ context.Context, _ config.Config, _, _ string, begin bool) (verification.GroupResult, error) {
		if begin {
			t.Fatal("query created group")
		}
		reads[1]++
		return verification.GroupResult{}, verification.ErrNotFound
	}
	deps.managedThread = func(_ context.Context, _ config.Config, _, _ string, begin bool) (verification.NativeSessionResult, error) {
		if begin {
			t.Fatal("query created thread")
		}
		reads[2]++
		return verification.NativeSessionResult{}, verification.ErrNotFound
	}
	deps.managedBinding = func(_ context.Context, _ config.Config, _, request string, begin bool) (verification.BindingResult, error) {
		if begin || request != id {
			t.Fatal("query registered/activated binding")
		}
		reads[3]++
		result := bindingCLIResult(id)
		result.ActorStarted = true
		return result, nil
	}
	if o, code := inspectVerify(context.Background(), path, verifyOptions{}, deps); code != 5 || reads != [4]int{} || o.Data.(map[string]any)["read_only"] != true {
		t.Fatal("default verify attempted a managed operation", o, code, reads)
	}
	o, code := inspectVerify(context.Background(), path, verifyOptions{RequestID: id}, deps)
	if code != 5 || o.State != "verification-managed-binding-observed" || reads != [4]int{1, 1, 1, 1} || o.Data.(map[string]any)["read_only"] != true {
		t.Fatal(o, code, reads)
	}
	assertBindingAcceptancePending(t, o)
	for _, check := range o.Checks {
		if check.ID == "verification.binding_actor_started" && (check.Required || check.Status != "pass" || !strings.Contains(check.Detail.(string), "does not prove")) {
			t.Fatal("actor status masqueraded as UI", check)
		}
		if check.ID == "verification.binding_activation_requested" && check.Status != "pending" {
			t.Fatal("query inferred activation", check)
		}
	}
	deps.managedBinding = func(_ context.Context, _ config.Config, _, _ string, begin bool) (verification.BindingResult, error) {
		if begin {
			t.Fatal("absent query registered")
		}
		return verification.BindingResult{}, verification.ErrNotFound
	}
	o, code = inspectVerify(context.Background(), path, verifyOptions{RequestID: id}, deps)
	if code != 5 || o.State != "verification-managed-probe-not-found" {
		t.Fatal(o, code)
	}
	found := false
	for _, check := range o.Checks {
		if check.ID == "verification.managed_binding" {
			found = check.Status == "skipped" && !check.Required
		}
	}
	if !found {
		t.Fatal("absent binding was not skipped")
	}
}

func TestVerifyBindingHistoryAndDegradedDiagnosisKeepPending(t *testing.T) {
	for _, state := range []string{"other-build", "expired", "resources-lost", "disconnected", "future-window", "mapping-missing", "not-found", "error", "diagnosis"} {
		t.Run(state, func(t *testing.T) {
			path, _, deps := bindingCLIFixture(t)
			original := deps.doctor.health
			if state == "diagnosis" {
				deps.doctor.health = func(ctx context.Context, path string, cfg config.Config) (control.Snapshot, error) {
					live, err := original(ctx, path, cfg)
					live.NativeConnected = false
					return live, err
				}
			}
			calls := 0
			deps.managedBinding = func(_ context.Context, _ config.Config, _, id string, begin bool) (verification.BindingResult, error) {
				if begin {
					t.Fatal("query was a mutation")
				}
				calls++
				p := bindingCLIResult(id)
				switch state {
				case "other-build":
					p.BuildChanged = true
				case "expired":
					p.Expired = true
					p.ExpiresAtMS = time.Now().Add(-time.Second).UnixMilli()
				case "resources-lost":
					p.ResourcesReady = false
				case "disconnected", "diagnosis":
					p.CreatorConnected = false
				case "future-window":
					p.BoundAtMS = time.Now().Add(time.Minute).UnixMilli()
				case "mapping-missing":
					p.BindingStatus = "pending"
				case "not-found":
					return verification.BindingResult{}, verification.ErrNotFound
				case "error":
					return verification.BindingResult{}, errors.New("PRIVATE connection/model payload")
				}
				return p, nil
			}
			id := store.ID()
			o, code := inspectVerify(context.Background(), path, verifyOptions{RequestID: id}, deps)
			if code != 5 || calls != 1 || o.State == "verification-managed-binding-observed" || o.Data.(map[string]any)["read_only"] != true {
				t.Fatal(o, code, calls)
			}
			if state == "other-build" && o.State != "verification-managed-binding-other-build" {
				t.Fatal("old build misreported", o.State)
			}
			assertBindingAcceptancePending(t, o)
			wire, _ := json.Marshal(o)
			if strings.Contains(string(wire), "PRIVATE") {
				t.Fatal("diagnostic leaked error")
			}
			if state == "diagnosis" {
				inspectVerify(context.Background(), path, verifyOptions{Binding: true, RequestID: id}, deps)
				if calls != 1 {
					t.Fatal("degraded diagnosis authorized binding")
				}
			}
		})
	}
}

func TestVerifyBindingNeverFallsBackToLegacyPreparation(t *testing.T) {
	path, cfg, doctor, _ := doctorFixture(t)
	createLegacyVerificationDB(t, cfg.Storage.DatabaseFile)
	doctor.health = func(context.Context, string, config.Config) (control.Snapshot, error) {
		return control.Snapshot{}, control.ErrUnavailable
	}
	doctor.snapshot = defaultDoctorDependencies().snapshot
	deps := readonlyVerifyDependencies(t, doctor)
	for _, opts := range []verifyOptions{{Binding: true, RequestID: store.ID()}, {Card: true, Group: true, Thread: true, Binding: true, RequestID: store.ID()}} {
		o, code := inspectVerify(context.Background(), path, opts, deps)
		if code != 5 || o.State != "verification-managed-probe-unavailable" || o.Data.(map[string]any)["read_only"] != true {
			t.Fatal("binding fell back to legacy", o, code)
		}
	}
}

func TestVerifyBindingInvalidArgumentsHaveNoDependencies(t *testing.T) {
	for _, args := range [][]string{{"--config", "/unused", "--allow-test-binding"}, {"--config", "/unused", "--allow-test-binding", "--request-id", "BAD"}, {"--config", "/unused", "--allow-test-binding", "--request-id", store.ID(), "--thread", "arbitrary"}, {"--config", "/unused", "--allow-test-binding", "--request-id", store.ID(), "--prompt", "text"}} {
		var result output
		deps := verifyDependencies{emit: func(o output) { result = o }}
		if code := runVerifyWithDependencies(context.Background(), args, deps); code != 2 || result.State != "invalid-arguments" {
			t.Fatal(result, code)
		}
	}
}

func TestVersionManagedBindingKeepsReleaseAndUIGates(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	original := os.Stdout
	os.Stdout = writer
	code := run(context.Background(), []string{"version", "--json"})
	os.Stdout = original
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	var result output
	if err := json.NewDecoder(reader).Decode(&result); err != nil || code != 0 {
		t.Fatal(err, code)
	}
	data := result.Data.(map[string]any)
	capabilities := data["capabilities"].(map[string]any)
	if capabilities["candidate_deployment"] != true || capabilities["verify_managed_binding"] != true || capabilities["verify_managed_tests"] != false || capabilities["verify_managed_thread_ui_verified"] != false || data["phase1_service_ui_verified"] != false || data["phase1_released"] != false {
		t.Fatal("capability promoted release/UI", data)
	}
}
