package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Keep only immutable bytes after closing the fully migrated source database.
// Every fixture still opens its own file through the production Open path.
var emptyStoreSnapshot = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "codex-feishu-store-fixture-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "empty.db")
	s, err := Open(path)
	if err != nil {
		return nil, err
	}
	var busy, logged, checkpointed int
	checkpointErr := s.DB.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logged, &checkpointed)
	closeErr := s.Close()
	if checkpointErr != nil {
		return nil, checkpointErr
	}
	if busy != 0 || logged != checkpointed {
		return nil, fmt.Errorf("fixture checkpoint incomplete: busy=%d logged=%d checkpointed=%d", busy, logged, checkpointed)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return os.ReadFile(path)
})

func freshStorePath(t *testing.T, filename string) string {
	t.Helper()
	snapshot, err := emptyStoreSnapshot()
	if err != nil {
		t.Fatalf("initialize empty store fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), filename)
	if err := os.WriteFile(path, snapshot, 0600); err != nil {
		t.Fatalf("copy empty store fixture: %v", err)
	}
	return path
}

func TestStoreFixtureCopiesAreIsolated(t *testing.T) {
	t.Run("concurrent copies", func(t *testing.T) {
		for _, name := range []string{"first", "second"} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				path := freshStorePath(t, "isolated.db")
				s, err := Open(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { s.Close() })
				if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
					t.Fatal("fixture database must remain private", err)
				}
				var journal string
				if err := s.DB.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
					t.Fatal("fixture did not enable WAL", journal, err)
				}
				if _, err := s.DB.Exec(`INSERT INTO thread_observations(host_id,thread_id,origin,state) VALUES('missing-host','thread','new','pending')`); err == nil {
					t.Fatal("fixture did not enforce foreign keys")
				}
				if fresh, err := s.Begin(context.Background(), "same-key", "fixture"); err != nil || !fresh {
					t.Fatal("concurrent fixture shared another copy's write", fresh, err)
				}
			})
		}
	})
	path := freshStorePath(t, "later.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var count int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM m0_operations").Scan(&count); err != nil || count != 0 {
		t.Fatal("prior fixture writes contaminated a later copy", count, err)
	}
}

func TestOpenFreshDatabaseCreatesCurrentSchema(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err := s.DB.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatal("fresh database did not migrate to the current schema", version, err)
	}
	var integrity string
	if err := s.DB.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatal("fresh schema failed its integrity check", integrity, err)
	}
}
