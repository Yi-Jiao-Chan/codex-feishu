package store

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// AcquireListener prevents two event consumers from racing one deployment.
// Closing releases the OS lock; the lock file is never deleted or stolen.
func AcquireListener(databasePath string) (*os.File, error) {
	if !filepath.IsAbs(databasePath) {
		return nil, errors.New("lock path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(databasePath), 0700); err != nil {
		return nil, err
	}
	// Symlink spellings of the same database must share one lock inode.
	canonical, err := filepath.EvalSymlinks(databasePath)
	if errors.Is(err, os.ErrNotExist) {
		// A dangling file symlink can resolve to a different lock after the DB
		// is created. Refuse it rather than locking the alias spelling.
		if info, statErr := os.Lstat(databasePath); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("database symlink target must already exist")
		}
		parent, parentErr := filepath.EvalSymlinks(filepath.Dir(databasePath))
		if parentErr != nil {
			return nil, parentErr
		}
		canonical, err = filepath.Join(parent, filepath.Base(databasePath)), nil
	}
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(canonical+".listener.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("deployment listener already running")
	}
	return f, nil
}

// AcquireApplication guards the Feishu ws receiver independently of database
// choice. Pairing and normal service must share this per-user application lock.
// The fixed home location remains stable across different config/XDG paths.
func AcquireApplication(appID string) (*os.File, error) {
	if appID == "" {
		return nil, errors.New("application ID required")
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return nil, errors.New("user home unavailable")
	}
	dir := filepath.Join(home, ".local", "state", "codex-feishu", "locks")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, fmt.Sprintf("%x.application.lock", sha256.Sum256([]byte(appID))))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("application receiver already running")
	}
	return f, nil
}
