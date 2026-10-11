package setup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
)

func fixture(t *testing.T) (Planner, Input, Paths) {
	t.Helper()
	root := t.TempDir()
	binary, secret := filepath.Join(root, "bridge"), filepath.Join(root, "secret")
	if err := os.WriteFile(binary, []byte("fixture artifact"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("fake-secret-for-local-tests"), 0600); err != nil {
		t.Fatal(err)
	}
	var in Input
	in.SchemaVersion = 1
	in.Artifact.BinaryPath = binary
	in.Feishu.AppID = "cli_setup_" + digest([]byte(root))[:12]
	in.Feishu.AppSecretFile = secret
	owner := "ou_setup_owner"
	in.Identity.OwnerOpenID = &owner
	in.Discovery.AllowedRoots = []string{root}
	in.Features.Phase = "phase1"
	p := Planner{Probe: func(context.Context, string) (Environment, error) {
		return Environment{CLIPath: binary, Daemon: codexclient.Daemon{Status: "running", SocketPath: filepath.Join(root, "daemon.sock"), AppServerVersion: "0.162.0", CLIVersion: "0.162.0"}, Artifact: Capabilities{Version: "fixture", ConfigSchema: 1, Released: true, CodexProtocol: "0.162.0", Commands: []string{"version", "doctor", "setup", "serve", "service", "status", "verify"}}}, nil
	}}
	return p, in, Paths{ConfigFile: filepath.Join(root, "config", "config.toml"), DatabaseFile: filepath.Join(root, "data", "bridge.db")}
}

func TestPlanIsStableReadOnlyAndApplyIsAtomicAndIdempotent(t *testing.T) {
	ctx := context.Background()
	p, in, paths := fixture(t)
	in.Verification.SendTestCardToOwner = true
	a, err := p.Build(ctx, in, paths)
	if err != nil || !a.CanApply {
		t.Fatal(err)
	}
	b, err := p.Build(ctx, in, paths)
	if err != nil || a.ID != b.ID || a.Config.Host.ID != b.Config.Host.ID {
		t.Fatal("preview identity is unstable", err)
	}
	if _, err := os.Stat(filepath.Dir(paths.ConfigFile)); !os.IsNotExist(err) {
		t.Fatal("plan created a configuration directory")
	}
	jsonPlan, _ := json.Marshal(a)
	if strings.Contains(string(jsonPlan), "fake-secret-for-local-tests") {
		t.Fatal("plan exposed credentials")
	}
	result, err := p.Apply(ctx, in, paths, a.ID)
	if err != nil || !result.Changed {
		t.Fatal("apply failed", err)
	}
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil || cfg.Host.ID != a.Config.Host.ID || cfg.Storage.DatabaseFile != paths.DatabaseFile {
		t.Fatal("applied configuration differs", err)
	}
	for _, path := range []string{paths.ConfigFile, paths.ConfigFile + ".setup-state.json", paths.ConfigFile + ".setup.lock"} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("file permissions", err)
		}
	}
	result, err = p.Apply(ctx, in, paths, a.ID)
	if err != nil || result.Changed || result.State != "configuration-unchanged" {
		t.Fatal("same apply was not no-op", err)
	}
	newPlan, err := p.Build(ctx, in, Paths{ConfigFile: paths.ConfigFile, DatabaseFile: filepath.Join(t.TempDir(), "ignored.db")})
	if err != nil || newPlan.Config.Host.ID != cfg.Host.ID || newPlan.Config.Storage.DatabaseFile != cfg.Storage.DatabaseFile || newPlan.Changed {
		t.Fatal("rerun replaced deployment identity or database", err)
	}
	if _, err := os.Stat(paths.DatabaseFile); !os.IsNotExist(err) {
		t.Fatal("configuration apply opened or migrated the database")
	}
}

func TestApplyRejectsChangedArtifactSecretConfigAndAuthorization(t *testing.T) {
	for _, change := range []string{"artifact", "secret", "configuration", "test-authorization"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			p, in, paths := fixture(t)
			first, err := p.Build(ctx, in, paths)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Apply(ctx, in, paths, first.ID); err != nil {
				t.Fatal(err)
			}
			in.Discovery.AutoGroup = new(bool)
			plan, err := p.Build(ctx, in, paths)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(paths.ConfigFile)
			switch change {
			case "artifact":
				os.WriteFile(in.Artifact.BinaryPath, []byte("new artifact"), 0700)
			case "secret":
				os.WriteFile(in.Feishu.AppSecretFile, []byte("new fake secret"), 0600)
			case "configuration":
				os.WriteFile(paths.ConfigFile, append(before, []byte("\n# user edit\n")...), 0600)
				before, _ = os.ReadFile(paths.ConfigFile)
			case "test-authorization":
				in.Verification.CreatePrivateTestGroup = true
			}
			if _, err := p.Apply(ctx, in, paths, plan.ID); !errors.Is(err, ErrStalePlan) {
				t.Fatal("stale plan accepted", err)
			}
			after, _ := os.ReadFile(paths.ConfigFile)
			if string(after) != string(before) {
				t.Fatal("stale plan overwrote current configuration")
			}
		})
	}
}

func TestPreparedApplyJournalReconcilesAfterRename(t *testing.T) {
	ctx := context.Background()
	p, in, paths := fixture(t)
	plan, err := p.Build(ctx, in, paths)
	if err != nil {
		t.Fatal(err)
	}
	if err := configDirectory(filepath.Dir(paths.ConfigFile)); err != nil {
		t.Fatal(err)
	}
	journal := applyJournal{PlanID: plan.ID, SourceDigest: plan.sourceDigest, BeforeDigest: "missing", AfterDigest: digest(plan.content), State: "prepared"}
	if err := atomicFile(paths.ConfigFile+".setup-state.json", encoded(journal)); err != nil {
		t.Fatal(err)
	}
	if err := atomicFile(paths.ConfigFile, plan.content); err != nil {
		t.Fatal(err)
	}
	result, err := p.Apply(ctx, in, paths, plan.ID)
	if err != nil || result.Changed {
		t.Fatal("saved configuration was applied twice", err)
	}
	b, _ := os.ReadFile(paths.ConfigFile + ".setup-state.json")
	if err := json.Unmarshal(b, &journal); err != nil || journal.State != "applied" {
		t.Fatal("journal not reconciled", err)
	}
}

func TestConfigurationUpdatePreservesBackupAndRejectsLiveBridge(t *testing.T) {
	for _, lockKind := range []string{"none", "application", "database"} {
		t.Run(lockKind, func(t *testing.T) {
			ctx := context.Background()
			p, in, paths := fixture(t)
			first, err := p.Build(ctx, in, paths)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Apply(ctx, in, paths, first.ID); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(paths.ConfigFile)
			if err != nil {
				t.Fatal(err)
			}
			in.Discovery.AutoGroup = new(bool)
			plan, err := p.Build(ctx, in, paths)
			if err != nil || !plan.Changed {
				t.Fatal("configuration change was not planned", err)
			}
			switch lockKind {
			case "application":
				lock, err := store.AcquireApplication(in.Feishu.AppID)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			case "database":
				lock, err := store.AcquireListener(paths.DatabaseFile)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			}
			result, err := p.Apply(ctx, in, paths, plan.ID)
			if lockKind != "none" {
				if err == nil || !strings.Contains(err.Error(), "stop") {
					t.Fatal("live bridge configuration was overwritten", err)
				}
				after, readErr := os.ReadFile(paths.ConfigFile)
				if readErr != nil || string(after) != string(before) {
					t.Fatal("rejected update changed the configuration", readErr)
				}
				return
			}
			if err != nil || !result.Changed || result.BackupFile == "" {
				t.Fatal("configuration update did not preserve a backup", err)
			}
			backup, err := os.ReadFile(result.BackupFile)
			if err != nil || string(backup) != string(before) {
				t.Fatal("backup differs from the original configuration", err)
			}
			cfg, err := config.Load(paths.ConfigFile)
			if err != nil || cfg.Discovery.Enabled || cfg.Host.ID != first.Config.Host.ID {
				t.Fatal("configuration update changed the deployment identity", err)
			}
			repeated, err := p.Apply(ctx, in, paths, plan.ID)
			if err != nil || repeated.Changed || repeated.BackupFile != result.BackupFile {
				t.Fatal("repeated configuration update was not idempotent", err)
			}
		})
	}
}

func TestExistingOffPolicyIsPreservedAndExplicitQuietIsSeparate(t *testing.T) {
	ctx := context.Background()
	p, in, paths := fixture(t)
	plan, err := p.Build(ctx, in, paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(ctx, in, paths, plan.ID); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.BeginBaseline(ctx, plan.Config.Host.ID, in.Feishu.AppID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	policy, _ := rules.DefaultPolicy("Asia/Shanghai")
	policy.Enabled = false
	if _, err := s.InitNotificationPolicy(ctx, in.Feishu.AppID, *in.Identity.OwnerOpenID, policy); err != nil {
		t.Fatal(err)
	}
	preview, err := p.Build(ctx, in, paths)
	if err != nil || len(preview.PolicyChanges) != 0 || len(preview.ExistingPolicies) != 1 || preview.ExistingPolicies[0].Policy.Enabled {
		t.Fatal("off policy lost", err)
	}
	quiet := "23:00-08:00"
	in.Notifications.QuietHours = &quiet
	preview, err = p.Build(ctx, in, paths)
	if err != nil || len(preview.PolicyChanges) != 1 || preview.PolicyChanges[0].After.Enabled {
		t.Fatal("explicit quiet change was not isolated from off", err)
	}
	before, _ := os.ReadFile(paths.ConfigFile)
	if _, err := p.Apply(ctx, in, paths, preview.ID); !errors.Is(err, ErrPolicyUpdatePending) {
		t.Fatal("unimplemented rule apply claimed success", err)
	}
	after, _ := os.ReadFile(paths.ConfigFile)
	if string(after) != string(before) {
		t.Fatal("policy pending path partially replaced config")
	}
}

func TestDevelopmentArtifactCanPreviewButCannotApply(t *testing.T) {
	p, in, paths := fixture(t)
	probe := p.Probe
	p.Probe = func(ctx context.Context, path string) (Environment, error) {
		e, err := probe(ctx, path)
		e.Artifact.Released = false
		return e, err
	}
	plan, err := p.Build(context.Background(), in, paths)
	if err != nil || plan.CanApply {
		t.Fatal("development release gate bypassed", err)
	}
	if _, err := p.Apply(context.Background(), in, paths, plan.ID); !errors.Is(err, ErrUnreleased) {
		t.Fatal("development artifact applied", err)
	}
	if _, err := os.Stat(paths.ConfigFile); !os.IsNotExist(err) {
		t.Fatal("release gate wrote official config")
	}
}

func TestDecodeRejectsUnknownAndTrailingJSON(t *testing.T) {
	for _, body := range []string{`{"schema_version":1,"secret":"must-not-be-accepted"}`, `{"schema_version":1} {}`, `{"schema_version":2}`} {
		if _, err := Decode(strings.NewReader(body)); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
}
