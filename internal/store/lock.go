package store

import (
	"errors"
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
	f, err := os.OpenFile(databasePath+".listener.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("deployment listener already running")
	}
	return f, nil
}
