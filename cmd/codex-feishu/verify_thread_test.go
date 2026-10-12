package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"codex-feishu/internal/config"
	"codex-feishu/internal/control"
	"codex-feishu/internal/store"
	"codex-feishu/internal/verification"
)

func threadCLIResult(id string) verification.NativeSessionResult {
	return verification.NativeSessionResult{NativeResult: verification.NativeResult{RequestID: id, BridgeCommit: "build", BinarySHA256: strings.Repeat("b", 64), State: "confirmed", CreationStatus: "pass", ThreadID: "isolated-thread", CreatorGeneration: 1, CreatorConnected: true, ExpiresAtMS: 100}, ResourcesReady: true}
}

func TestManagedThreadCreationFlagsQueryAndFullAcceptanceBoundary(t *testing.T) {
	path, cfg, d, _ := doctorFixture(t)
	original := d.health
	d.health = func(ctx context.Context, p string, c config.Config) (control.Snapshot, error) {
		s, err := original(ctx, p, c)
		s.VerificationCardSupported = true
		s.VerificationGroupSupported = true
		s.VerificationThreadCreationSupported = true
		return s, err
	}
	deps := readonlyVerifyDependencies(t, d)
	id := store.ID()
	cardReads, groupReads, threadBegins, threadReads := 0, 0, 0, 0
	deps.managed = func(_ context.Context, _ config.Config, _, _ string, begin bool) (verification.Result, error) {
		if begin {
			t.Fatal("thread flag authorized card")
		}
		cardReads++
		return verification.Result{}, verification.ErrNotFound
	}
	deps.managedGroup = func(_ context.Context, _ config.Config, _, _ string, begin bool) (verification.GroupResult, error) {
		if begin {
			t.Fatal("thread flag authorized group")
		}
		groupReads++
		return verification.GroupResult{}, verification.ErrNotFound
	}
	deps.managedThread = func(_ context.Context, got config.Config, identity, request string, begin bool) (verification.NativeSessionResult, error) {
		if got.Access.OwnerOpenID != cfg.Access.OwnerOpenID || identity == "" || request != id {
			t.Fatal("unscoped thread operation")
		}
		if begin {
			threadBegins++
		} else {
			threadReads++
		}
		return threadCLIResult(request), nil
	}
	o, code := inspectVerify(context.Background(), path, verifyOptions{Thread: true}, deps)
	if code != 2 || o.State != "verification-request-id-required" || threadBegins != 0 {
		t.Fatal(o, code)
	}
	for _, begin := range []bool{true, false} {
		o, code = inspectVerify(context.Background(), path, verifyOptions{Thread: begin, RequestID: id}, deps)
		if code != 5 || o.OK || o.State != "verification-managed-thread-creation-observed" {
			t.Fatal(o, code)
		}
		data := o.Data.(map[string]any)
		if data["read_only"] != !begin || data["verification_scope"] != "managed-local-thread-creation-components" || data["verification_thread_creation"] == nil {
			t.Fatal(data)
		}
		pending := 0
		for _, check := range o.Checks {
			if strings.HasPrefix(check.ID, "acceptance.P1-") {
				pending++
				if check.Status != "pending" || !check.Required {
					t.Fatal("birth promoted P1", check)
				}
			}
		}
		if pending != 12 {
			t.Fatal(pending)
		}
	}
	if threadBegins != 1 || threadReads != 1 || cardReads != 1 || groupReads != 1 {
		t.Fatal("wrong independently authorized components", threadBegins, threadReads, cardReads, groupReads)
	}
	if _, err := os.Stat(cfg.Storage.DatabaseFile); !os.IsNotExist(err) {
		t.Fatal("CLI opened a separate journal", err)
	}
}

func TestManagedThreadUnavailablePreventsPartialCombinedWrites(t *testing.T) {
	path, _, d, _ := doctorFixture(t)
	original := d.health
	cardStarts, groupStarts, threadStarts := 0, 0, 0
	deps := readonlyVerifyDependencies(t, d)
	deps.managed = func(context.Context, config.Config, string, string, bool) (verification.Result, error) {
		cardStarts++
		return verification.Result{}, nil
	}
	deps.managedGroup = func(context.Context, config.Config, string, string, bool) (verification.GroupResult, error) {
		groupStarts++
		return verification.GroupResult{}, nil
	}
	deps.managedThread = func(context.Context, config.Config, string, string, bool) (verification.NativeSessionResult, error) {
		threadStarts++
		return verification.NativeSessionResult{}, nil
	}
	for _, missing := range []string{"thread", "group", "card", "thread-dependency"} {
		t.Run(missing, func(t *testing.T) {
			deps.doctor.health = func(ctx context.Context, p string, c config.Config) (control.Snapshot, error) {
				s, err := original(ctx, p, c)
				s.VerificationCardSupported = missing != "card"
				s.VerificationGroupSupported = missing != "group"
				s.VerificationThreadCreationSupported = missing != "thread"
				return s, err
			}
			thread := deps.managedThread
			if missing == "thread-dependency" {
				deps.managedThread = nil
			}
			defer func() { deps.managedThread = thread }()
			o, code := inspectVerify(context.Background(), path, verifyOptions{Card: true, Group: true, Thread: true, RequestID: store.ID()}, deps)
			if code != 5 || o.OK || cardStarts != 0 || groupStarts != 0 || threadStarts != 0 {
				t.Fatal("unsupported combined request partially mutated", o, code)
			}
		})
	}
}

func TestManagedThreadHistoryDoesNotImplyCurrentResourcesOrUI(t *testing.T) {
	for _, outcome := range []string{"unknown", "rejected", "recovered", "disconnected", "expired", "other-build", "not-found", "error"} {
		t.Run(outcome, func(t *testing.T) {
			path, _, d, _ := doctorFixture(t)
			original := d.health
			d.health = func(ctx context.Context, p string, c config.Config) (control.Snapshot, error) {
				s, err := original(ctx, p, c)
				s.VerificationThreadCreationSupported = true
				return s, err
			}
			deps := readonlyVerifyDependencies(t, d)
			id := store.ID()
			deps.managedThread = func(_ context.Context, _ config.Config, _, got string, begin bool) (verification.NativeSessionResult, error) {
				if begin || got != id {
					t.Fatal("query mutated thread")
				}
				p := threadCLIResult(got)
				switch outcome {
				case "unknown":
					p.State = "unknown"
					p.CreationStatus = "pending"
					p.ThreadID = ""
				case "rejected":
					p.State = "rejected"
					p.CreationStatus = "fail"
					p.ThreadID = ""
				case "recovered":
					p.ResourcesReady = false
					p.ResourceRecoveryRequired = true
				case "disconnected":
					p.CreatorConnected = false
				case "expired":
					p.Expired = true
				case "other-build":
					p.BuildChanged = true
				case "not-found":
					return verification.NativeSessionResult{}, verification.ErrNotFound
				case "error":
					return verification.NativeSessionResult{}, errors.New("PRIVATE model URL and body")
				}
				return p, nil
			}
			o, code := inspectVerify(context.Background(), path, verifyOptions{RequestID: id}, deps)
			want := 5
			if outcome == "rejected" {
				want = 4
			}
			if code != want || o.OK || o.State == "verification-managed-thread-creation-observed" {
				t.Fatal("incomplete state promoted", o, code)
			}
			b, _ := json.Marshal(o)
			if strings.Contains(string(b), "PRIVATE") {
				t.Fatal("raw error escaped")
			}
		})
	}
}

func TestManagedThreadQueryRemainsReadOnlyWhenNativeDoctorDegraded(t *testing.T) {
	path, _, d, _ := doctorFixture(t)
	original := d.health
	d.health = func(ctx context.Context, p string, c config.Config) (control.Snapshot, error) {
		s, err := original(ctx, p, c)
		s.VerificationThreadCreationSupported = true
		s.NativeConnected = false
		return s, err
	}
	deps := readonlyVerifyDependencies(t, d)
	reads := 0
	deps.managedThread = func(_ context.Context, _ config.Config, _, id string, begin bool) (verification.NativeSessionResult, error) {
		if begin {
			t.Fatal("degraded doctor allowed mutation")
		}
		reads++
		p := threadCLIResult(id)
		p.CreatorConnected = false
		return p, nil
	}
	id := store.ID()
	o, code := inspectVerify(context.Background(), path, verifyOptions{RequestID: id}, deps)
	if code != 5 || o.OK || reads != 1 || o.Data.(map[string]any)["read_only"] != true {
		t.Fatal(o, code, reads)
	}
	inspectVerify(context.Background(), path, verifyOptions{Thread: true, RequestID: id}, deps)
	if reads != 1 {
		t.Fatal("degraded native initiated creation")
	}
}
