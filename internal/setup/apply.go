package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"codex-feishu/internal/store"
)

type ApplyResult struct {
	PlanID        string `json:"plan_id"`
	ConfigFile    string `json:"config_file"`
	BackupFile    string `json:"backup_file,omitempty"`
	Changed       bool   `json:"changed"`
	State         string `json:"state"`
	CandidateTest bool   `json:"candidate_test_deployment,omitempty"`
}

// The journal precedes config replacement. A crash after rename can be
// reconciled from its expected bytes without applying an old plan twice.
type applyJournal struct {
	Version                                                            int
	PlanID, SourceDigest, BeforeDigest, AfterDigest, BackupFile, State string
	HostID, AppID, DatabaseFile                                        string
	PolicyChanges                                                      []PolicyChange
	ExistingPolicies                                                   []PolicyView
	DatabaseSchema                                                     int
}

func atomicFile(path string, body []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".codex-feishu-setup-*")
	if err != nil {
		return errors.New("setup temporary file unavailable")
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(body)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.New("setup file could not be saved")
	}
	if err = os.Rename(name, path); err != nil {
		return errors.New("setup atomic replacement failed")
	}
	d, err := os.Open(dir)
	if err != nil {
		return errors.New("setup directory durability is uncertain")
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return errors.New("setup directory durability is uncertain")
	}
	return nil
}

func configDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return errors.New("configuration directory unavailable")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("configuration directory must be physical")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return errors.New("configuration directory must belong to this user")
	}
	// Only this tool's chosen configuration directory is secured, not HOME or
	// a common XDG parent. Refuse dangerous shared targets instead of chmodding.
	if path == filepath.Dir(path) || path == os.Getenv("HOME") || info.Mode().Perm() != 0700 {
		return errors.New("configuration directory must be a private deployment directory")
	}
	return nil
}

func (p Planner) Apply(ctx context.Context, in Input, paths Paths, planID string) (ApplyResult, error) {
	if !validDigest(planID) {
		return ApplyResult{}, ErrStalePlan
	}
	target, err := canonicalTarget(paths.ConfigFile)
	if err != nil {
		return ApplyResult{}, err
	}
	if p.CandidateTest {
		// Reject unsupported binaries/legacy databases before creating setup files.
		if _, err := p.Build(ctx, in, paths); err != nil {
			return ApplyResult{}, err
		}
	}
	if err := configDirectory(filepath.Dir(target)); err != nil {
		return ApplyResult{}, err
	}
	lockPath := target + ".setup.lock"
	if info, err := os.Lstat(lockPath); err == nil && !info.Mode().IsRegular() {
		return ApplyResult{}, errors.New("setup lock must be regular")
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return ApplyResult{}, errors.New("setup lock unavailable")
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return ApplyResult{}, errors.New("setup configuration is being changed")
	}
	plan, err := p.Build(ctx, in, paths)
	if err != nil {
		return ApplyResult{}, err
	}
	if !plan.CanApply {
		return ApplyResult{}, ErrUnreleased
	}
	result := ApplyResult{PlanID: planID, ConfigFile: target, State: "configuration-applied", CandidateTest: p.CandidateTest}
	old, found, err := readApplyJournal(target)
	if err != nil {
		return result, err
	}
	if found && old.State == "prepared" && old.PlanID != planID {
		return result, ErrApplyPending
	}
	// All replay/recovery and policy-only updates acquire both bridge locks.
	// No early journal shortcut may finalize a transaction beside a live bridge.
	applicationLock, err := store.AcquireApplication(plan.Config.Feishu.AppID)
	if err != nil {
		return result, errors.New("stop this application's running bridge before changing configuration")
	}
	defer applicationLock.Close()
	databaseLock, err := store.AcquireListener(plan.Config.Storage.DatabaseFile)
	if err != nil {
		return result, errors.New("stop the running bridge before changing configuration")
	}
	defer databaseLock.Close()
	// Rebuild within receiver locks; the preview was not a write reservation.
	plan, err = p.Build(ctx, in, paths)
	if err != nil {
		return result, err
	}
	if !plan.CanApply {
		return result, ErrUnreleased
	}
	if err := checkDeploymentApply(plan.Config, target, planID); err != nil {
		return result, err
	}
	journal := applyJournal{Version: 1, PlanID: planID, SourceDigest: plan.sourceDigest, BeforeDigest: plan.previousDigest, AfterDigest: digest(plan.content), HostID: plan.Config.Host.ID, AppID: plan.Config.Feishu.AppID, DatabaseFile: plan.Config.Storage.DatabaseFile, PolicyChanges: plan.PolicyChanges, ExistingPolicies: plan.ExistingPolicies, DatabaseSchema: plan.DatabaseSchema, State: "prepared"}
	replay := found && old.PlanID == planID
	if replay {
		if err := validateJournalPlan(old, plan, in); err != nil {
			return result, err
		}
		journal = old
	} else {
		if plan.ID != planID {
			return result, ErrStalePlan
		}
		if !plan.Changed {
			result.State = "configuration-unchanged"
			return result, nil
		}
	}
	var db *store.Store
	beforePolicies := true
	if len(journal.PolicyChanges) > 0 {
		db, err = store.OpenExisting(ctx, journal.DatabaseFile, store.SchemaVersion)
		if err != nil {
			return result, errors.New("policy update requires a private existing current-schema database; nothing migrated")
		}
		defer db.Close()
		beforePolicies, err = journalPolicyState(ctx, db, journal)
		if err != nil {
			return result, ErrStalePlan
		}
		if replay && journal.State == "applied" && beforePolicies {
			return result, ErrStalePlan
		}
	}
	currentDigest := plan.previousDigest
	if replay && journal.State == "applied" {
		if currentDigest != journal.AfterDigest {
			return result, ErrStalePlan
		}
		result.State, result.BackupFile = "configuration-unchanged", journal.BackupFile
		if _, found, err := readDeploymentApply(plan.Config.Storage.DatabaseFile); err != nil {
			return result, err
		} else if found {
			if err := clearDeploymentApply(plan.Config.Storage.DatabaseFile); err != nil {
				return result, err
			}
		}
		return result, nil
	}
	if !replay {
		if journal.BeforeDigest != "missing" && journal.BeforeDigest != journal.AfterDigest {
			journal.BackupFile = target + ".backup." + planID[:16]
			if b, err := regularFile(journal.BackupFile, false); err == nil {
				if digest(b) != journal.BeforeDigest {
					return result, errors.New("existing setup backup conflicts with this plan")
				}
			} else if _, statErr := os.Lstat(journal.BackupFile); !errors.Is(statErr, os.ErrNotExist) {
				return result, errors.New("setup backup unavailable")
			} else if err := atomicFile(journal.BackupFile, plan.previous); err != nil {
				return result, err
			}
		}
		// Retire a completed pointer before replacing its referenced journal.
		marker, exists, err := readDeploymentApply(journal.DatabaseFile)
		if err != nil {
			return result, err
		}
		if exists && deploymentApplyComplete(marker) {
			if err := clearDeploymentApply(journal.DatabaseFile); err != nil {
				return result, err
			}
		}
		if db != nil {
			physical, err := deploymentDatabasePath(journal.DatabaseFile)
			if err != nil {
				return result, ErrApplyPending
			}
			marker := deploymentApply{Version: 1, ConfigFile: target, PlanID: planID, HostID: journal.HostID, AppID: journal.AppID, DatabaseFile: physical}
			if err := atomicFile(physical+".setup-state.json", encoded(marker)); err != nil {
				return result, ErrApplyPending
			}
			if err := p.applyCheckpoint(ctx, "deployment"); err != nil {
				return result, ErrApplyPending
			}
		}
		if err := atomicFile(target+".setup-state.json", encoded(journal)); err != nil {
			return result, err
		}
	}
	if err := p.applyCheckpoint(ctx, "prepared"); err != nil {
		return result, ErrApplyPending
	}
	// A current after-policy with a before-config cannot be produced by this
	// protocol; reject outside edits instead of inventing a recovery order.
	if !beforePolicies && currentDigest != journal.AfterDigest {
		return result, ErrStalePlan
	}
	if currentDigest == journal.BeforeDigest && currentDigest != journal.AfterDigest {
		latest, readErr := regularFile(target, false)
		latestDigest := digest(latest)
		if readErr != nil {
			if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
				return result, ErrStalePlan
			}
			latestDigest = "missing"
		}
		if latestDigest != currentDigest {
			return result, ErrStalePlan
		}
		if err := atomicFile(target, plan.content); err != nil {
			return result, ErrApplyPending
		}
		result.Changed = true
	} else if currentDigest != journal.AfterDigest {
		return result, ErrStalePlan
	}
	if err := p.applyCheckpoint(ctx, "configuration"); err != nil {
		return result, ErrApplyPending
	}
	if db != nil {
		changes := make([]store.NotificationPolicyChange, 0, len(journal.PolicyChanges))
		for _, change := range journal.PolicyChanges {
			changes = append(changes, store.NotificationPolicyChange{OpenID: change.OpenID, Before: change.Before, After: change.After})
		}
		changed, err := db.ApplyNotificationPolicies(ctx, journal.HostID, journal.AppID, changes, time.Now())
		if err != nil {
			return result, ErrApplyPending
		}
		result.Changed = result.Changed || changed
	}
	if err := p.applyCheckpoint(ctx, "policies"); err != nil {
		return result, ErrApplyPending
	}
	journal.State = "applied"
	if err := atomicFile(target+".setup-state.json", encoded(journal)); err != nil {
		return result, ErrApplyPending
	}
	if err := p.applyCheckpoint(ctx, "applied"); err != nil {
		return result, ErrApplyPending
	}
	if db != nil {
		if err := clearDeploymentApply(journal.DatabaseFile); err != nil {
			return result, err
		}
	}
	result.BackupFile = journal.BackupFile
	if !result.Changed {
		result.State = "configuration-unchanged"
	}
	return result, nil
}

func (p Planner) applyCheckpoint(ctx context.Context, stage string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.checkpoint != nil {
		return p.checkpoint(stage)
	}
	return nil
}
