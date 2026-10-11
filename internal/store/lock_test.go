package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListenerLockCanonicalizesDatabaseSymlinkAliases(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	first, err := AcquireListener(filepath.Join(real, "new.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if another, err := AcquireListener(filepath.Join(alias, "new.db")); err == nil {
		another.Close()
		t.Fatal("directory alias acquired a second listener")
	}
	target := filepath.Join(real, "new.db")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	fileAlias := filepath.Join(root, "db-alias")
	if err := os.Symlink(target, fileAlias); err != nil {
		t.Fatal(err)
	}
	if another, err := AcquireListener(fileAlias); err == nil {
		another.Close()
		t.Fatal("database alias acquired a second listener")
	}
}

func TestListenerLockRejectsDanglingDatabaseSymlink(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(root, "alias.db")
	if err := os.Symlink(filepath.Join(root, "future.db"), alias); err != nil {
		t.Fatal(err)
	}
	if lock, err := AcquireListener(alias); err == nil {
		lock.Close()
		t.Fatal("dangling alias could change lock identity after database creation")
	}
}
