package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
)

// OpenReadOnly never creates, chmods or migrates a database. It is safe for
// diagnostics and configuration preview alongside an older running listener.
func OpenReadOnly(ctx context.Context, path string) (*Store, int, error) {
	if !filepath.IsAbs(path) {
		return nil, 0, errors.New("database must be absolute")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, 0, errors.New("existing database unavailable")
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, 0, errors.New("read-only database unavailable")
	}
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, `PRAGMA query_only=ON; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, 0, errors.New("read-only database unavailable")
	}
	var version int
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version < 1 || version > SchemaVersion {
		db.Close()
		return nil, version, errors.New("database schema is unsupported")
	}
	return &Store{DB: db}, version, nil
}
