package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
)

// OpenExisting opens a private existing database for an explicitly authorized
// local update. The caller must hold the deployment locks. It never creates,
// chmods or migrates the database; a different schema requires separate migration.
func OpenExisting(ctx context.Context, path string, requiredSchema int) (*Store, error) {
	if !filepath.IsAbs(path) || requiredSchema < 1 || requiredSchema > SchemaVersion {
		return nil, errors.New("existing database input invalid")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, errors.New("private deployment directory unavailable")
	}
	directory, err := os.Stat(parent)
	if err != nil || !directory.IsDir() || directory.Mode().Perm() != 0700 {
		return nil, errors.New("private deployment directory unavailable")
	}
	directoryOwner, ok := directory.Sys().(*syscall.Stat_t)
	if !ok || directoryOwner.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("deployment directory owner invalid")
	}
	path = filepath.Join(parent, filepath.Base(path))
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("private existing database unavailable")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("existing database owner invalid")
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=rw"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, errors.New("existing database unavailable")
	}
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, `PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, errors.New("existing database unavailable")
	}
	var schema int
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&schema); err != nil || schema != requiredSchema {
		db.Close()
		return nil, errors.New("existing database schema differs; explicit migration required")
	}
	return &Store{DB: db}, nil
}
