package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestExistingUpdateDoesNotCreateMigrateOrChangePermissions(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "legacy # rw.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE fixture(value TEXT); INSERT INTO fixture VALUES('before'); PRAGMA user_version=1;`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if db, err := OpenExisting(ctx, path, SchemaVersion); err == nil {
		db.Close()
		t.Fatal("implicitly migrated a legacy database")
	}
	db, err := OpenExisting(ctx, path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `UPDATE fixture SET value='after'`); err != nil {
		t.Fatal(err)
	}
	var version, tables int
	if err := db.DB.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil || version != 1 {
		t.Fatal("schema changed", version, err)
	}
	if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table'`).Scan(&tables); err != nil || tables != 1 {
		t.Fatal("opener created tables", tables, err)
	}
	db.Close()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("permissions changed", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.db")
	if db, err := OpenExisting(ctx, missing, 1); err == nil {
		db.Close()
		t.Fatal("missing opened")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("created missing database", err)
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	if db, err := OpenExisting(ctx, path, 1); err == nil {
		db.Close()
		t.Fatal("accepted nonprivate file")
	}
	info, _ = os.Stat(path)
	if info.Mode().Perm() != 0640 {
		t.Fatal("repaired permissions implicitly")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias.db")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	if db, err := OpenExisting(ctx, alias, 1); err == nil {
		db.Close()
		t.Fatal("accepted final symlink")
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), 0777); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Dir(path), 0700)
	if db, err := OpenExisting(ctx, path, 1); err == nil {
		db.Close()
		t.Fatal("accepted private file in shared writable deployment directory")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("directory rejection changed database", err)
	}
}
