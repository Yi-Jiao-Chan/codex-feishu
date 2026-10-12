package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"codex-feishu/internal/store"
)

func candidateFixture(t *testing.T) (Planner, Input, Paths) {
	t.Helper()
	p, in, paths := fixture(t)
	probe := p.Probe
	p.Probe = func(ctx context.Context, binary string) (Environment, error) {
		e, err := probe(ctx, binary)
		e.Artifact.Released = false
		e.Artifact.Capabilities.CandidateDeployment = true
		return e, err
	}
	p.CandidateTest = true
	return p, in, paths
}

func TestCandidateRequiresExplicitModeAndKeepsReleasePending(t *testing.T) {
	ctx := context.Background()
	p, in, paths := candidateFixture(t)
	normal := p
	normal.CandidateTest = false
	preview, err := normal.Build(ctx, in, paths)
	if err != nil || preview.CanApply {
		t.Fatal("default development gate changed", err)
	}
	plan, err := p.Build(ctx, in, paths)
	if err != nil || !plan.CanApply || !plan.CandidateTest || plan.Environment.Artifact.Released || plan.ID == preview.ID {
		t.Fatal("candidate was not explicitly scoped", plan, err)
	}
	result, err := p.Apply(ctx, in, paths, plan.ID)
	if err != nil || !result.CandidateTest || !result.Changed {
		t.Fatal("candidate apply failed", result, err)
	}
	result, err = p.Apply(ctx, in, paths, plan.ID)
	if err != nil || result.Changed || result.State != "configuration-unchanged" {
		t.Fatal("candidate replay was not a no-op", result, err)
	}
	if _, err := normal.Apply(ctx, in, paths, plan.ID); !errors.Is(err, ErrUnreleased) {
		t.Fatal("candidate authorized ordinary development apply", err)
	}
	if _, err := os.Stat(paths.DatabaseFile); !os.IsNotExist(err) {
		t.Fatal("candidate setup created database")
	}
}

func TestCandidateUnsupportedArtifactRejectedBeforeSetupWrites(t *testing.T) {
	ctx := context.Background()
	p, in, paths := candidateFixture(t)
	plan, err := p.Build(ctx, in, paths)
	if err != nil {
		t.Fatal(err)
	}
	probe := p.Probe
	p.Probe = func(ctx context.Context, binary string) (Environment, error) {
		e, err := probe(ctx, binary)
		e.Artifact.Capabilities.CandidateDeployment = false
		return e, err
	}
	if _, err := p.Apply(ctx, in, paths, plan.ID); err == nil {
		t.Fatal("older binary accepted")
	}
	if _, err := os.Stat(filepath.Dir(paths.ConfigFile)); !os.IsNotExist(err) {
		t.Fatal("unsupported artifact created setup directory")
	}
}

func TestCandidateRecoversOriginalPreparedTransactionAndKeepsDatabasePath(t *testing.T) {
	ctx := context.Background()
	p, in, paths := candidateFixture(t)
	plan, err := p.Build(ctx, in, paths)
	if err != nil {
		t.Fatal(err)
	}
	p.checkpoint = func(stage string) error {
		if stage == "configuration" {
			return errors.New("fixture interruption")
		}
		return nil
	}
	if _, err := p.Apply(ctx, in, paths, plan.ID); !errors.Is(err, ErrApplyPending) {
		t.Fatal("candidate interruption not recorded", err)
	}
	before, _ := os.ReadFile(paths.ConfigFile)
	p.checkpoint = nil
	result, err := p.Apply(ctx, in, paths, plan.ID)
	if err != nil || !result.CandidateTest || result.Changed {
		t.Fatal("candidate did not reconcile original rename", result, err)
	}
	if err := CheckApplyReady(paths.ConfigFile); err != nil {
		t.Fatal("candidate transaction still blocks startup", err)
	}
	other := paths
	other.DatabaseFile = filepath.Join(t.TempDir(), "different.db")
	if _, err := p.Build(ctx, in, other); err == nil {
		t.Fatal("candidate ignored explicit database mismatch")
	}
	after, _ := os.ReadFile(paths.ConfigFile)
	if string(before) != string(after) {
		t.Fatal("candidate recovery/mismatched path rewrote config")
	}
}

func TestCandidateModeCannotReplayOrdinaryOrRecoverDifferentMode(t *testing.T) {
	ctx := context.Background()
	for _, prepared := range []bool{false, true} {
		t.Run(fmt.Sprint(prepared), func(t *testing.T) {
			p, in, paths := fixture(t)
			probe := p.Probe
			p.Probe = func(ctx context.Context, binary string) (Environment, error) {
				e, err := probe(ctx, binary)
				e.Artifact.Capabilities.CandidateDeployment = true
				return e, err
			}
			plan, err := p.Build(ctx, in, paths)
			if err != nil {
				t.Fatal(err)
			}
			candidate := p
			candidate.CandidateTest = true
			if _, err := candidate.Apply(ctx, in, paths, plan.ID); !errors.Is(err, ErrStalePlan) {
				t.Fatal("normal plan reused for candidate", err)
			}
			if prepared {
				p.checkpoint = func(stage string) error {
					if stage == "configuration" {
						return errors.New("fixture interruption")
					}
					return nil
				}
			}
			_, err = p.Apply(ctx, in, paths, plan.ID)
			if prepared && !errors.Is(err, ErrApplyPending) || !prepared && err != nil {
				t.Fatal("fixture apply", err)
			}
			before, _ := os.ReadFile(paths.ConfigFile)
			if _, err := candidate.Apply(ctx, in, paths, plan.ID); !errors.Is(err, ErrStalePlan) {
				t.Fatal("normal journal recovered with candidate mode", err)
			}
			after, _ := os.ReadFile(paths.ConfigFile)
			if string(before) != string(after) {
				t.Fatal("mode mismatch replaced config")
			}
			p.checkpoint = nil
			if _, err := p.Apply(ctx, in, paths, plan.ID); err != nil {
				t.Fatal("original mode could not recover", err)
			}
		})
	}
}

func TestCandidateDatabaseRejectsOldSchemaPermissionsAndSymlinks(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"missing", "current", "legacy", "older", "permissions", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "bridge.db")
			if kind != "missing" {
				db, err := store.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "legacy" || kind == "older" {
					version := 1
					if kind == "older" {
						version = store.SchemaVersion - 1
					}
					if _, err := db.DB.Exec(fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
						t.Fatal(err)
					}
				}
				db.Close()
			}
			if kind == "permissions" {
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "directory" {
				if err := os.Chmod(root, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "symlink" {
				alias := path + ".alias"
				if err := os.Symlink(path, alias); err != nil {
					t.Fatal(err)
				}
				path = alias
			}
			before, _ := os.ReadFile(path)
			err := CheckCandidateDatabase(ctx, path)
			allowed := kind == "missing" || kind == "current"
			if (err == nil) != allowed {
				t.Fatal("incorrect candidate schema decision", kind, err)
			}
			after, _ := os.ReadFile(path)
			if string(before) != string(after) {
				t.Fatal("preflight changed database")
			}
		})
	}
}

func TestCandidateApplyRetainsApplicationAndDatabaseLocks(t *testing.T) {
	ctx := context.Background()
	for _, application := range []bool{false, true} {
		t.Run(fmt.Sprint(application), func(t *testing.T) {
			p, in, paths := candidateFixture(t)
			plan, err := p.Build(ctx, in, paths)
			if err != nil {
				t.Fatal(err)
			}
			var lock *os.File
			if application {
				lock, err = store.AcquireApplication(in.Feishu.AppID)
			} else {
				lock, err = store.AcquireListener(paths.DatabaseFile)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if _, err := p.Apply(ctx, in, paths, plan.ID); err == nil {
				t.Fatal("candidate bypassed running bridge lock")
			}
			if _, err := os.Stat(paths.ConfigFile); !os.IsNotExist(err) {
				t.Fatal("candidate replaced config beside live bridge")
			}
		})
	}
}
