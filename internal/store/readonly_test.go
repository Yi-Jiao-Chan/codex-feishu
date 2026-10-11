package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestReadOnlyPreservesLegacySchemaAndPermissions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy # name.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE fixture(value TEXT); INSERT INTO fixture VALUES('original'); PRAGMA user_version=1;`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	s, version, err := OpenReadOnly(ctx, path)
	if err != nil || version != 1 {
		t.Fatal("legacy read failed", err)
	}
	defer s.Close()
	var value string
	if err := s.DB.QueryRowContext(ctx, `SELECT value FROM fixture`).Scan(&value); err != nil || value != "original" {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `CREATE TABLE forbidden(value TEXT)`); err == nil {
		t.Fatal("read-only store allowed a write")
	}
	if err := s.DB.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatal("read-only open migrated legacy database", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatal("read-only open changed file permissions", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.db")
	if db, _, err := OpenReadOnly(ctx, missing); err == nil {
		db.Close()
		t.Fatal("nonexistent database opened")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("read-only open created missing database", err)
	}
}
