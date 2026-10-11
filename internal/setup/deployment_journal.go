package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"codex-feishu/internal/config"
)

// Policy changes concern a deployment, even when serve uses a copied or backup
// config. This pointer is durable before either configuration or SQLite writes.
type deploymentApply struct {
	Version                                         int
	ConfigFile, PlanID, HostID, AppID, DatabaseFile string
}

func deploymentDatabasePath(database string) (string, error) {
	if !filepath.IsAbs(database) {
		return "", ErrApplyPending
	}
	physical, err := filepath.EvalSymlinks(database)
	if errors.Is(err, os.ErrNotExist) {
		return canonicalTarget(database)
	}
	if err != nil {
		return "", ErrApplyPending
	}
	return physical, nil
}

func readDeploymentApply(database string) (deploymentApply, bool, error) {
	var marker deploymentApply
	physical, err := deploymentDatabasePath(database)
	if err != nil {
		return marker, true, ErrApplyPending
	}
	path := physical + ".setup-state.json"
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return marker, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return marker, true, ErrApplyPending
	}
	b, err := regularFile(path, false)
	if err != nil {
		return marker, true, ErrApplyPending
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	var extra any
	if d.Decode(&marker) != nil || !errors.Is(d.Decode(&extra), io.EOF) || marker.Version != 1 || !validDigest(marker.PlanID) || !filepath.IsAbs(marker.ConfigFile) || marker.DatabaseFile != physical || marker.HostID == "" || !config.ValidAppID(marker.AppID) {
		return marker, true, ErrApplyPending
	}
	canonical, err := canonicalTarget(marker.ConfigFile)
	if err != nil || canonical != marker.ConfigFile {
		return marker, true, ErrApplyPending
	}
	return marker, true, nil
}

func deploymentApplyComplete(marker deploymentApply) bool {
	j, found, err := readApplyJournal(marker.ConfigFile)
	if err != nil || !found {
		return false
	}
	physical, err := deploymentDatabasePath(j.DatabaseFile)
	return err == nil && j.Version == 1 && j.State == "applied" && j.PlanID == marker.PlanID && j.HostID == marker.HostID && j.AppID == marker.AppID && physical == marker.DatabaseFile
}

// Held application/database locks serialize pointer ownership. A pointer whose
// journal has not been written yet can only be resumed by its original plan.
func checkDeploymentApply(c config.Config, target, planID string) error {
	if err := checkMigrationReady(c.Storage.DatabaseFile, c.Host.ID, c.Feishu.AppID); err != nil {
		return err
	}
	marker, found, err := readDeploymentApply(c.Storage.DatabaseFile)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if marker.HostID != c.Host.ID || marker.AppID != c.Feishu.AppID {
		return ErrApplyPending
	}
	if deploymentApplyComplete(marker) {
		return nil
	}
	if marker.ConfigFile != target || marker.PlanID != planID {
		return ErrApplyPending
	}
	return nil
}

func clearDeploymentApply(database string) error {
	physical, err := deploymentDatabasePath(database)
	if err != nil {
		return ErrApplyPending
	}
	if err := os.Remove(physical + ".setup-state.json"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrApplyPending
	}
	d, err := os.Open(filepath.Dir(physical))
	if err != nil {
		return ErrApplyPending
	}
	defer d.Close()
	if d.Sync() != nil {
		return ErrApplyPending
	}
	return nil
}
