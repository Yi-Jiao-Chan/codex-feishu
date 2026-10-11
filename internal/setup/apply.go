package setup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"codex-feishu/internal/store"
)

type ApplyResult struct {
	PlanID     string `json:"plan_id"`
	ConfigFile string `json:"config_file"`
	BackupFile string `json:"backup_file,omitempty"`
	Changed    bool   `json:"changed"`
	State      string `json:"state"`
}

// The journal precedes config replacement. A crash after rename can be
// reconciled from its expected bytes without applying an old plan twice.
type applyJournal struct {
	PlanID, SourceDigest, BeforeDigest, AfterDigest, BackupFile, State string
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
	if planID == "" {
		return ApplyResult{}, ErrStalePlan
	}
	target, err := canonicalTarget(paths.ConfigFile)
	if err != nil {
		return ApplyResult{}, err
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
	if !plan.Environment.Artifact.Released {
		return ApplyResult{}, ErrUnreleased
	}
	if len(plan.PolicyChanges) > 0 {
		return ApplyResult{}, ErrPolicyUpdatePending
	}
	result := ApplyResult{PlanID: planID, ConfigFile: plan.Paths.ConfigFile, State: "configuration-applied"}
	journalPath := target + ".setup-state.json"
	if info, err := os.Lstat(journalPath); err == nil && !info.Mode().IsRegular() {
		return result, errors.New("setup journal must be regular")
	}
	var old applyJournal
	if b, err := regularFile(journalPath, false); err == nil {
		if json.Unmarshal(b, &old) != nil {
			return result, errors.New("setup journal is invalid; preserve it for recovery")
		}
		if old.PlanID == planID && old.SourceDigest == plan.sourceDigest && old.AfterDigest == plan.previousDigest {
			old.State = "applied"
			if err := atomicFile(journalPath, encoded(old)); err != nil {
				return result, err
			}
			result.State, result.BackupFile = "configuration-unchanged", old.BackupFile
			return result, nil
		}
	} else if _, statErr := os.Lstat(journalPath); !errors.Is(statErr, os.ErrNotExist) {
		return result, errors.New("setup journal unavailable")
	}
	if plan.ID != planID {
		return result, ErrStalePlan
	}
	if !plan.Changed {
		result.State = "configuration-unchanged"
		return result, nil
	}
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
	journal := applyJournal{PlanID: planID, SourceDigest: plan.sourceDigest, BeforeDigest: plan.previousDigest, AfterDigest: digest(plan.content), State: "prepared"}
	if plan.previousDigest != "missing" {
		journal.BackupFile = target + ".backup." + planID[:16]
		if b, err := regularFile(journal.BackupFile, false); err == nil {
			if digest(b) != plan.previousDigest {
				return result, errors.New("existing setup backup conflicts with this plan")
			}
		} else if _, statErr := os.Lstat(journal.BackupFile); !errors.Is(statErr, os.ErrNotExist) {
			return result, errors.New("setup backup unavailable")
		} else if err := atomicFile(journal.BackupFile, plan.previous); err != nil {
			return result, err
		}
	}
	if err := atomicFile(journalPath, encoded(journal)); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	current, readErr := regularFile(target, false)
	currentDigest := digest(current)
	if readErr != nil {
		if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
			return result, ErrStalePlan
		}
		currentDigest = "missing"
	}
	if currentDigest != plan.previousDigest {
		return result, ErrStalePlan
	}
	if err := atomicFile(target, plan.content); err != nil {
		return result, err
	}
	journal.State = "applied"
	result.Changed, result.BackupFile = true, journal.BackupFile
	if err := atomicFile(journalPath, encoded(journal)); err != nil {
		return result, errors.New("configuration saved; repeat the same plan to reconcile its journal")
	}
	return result, nil
}
