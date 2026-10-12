package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"codex-feishu/internal/store"
)

// CheckCandidateDatabase never creates or migrates a database. A candidate may
// initialize a fresh database or use an existing private, current-schema one.
// Start callers must repeat this check under both receiver locks before opening
// the store; preflight does not reserve the deployment. SQLite read-only access
// may create reader sidecars, but does not change rows, schema or permissions.
func CheckCandidateDatabase(ctx context.Context, path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("candidate database path must be absolute")
	}
	info, err := os.Lstat(path)
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return errors.New("candidate database unavailable")
	}
	directory, err := os.Stat(filepath.Dir(path))
	if missing && errors.Is(err, os.ErrNotExist) {
		return nil // Store.Open will create the deployment directory restrictively.
	}
	if err != nil || !directory.IsDir() || directory.Mode().Perm() != 0700 {
		return errors.New("candidate database directory must be private")
	}
	directoryOwner, ok := directory.Sys().(*syscall.Stat_t)
	if !ok || int(directoryOwner.Uid) != os.Geteuid() {
		return errors.New("candidate database directory must belong to the current user")
	}
	if missing {
		return nil
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !ok || int(owner.Uid) != os.Geteuid() {
		return errors.New("candidate requires a private regular database owned by the current user")
	}
	db, version, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		return errors.New("candidate database schema could not be verified")
	}
	defer db.Close()
	if version != store.SchemaVersion {
		return errors.New("candidate database must have the current schema; migration is disabled")
	}
	return nil
}
