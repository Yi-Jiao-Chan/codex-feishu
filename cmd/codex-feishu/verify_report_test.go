package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-feishu/compat"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/control"
	"codex-feishu/internal/store"
)

func reportFixture(t *testing.T) (string, acceptanceReport, verifyDependencies) {
	t.Helper()
	path, cfg, d, _ := doctorFixture(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	a := verificationArtifact{Commit: strings.Repeat("a", 40), SHA256: strings.Repeat("b", 64)}
	originalHealth := d.health
	d.health = func(ctx context.Context, path string, cfg config.Config) (control.Snapshot, error) {
		s, err := originalHealth(ctx, path, cfg)
		s.BridgeCommit, s.BinarySHA256 = a.Commit, a.SHA256
		return s, err
	}
	originalDaemon := d.discover
	d.discover = func(ctx context.Context, path string) (codexclient.Daemon, error) {
		s, err := originalDaemon(ctx, path)
		s.CLIVersion = "0.162.0"
		return s, err
	}
	deps := readonlyVerifyDependencies(t, d)
	deps.artifact = func() (verificationArtifact, error) { return a, nil }
	identity, err := control.Identity(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := acceptanceReport{SchemaVersion: 1, Phase: "phase1", ArtifactCommit: a.Commit, BinarySHA256: a.SHA256, BridgeVersion: "0.0.1-dev", CodexCLIVersion: "0.162.0", CodexAppServerVersion: "0.162.0", ProtocolSchemaDigest: compat.ProtocolSchemaDigest, FeishuSDKVersion: compat.FeishuManifest().SDKVersion, HostID: cfg.Host.ID, AppID: cfg.Feishu.AppID, OwnerID: cfg.Access.OwnerOpenID, ConfigurationIdentity: identity, DatabaseSchema: store.SchemaVersion, RequiredCaseIDs: append([]string{}, phase1CaseIDs...)}
	file := filepath.Join(t.TempDir(), "manual-fixture.json")
	// Test substitute only; it deliberately does not represent actual UI proof.
	body := []byte(`{"fixture_only":true,"note":"PRIVATE mock confirmation"}`)
	if err := os.WriteFile(file, body, 0600); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(body)
	for _, id := range phase1CaseIDs {
		r.Cases = append(r.Cases, acceptanceCase{ID: id, Status: "pass", Evidence: []acceptanceEvidence{{Kind: "manual", Path: file, SHA256: hex.EncodeToString(h[:]), ObservedBy: cfg.Access.OwnerOpenID}}})
	}
	return path, r, deps
}

func writeReport(t *testing.T, report acceptanceReport) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "acceptance.json")
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAcceptanceReportValidatesRecordedEvidenceWithoutWritesOrRelease(t *testing.T) {
	path, r, deps := reportFixture(t)
	file := writeReport(t, r)
	var o output
	deps.emit = func(value output) { o = value }
	if code := runVerifyWithDependencies(context.Background(), []string{"--config", path, "--acceptance-report", file}, deps); code != 0 || !o.OK || o.State != "acceptance-report-validated" {
		t.Fatal("recorded evidence did not finish requested validation", o.State, code)
	}
	if o.Data.(map[string]any)["phase1_released"] != false || phase1Released || o.Data.(map[string]any)["ui_evidence_source"] != "explicit-manual-report" {
		t.Fatal("report promoted release or claimed automatic UI observation", o)
	}
	count := 0
	for _, c := range o.Checks {
		if strings.HasPrefix(c.ID, "acceptance.P1-") {
			count++
			if c.Status != "pass" || !c.Required || c.Detail.(map[string]any)["independently_observed_ui"] != false {
				t.Fatal("incorrect report provenance", c)
			}
		}
	}
	if count != 12 {
		t.Fatal("omitted case", count)
	}
	wire, _ := json.Marshal(o)
	for _, private := range []string{"PRIVATE", r.OwnerID, r.AppID, file, r.Cases[0].Evidence[0].Path} {
		if strings.Contains(string(wire), private) {
			t.Fatal("report leaked private evidence")
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "bridge.db")); !os.IsNotExist(err) {
		t.Fatal("report validation opened or migrated a database")
	}
}

func TestAcceptanceReportRejectsStaleContextAndUnknownProviders(t *testing.T) {
	for _, change := range []string{"commit", "binary", "host", "app", "owner", "configuration", "protocol", "cli", "sdk", "live-build", "old-provider", "unknown-cli", "artifact-unavailable"} {
		t.Run(change, func(t *testing.T) {
			path, r, deps := reportFixture(t)
			want := 6
			switch change {
			case "commit":
				r.ArtifactCommit = strings.Repeat("c", 40)
			case "binary":
				r.BinarySHA256 = strings.Repeat("c", 64)
			case "host":
				r.HostID = "other-host"
			case "app":
				r.AppID = "cli_other_application"
			case "owner":
				r.OwnerID = "ou_other_owner"
			case "configuration":
				r.ConfigurationIdentity = strings.Repeat("c", 64)
			case "protocol":
				r.ProtocolSchemaDigest = strings.Repeat("c", 64)
			case "cli":
				r.CodexCLIVersion = "other-version"
			case "sdk":
				r.FeishuSDKVersion = "other-sdk"
			case "live-build", "old-provider":
				original := deps.doctor.health
				deps.doctor.health = func(ctx context.Context, p string, c config.Config) (control.Snapshot, error) {
					s, err := original(ctx, p, c)
					if change == "old-provider" {
						s.BinarySHA256 = ""
					} else {
						s.BinarySHA256 = strings.Repeat("c", 64)
					}
					return s, err
				}
				if change == "old-provider" {
					want = 5
				}
			case "unknown-cli":
				original := deps.doctor.discover
				deps.doctor.discover = func(ctx context.Context, path string) (codexclient.Daemon, error) {
					d, err := original(ctx, path)
					d.CLIVersion = ""
					return d, err
				}
				want = 5
			case "artifact-unavailable":
				deps.artifact = func() (verificationArtifact, error) {
					return verificationArtifact{}, errors.New("PRIVATE binary error")
				}
				want = 5
			}
			o, code := inspectVerify(context.Background(), path, verifyOptions{Report: &r}, deps)
			if code != want || o.OK {
				t.Fatal("stale/unknown context passed", change, code, o)
			}
		})
	}
}

func TestAcceptanceReportDoesNotOverrideCurrentFailures(t *testing.T) {
	for _, failure := range []string{"permission", "disconnect", "no-instance"} {
		t.Run(failure, func(t *testing.T) {
			path, r, deps := reportFixture(t)
			want := 5
			if failure == "permission" {
				deps.doctor.gateway = func(config.Config, string) doctorGateway { return &doctorFixtureGateway{} }
				want = 4
			} else if failure == "no-instance" {
				deps.doctor.health = func(context.Context, string, config.Config) (control.Snapshot, error) {
					return control.Snapshot{}, control.ErrUnavailable
				}
				deps.doctor.snapshot = func(context.Context, config.Config) (store.PersistentHealth, error) {
					return store.PersistentHealth{}, errDoctorNoDatabase
				}
			} else {
				original := deps.doctor.health
				deps.doctor.health = func(ctx context.Context, p string, c config.Config) (control.Snapshot, error) {
					s, err := original(ctx, p, c)
					s.NativeConnected = false
					return s, err
				}
			}
			o, code := inspectVerify(context.Background(), path, verifyOptions{Report: &r}, deps)
			if code != want || o.OK {
				t.Fatal("report overrode current failure", failure, code, o)
			}
		})
	}
}

func TestAcceptanceReportCannotSubstituteCountersForManualOrAlteredEvidence(t *testing.T) {
	for _, change := range []string{"pending-case", "failed-case", "automated-only", "missing-file", "changed-file", "nonprivate-file"} {
		t.Run(change, func(t *testing.T) {
			path, r, deps := reportFixture(t)
			e := &r.Cases[0].Evidence[0]
			switch change {
			case "pending-case":
				r.Cases[0].Status = "pending"
			case "failed-case":
				r.Cases[0].Status = "fail"
			case "automated-only":
				e.Kind, e.ObservedBy = "automated", ""
			case "missing-file":
				if err := os.Remove(e.Path); err != nil {
					t.Fatal(err)
				}
			case "changed-file":
				if err := os.WriteFile(e.Path, []byte("different PRIVATE evidence"), 0600); err != nil {
					t.Fatal(err)
				}
			case "nonprivate-file":
				if err := os.Chmod(e.Path, 0644); err != nil {
					t.Fatal(err)
				}
			}
			r.ReadyToRelease = true // A summary boolean must never grant success.
			o, code := inspectVerify(context.Background(), path, verifyOptions{Report: &r}, deps)
			if code != 5 || o.OK {
				t.Fatal("unproven case passed", change, code, o)
			}
		})
	}
}

func TestAcceptanceReportInputAndAuthorizationRejectedBeforeDependencies(t *testing.T) {
	for _, change := range []string{"template", "omitted", "duplicate", "unknown-case", "required-list", "owner", "empty-cli", "empty-protocol", "unknown-field", "trailing", "permissions", "symlink"} {
		t.Run(change, func(t *testing.T) {
			_, r, _ := reportFixture(t)
			switch change {
			case "template":
				r.Template = true
			case "omitted":
				r.Cases = r.Cases[:11]
			case "duplicate":
				r.Cases[1].ID = r.Cases[0].ID
			case "unknown-case":
				r.Cases[0].ID = "P1-99"
			case "required-list":
				r.RequiredCaseIDs[0] = "P1-99"
			case "owner":
				r.Cases[0].Evidence[0].ObservedBy = "ou_other_owner"
			case "empty-cli":
				r.CodexCLIVersion = ""
			case "empty-protocol":
				r.ProtocolSchemaDigest = ""
			}
			file := writeReport(t, r)
			if change == "unknown-field" || change == "trailing" {
				body, _ := os.ReadFile(file)
				if change == "unknown-field" {
					body = append([]byte(`{"unknown":true,`), body[1:]...)
				} else {
					body = append(body, []byte(` {}`)...)
				}
				if err := os.WriteFile(file, body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if change == "permissions" {
				if err := os.Chmod(file, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if change == "symlink" {
				alias := file + ".alias"
				if err := os.Symlink(file, alias); err != nil {
					t.Fatal(err)
				}
				file = alias
			}
			var o output
			deps := verifyDependencies{emit: func(value output) { o = value }}
			if code := runVerifyWithDependencies(context.Background(), []string{"--config", "/unused", "--acceptance-report", file}, deps); code != 2 || o.OK || o.State != "acceptance-report-invalid" {
				t.Fatal("invalid report reached dependencies", change, code, o)
			}
		})
	}
	for _, extra := range []string{"--allow-test-card", "--allow-test-group", "--allow-test-thread", "--allow-test-binding", "--release-test-turn", "--request-id"} {
		args := []string{"--config", "/unused", "--acceptance-report", "/unused", extra}
		if extra == "--request-id" || extra == "--allow-test-binding" || extra == "--release-test-turn" {
			args = append(args, strings.Repeat("a", 32))
		}
		var o output
		if code := runVerifyWithDependencies(context.Background(), args, verifyDependencies{emit: func(value output) { o = value }}); code != 2 || o.State != "invalid-arguments" {
			t.Fatal("report authorized a mutation/query", extra, code, o)
		}
	}
}
